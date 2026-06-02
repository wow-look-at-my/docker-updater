package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

//go:embed dashboard/index.html dashboard/dashboard.css dashboard/dashboard.js
var dashboardAssets embed.FS

// dashboardServer serves the read-only status dashboard and JSON API.
type dashboardServer struct {
	cli   DockerClient
	cfg   Config
	store *Store
}

func newDashboardServer(cli DockerClient, cfg Config, store *Store) *dashboardServer {
	return &dashboardServer{cli: cli, cfg: cfg, store: store}
}

// handler builds the HTTP routes: the JSON API, a health probe, and the
// embedded static assets.
func (s *dashboardServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/containers", s.handleAPIContainers)
	mux.HandleFunc("/healthz", s.handleHealthz)

	sub, err := fs.Sub(dashboardAssets, "dashboard")
	if err != nil {
		// The embed path is a compile-time constant; this cannot fail at runtime.
		log.Printf("dashboard: failed to mount assets: %v", err)
		return mux
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

// run starts the dashboard server and blocks until the context is cancelled. A
// failure to bind is logged but never fatal: the update loop keeps running.
func (s *dashboardServer) run(ctx context.Context) {
	srv := &http.Server{
		Addr:              s.cfg.DashboardAddr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("dashboard listening on %s", s.cfg.DashboardAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("dashboard server error on %s: %v (set DOCKER_UPDATER_DASHBOARD_ADDR to change the address, or empty to disable)", s.cfg.DashboardAddr, err)
	}
}

func (s *dashboardServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// apiContainer is the JSON shape for a single container row in the dashboard.
type apiContainer struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Health  string `json:"health"`
	Created int64  `json:"created"`

	// Restarts is Docker's RestartCount: how many times the daemon's restart
	// policy has restarted the container since it was created. docker-updater
	// creates a fresh container on every pull/update, so for monitored
	// containers this is effectively "restarts since the last pull". nil when
	// the container could not be inspected.
	Restarts *int `json:"restarts,omitempty"`

	AutoUpdate bool   `json:"auto_update"`
	Mode       string `json:"mode,omitempty"`

	LastChecked     *time.Time `json:"last_checked,omitempty"`
	LastPulled      *time.Time `json:"last_pulled,omitempty"`
	LastUpdated     *time.Time `json:"last_updated,omitempty"`
	UpdateAvailable bool       `json:"update_available"`
	CurrentRef      string     `json:"current_ref,omitempty"`
	AvailableRef    string     `json:"available_ref,omitempty"`
	Error           string     `json:"error,omitempty"`
	Skipped         bool       `json:"skipped,omitempty"`
	SkipReason      string     `json:"skip_reason,omitempty"`
}

// apiResponse is the top-level JSON payload served at /api/containers.
type apiResponse struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Interval    string         `json:"interval"`
	DryRun      bool           `json:"dry_run"`
	Label       string         `json:"label"`
	LastCycle   *time.Time     `json:"last_cycle,omitempty"`
	NextCycle   *time.Time     `json:"next_cycle,omitempty"`
	Containers  []apiContainer `json:"containers"`
}

func (s *dashboardServer) handleAPIContainers(w http.ResponseWriter, r *http.Request) {
	list, err := s.cli.ContainerList(r.Context(), container.ListOptions{All: true})
	if err != nil {
		http.Error(w, "failed to list containers: "+err.Error(), http.StatusInternalServerError)
		return
	}

	snap := s.store.Snapshot()

	resp := apiResponse{
		GeneratedAt: time.Now(),
		Interval:    s.cfg.Interval.String(),
		DryRun:      s.cfg.DryRun,
		Label:       s.cfg.Label,
	}
	if !snap.LastCycle.IsZero() {
		lc := snap.LastCycle
		nc := lc.Add(s.cfg.Interval)
		resp.LastCycle = &lc
		resp.NextCycle = &nc
	}

	for _, c := range list {
		name := containerName(c.Names)

		ac := apiContainer{
			Name:       name,
			Image:      c.Image,
			ImageID:    shortRef(c.ImageID),
			State:      c.State,
			Status:     c.Status,
			Health:     parseHealth(c.Status),
			Created:    c.Created,
			AutoUpdate: c.Labels[s.cfg.Label] == "true",
			Restarts:   restartCount(r.Context(), s.cli, c.ID),
		}

		if ac.AutoUpdate {
			ac.Mode = c.Labels["docker-updater.mode"]
			if ac.Mode == "" {
				ac.Mode = string(UpdateModeImage)
			}
		}

		if st, ok := snap.Statuses[name]; ok {
			ac.UpdateAvailable = st.UpdateAvailable
			ac.CurrentRef = st.CurrentRef
			ac.AvailableRef = st.AvailableRef
			ac.Error = st.LastError
			ac.Skipped = st.Skipped
			ac.SkipReason = st.SkipReason
			ac.LastChecked = nonZeroTime(st.LastChecked)
			ac.LastPulled = nonZeroTime(st.LastPulled)
			ac.LastUpdated = nonZeroTime(st.LastUpdated)
		}

		resp.Containers = append(resp.Containers, ac)
	}

	// Stable, predictable ordering: monitored containers first, then by name.
	sort.SliceStable(resp.Containers, func(i, j int) bool {
		a, b := resp.Containers[i], resp.Containers[j]
		if a.AutoUpdate != b.AutoUpdate {
			return a.AutoUpdate
		}
		return a.Name < b.Name
	})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		log.Printf("dashboard: failed to encode response: %v", err)
	}
}

// restartCount inspects a container and returns its Docker RestartCount, or nil
// if the container cannot be inspected. RestartCount is not exposed by the
// container-list endpoint, so a per-container inspect is required.
func restartCount(ctx context.Context, cli DockerClient, id string) *int {
	inspect, err := cli.ContainerInspect(ctx, id)
	if err != nil || inspect.ContainerJSONBase == nil {
		return nil
	}
	n := inspect.RestartCount
	return &n
}

func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

// parseHealth extracts the healthcheck state encoded in a docker ps status
// string, e.g. "Up 2 hours (healthy)".
func parseHealth(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "health: starting"):
		return "starting"
	default:
		return ""
	}
}

func nonZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

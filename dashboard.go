package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// dashboard.js is tsc output from dashboard.ts, and it is gitignored: a
// committed copy is a second source of truth that drifts, with nothing to mark
// the moment it stops matching. The embed below needs it on disk, so generate
// produces it and a build without generate fails to compile rather than
// shipping whatever happened to be lying there.
//go:generate npm --prefix dashboard ci
//go:generate npm --prefix dashboard run build

//go:embed dashboard/index.html dashboard/dashboard.css dashboard/dashboard.js dashboard/favicon.svg
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
	mux.HandleFunc("/", staticAssetHandler())
	return mux
}

// staticAsset is one embedded dashboard file plus its precomputed validator.
type staticAsset struct {
	body        []byte
	contentType string
	etag        string // strong ETag: quoted hex SHA-256 of body
}

// assetVersionLen is how many hex chars of an asset's SHA-256 go into the
// ?v= stamp index.html references it by. 12 (48 bits) is far beyond any
// realistic collision between deploys while keeping URLs readable.
const assetVersionLen = 12

// staticAssetHandler serves the embedded dashboard files with a strong
// SHA-256 ETag and Cache-Control: no-cache. Embedded files carry a zero
// modtime, so the old http.FileServer emitted no validators at all — browsers
// and intermediary caches fell back to heuristic caching and could keep
// serving an old dashboard.js/dashboard.css against a freshly deployed
// index.html, which then crashes on the previous markup's element ids.
// no-cache means every load revalidates (any cache in the path included) and
// gets a cheap 304 while the binary is unchanged; a deploy changes the hashes
// and all assets flip atomically. Hashes are computed once here, not per
// request. Last-Modified is deliberately never sent (a zero modtime is
// meaningless), leaving the ETag as the only — and sufficient — validator.
//
// Belt and suspenders on top of the validators: the served index.html is
// rewritten at startup to reference its scripts as dashboard.js?v=<hash> /
// dashboard.css?v=<hash> (a prefix of each asset's own content hash). A cache
// that ignores no-cache — e.g. a cache-everything edge rule, whose default
// cache key includes the query string — then stores every asset version under
// a distinct key, so a given index.html can only ever resolve to the exact js
// and css bytes it shipped with; an old-script/new-page pairing is impossible
// by construction. The query string is a cache key only: serving ignores it
// (lookup is by r.URL.Path), so any ?v= returns the same 200 + ETag.
func staticAssetHandler() http.HandlerFunc {
	read := func(name string) []byte {
		body, err := dashboardAssets.ReadFile("dashboard/" + name)
		if err != nil {
			// The embed paths are compile-time constants; this cannot fail at
			// runtime. Log and serve 404 for the file rather than crashing.
			log.Printf("dashboard: failed to load embedded asset %s: %v", name, err)
			return nil
		}
		return body
	}
	hexSum := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}

	js := read("dashboard.js")
	css := read("dashboard.css")
	html := read("index.html")
	favicon := read("favicon.svg")

	// Version-stamp the asset references before hashing index.html, so the
	// page's own ETag covers the rewritten bytes.
	if html != nil && js != nil {
		html = bytes.Replace(html,
			[]byte(`src="dashboard.js"`),
			[]byte(`src="dashboard.js?v=`+hexSum(js)[:assetVersionLen]+`"`), 1)
	}
	if html != nil && css != nil {
		html = bytes.Replace(html,
			[]byte(`href="dashboard.css"`),
			[]byte(`href="dashboard.css?v=`+hexSum(css)[:assetVersionLen]+`"`), 1)
	}

	assets := map[string]staticAsset{}
	for _, f := range []struct {
		name        string
		contentType string
		body        []byte
	}{
		{"index.html", "text/html; charset=utf-8", html},
		{"dashboard.css", "text/css; charset=utf-8", css},
		{"dashboard.js", "text/javascript; charset=utf-8", js},
		{"favicon.svg", "image/svg+xml", favicon},
	} {
		if f.body == nil {
			continue
		}
		assets["/"+f.name] = staticAsset{
			body:        f.body,
			contentType: f.contentType,
			etag:        `"` + hexSum(f.body) + `"`,
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		a, ok := assets[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", a.contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", a.etag)
		// ServeContent answers a matching If-None-Match with a bodyless 304
		// and, given the zero modtime, never emits Last-Modified.
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.body))
	}
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
	// Warnings describe how the container is configured for update checks --
	// no standard /.well-known/docker-updater/ endpoints, or nonstandard
	// label overrides. Not errors: the update path still works.
	Warnings []string `json:"warnings,omitempty"`
}

// apiResponse is the top-level JSON payload served at /api/containers.
type apiResponse struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Interval    string         `json:"interval"`
	DryRun      bool           `json:"dry_run"`
	Label       string         `json:"label"`
	Version     string         `json:"version,omitempty"`
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
		Version:     buildVersion(),
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
			ac.Warnings = st.Warnings
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

package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// composeConfigHashLabel holds the service hash compose stamps on each container it creates.
const composeConfigHashLabel = "com.docker.compose.config-hash"

// The states a ComposeDrift reports. A matching hash produces no ComposeDrift at all.
const (
	driftFound   = "drift"
	driftUnknown = "unknown"
)

// selfDriftBlocked is why the updater never applies a compose file to its own container.
const selfDriftBlocked = "docker-updater cannot recreate its own container through compose, because compose stops this process before it starts the replacement"

// ComposeDrift is a compose-managed container whose running config no longer
// matches its compose file, or one whose match nobody could confirm.
type ComposeDrift struct {
	State       string   `json:"state"`
	Service     string   `json:"service"`
	ConfigFiles []string `json:"config_files"`
	// RunningHash is the container's config-hash label. FileHash is what compose computes from the file now.
	RunningHash string `json:"running_hash,omitempty"`
	FileHash    string `json:"file_hash,omitempty"`
	// Command applies the file to this service.
	Command string `json:"command"`
	// Error says why the state is unknown.
	Error string `json:"error,omitempty"`
	// Blocked says why the updater cannot apply the file itself. The operator must run Command.
	Blocked string `json:"blocked,omitempty"`
}

// applies reports whether the updater recreates this container from its
// compose file. It is false for a nil drift.
func (d *ComposeDrift) applies() bool {
	return d != nil && d.State == driftFound && d.Blocked == ""
}

// checkComposeDrift compares a compose-managed container with its compose
// file. It returns nil when the hashes match, and for a container that compose
// does not own.
func checkComposeDrift(ctx context.Context, runner composeRunner, info ContainerInfo, cfg Config) *ComposeDrift {
	if !composeManaged(info) {
		return nil
	}
	target := composeTargetFor(info)
	d := &ComposeDrift{
		Service:     target.Service,
		ConfigFiles: target.ConfigFiles,
		RunningHash: info.Labels[composeConfigHashLabel],
		Command:     "docker " + shellJoin(composeArgs(target, "up", "-d", "--no-deps", target.Service)),
	}
	if d.RunningHash == "" {
		d.State = driftUnknown
		d.Error = "the container carries no " + composeConfigHashLabel + " label"
	} else if hash, err := runner.ConfigHash(ctx, target); err != nil {
		d.State = driftUnknown
		d.Error = err.Error()
	} else if hash == d.RunningHash {
		return nil
	} else {
		d.State = driftFound
		d.FileHash = hash
		if sameContainer(info.ID, cfg.SelfContainerID) {
			d.Blocked = selfDriftBlocked
		}
	}
	log.Print(d.logLine(info.Name))
	return d
}

// logLine is the log line for one drifted or unknown container. It is an
// ERROR when the updater cannot apply the file itself.
func (d ComposeDrift) logLine(name string) string {
	files := strings.Join(d.ConfigFiles, ", ")
	if d.State == driftUnknown {
		return fmt.Sprintf("ERROR container %s: compose drift unknown for service %s in %s: %s. "+
			"Nothing confirms that the container still matches the file. Apply the file with: %s",
			name, d.Service, files, d.Error, d.Command)
	}
	drift := fmt.Sprintf("drifted from its compose file: service %s in %s hashes to %s, but the container runs %s",
		d.Service, files, shortID(d.FileHash), shortID(d.RunningHash))
	if d.Blocked != "" {
		return fmt.Sprintf("ERROR container %s: %s. %s. Apply the file with: %s", name, drift, d.Blocked, d.Command)
	}
	return fmt.Sprintf("container %s: %s. The updater recreates it from the file, as it does for an update", name, drift)
}

// applyComposeDrift recreates a drifted container from its compose file when
// the check found no update to do it. It goes through the same pre-update gate
// and dry-run mode as an update. A held-back update keeps the drift: the
// update applies the file when it goes.
func applyComposeDrift(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo, cfg Config, result UpdateResult) UpdateResult {
	if !result.Drift.applies() {
		return result
	}
	if result.Updated && !result.DryRun {
		// The update recreated the container through compose, so the file applied with it.
		result.Drift = nil
		return result
	}
	if result.NewRef != "" || result.Error != nil || result.Skipped || result.Container.Unmonitorable != "" {
		return result
	}

	result.Resync = true
	if reason := preCheckRefuses(ctx, cli, info); reason != "" {
		result.Skipped = true
		result.SkipReason = reason
		log.Printf("container %s: pre-check failed, skipping recreate from the compose file: %s", info.Name, reason)
		return result
	}
	if dryRun(cfg, info) {
		log.Printf("container %s: dry-run mode, skipping recreate from the compose file", info.Name)
		result.Updated = true
		return result
	}
	if err := resyncViaCompose(ctx, cli, runner, info); err != nil {
		result.Error = err
		log.Printf("container %s: error recreating from the compose file: %v", info.Name, err)
		return result
	}
	result.Updated = true
	result.Drift = nil
	return result
}

// resyncViaCompose recreates a compose service from its compose file with
// `docker compose up -d --no-deps`. The image stays the same. Compose keeps
// no copy of the config, so a failure after compose replaced the container
// cannot roll back.
func resyncViaCompose(ctx context.Context, cli DockerClient, runner composeRunner, info ContainerInfo) error {
	target := composeTargetFor(info)
	inspect, err := cli.ContainerInspect(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("inspecting container %s: %w", info.Name, err)
	}

	log.Printf("container %s: recreating compose service %s from its compose file", info.Name, info.ComposeService)
	if err := runner.UpNoDeps(ctx, target); err != nil {
		cause := fmt.Errorf("recreating service %s from its compose file: %w", info.ComposeService, err)
		if containerUnchanged(ctx, cli, info.ID, inspect.Image) {
			return fmt.Errorf("%w; compose failed before it touched the container, which still runs its old config", cause)
		}
		return fmt.Errorf("%w; compose cannot restore the old config, so %s may be down", cause, info.Name)
	}

	newID := currentServiceContainerID(ctx, cli, info)
	if err := waitPostUpdateHealthy(ctx, cli, newID, info); err != nil {
		return fmt.Errorf("container %s not healthy after the recreate from its compose file: %w; compose cannot restore the old config, so fix the file and apply it again", info.Name, err)
	}

	log.Printf("container %s recreated from its compose file and healthy (%s)", info.Name, shortID(newID))
	if newInspect, err := cli.ContainerInspect(ctx, newID); err == nil {
		if msg := composeSecurityOptGap(ctx, cli, newInspect, info); msg != "" {
			log.Print(msg)
		}
	}
	return nil
}

// ConfigHash runs `docker compose <target> config --hash <service>` and returns
// the hash compose would stamp on a container it creates for the service now.
func (execComposeRunner) ConfigHash(ctx context.Context, t composeTarget) (string, error) {
	if err := checkComposeFilesVisible(t.ConfigFiles); err != nil {
		return "", err
	}
	args := composeArgs(t, "config", "--hash", t.Service)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: composeErrTailBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, stderr.tail(composeErrTailLines))
	}
	return parseConfigHash(stdout.String(), t.Service)
}

// parseConfigHash reads the hash for service from `config --hash` output,
// which prints one "<service> <hash>" line per service.
func parseConfigHash(out, service string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == service {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("compose config --hash printed no hash for service %s: %q", service, strings.TrimSpace(out))
}

// shellJoin renders args as one shell command line, and quotes each argument
// that a shell would split or expand.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:=,@+%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

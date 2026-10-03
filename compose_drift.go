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
}

// clonesConfig reports whether updateContainer replaces this container from
// the running container's own config rather than from the compose file. It
// follows the branches of updateContainer.
func clonesConfig(info ContainerInfo, cfg Config) bool {
	switch {
	case info.Mode == UpdateModeBuild:
		return false
	case sameContainer(info.ID, cfg.SelfContainerID), info.Rolling:
		return true
	default:
		return info.Mode != UpdateModeImage
	}
}

// checkComposeDrift compares a compose-managed container with its compose file
// when its updates clone the running config. It returns nil when the hashes
// match, and for any container whose updates go through compose or that
// compose does not own. It never converges anything: applying the file can
// swap a bind mount for a tmpfs and wipe the data, so the operator decides.
func checkComposeDrift(ctx context.Context, runner composeRunner, info ContainerInfo, cfg Config) *ComposeDrift {
	if !composeManaged(info) || !clonesConfig(info, cfg) {
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
	}
	log.Print(d.logLine(info.Name))
	return d
}

// logLine is the ERROR line for one drifted or unknown container.
func (d ComposeDrift) logLine(name string) string {
	files := strings.Join(d.ConfigFiles, ", ")
	if d.State == driftUnknown {
		return fmt.Sprintf("ERROR container %s: compose drift unknown for service %s in %s: %s. "+
			"Updates clone the running config, and nothing confirms that it still matches the file. Apply the file with: %s",
			name, d.Service, files, d.Error, d.Command)
	}
	return fmt.Sprintf("ERROR container %s: drifted from its compose file: service %s in %s hashes to %s, but the container runs %s. "+
		"Updates clone the running config, so the file does not apply. Applying it can replace mounts and lose data, so compare both sides first. Apply the file with: %s",
		name, d.Service, files, shortID(d.FileHash), shortID(d.RunningHash), d.Command)
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

package main

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

// none marks a value one side does not set.
const none = "(none)"

// ComposeDifference is one field where the compose file and the running
// container disagree.
type ComposeDifference struct {
	Field     string `json:"field"`
	File      string `json:"file"`
	Container string `json:"container"`
}

// composeProject is the part of `docker compose config --format json` that the
// differences read.
type composeProject struct {
	Name     string                    `json:"name"`
	Services map[string]composeService `json:"services"`
	Networks map[string]composeNamed   `json:"networks"`
	Volumes  map[string]composeNamed   `json:"volumes"`
}

// composeNamed is a top-level network or volume. Name is the Docker object's real name.
type composeNamed struct {
	Name string `json:"name"`
}

type composeService struct {
	Image       string              `json:"image"`
	Entrypoint  []string            `json:"entrypoint"`
	Command     []string            `json:"command"`
	Environment map[string]*string  `json:"environment"`
	Labels      map[string]string   `json:"labels"`
	Volumes     []composeMount      `json:"volumes"`
	Tmpfs       []string            `json:"tmpfs"`
	Ports       []composePort       `json:"ports"`
	Networks    map[string]struct{} `json:"networks"`
	NetworkMode string              `json:"network_mode"`
	Restart     string              `json:"restart"`
	User        string              `json:"user"`
	WorkingDir  string              `json:"working_dir"`
	Privileged  bool                `json:"privileged"`
	CapAdd      []string            `json:"cap_add"`
	CapDrop     []string            `json:"cap_drop"`
}

type composeMount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type composePort struct {
	Target    uint32 `json:"target"`
	Published string `json:"published"`
	Protocol  string `json:"protocol"`
	HostIP    string `json:"host_ip"`
}

// composeDifferences lists the fields where service in proj differs from the
// container. img is the config of the container's image: it supplies what the
// file leaves unset. Environment values never appear, because they can hold
// secrets and the dashboard has no login.
func composeDifferences(proj composeProject, service string, c types.ContainerJSON, img *container.Config) ([]ComposeDifference, error) {
	svc, ok := proj.Services[service]
	if !ok {
		return nil, fmt.Errorf("compose config has no service %s", service)
	}
	if c.Config == nil || c.HostConfig == nil {
		return nil, fmt.Errorf("the container inspect carries no config")
	}
	if img == nil {
		img = &container.Config{}
	}
	var out []ComposeDifference
	add := func(field, file, running string) {
		if file != running {
			out = append(out, ComposeDifference{Field: field, File: file, Container: running})
		}
	}

	if svc.Image != "" {
		add("image", svc.Image, c.Config.Image)
	}
	add("entrypoint", orImage(svc.Entrypoint, img.Entrypoint), shellJoinOrNone(c.Config.Entrypoint))
	add("command", orImage(svc.Command, img.Cmd), shellJoinOrNone(c.Config.Cmd))
	out = append(out, envDifferences(svc.Environment, c.Config.Env, img.Env)...)
	out = append(out, labelDifferences(svc.Labels, c.Config.Labels, img.Labels)...)
	out = append(out, mountDifferences(proj, svc, c, img)...)
	add("ports", strings.Join(filePorts(svc.Ports), ", "), strings.Join(containerPorts(c.HostConfig.PortBindings), ", "))
	if svc.NetworkMode != "" {
		add("network_mode", svc.NetworkMode, string(c.HostConfig.NetworkMode))
	} else {
		add("networks", strings.Join(fileNetworks(proj, svc), ", "), strings.Join(containerNetworks(c), ", "))
	}
	add("restart", orDefault(svc.Restart, "no"), restartPolicy(c.HostConfig.RestartPolicy))
	add("user", orDefault(svc.User, img.User), c.Config.User)
	add("working_dir", orDefault(svc.WorkingDir, img.WorkingDir), c.Config.WorkingDir)
	add("privileged", fmt.Sprint(svc.Privileged), fmt.Sprint(c.HostConfig.Privileged))
	add("cap_add", capSet(svc.CapAdd), capSet(c.HostConfig.CapAdd))
	add("cap_drop", capSet(svc.CapDrop), capSet(c.HostConfig.CapDrop))
	return out, nil
}

// orImage is the file's value, or the image's when the file sets none.
func orImage(file, image []string) string {
	if file != nil {
		return shellJoinOrNone(file)
	}
	return shellJoinOrNone(image)
}

func shellJoinOrNone(args []string) string {
	if len(args) == 0 {
		return none
	}
	return shellJoin(args)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func splitEnv(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// envDifferences names each variable the file and the container disagree on.
// A variable the image sets is not a difference.
func envDifferences(file map[string]*string, running, image []string) []ComposeDifference {
	have, base := splitEnv(running), splitEnv(image)
	var out []ComposeDifference
	for _, k := range sortedKeys(file) {
		v := file[k]
		if v == nil {
			// Compose passes the variable through from its own environment, so the file does not fix a value.
			continue
		}
		got, ok := have[k]
		switch {
		case !ok:
			out = append(out, ComposeDifference{Field: "environment " + k, File: "set", Container: none})
		case got != *v:
			out = append(out, ComposeDifference{Field: "environment " + k, File: "set (value hidden)", Container: "a different value"})
		}
	}
	for _, k := range sortedKeys(have) {
		if _, inFile := file[k]; inFile {
			continue
		}
		if bv, ok := base[k]; ok && bv == have[k] {
			continue
		}
		out = append(out, ComposeDifference{Field: "environment " + k, File: none, Container: "set"})
	}
	return out
}

// labelDifferences compares labels. Compose's own labels and the image's labels are not differences.
func labelDifferences(file, running, image map[string]string) []ComposeDifference {
	var out []ComposeDifference
	for _, k := range sortedKeys(file) {
		got, ok := running[k]
		if !ok {
			got = none
		}
		if got != file[k] {
			out = append(out, ComposeDifference{Field: "label " + k, File: file[k], Container: got})
		}
	}
	for _, k := range sortedKeys(running) {
		if _, inFile := file[k]; inFile || strings.HasPrefix(k, "com.docker.compose.") {
			continue
		}
		if iv, ok := image[k]; ok && iv == running[k] {
			continue
		}
		out = append(out, ComposeDifference{Field: "label " + k, File: none, Container: running[k]})
	}
	return out
}

// anonymousVolume matches the random name Docker gives a volume nobody named.
var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// mountDifferences compares mounts by their target path.
func mountDifferences(proj composeProject, svc composeService, c types.ContainerJSON, img *container.Config) []ComposeDifference {
	want := map[string]string{}
	for _, m := range svc.Volumes {
		want[m.Target] = fileMount(proj, m)
	}
	for _, t := range svc.Tmpfs {
		path, _, _ := strings.Cut(t, ":")
		want[path] = "tmpfs"
	}

	have := map[string]string{}
	for _, m := range c.Mounts {
		desc := string(m.Type)
		switch m.Type {
		case "bind":
			desc = "bind " + m.Source
		case "volume":
			desc = "volume " + m.Name
			if anonymousVolume.MatchString(m.Name) {
				desc = "anonymous volume"
			}
		}
		if !m.RW && m.Type != "tmpfs" {
			desc += " (read-only)"
		}
		have[m.Destination] = desc
	}
	for path := range c.HostConfig.Tmpfs {
		have[path] = "tmpfs"
	}
	for _, m := range c.HostConfig.Mounts {
		if m.Type == "tmpfs" {
			have[m.Target] = "tmpfs"
		}
	}

	targets := append(sortedKeys(want), sortedKeys(have)...)
	sort.Strings(targets)
	var out []ComposeDifference
	for _, t := range slices.Compact(targets) {
		w, inFile := want[t]
		h, running := have[t]
		if !inFile {
			if _, fromImage := img.Volumes[t]; fromImage {
				continue
			}
			w = none
		}
		if !running {
			h = none
		}
		if w != h {
			out = append(out, ComposeDifference{Field: "mount " + t, File: w, Container: h})
		}
	}
	return out
}

func fileMount(proj composeProject, m composeMount) string {
	var desc string
	switch m.Type {
	case "bind":
		desc = "bind " + m.Source
	case "volume":
		if m.Source == "" {
			desc = "anonymous volume"
		} else {
			desc = "volume " + orDefault(proj.Volumes[m.Source].Name, proj.Name+"_"+m.Source)
		}
	case "tmpfs":
		return "tmpfs"
	default:
		desc = m.Type + " " + m.Source
	}
	if m.ReadOnly {
		desc += " (read-only)"
	}
	return desc
}

func filePorts(ports []composePort) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, portString(p.HostIP, p.Published, fmt.Sprint(p.Target), orDefault(p.Protocol, "tcp")))
	}
	sort.Strings(out)
	return out
}

func containerPorts(bindings nat.PortMap) []string {
	var out []string
	for key, list := range bindings {
		target, proto, _ := strings.Cut(string(key), "/")
		for _, b := range list {
			out = append(out, portString(b.HostIP, b.HostPort, target, orDefault(proto, "tcp")))
		}
	}
	sort.Strings(out)
	return out
}

func portString(hostIP, published, target, proto string) string {
	if hostIP == "0.0.0.0" || hostIP == "::" {
		hostIP = ""
	}
	s := target + "/" + proto
	if published != "" {
		s = published + "->" + s
	}
	if hostIP != "" {
		s = hostIP + ":" + s
	}
	return s
}

func fileNetworks(proj composeProject, svc composeService) []string {
	keys := sortedKeys(svc.Networks)
	if len(keys) == 0 {
		keys = []string{"default"}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, orDefault(proj.Networks[k].Name, proj.Name+"_"+k))
	}
	sort.Strings(out)
	return out
}

func containerNetworks(c types.ContainerJSON) []string {
	if c.NetworkSettings == nil {
		return nil
	}
	return sortedKeys(c.NetworkSettings.Networks)
}

func restartPolicy(p container.RestartPolicy) string {
	name := orDefault(string(p.Name), "no")
	if p.MaximumRetryCount > 0 {
		return fmt.Sprintf("%s:%d", name, p.MaximumRetryCount)
	}
	return name
}

// capSet normalizes capabilities: Docker accepts them with or without the CAP_ prefix.
func capSet(caps []string) string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, strings.TrimPrefix(strings.ToUpper(c), "CAP_"))
	}
	sort.Strings(out)
	if len(out) == 0 {
		return none
	}
	return strings.Join(out, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

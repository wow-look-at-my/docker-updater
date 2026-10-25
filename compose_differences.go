// none marks a value one side does not set.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/wow-look-at-my/go-containers/set"
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

// daemonDefaults are the fields the daemon fills in when a create request
// leaves them empty. Only a value the request sets can disagree with them.
var daemonDefaults = set.Of[string]("Config.Hostname",
	"Config.Domainname",
	"HostConfig.LogConfig",
	"HostConfig.CgroupnsMode",
	"HostConfig.IpcMode",
	"HostConfig.ShmSize",
	"HostConfig.Runtime",
	"HostConfig.MemorySwap",
	"HostConfig.Isolation",
	"HostConfig.RestartPolicy",
	"HostConfig.MaskedPaths",
	"HostConfig.ReadonlyPaths")

// imageDefaults are the Config fields the daemon copies from the image when a create request leaves them empty.
var imageDefaults = set.Of[string]("Cmd", "Entrypoint", "WorkingDir", "User", "StopSignal",
	"Healthcheck", "Shell", "OnBuild", "ArgsEscaped")

// imageMerged are the Config maps the daemon merges with the image's. The container carries both sets of keys.
var imageMerged = set.Of[string]("Labels", "ExposedPorts", "Volumes")

// nullOnlyZero are fields where an empty list is a value. systempaths=unconfined sends empty lists, and null asks for the daemon's defaults.
var nullOnlyZero = set.Of[string]("HostConfig.MaskedPaths", "HostConfig.ReadonlyPaths")

// composeDifferences compares every field of the create request compose
// would send now with the running container. The request carries compose's
// own translation of the file, so only the fields the daemon or the image
// fills in need a rule. img is the config of the container's image.
func composeDifferences(createRequest []byte, c types.ContainerJSON, img *container.Config) ([]ComposeDifference, error) {
	if c.ContainerJSONBase == nil || c.Config == nil || c.HostConfig == nil {
		return nil, fmt.Errorf("the container inspect carries no config")
	}
	if img == nil {
		img = &container.Config{}
	}
	want, err := decodeObject(createRequest)
	if err != nil {
		return nil, fmt.Errorf("reading the create request compose sent: %w", err)
	}
	wantHost, _ := want["HostConfig"].(map[string]any)
	wantNet, _ := want["NetworkingConfig"].(map[string]any)
	delete(want, "HostConfig")
	delete(want, "NetworkingConfig")
	haveCfg, haveHost, imgCfg := mustObject(c.Config), mustObject(c.HostConfig), mustObject(img)

	var out []ComposeDifference
	for _, k := range unionKeys(want, haveCfg) {
		switch {
		case k == "Env":
			out = append(out, envDifferences(stringList(want[k]), c.Config.Env, img.Env)...)
		case imageMerged.Contains(k):
			out = append(out, mergedDifferences("Config."+k, objectOf(want[k]), objectOf(haveCfg[k]), objectOf(imgCfg[k]))...)
		default:
			w := want[k]
			// The daemon drops the image's command when the request names its own entrypoint.
			if isZero(w) && imageDefaults.Contains(k) && !(k == "Cmd" && !isZero(want["Entrypoint"])) {
				w = imgCfg[k]
			}
			out = appendDifference(out, "Config."+k, w, haveCfg[k])
		}
	}
	for _, k := range unionKeys(wantHost, haveHost) {
		out = appendDifference(out, "HostConfig."+k, wantHost[k], haveHost[k])
	}
	mode := c.HostConfig.NetworkMode
	if !mode.IsHost() && !mode.IsNone() && !mode.IsContainer() {
		var have map[string]any
		if c.NetworkSettings != nil {
			have = mustObject(c.NetworkSettings.Networks)
		}
		out = append(out, endpointDifferences(objectOf(wantNet["EndpointsConfig"]), have)...)
	}
	return out, nil
}

// endpointRuntime are the endpoint fields the daemon assigns when the container joins the network.
var endpointRuntime = set.Of[string]("NetworkID", "EndpointID", "Gateway", "IPAddress", "IPPrefixLen",
	"IPv6Gateway", "GlobalIPv6Address", "GlobalIPv6PrefixLen", "DNSNames")

// endpointDifferences compares the networks the request joins, and every setting of each endpoint.
func endpointDifferences(want, have map[string]any) []ComposeDifference {
	var out []ComposeDifference
	for _, name := range unionKeys(want, have) {
		w, inReq := want[name].(map[string]any)
		h, joined := have[name].(map[string]any)
		switch {
		case !inReq:
			out = append(out, ComposeDifference{Field: "network " + name, File: none, Container: "joined"})
		case !joined:
			out = append(out, ComposeDifference{Field: "network " + name, File: "joined", Container: none})
		default:
			for _, k := range unionKeys(w, h) {
				// The daemon picks a MAC address when the request names none.
				if endpointRuntime.Contains(k) || (k == "MacAddress" && isZero(w[k])) {
					continue
				}
				if !equalValues(w[k], h[k]) {
					out = append(out, ComposeDifference{Field: "network " + name + " " + k, File: render(w[k]), Container: render(h[k])})
				}
			}
		}
	}
	return out
}

// appendDifference adds field when the request's value and the container's
// disagree. A field the daemon fills in counts only when the request sets it.
func appendDifference(out []ComposeDifference, field string, want, have any) []ComposeDifference {
	zero := isZero(want)
	if nullOnlyZero.Contains(field) {
		zero = want == nil
	}
	if zero && daemonDefaults.Contains(field) {
		return out
	}
	if equalValues(want, have) && (!nullOnlyZero.Contains(field) || (want == nil) == (have == nil)) {
		return out
	}
	return append(out, ComposeDifference{Field: field, File: render(want), Container: render(have)})
}

// mergedDifferences compares a map the daemon merges with the image's. The
// container must carry every key the request sets, and no key past the
// image's own. Compose's own labels change on every recreate.
func mergedDifferences(field string, want, have, image map[string]any) []ComposeDifference {
	var out []ComposeDifference
	for _, k := range unionKeys(want, have) {
		if strings.HasPrefix(k, "com.docker.compose.") {
			continue
		}
		w, inReq := want[k]
		if !inReq {
			w, inReq = image[k]
		}
		h, running := have[k]
		switch {
		case !inReq:
			out = append(out, ComposeDifference{Field: field + " " + k, File: none, Container: render(h)})
		case !running:
			out = append(out, ComposeDifference{Field: field + " " + k, File: render(w), Container: none})
		case !equalValues(w, h):
			out = append(out, ComposeDifference{Field: field + " " + k, File: render(w), Container: render(h)})
		}
	}
	return out
}

func splitEnv(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// envDifferences lists each variable the request and the container disagree
// on. The daemon adds the image's variables under the request's.
func envDifferences(want, running, image []string) []ComposeDifference {
	expect, have := splitEnv(image), splitEnv(running)
	for k, v := range splitEnv(want) {
		expect[k] = v
	}
	var out []ComposeDifference
	for _, k := range unionKeys(expect, have) {
		w, inReq := expect[k]
		h, ok := have[k]
		if inReq && ok && w == h {
			continue
		}
		file, running := none, none
		if inReq {
			file = strconv.Quote(w)
		}
		if ok {
			running = strconv.Quote(h)
		}
		out = append(out, ComposeDifference{Field: "Config.Env " + k, File: file, Container: running})
	}
	return out
}

// decodeObject reads a JSON object. Numbers stay exact, so a byte count compares digit for digit.
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return m, nil
}

// mustObject is v as a decoded JSON object. The Docker API types always marshal.
func mustObject(v any) map[string]any {
	data, _ := json.Marshal(v)
	m, _ := decodeObject(data)
	return m
}

func objectOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// isZero reports whether v sets nothing: null, an empty string, list or map, zero, false, or a map of only such values.
func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case json.Number:
		return x.String() == "0"
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, e := range x {
			if !isZero(e) {
				return false
			}
		}
		return true
	}
	return false
}

func equalValues(a, b any) bool {
	if isZero(a) && isZero(b) {
		return true
	}
	return canonical(a) == canonical(b)
}

// canonical is v as JSON. encoding/json sorts map keys, so equal values give equal text.
func canonical(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	// The dashboard shows this text as is, so a <, > or & must not turn into an escape code.
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// render is v for the dashboard, in full: a bare string, or compact JSON.
func render(v any) string {
	if list, ok := v.([]any); ok && len(list) == 0 {
		return "[]"
	}
	if isZero(v) {
		return none
	}
	if s, ok := v.(string); ok {
		return s
	}
	return canonical(v)
}

func unionKeys[V any](a, b map[string]V) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	return sortedKeys(seen)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

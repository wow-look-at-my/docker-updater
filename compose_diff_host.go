package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-units"
)

// dockerDefaultShmSize is the /dev/shm size the daemon gives a container that asks for none.
const dockerDefaultShmSize = 64 * 1024 * 1024

// composeBytes is a byte count. Compose prints it as a quoted integer.
type composeBytes int64

func (b *composeBytes) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*b = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("byte count %s: %w", data, err)
	}
	*b = composeBytes(n)
	return nil
}

// composeUlimit is one ulimit. Compose prints a bare number when soft and hard are the same.
type composeUlimit struct {
	Soft int64 `json:"soft"`
	Hard int64 `json:"hard"`
}

func (u *composeUlimit) UnmarshalJSON(data []byte) error {
	var single int64
	if err := json.Unmarshal(data, &single); err == nil {
		u.Soft, u.Hard = single, single
		return nil
	}
	type plain composeUlimit
	return json.Unmarshal(data, (*plain)(u))
}

type composeDeploy struct {
	Resources struct {
		Limits *struct {
			Cpus   float64      `json:"cpus"`
			Memory composeBytes `json:"memory"`
			Pids   int64        `json:"pids"`
		} `json:"limits"`
		Reservations *struct {
			Memory composeBytes `json:"memory"`
		} `json:"reservations"`
	} `json:"resources"`
}

type composeDevice struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Permissions string `json:"permissions"`
}

type composeLogging struct {
	Driver  string            `json:"driver"`
	Options map[string]string `json:"options"`
}

// hostDifferences compares the resource, security and process fields that
// compose also hashes. Each value follows what compose passes to the daemon,
// and what the daemon fills in for a value nobody set.
func hostDifferences(svc composeService, dir string, c types.ContainerJSON, img *container.Config) []ComposeDifference {
	hc, cfg := c.HostConfig, c.Config
	var out []ComposeDifference
	add := func(field, file, running string) {
		if file != running {
			out = append(out, ComposeDifference{Field: field, File: file, Container: running})
		}
	}

	add("security_opt", joinOrNone(fileSecurityOpts(svc.SecurityOpt, dir)), joinOrNone(containerSecurityOpts(hc)))

	// deploy.resources overrides the top-level keys, as it does in compose.
	mem, memRes, cpus, pids := int64(svc.MemLimit), int64(svc.MemReservation), svc.Cpus, svc.PidsLimit
	if d := svc.Deploy; d != nil {
		if l := d.Resources.Limits; l != nil {
			mem = orNonZero(int64(l.Memory), mem)
			if l.Cpus != 0 {
				cpus = l.Cpus
			}
			if l.Pids > 0 {
				pids = l.Pids
			}
		}
		if r := d.Resources.Reservations; r != nil {
			memRes = orNonZero(int64(r.Memory), memRes)
		}
	}
	add("mem_limit", bytesOrNone(mem), bytesOrNone(hc.Memory))
	add("memswap_limit", memswapExpected(int64(svc.MemswapLimit), mem, hc.MemorySwap), bytesOrNone(hc.MemorySwap))
	add("mem_reservation", bytesOrNone(memRes), bytesOrNone(hc.MemoryReservation))
	add("cpus", cpusOrNone(int64(cpus*1e9)), cpusOrNone(hc.NanoCPUs))
	add("pids_limit", pidsOrNone(pids), pidsOrNone(derefInt64(hc.PidsLimit)))
	shm := int64(svc.ShmSize)
	if shm == 0 && (hc.ShmSize == 0 || hc.ShmSize == dockerDefaultShmSize) {
		shm = hc.ShmSize
	}
	add("shm_size", bytesOrNone(shm), bytesOrNone(hc.ShmSize))
	add("ulimits", joinOrNone(fileUlimits(svc.Ulimits)), joinOrNone(containerUlimits(hc.Ulimits)))
	add("sysctls", joinOrNone(mapPairs(svc.Sysctls)), joinOrNone(mapPairs(hc.Sysctls)))
	add("devices", joinOrNone(fileDevices(svc.Devices)), joinOrNone(containerDevices(hc.Devices)))

	add("init", fmt.Sprint(derefBool(svc.Init)), fmt.Sprint(derefBool(hc.Init)))
	add("read_only", fmt.Sprint(svc.ReadOnly), fmt.Sprint(hc.ReadonlyRootfs))
	add("tty", fmt.Sprint(svc.Tty), fmt.Sprint(cfg.Tty))
	add("stdin_open", fmt.Sprint(svc.StdinOpen), fmt.Sprint(cfg.OpenStdin))
	add("stop_signal", orDefault(orDefault(svc.StopSignal, img.StopSignal), none), orDefault(cfg.StopSignal, none))
	add("pid", orDefault(svc.Pid, none), orDefault(string(hc.PidMode), none))
	add("group_add", joinOrNone(sortedCopy(svc.GroupAdd)), joinOrNone(sortedCopy(hc.GroupAdd)))
	add("dns", joinOrNone(svc.DNS), joinOrNone(hc.DNS))
	add("extra_hosts", joinOrNone(fileExtraHosts(svc.ExtraHosts)), joinOrNone(sortedCopy(hc.ExtraHosts)))
	add("oom_score_adj", fmt.Sprint(svc.OomScoreAdj), fmt.Sprint(hc.OomScoreAdj))

	// The daemon fills these in when the file sets none, so only a value the file sets can disagree.
	if svc.Hostname != "" {
		add("hostname", svc.Hostname, cfg.Hostname)
	}
	if svc.Ipc != "" {
		add("ipc", svc.Ipc, string(hc.IpcMode))
	}
	if svc.Runtime != "" {
		add("runtime", svc.Runtime, hc.Runtime)
	}
	if svc.StopGracePeriod != "" {
		add("stop_grace_period", gracePeriodSeconds(svc.StopGracePeriod), stopTimeout(cfg.StopTimeout))
	}
	if svc.Logging != nil && svc.Logging.Driver != "" {
		add("logging", svc.Logging.Driver+" "+joinOrNone(mapPairs(svc.Logging.Options)),
			hc.LogConfig.Type+" "+joinOrNone(mapPairs(hc.LogConfig.Config)))
	}
	return out
}

// fileSecurityOpts is the security_opt list as compose hands it to the
// daemon: a seccomp profile path becomes the profile's content.
func fileSecurityOpts(opts []string, dir string) []string {
	out := make([]string, 0, len(opts))
	for _, opt := range opts {
		name, val, found := strings.Cut(opt, "=")
		if !found {
			name, val, _ = strings.Cut(opt, ":")
		}
		if name != "seccomp" || val == "unconfined" || val == "builtin" {
			out = append(out, opt)
			continue
		}
		path := val
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			out = append(out, "seccomp profile "+val+" (cannot read: "+err.Error()+")")
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			out = append(out, "seccomp profile "+val+" (not JSON: "+err.Error()+")")
			continue
		}
		out = append(out, seccompProfile(compact.Bytes()))
	}
	sort.Strings(out)
	return out
}

// containerSecurityOpts is the container's security options. An inline
// seccomp profile shows as its hash. systempaths=unconfined leaves no
// option behind, only empty masked and read-only path lists.
func containerSecurityOpts(hc *container.HostConfig) []string {
	out := make([]string, 0, len(hc.SecurityOpt)+1)
	for _, opt := range hc.SecurityOpt {
		if profile, ok := strings.CutPrefix(opt, "seccomp="); ok && strings.HasPrefix(profile, "{") {
			out = append(out, seccompProfile([]byte(profile)))
			continue
		}
		if opt != "systempaths=unconfined" {
			out = append(out, opt)
		}
	}
	if !hc.Privileged && hc.MaskedPaths != nil && len(hc.MaskedPaths) == 0 && hc.ReadonlyPaths != nil && len(hc.ReadonlyPaths) == 0 {
		out = append(out, "systempaths=unconfined")
	}
	sort.Strings(out)
	return out
}

// seccompProfile names a profile by the hash of its compact JSON, because the profile itself is too long to show.
func seccompProfile(compact []byte) string {
	sum := sha256.Sum256(compact)
	return "seccomp profile " + hex.EncodeToString(sum[:])[:12]
}

// memswapExpected is the swap limit the file asks for. With none, the daemon
// stores either nothing or twice the memory limit, so the running value counts.
func memswapExpected(file, mem, running int64) string {
	if file == 0 && (running == 0 || running == 2*mem) {
		return bytesOrNone(running)
	}
	return bytesOrNone(file)
}

func bytesOrNone(n int64) string {
	switch {
	case n == 0:
		return none
	case n < 0:
		return "unlimited"
	}
	return units.BytesSize(float64(n))
}

func cpusOrNone(nano int64) string {
	if nano == 0 {
		return none
	}
	return strconv.FormatFloat(float64(nano)/1e9, 'f', -1, 64)
}

// pidsOrNone treats zero and negative limits alike: the daemon stores both as no limit.
func pidsOrNone(n int64) string {
	if n <= 0 {
		return none
	}
	return strconv.FormatInt(n, 10)
}

func fileUlimits(m map[string]composeUlimit) []string {
	out := make([]string, 0, len(m))
	for name, u := range m {
		out = append(out, fmt.Sprintf("%s=%d:%d", name, u.Soft, u.Hard))
	}
	sort.Strings(out)
	return out
}

func containerUlimits(list []*container.Ulimit) []string {
	out := make([]string, 0, len(list))
	for _, u := range list {
		if u != nil {
			out = append(out, fmt.Sprintf("%s=%d:%d", u.Name, u.Soft, u.Hard))
		}
	}
	sort.Strings(out)
	return out
}

func fileDevices(list []composeDevice) []string {
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.Source+":"+d.Target+":"+orDefault(d.Permissions, "rwm"))
	}
	sort.Strings(out)
	return out
}

func containerDevices(list []container.DeviceMapping) []string {
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.PathOnHost+":"+d.PathInContainer+":"+orDefault(d.CgroupPermissions, "rwm"))
	}
	sort.Strings(out)
	return out
}

// fileExtraHosts converts compose's "host=address" form to the daemon's "host:address".
func fileExtraHosts(list []string) []string {
	out := make([]string, 0, len(list))
	for _, h := range list {
		out = append(out, strings.Replace(h, "=", ":", 1))
	}
	sort.Strings(out)
	return out
}

func mapPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return out
}

func gracePeriodSeconds(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	return strconv.Itoa(int(d.Seconds())) + "s"
}

func stopTimeout(t *int) string {
	if t == nil {
		return none
	}
	return strconv.Itoa(*t) + "s"
}

func joinOrNone(list []string) string {
	if len(list) == 0 {
		return none
	}
	return strings.Join(list, ", ")
}

func sortedCopy(list []string) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}

func orNonZero(v, def int64) int64 {
	if v != 0 {
		return v
	}
	return def
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefBool(p *bool) bool {
	return p != nil && *p
}

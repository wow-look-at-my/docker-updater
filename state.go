package main

import (
	"strings"
	"sync"
	"time"
)

// ContainerStatus is the updater's tracked knowledge about a single monitored
// container, captured after each update-check cycle. It is keyed by container
// name, which is stable across container recreation.
type ContainerStatus struct {
	Name        string
	Image       string
	Mode        UpdateMode
	LastChecked time.Time
	LastPulled  time.Time // zero if the updater has never pulled this image
	LastUpdated time.Time // zero if the updater has never applied an update

	// UpdateAvailable is true when a newer image digest or git commit was
	// detected but is not (yet) the running version: either dry-run is on, the
	// pre-check skipped the update, or applying it errored.
	UpdateAvailable bool
	CurrentRef      string // current digest or commit (short form)
	AvailableRef    string // newer digest or commit (short form), if available

	// Warnings about how the container is configured for update checks
	// (missing standard endpoints, nonstandard label overrides).
	Warnings []string

	LastError  string // last error message, if the most recent check failed
	Skipped    bool   // the most recent update was skipped by a pre-check
	SkipReason string
	DryRun     bool

	// StuckCycles counts the consecutive cycles that found an update and did
	// not apply it. StuckSince is when that run began.
	//
	// One failed update is ordinary. The same one every cycle for weeks is a
	// deployment frozen on the version it already runs, and the per-cycle log
	// line reads the same on the first cycle as on the thousandth. Counting the
	// run is what separates the two.
	StuckCycles int
	StuckSince  time.Time
}

// Stuck reports whether this container has failed to take an available update
// for more than one cycle.
func (s ContainerStatus) Stuck() bool { return s.StuckCycles > 1 }

// Store holds the latest snapshot of the updater's per-container knowledge. It
// is safe for concurrent use by the update loop (writer) and dashboard server
// (reader).
type Store struct {
	mu        sync.RWMutex
	statuses  map[string]ContainerStatus
	lastCycle time.Time
}

// Snapshot is an immutable copy of the store's state for a reader.
type Snapshot struct {
	Statuses  map[string]ContainerStatus
	LastCycle time.Time
}

func newStore() *Store {
	return &Store{statuses: make(map[string]ContainerStatus)}
}

// Record folds a cycle's update results into the store, preserving historical
// timestamps (LastPulled, LastUpdated) for fields the current cycle did not
// touch. It also marks the time the cycle completed.
func (s *Store) Record(results []UpdateResult, cycleEnd time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range results {
		name := r.Container.Name
		st := s.statuses[name] // existing entry, or zero value for a new one

		st.Name = name
		st.Image = r.Container.Image
		st.Mode = r.Container.Mode
		st.LastChecked = r.CheckedAt
		st.DryRun = r.DryRun
		st.Warnings = r.Warnings
		if r.Pulled {
			st.LastPulled = r.CheckedAt
		}

		// Reset per-cycle fields; historical timestamps above are preserved.
		st.LastError = ""
		st.Skipped = false
		st.SkipReason = ""
		st.UpdateAvailable = false
		st.AvailableRef = ""
		st.CurrentRef = shortRef(r.OldRef)

		// An update that was found and not applied continues the run. Anything
		// else ends it, including a cycle with nothing to apply: the version
		// the container runs is then the one that was offered.
		stuck := r.Error != nil || r.Skipped
		switch {
		case !stuck:
			st.StuckCycles = 0
			st.StuckSince = time.Time{}
		case st.StuckCycles == 0:
			st.StuckCycles = 1
			st.StuckSince = r.CheckedAt
		default:
			st.StuckCycles++
		}

		switch {
		case r.Error != nil:
			st.LastError = r.Error.Error()
			if r.NewRef != "" {
				st.UpdateAvailable = true
				st.AvailableRef = shortRef(r.NewRef)
			}
		case r.Skipped:
			st.Skipped = true
			st.SkipReason = r.SkipReason
			st.UpdateAvailable = true
			st.AvailableRef = shortRef(r.NewRef)
		case r.Updated && r.DryRun:
			// Dry-run: an update is available but was deliberately not applied.
			st.UpdateAvailable = true
			st.AvailableRef = shortRef(r.NewRef)
		case r.Updated:
			// Update applied: the new ref is now the running version.
			st.LastUpdated = r.CheckedAt
			if r.NewRef != "" {
				st.CurrentRef = shortRef(r.NewRef)
			}
		}

		s.statuses[name] = st
	}

	s.lastCycle = cycleEnd
}

// Snapshot returns a deep copy of the current state for safe concurrent reads.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	statuses := make(map[string]ContainerStatus, len(s.statuses))
	for k, v := range s.statuses {
		statuses[k] = v
	}
	return Snapshot{Statuses: statuses, LastCycle: s.lastCycle}
}

// shortRef trims the digest algorithm prefix and shortens a digest or commit
// SHA for compact display.
func shortRef(ref string) string {
	ref = strings.TrimPrefix(ref, "sha256:")
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

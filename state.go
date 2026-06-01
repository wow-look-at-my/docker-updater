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

	LastError  string // last error message, if the most recent check failed
	Skipped    bool   // the most recent update was skipped by a pre-check
	SkipReason string
	DryRun     bool
}

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

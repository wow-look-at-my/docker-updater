// ContainerStatus is the updater's tracked knowledge about a single monitored container, captured after each update-check cycle.

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

	// UpdateAvailable is true when a newer image digest or git commit was detected but is not (yet) the running version.
	UpdateAvailable bool
	CurrentRef      string // current digest or commit (short form)
	AvailableRef    string // newer digest or commit (short form), if available
	// The commit the available image was built from, and its page. Empty when the image carries no commit.
	AvailableCommit    string
	AvailableCommitURL string

	// Warnings about how the container is configured for update checks.
	Warnings []string

	// Drift is the compose drift the latest check found, or nil.
	Drift *ComposeDrift

	LastError  string // last error message, if the most recent check failed
	Skipped    bool   // the most recent update was skipped by a pre-check
	SkipReason string
	DryRun     bool

	// Unmonitorable is why this container can never be checked as configured.
	Unmonitorable string

	// StuckCycles counts the consecutive cycles that left this container on the version it runs against the updater's will.
	StuckCycles int
	StuckSince  time.Time
}

// Stuck reports whether this container has been unable to move for more than one cycle.
func (s ContainerStatus) Stuck() bool { return s.StuckCycles > 1 }

// Store holds the latest snapshot of the updater's per-container knowledge.
type Store struct {
	mu        sync.RWMutex
	statuses  map[string]ContainerStatus
	recreates map[string]QueuedRecreate
	lastCycle time.Time
}

// QueuedRecreate is a recreate from the compose file that an operator asked
// for.
type QueuedRecreate struct {
	FileHash string
	At       time.Time
}

// Snapshot is an immutable copy of the store's state for a reader.
type Snapshot struct {
	Statuses  map[string]ContainerStatus
	Recreates map[string]QueuedRecreate
	LastCycle time.Time
}

func newStore() *Store {
	return &Store{statuses: make(map[string]ContainerStatus), recreates: make(map[string]QueuedRecreate)}
}

// QueueRecreate queues a recreate of the named container from the compose file that hashes to fileHash.
func (s *Store) QueueRecreate(name, fileHash string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recreates[name] = QueuedRecreate{FileHash: fileHash, At: at}
}

// QueuedRecreate returns the file hash of the named container's queued recreate.
func (s *Store) QueuedRecreate(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q, ok := s.recreates[name]
	return q.FileHash, ok
}

// DropRecreate removes the named container's queued recreate.
func (s *Store) DropRecreate(name string) {
	s.CancelRecreate(name)
}

// CancelRecreate removes the named container's queued recreate and reports whether one was queued.
func (s *Store) CancelRecreate(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.recreates[name]
	delete(s.recreates, name)
	return ok
}

// Record folds a cycle's update results into the store.
func (s *Store) Record(results []UpdateResult, cycleEnd time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record(results)
	s.lastCycle = cycleEnd
}

// RecordContainers folds results that are not a full cycle, such as a forced
// update of one container. The last-cycle time stays where it is.
func (s *Store) RecordContainers(results []UpdateResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record(results)
}

func (s *Store) record(results []UpdateResult) {
	for _, r := range results {
		name := r.Container.Name
		st := s.statuses[name] // existing entry, or zero value for a new one

		st.Name = name
		st.Image = r.Container.Image
		st.Mode = r.Container.Mode
		st.LastChecked = r.CheckedAt
		st.DryRun = r.DryRun
		st.Warnings = r.Warnings
		st.Drift = r.Drift
		if r.Pulled {
			st.LastPulled = r.CheckedAt
		}

		// Reset per-cycle fields; historical timestamps above are preserved.
		st.Unmonitorable = r.Container.Unmonitorable
		st.LastError = ""
		st.Skipped = false
		st.SkipReason = ""
		st.UpdateAvailable = false
		st.AvailableRef = ""
		st.AvailableCommit, st.AvailableCommitURL = "", ""
		st.CurrentRef = shortRef(r.OldRef)

		// A cycle that could not move the container continues the run.
		stuck := r.Error != nil || r.Skipped || r.Container.Unmonitorable != ""
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
			// A failed check can still hold back a queued recreate from the compose file.
			st.Skipped = r.Skipped
			st.SkipReason = r.SkipReason
			if r.NewRef != "" {
				st.UpdateAvailable = true
				st.AvailableRef = shortRef(r.NewRef)
				st.AvailableCommit, st.AvailableCommitURL = r.NewCommit, r.NewCommitURL
			}
		case r.Skipped:
			st.Skipped = true
			st.SkipReason = r.SkipReason
			st.UpdateAvailable = !r.Resync
			st.AvailableRef = shortRef(r.NewRef)
			st.AvailableCommit, st.AvailableCommitURL = r.NewCommit, r.NewCommitURL
		case r.Updated && r.DryRun:
			// Dry-run: an update or a recreate from the compose file is due but was deliberately not applied. The held drift stays in st.Drift.
			st.UpdateAvailable = !r.Resync
			st.AvailableRef = shortRef(r.NewRef)
			st.AvailableCommit, st.AvailableCommitURL = r.NewCommit, r.NewCommitURL
		case r.Updated:
			// Update applied: the new ref is now the running version.
			st.LastUpdated = r.CheckedAt
			if r.NewRef != "" {
				st.CurrentRef = shortRef(r.NewRef)
			}
		}

		s.statuses[name] = st
	}
}

// Snapshot returns a deep copy of the current state for safe concurrent reads.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	statuses := make(map[string]ContainerStatus, len(s.statuses))
	for k, v := range s.statuses {
		statuses[k] = v
	}
	recreates := make(map[string]QueuedRecreate, len(s.recreates))
	for k, v := range s.recreates {
		recreates[k] = v
	}
	return Snapshot{Statuses: statuses, Recreates: recreates, LastCycle: s.lastCycle}
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

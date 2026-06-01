package main

import (
	"errors"
	"testing"
	"time"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestShortRef(t *testing.T) {
	assert.Equal(t, "abcdef012345", shortRef("sha256:abcdef0123456789"))
	assert.Equal(t, "abcdef012345", shortRef("abcdef0123456789"))
	assert.Equal(t, "short", shortRef("short"))
	assert.Equal(t, "", shortRef(""))
}

func TestStoreRecordUpToDate(t *testing.T) {
	s := newStore()
	now := time.Now()

	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "web", Image: "nginx:latest", Mode: UpdateModeImage},
		OldRef:    "sha256:currentdigest0000",
		Pulled:    true,
		CheckedAt: now,
	}}, now)

	snap := s.Snapshot()
	st, ok := snap.Statuses["web"]
	require.True(t, ok)
	assert.Equal(t, "nginx:latest", st.Image)
	assert.Equal(t, UpdateModeImage, st.Mode)
	assert.Equal(t, now, st.LastChecked)
	assert.Equal(t, now, st.LastPulled)
	assert.False(t, st.UpdateAvailable)
	assert.Equal(t, "currentdiges", st.CurrentRef)
	assert.Empty(t, st.LastError)
	assert.True(t, st.LastUpdated.IsZero())
	assert.Equal(t, now, snap.LastCycle)
}

func TestStoreRecordDryRunAvailable(t *testing.T) {
	s := newStore()
	now := time.Now()

	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "web", Image: "nginx:latest", Mode: UpdateModeImage},
		Updated:   true,
		DryRun:    true,
		OldRef:    "sha256:oldoldoldold0000",
		NewRef:    "sha256:newnewnewnew1111",
		Pulled:    true,
		CheckedAt: now,
	}}, now)

	st := s.Snapshot().Statuses["web"]
	assert.True(t, st.UpdateAvailable)
	assert.Equal(t, "oldoldoldold", st.CurrentRef)
	assert.Equal(t, "newnewnewnew", st.AvailableRef)
	assert.True(t, st.DryRun)
	assert.True(t, st.LastUpdated.IsZero(), "dry-run must not record an applied update")
}

func TestStoreRecordUpdateApplied(t *testing.T) {
	s := newStore()
	now := time.Now()

	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "web", Image: "nginx:latest", Mode: UpdateModeImage},
		Updated:   true,
		OldRef:    "sha256:oldoldoldold0000",
		NewRef:    "sha256:newnewnewnew1111",
		Pulled:    true,
		CheckedAt: now,
	}}, now)

	st := s.Snapshot().Statuses["web"]
	assert.False(t, st.UpdateAvailable, "applied update is no longer pending")
	assert.Equal(t, "newnewnewnew", st.CurrentRef, "current ref advances to the new digest")
	assert.Equal(t, now, st.LastUpdated)
}

func TestStoreRecordSkipped(t *testing.T) {
	s := newStore()
	now := time.Now()

	s.Record([]UpdateResult{{
		Container:  ContainerInfo{Name: "db", Image: "postgres:16", Mode: UpdateModeImage},
		Skipped:    true,
		SkipReason: "pre-check returned status 503",
		OldRef:     "sha256:oldoldoldold0000",
		NewRef:     "sha256:newnewnewnew1111",
		Pulled:     true,
		CheckedAt:  now,
	}}, now)

	st := s.Snapshot().Statuses["db"]
	assert.True(t, st.Skipped)
	assert.True(t, st.UpdateAvailable)
	assert.Equal(t, "pre-check returned status 503", st.SkipReason)
	assert.Equal(t, "newnewnewnew", st.AvailableRef)
}

func TestStoreRecordError(t *testing.T) {
	s := newStore()
	now := time.Now()

	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "web", Image: "nginx:latest", Mode: UpdateModeImage},
		Error:     errors.New("registry unreachable"),
		OldRef:    "sha256:currentdigest0000",
		CheckedAt: now,
	}}, now)

	st := s.Snapshot().Statuses["web"]
	assert.Equal(t, "registry unreachable", st.LastError)
	assert.False(t, st.UpdateAvailable, "no NewRef means nothing pending")
}

func TestStoreRecordErrorWithPendingUpdate(t *testing.T) {
	s := newStore()
	now := time.Now()

	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "web", Image: "nginx:latest", Mode: UpdateModeImage},
		Error:     errors.New("not healthy after update"),
		OldRef:    "sha256:oldoldoldold0000",
		NewRef:    "sha256:newnewnewnew1111",
		Pulled:    true,
		CheckedAt: now,
	}}, now)

	st := s.Snapshot().Statuses["web"]
	assert.Equal(t, "not healthy after update", st.LastError)
	assert.True(t, st.UpdateAvailable)
	assert.Equal(t, "newnewnewnew", st.AvailableRef)
}

func TestStoreRecordPreservesHistory(t *testing.T) {
	s := newStore()
	t1 := time.Now().Add(-time.Hour)
	t2 := time.Now()

	// First cycle: an update is applied (sets LastUpdated and LastPulled).
	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "app", Image: "app:latest", Mode: UpdateModeImage},
		Updated:   true,
		OldRef:    "sha256:oldoldoldold0000",
		NewRef:    "sha256:newnewnewnew1111",
		Pulled:    true,
		CheckedAt: t1,
	}}, t1)

	// Second cycle: up to date with no pull (git-style). History must survive.
	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "app", Image: "app:latest", Mode: UpdateModeGit},
		OldRef:    "sha256:newnewnewnew1111",
		Pulled:    false,
		CheckedAt: t2,
	}}, t2)

	st := s.Snapshot().Statuses["app"]
	assert.Equal(t, t1, st.LastUpdated, "LastUpdated preserved across cycles")
	assert.Equal(t, t1, st.LastPulled, "LastPulled preserved when no pull occurred")
	assert.Equal(t, t2, st.LastChecked, "LastChecked advances every cycle")
	assert.False(t, st.UpdateAvailable)
}

func TestStoreSnapshotIsCopy(t *testing.T) {
	s := newStore()
	now := time.Now()
	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "web", Image: "nginx", Mode: UpdateModeImage},
		CheckedAt: now,
	}}, now)

	snap := s.Snapshot()
	snap.Statuses["web"] = ContainerStatus{Name: "mutated"}
	snap.Statuses["injected"] = ContainerStatus{}

	// The store's own state must be unaffected by mutating the snapshot.
	fresh := s.Snapshot()
	assert.Equal(t, "web", fresh.Statuses["web"].Name)
	_, injected := fresh.Statuses["injected"]
	assert.False(t, injected)
}

func TestStoreSnapshotEmpty(t *testing.T) {
	s := newStore()
	snap := s.Snapshot()
	assert.Empty(t, snap.Statuses)
	assert.True(t, snap.LastCycle.IsZero())
}

package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failedResult is a cycle that found an update and could not apply it.
func failedResult(name string, at time.Time) UpdateResult {
	return UpdateResult{
		Container: ContainerInfo{Name: name, Image: "reg/" + name + ":latest", Mode: UpdateModeImage},
		OldRef:    "sha256:oldoldoldold0000",
		NewRef:    "sha256:newnewnewnew0000",
		Error:     errors.New("container " + name + " not healthy after update: exec /" + name + ": no such file or directory"),
		CheckedAt: at,
	}
}

// The same failure every cycle is a frozen deployment, and the run is the only
// thing that says so: one failure and the thousandth log the same line.
func TestStoreTracksAStuckRun(t *testing.T) {
	s := newStore()
	start := time.Now()

	s.Record([]UpdateResult{failedResult("web", start)}, start)
	st := s.Snapshot().Statuses["web"]
	assert.Equal(t, 1, st.StuckCycles)
	assert.Equal(t, start, st.StuckSince)
	assert.False(t, st.Stuck(), "a single failure is ordinary")

	for i := 1; i < 4; i++ {
		at := start.Add(time.Duration(i) * time.Hour)
		s.Record([]UpdateResult{failedResult("web", at)}, at)
	}
	st = s.Snapshot().Statuses["web"]
	assert.Equal(t, 4, st.StuckCycles)
	assert.Equal(t, start, st.StuckSince, "the run keeps the time it began")
	assert.True(t, st.Stuck())
}

// A pre-check that refuses forever freezes the deployment exactly as a failed
// start does, so it continues the same run.
func TestStoreCountsASkipAsStuck(t *testing.T) {
	s := newStore()
	now := time.Now()
	skip := UpdateResult{
		Container:  ContainerInfo{Name: "api", Mode: UpdateModeImage},
		NewRef:     "sha256:newnewnewnew0000",
		Skipped:    true,
		SkipReason: "pre-check returned 503",
		CheckedAt:  now,
	}
	s.Record([]UpdateResult{skip}, now)
	s.Record([]UpdateResult{skip}, now)

	st := s.Snapshot().Statuses["api"]
	assert.Equal(t, 2, st.StuckCycles)
	assert.True(t, st.Stuck())
}

// A cycle that applies the update, or finds nothing to apply, ends the run:
// the container then runs the version it was offered.
func TestStoreClearsTheRunOnSuccess(t *testing.T) {
	s := newStore()
	now := time.Now()
	s.Record([]UpdateResult{failedResult("web", now)}, now)
	s.Record([]UpdateResult{failedResult("web", now)}, now)
	require.True(t, s.Snapshot().Statuses["web"].Stuck())

	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "web", Mode: UpdateModeImage},
		NewRef:    "sha256:newnewnewnew0000",
		Updated:   true,
		CheckedAt: now,
	}}, now)

	st := s.Snapshot().Statuses["web"]
	assert.Zero(t, st.StuckCycles)
	assert.True(t, st.StuckSince.IsZero())
	assert.False(t, st.Stuck())
}

// The report names the container, the update it will not take, the age of the
// run and the reason. Without the age an operator cannot tell a transient
// failure from a deployment that has stood still for weeks.
func TestReportStuckNamesTheAgeAndTheReason(t *testing.T) {
	s := newStore()
	start := time.Now().Add(-72 * time.Hour)
	s.Record([]UpdateResult{failedResult("buildhost", start)}, start)
	s.Record([]UpdateResult{failedResult("buildhost", start)}, start)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	reportStuck(s.Snapshot(), start.Add(72*time.Hour))

	line := buf.String()
	assert.Contains(t, line, "buildhost is STUCK")
	assert.Contains(t, line, "newnewnewnew")
	assert.Contains(t, line, "72h0m0s")
	assert.Contains(t, line, "no such file or directory")
}

// Nothing stuck says nothing: a line on every quiet cycle is what teaches an
// operator to skim past the one that matters.
func TestReportStuckSaysNothingWhenHealthy(t *testing.T) {
	s := newStore()
	now := time.Now()
	s.Record([]UpdateResult{{
		Container: ContainerInfo{Name: "web", Mode: UpdateModeImage},
		OldRef:    "sha256:currentdigest0000",
		CheckedAt: now,
	}}, now)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	reportStuck(s.Snapshot(), now)

	assert.Empty(t, strings.TrimSpace(buf.String()))
}

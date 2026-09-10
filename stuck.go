package main

import (
	"log"
	"sort"
	"time"
)

// reportStuck says, once per cycle, which containers the updater could not
// move: an update they did not take, a check that keeps failing, or no
// possible check.
//
// The per-container failure line is already logged where the failure happens.
// It is identical on every cycle, so a deployment frozen for weeks reads
// exactly like one that failed a minute ago. This line carries the run and its
// age, which is the part an operator can act on.
//
// printf is where the lines go. A test passes its own rather than redirect the
// global logger, which two tests running at once cannot share.
func reportStuck(snap Snapshot, now time.Time, printf func(string, ...any)) {
	if printf == nil {
		printf = log.Printf
	}
	names := make([]string, 0, len(snap.Statuses))
	for name, st := range snap.Statuses {
		if st.Stuck() {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	for _, name := range names {
		st := snap.Statuses[name]
		age := now.Sub(st.StuckSince).Round(time.Minute)
		if st.UpdateAvailable {
			printf("ERROR container %s is STUCK: %s has been available for %s over %d cycles and is still not running. Reason: %s",
				name, availableDesc(st), age, st.StuckCycles, stuckReason(st))
			continue
		}
		printf("ERROR container %s is STUCK: it could not be checked for %s over %d cycles. Reason: %s",
			name, age, st.StuckCycles, stuckReason(st))
	}
}

func availableDesc(st ContainerStatus) string {
	if st.AvailableRef == "" {
		return "an update"
	}
	return "update " + st.AvailableRef
}

func stuckReason(st ContainerStatus) string {
	switch {
	case st.Unmonitorable != "":
		return st.Unmonitorable
	case st.LastError != "":
		return st.LastError
	case st.SkipReason != "":
		return "pre-check refused: " + st.SkipReason
	default:
		return "unknown"
	}
}

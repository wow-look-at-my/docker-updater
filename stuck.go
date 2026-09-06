package main

import (
	"log"
	"sort"
	"time"
)

// reportStuck says, once per cycle, which containers have been offered an
// update they did not take.
//
// The per-container failure line is already logged where the failure happens.
// It is identical on every cycle, so a deployment frozen for weeks reads
// exactly like one that failed a minute ago. This line carries the run and its
// age, which is the part an operator can act on.
func reportStuck(snap Snapshot, now time.Time) {
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
		log.Printf("ERROR container %s is STUCK: %s has been available for %s over %d cycles and is still not running. Reason: %s",
			name, availableDesc(st), now.Sub(st.StuckSince).Round(time.Minute), st.StuckCycles, stuckReason(st))
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
	case st.LastError != "":
		return st.LastError
	case st.SkipReason != "":
		return "pre-check refused: " + st.SkipReason
	default:
		return "unknown"
	}
}

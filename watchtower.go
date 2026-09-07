package main

// Watchtower is the other updater a host commonly runs, and the dashboard used
// to report everything it manages as "Manual".
//
// That reading is wrong in the direction that costs an operator time. A
// container watchtower updates IS auto-updated, so an empty Last-updated column
// against it reads as neglect, and the operator goes looking for a
// docker-updater label that was never meant to be there. Naming the updater
// says what is true: this container is somebody's, just not ours.
const (
	// watchtowerEnableLabel is watchtower's own opt-in. It is "true" on a
	// container watchtower updates and "false" on one it must leave alone --
	// which is the label's whole use under watchtower's default all-containers
	// mode, so "false" is an answer, not an absence.
	watchtowerEnableLabel = "com.centurylinklabs.watchtower.enable"
	// watchtowerScopeLabel pins a container to one watchtower instance. It
	// marks the container as watchtower's whatever the scope's value is.
	watchtowerScopeLabel = "com.centurylinklabs.watchtower.scope"
)

// The names the dashboard reports in the updater field.
const (
	updaterDockerUpdater = "docker-updater"
	updaterWatchtower    = "watchtower"
	updaterBoth          = "both"
)

// updaterOf reports which updater owns a container, from its labels alone.
//
// "both" is not a tidy third case: two updaters racing to replace one container
// is a real hazard, and each will recreate what the other just made. The
// dashboard surfaces it rather than pick a winner, because picking one hides
// the misconfiguration that produced it.
func updaterOf(labels map[string]string, ownLabel string) string {
	ours := labels[ownLabel] == "true"
	theirs := labels[watchtowerEnableLabel] == "true" || labels[watchtowerScopeLabel] != ""
	switch {
	case ours && theirs:
		return updaterBoth
	case ours:
		return updaterDockerUpdater
	case theirs:
		return updaterWatchtower
	default:
		return ""
	}
}

// watchtowerOwns reports whether watchtower updates this container, including
// the case where docker-updater claims it too.
func watchtowerOwns(updater string) bool {
	return updater == updaterWatchtower || updater == updaterBoth
}

// updaterRank orders the dashboard's rows: ours, then watchtower's, then the
// containers nothing updates. A container claimed by both leads, because it is
// the one that needs an edit.
func updaterRank(c apiContainer) int {
	switch {
	case c.Updater == updaterBoth:
		return 0
	case c.AutoUpdate:
		return 1
	case watchtowerOwns(c.Updater):
		return 2
	default:
		return 3
	}
}

// bothUpdatersWarning is the note a container claimed by both carries. It names
// the two labels, because the fix is deleting one of them.
const bothUpdatersWarning = "claimed by docker-updater and watchtower at once: both will recreate this container, each undoing the other. Remove one of the two labels."

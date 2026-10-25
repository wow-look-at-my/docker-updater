// Watchtower is the other updater a host commonly runs, and the dashboard used to report everything it manages as "Manual".

package main

// Watchtower is the other updater a host commonly runs, and the dashboard
// used to report everything it manages as "Manual".
const (
	// watchtowerEnableLabel is watchtower's own opt-in.
	watchtowerEnableLabel = "com.centurylinklabs.watchtower.enable"
	// watchtowerScopeLabel pins a container to one watchtower instance.
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
// "both" is not a tidy third case: updaters racing to replace one container
// is a real hazard, and each will recreate what the other made. The dashboard
// surfaces it rather than pick a winner, because picking one hides the
// misconfiguration that produced it.
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
// containers nothing updates.
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

// bothUpdatersWarning is the note a container claimed by both carries. It names both labels, because the fix is deleting one of them.
const bothUpdatersWarning = "claimed by docker-updater and watchtower at once: both will recreate this container, each undoing the other. Remove one of the two labels."

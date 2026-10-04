package main

import (
	"log"
	"net/http"
	"time"
)

// forceQueueDepth is how many force-update requests can wait for the loop.
const forceQueueDepth = 8

// forceUpdateHeader must be on every force-update request.
const forceUpdateHeader = "X-Docker-Updater-Action"

const recreateAction = "recreate"

// handleAPIRecreate queues (POST) or cancels (DELETE) a recreate of one
// container from the compose file it drifted from. A queued recreate waits for
// the next check, the pre-update gate and dry-run mode, like an update.
func (s *dashboardServer) handleAPIRecreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get(forceUpdateHeader) != recreateAction {
		http.Error(w, "missing "+forceUpdateHeader+": "+recreateAction+" header", http.StatusForbidden)
		return
	}
	name := r.URL.Query().Get("container")
	if name == "" {
		http.Error(w, "missing container parameter", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if r.Method == http.MethodDelete {
		if !s.store.CancelRecreate(name) {
			http.Error(w, "container "+name+" has no queued recreate", http.StatusConflict)
			return
		}
		log.Printf("dashboard: recreate of %s from its compose file cancelled", name)
		_, _ = w.Write([]byte("recreate of " + name + " cancelled"))
		return
	}

	st, ok := s.store.Snapshot().Statuses[name]
	if !ok || !st.Drift.applies() {
		http.Error(w, "container "+name+" has no compose drift the updater can apply", http.StatusConflict)
		return
	}
	s.store.QueueRecreate(name, st.Drift.FileHash, time.Now())
	log.Printf("dashboard: recreate of %s from its compose file queued (file hash %s)", name, shortID(st.Drift.FileHash))
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("recreate of " + name + " queued"))
}

// handleAPIForceUpdate queues an update of one container that has an update
// pending, or runs the queued recreate of one that drifted from its compose
// file. Either skips the pre-update gate and dry-run mode.
func (s *dashboardServer) handleAPIForceUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get(forceUpdateHeader) != "force-update" {
		http.Error(w, "missing "+forceUpdateHeader+": force-update header", http.StatusForbidden)
		return
	}
	if s.force == nil {
		http.Error(w, "force update is not available", http.StatusServiceUnavailable)
		return
	}
	name := r.URL.Query().Get("container")
	if name == "" {
		http.Error(w, "missing container parameter", http.StatusBadRequest)
		return
	}
	snap := s.store.Snapshot()
	st, ok := snap.Statuses[name]
	queued, isQueued := snap.Recreates[name]
	recreate := isQueued && st.Drift.applies() && queued.FileHash == st.Drift.FileHash
	if !ok || !(st.UpdateAvailable || recreate) {
		http.Error(w, "container "+name+" has no pending update and no queued recreate", http.StatusConflict)
		return
	}

	select {
	case s.force <- name:
	default:
		http.Error(w, "too many force updates are queued; try again later", http.StatusServiceUnavailable)
		return
	}
	log.Printf("dashboard: force update of %s queued", name)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("force update of " + name + " queued"))
}

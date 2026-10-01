package main

import (
	"log"
	"net/http"
)

// forceQueueDepth is how many force-update requests can wait for the loop.
const forceQueueDepth = 8

// forceUpdateHeader must be on every force-update request.
const forceUpdateHeader = "X-Docker-Updater-Action"

// handleAPIForceUpdate queues an update of one container that has an update
// pending. The update skips the pre-update gate and dry-run mode.
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
	st, ok := s.store.Snapshot().Statuses[name]
	if !ok || !st.UpdateAvailable {
		http.Error(w, "container "+name+" has no pending update", http.StatusConflict)
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

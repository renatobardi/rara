package main

import (
	"encoding/json"
	"net/http"
	"net/url"
)

// Agent task queue BFF (CONSOLE-#10c1) — thin proxy over rara-core's /v1/agent(-task)s surface,
// bearer injected, {id} path params validated before they reach the upstream.

// handleEnqueueAgentTask proxies POST /v1/agents/{id}/tasks — enqueues a task for an agent.
func (s *server) handleEnqueueAgentTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent id"})
		return
	}
	status, body, err := s.postCore(r.Context(), "/v1/agents/"+id+"/tasks", r.Body)
	proxyWrite(w, status, body, err)
}

// handleAgentTasks proxies GET /v1/agents/{id}/tasks — one agent's task history.
func (s *server) handleAgentTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent id"})
		return
	}
	body, err := s.fetchCore(r.Context(), "/v1/agents/"+id+"/tasks")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// agentTaskStatuses mirrors the core agent_tasks.status CHECK. The BFF validates the filter itself
// so an unknown value is a clean 400 here, rather than a core 400 that fetchCore would collapse
// into a 502 (bad gateway) — a misleading status for what is really client input.
var agentTaskStatuses = map[string]bool{
	"queued": true, "dispatched": true, "running": true,
	"done": true, "failed": true, "cancelled": true,
}

// handleAgentTaskFeed proxies GET /v1/agent-tasks — the global board feed, forwarding only the
// optional ?status= filter (re-encoded to prevent query-parameter injection).
func (s *server) handleAgentTaskFeed(w http.ResponseWriter, r *http.Request) {
	path := "/v1/agent-tasks"
	if status := r.URL.Query().Get("status"); status != "" {
		if !agentTaskStatuses[status] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid status"})
			return
		}
		q := url.Values{}
		q.Set("status", status)
		path += "?" + q.Encode()
	}
	body, err := s.fetchCore(r.Context(), path)
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

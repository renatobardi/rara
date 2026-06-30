package main

import (
	"encoding/json"
	"net/http"
)

// Agent registry BFF (CONSOLE-#10b) — thin proxy over rara-core's /v1/agents surface, with the
// bearer injected and {id} path params validated before they reach the upstream.

// handleAgents proxies GET /v1/agents — the agent roster.
func (s *server) handleAgents(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/agents")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleUpsertAgent proxies PUT /v1/agents.
func (s *server) handleUpsertAgent(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.putCore(r.Context(), "/v1/agents", r.Body)
	proxyWrite(w, status, body, err)
}

// handleGetAgent proxies GET /v1/agents/{id} — the agent with its attached skill ids.
// Uses fetchCoreWithStatus so the core's status propagates: a missing/soft-deleted agent is a 404,
// not a 502 (plain fetchCore collapses every non-2xx into bad-gateway).
func (s *server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent id"})
		return
	}
	status, body, err := s.fetchCoreWithStatus(r.Context(), "/v1/agents/"+id)
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, status, json.RawMessage(body))
}

// handleDeleteAgent proxies DELETE /v1/agents/{id}. Rejects non-numeric ids before the upstream.
func (s *server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent id"})
		return
	}
	status, body, err := s.doCore(r.Context(), http.MethodDelete, "/v1/agents/"+id, r.Body)
	proxyWrite(w, status, body, err)
}

// handleSetAgentSkills proxies PUT /v1/agents/{id}/skills — replaces the agent's skill set.
func (s *server) handleSetAgentSkills(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid agent id"})
		return
	}
	status, body, err := s.putCore(r.Context(), "/v1/agents/"+id+"/skills", r.Body)
	proxyWrite(w, status, body, err)
}

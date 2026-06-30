package main

import (
	"encoding/json"
	"net/http"
)

// Skill registry BFF (CONSOLE-#10a) — thin proxy over rara-core's /v1/skills surface, with the
// bearer injected and {id} path params validated before they reach the upstream. An imported
// skill is born trusted=false in core; the trust toggle is just a full-record PUT.

// handleSkills proxies GET /v1/skills — the skill registry (SKILL.md + config + trusted flag).
func (s *server) handleSkills(w http.ResponseWriter, r *http.Request) {
	body, err := s.fetchCore(r.Context(), "/v1/skills")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleUpsertSkill proxies PUT /v1/skills.
func (s *server) handleUpsertSkill(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.putCore(r.Context(), "/v1/skills", r.Body)
	proxyWrite(w, status, body, err)
}

// handleImportSkill proxies POST /v1/skills/import — core fetches the URL and stores trusted=false.
func (s *server) handleImportSkill(w http.ResponseWriter, r *http.Request) {
	status, body, err := s.postCore(r.Context(), "/v1/skills/import", r.Body)
	proxyWrite(w, status, body, err)
}

// handleDeleteSkill proxies DELETE /v1/skills/{id}. Rejects non-numeric ids before the upstream.
func (s *server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill id"})
		return
	}
	status, body, err := s.doCore(r.Context(), http.MethodDelete, "/v1/skills/"+id, r.Body)
	proxyWrite(w, status, body, err)
}

// handleSkillFiles proxies GET /v1/skills/{id}/files.
func (s *server) handleSkillFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill id"})
		return
	}
	body, err := s.fetchCore(r.Context(), "/v1/skills/"+id+"/files")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(body))
}

// handleUpsertSkillFile proxies PUT /v1/skills/{id}/files.
func (s *server) handleUpsertSkillFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill id"})
		return
	}
	status, body, err := s.putCore(r.Context(), "/v1/skills/"+id+"/files", r.Body)
	proxyWrite(w, status, body, err)
}

// handleDeleteSkillFile proxies DELETE /v1/skills/{id}/files (path travels in the body).
func (s *server) handleDeleteSkillFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isNumericID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid skill id"})
		return
	}
	status, body, err := s.doCore(r.Context(), http.MethodDelete, "/v1/skills/"+id+"/files", r.Body)
	proxyWrite(w, status, body, err)
}

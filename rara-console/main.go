// rara-console — the operator/curator UI for the rara 2.0 control plane.
//
// One Go binary: it serves the SvelteKit SPA (embedded via embed.FS, so production is a single
// artifact, no Node at runtime) and acts as a thin BFF in front of the rara-core surface. The
// console holds the surface bearer token SERVER-SIDE and proxies/aggregates reads — the SPA never
// sees the token, and the console never touches Neon directly (rara-core is the single source of
// truth). It binds to the tailnet interface only (CONSOLE_ADDR), never the public Oracle IP.
//
// C0 scope: the shell + the "Visão geral" screen, which calls /api/overview — a real aggregate of
// the live surface (flows + providers) that proves the BFF end to end.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"
)

//go:embed all:web/build
var embedded embed.FS

// server is the BFF: it talks to the rara-core surface at coreURL, authenticating with the
// server-side token. client is injected so handlers are unit-testable against an httptest core.
type server struct {
	coreURL string
	token   string
	client  *http.Client
}

// fetchCore does an authenticated GET against the surface and returns the raw JSON body. A
// transport failure or any non-2xx status is an error — the caller maps it to 502 (bad gateway).
func (s *server) fetchCore(ctx context.Context, path string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.coreURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &gatewayError{status: resp.StatusCode}
	}
	return body, nil
}

type gatewayError struct{ status int }

func (e *gatewayError) Error() string {
	return "core surface returned status " + http.StatusText(e.status)
}

// handleOverview aggregates the two seeded reads the Visão geral needs into one response, so the
// SPA makes a single request and never learns the surface URL or token.
func (s *server) handleOverview(w http.ResponseWriter, r *http.Request) {
	flows, err := s.fetchCore(r.Context(), "/v1/flows")
	if err != nil {
		badGateway(w, err)
		return
	}
	providers, err := s.fetchCore(r.Context(), "/v1/providers")
	if err != nil {
		badGateway(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Flows     json.RawMessage `json:"flows"`
		Providers json.RawMessage `json:"providers"`
	}{flows, providers})
}

// handleHealthz is the console's own liveness probe; it also reports whether the core surface is
// reachable so a deploy can confirm the BFF link is live. It is always 200 (the console is up).
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.fetchCore(ctx, "/healthz")
	writeJSON(w, http.StatusOK, map[string]any{"console": true, "core": err == nil})
}

func badGateway(w http.ResponseWriter, err error) {
	log.Printf("console: core surface unreachable: %v", err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "core surface unreachable"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("console: encode response: %v", err)
	}
}

func main() {
	s := &server{
		coreURL: mustEnv("CORE_SURFACE_URL"), // e.g. http://100.x.x.x:8080
		token:   mustEnv("SURFACE_TOKEN"),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
	addr := mustEnv("CONSOLE_ADDR") // tailnet IP only, e.g. 100.x.x.x:8081 — never 0.0.0.0

	dist, err := fs.Sub(embedded, "web/build")
	if err != nil {
		log.Fatalf("console: embedded SPA: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /", spaHandler(dist))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("rara-console: listening on %s (core=%s)", addr, s.coreURL)
	log.Fatal(srv.ListenAndServe())
}

// spaHandler serves the embedded static build, falling back to index.html for any unknown path so
// client-side routing works (adapter-static SPA fallback).
func spaHandler(dist fs.FS) http.Handler {
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(dist, trimSlash(r.URL.Path)); err != nil {
			r.URL.Path = "/" // unknown route -> index.html
		}
		files.ServeHTTP(w, r)
	})
}

func trimSlash(p string) string {
	if p == "/" || p == "" {
		return "index.html"
	}
	return p[1:]
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("console: %s is required", k)
	}
	return v
}

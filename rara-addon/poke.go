// poke.go — the worker side of symmetric activation.
//
// Trabalho = pull always; ativação = symmetric. The orchestrator pokes a resident worker over the
// tailnet to drain NOW instead of waiting for the next poll tick; the slow poll stays as the safety
// net (a poke is best-effort — it never wakes a sleeping Mac). This is a deliberately tiny listener:
// one authenticated endpoint that signals "drain". The actual draining stays in the Run loop, which
// selects on the channel this listener feeds.
package addon

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"time"
)

// startPokeListener binds a minimal HTTP server on addr and returns it started, with the resolved
// listen address (so a ":0" bind can be discovered). POST /poke (Bearer token) does a non-blocking
// send on pokeCh to request a drain; GET /healthz is an unauthenticated liveness probe. Auth fails
// CLOSED — an empty token rejects everything — so the listener is never accidentally open. addr is
// expected to be tailnet-only (bound by the host's network config).
func startPokeListener(addr, token string, pokeCh chan<- struct{}) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}

	want := []byte(token)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/poke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		got, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !hasBearer || len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Coalesce: a full buffer already has a drain pending, and one drain empties whatever
		// accumulated — so a dropped send loses nothing.
		select {
		case pokeCh <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return srv, ln.Addr().String(), nil
}

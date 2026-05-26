package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// authMiddleware enforces the configured auth mode on every handler EXCEPT
// /healthz (load balancers and Kubernetes probes need it open) and the
// static frontend (the index page itself needs to load before the user can
// authenticate). API endpoints under /api/v1/* are always gated.
//
// init-plan.md §4 documents that production deployments should put this
// behind a real reverse proxy — basic/token are a backstop, not a substitute.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthMode == "none" || !needsAuth(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authorized(r) {
			s.challenge(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// needsAuth returns true for paths that require credentials when auth is
// enabled. Keep static assets + healthz + the OpenAPI spec open so the
// page can render and client generators can fetch the contract without
// credentials (the spec contains no secrets).
func needsAuth(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	if path == "/api/v1/openapi.yaml" {
		return false
	}
	return true
}

func (s *Server) authorized(r *http.Request) bool {
	switch s.cfg.AuthMode {
	case "basic":
		user, pass, ok := r.BasicAuth()
		if !ok {
			return false
		}
		return constantTimeEqual(user, s.cfg.BasicUser) && constantTimeEqual(pass, s.cfg.BasicPass)
	case "token":
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			return false
		}
		return constantTimeEqual(strings.TrimPrefix(header, prefix), s.cfg.APIToken)
	default:
		return true
	}
}

func (s *Server) challenge(w http.ResponseWriter) {
	if s.cfg.AuthMode == "basic" {
		w.Header().Set("WWW-Authenticate", `Basic realm="auditor"`)
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

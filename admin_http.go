package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/opentalon/tln-language/pkg/tln"
)

// adminServer hosts the tln-plugin's management HTTP API — rule CRUD
// only. Mounted by the opentalon host at the reverse-proxy path
// /{config-map-key}/*. Every request must carry
// `Authorization: Bearer <token>`; the token comes from the
// plugin's `admin_token` config field.
//
// tln-plugin is a gateway for executing the Tln language, not a
// data store, so it deliberately exposes no fact-seeding API.
//
// Auth model: a shared bearer secret only. The opentalon host's
// webhook gate is the outer perimeter; this token is the inner one
// so a leaked webhook session can't quietly rewrite production rules.
// Operators rotate the token by restarting the plugin with a new
// config block.
type adminServer struct {
	token string
	rules *ruleStore
}

// routes returns the configured http.Handler. Kept as a method so
// tests can mount the server directly via httptest without booting a
// real listener.
func (a *adminServer) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/rules", a.handleRules)
	mux.HandleFunc("/rules/", a.handleRule)
	mux.HandleFunc("/check", a.handleCheck)

	return a.authMiddleware(mux)
}

// authMiddleware enforces the bearer token on every request. Returns
// 401 with no detail on auth failure to keep the surface minimal for
// scanners; logs the failure server-side for audit.
func (a *adminServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.token == "" {
			http.Error(w, "admin api disabled", http.StatusServiceUnavailable)
			return
		}
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			slog.Warn("admin api: missing or malformed Authorization header",
				"remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := []byte(header[len(prefix):])
		want := []byte(a.token)
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			slog.Warn("admin api: invalid bearer token",
				"remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleRules covers /rules (no trailing name): GET lists, POST
// creates. The body for POST is the Tln source as a string
// alongside the rule's name; storing the metadata side-by-side
// in JSON keeps the API self-describing.
func (a *adminServer) handleRules(w http.ResponseWriter, r *http.Request) {
	if a.rules == nil {
		http.Error(w, "rules_dir not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		names, err := a.rules.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": names})
	case http.MethodPost:
		var body struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
			return
		}
		if body.Name == "" || body.Source == "" {
			writeError(w, http.StatusBadRequest, errors.New("name and source are required"))
			return
		}
		if err := a.rules.Save(body.Name, body.Source); err != nil {
			a.writeRuleError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"name": body.Name})
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCheck covers POST /check: validate Tln source WITHOUT storing or
// running it. Pure compile (lex → parse → resolve → validate → plan) via
// validateRuleSource — no MCP calls, no HostCaller, no side effects — so a
// host app (Timly) can deterministically verify machine-authored Tln before
// storing it, with no LLM in the loop and no need for the bidi host stream.
// Returns 200 {"ok":true} for valid source, or 200 {"ok":false,
// "diagnostics":"<compiler diagnostics>"} for invalid source. Needs no
// rules_dir — it is a pure function of the posted source.
func (a *adminServer) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Accept `source` (matches /rules) or `tln_source` (matches the agents
	// plugin's validate action arg) so either caller shape works.
	var body struct {
		Source    string `json:"source"`
		TlnSource string `json:"tln_source"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	src := body.Source
	if src == "" {
		src = body.TlnSource
	}
	if src == "" {
		writeError(w, http.StatusBadRequest, errors.New("source (or tln_source) is required"))
		return
	}
	if err := validateRuleSource(src); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "diagnostics": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "diagnostics": ""})
}

// handleRule covers /rules/{name}: GET fetches, PUT replaces, DELETE
// removes. The name comes from the URL so the rule's identity is
// always explicit at the path level.
func (a *adminServer) handleRule(w http.ResponseWriter, r *http.Request) {
	if a.rules == nil {
		http.Error(w, "rules_dir not configured", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/rules/")
	if name == "" {
		http.Error(w, "rule name required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		src, err := a.rules.Read(name)
		if err != nil {
			a.writeRuleError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(src))
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("read body: %w", err))
			return
		}
		if len(body) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("rule source required"))
			return
		}
		if err := a.rules.Save(name, string(body)); err != nil {
			a.writeRuleError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name})
	case http.MethodDelete:
		if err := a.rules.Delete(name); err != nil {
			a.writeRuleError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// writeRuleError maps storage errors to HTTP statuses. Keeps the
// handler bodies focused on the happy path.
func (a *adminServer) writeRuleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidRuleName):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, errRuleNotFound):
		writeError(w, http.StatusNotFound, err)
	default:
		// Compile errors from validateRuleSource are 400 (the request
		// is bad: invalid Tln source). Everything else is 500.
		if _, ok := err.(*tln.CompileError); ok {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

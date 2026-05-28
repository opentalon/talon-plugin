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

	"github.com/opentalon/talon-language/pkg/talon"
)

// adminServer hosts the talon-plugin's management HTTP API — rule
// CRUD and one-at-a-time fact CRUD against the configured Datalevin
// backend. Mounted by the opentalon host at the reverse-proxy path
// /{config-map-key}/*. Every request must carry
// `Authorization: Bearer <token>`; the token comes from the
// plugin's `admin_token` config field.
//
// Auth model: a shared bearer secret only. The opentalon host's
// webhook gate is the outer perimeter; this token is the inner one
// so a leaked webhook session can't quietly rewrite production rules.
// Operators rotate the token by restarting the plugin with a new
// config block.
type adminServer struct {
	token string
	rules *ruleStore
	// facts is the FactStore behind /facts/*. Nil when no
	// datalevin_url is configured; in that mode every /facts/*
	// request returns 503 — admins and the LLM-driven detect rules
	// see the same backend by construction, so a missing backend is
	// a setup problem to surface, not a silent no-op.
	facts talon.FactStore
}

// routes returns the configured http.Handler. Kept as a method so
// tests can mount the server directly via httptest without booting a
// real listener.
func (a *adminServer) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/rules", a.handleRules)
	mux.HandleFunc("/rules/", a.handleRule)
	mux.HandleFunc("/facts", a.handleFacts)
	mux.HandleFunc("/facts/", a.handleFact)

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
// creates. The body for POST is the Talon source as a string
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

// handleFacts covers /facts (no entity id): POST adds a single fact
// (entity + one-or-more attribute/value pairs) via a Transact call.
// JSON body: {"entity_id": 808, "attrs": {"name": "...", "stock": 12}}.
// Entity id and attrs are required; attrs values can be strings,
// numbers, or bools — anything json.Unmarshal yields into an any.
func (a *adminServer) handleFacts(w http.ResponseWriter, r *http.Request) {
	if a.facts == nil {
		http.Error(w, "fact store not configured (set datalevin_url)", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		EntityID int            `json:"entity_id"`
		Attrs    map[string]any `json:"attrs"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.EntityID == 0 {
		writeError(w, http.StatusBadRequest, errors.New("entity_id is required"))
		return
	}
	if len(body.Attrs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("attrs is required (non-empty)"))
		return
	}
	fact := map[string]any{"db/id": body.EntityID}
	for k, v := range body.Attrs {
		fact[k] = v
	}
	if err := a.facts.Transact(r.Context(), []map[string]any{fact}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"entity_id": body.EntityID, "attrs_set": len(body.Attrs)})
}

// handleFact covers /facts/{id} and /facts/{id}/{attr}:
//   - GET    /facts/{id}        — query all attrs on the entity
//   - PUT    /facts/{id}        — patch attrs (body: {"attrs": {...}})
//   - DELETE /facts/{id}        — retract the entity (all its attrs)
//   - DELETE /facts/{id}/{attr} — retract a single attribute
func (a *adminServer) handleFact(w http.ResponseWriter, r *http.Request) {
	if a.facts == nil {
		http.Error(w, "fact store not configured (set datalevin_url)", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/facts/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "entity id required", http.StatusBadRequest)
		return
	}
	entityID, err := parseEntityID(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		a.serveFactRead(w, r, entityID)
	case r.Method == http.MethodPut && len(parts) == 1:
		a.serveFactPut(w, r, entityID)
	case r.Method == http.MethodDelete && len(parts) == 1:
		a.serveFactDelete(w, r, entityID, "")
	case r.Method == http.MethodDelete && len(parts) == 2:
		a.serveFactDelete(w, r, entityID, parts[1])
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveFactRead returns every (attr, value) pair on the entity. Uses
// the universal Datalog query `[:find ?a ?v :where [<id> ?a ?v]]` so
// no schema knowledge is needed.
func (a *adminServer) serveFactRead(w http.ResponseWriter, r *http.Request, entityID int) {
	q := fmt.Sprintf(`[:find ?a ?v :where [%d ?a ?v]]`, entityID)
	rows, err := a.facts.Query(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	attrs := make(map[string]any, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			continue
		}
		k, ok := row[0].(string)
		if !ok {
			continue
		}
		attrs[k] = row[1]
	}
	if len(attrs) == 0 {
		http.Error(w, "entity not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entity_id": entityID, "attrs": attrs})
}

// serveFactPut updates one or more attributes on the entity. Body:
// {"attrs": {"current_stock": 8, "label": "new"}}. Datalevin treats
// asserting a new value for an attribute as an update — no separate
// retract-then-assert dance required.
func (a *adminServer) serveFactPut(w http.ResponseWriter, r *http.Request, entityID int) {
	var body struct {
		Attrs map[string]any `json:"attrs"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if len(body.Attrs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("attrs is required (non-empty)"))
		return
	}
	fact := map[string]any{"db/id": entityID}
	for k, v := range body.Attrs {
		fact[k] = v
	}
	if err := a.facts.Transact(r.Context(), []map[string]any{fact}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entity_id": entityID, "attrs_set": len(body.Attrs)})
}

// serveFactDelete retracts either the whole entity (attr == "") or a
// single attribute. Whole-entity deletion uses the special
// `:db.fn/retractEntity` operation; single-attr deletion uses
// `:db/retract` against the current value (which we look up first).
func (a *adminServer) serveFactDelete(w http.ResponseWriter, r *http.Request, entityID int, attr string) {
	if attr == "" {
		tx := []map[string]any{{
			":db/id": entityID,
			":db/op": ":db.fn/retractEntity",
		}}
		if err := a.facts.Transact(r.Context(), tx); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Per-attribute retract: look up the current value, then issue a
	// retract. If the attribute isn't set, return 404 so callers can
	// distinguish "I removed it" from "nothing to do".
	q := fmt.Sprintf(`[:find ?v :where [%d %q ?v]]`, entityID, attr)
	rows, err := a.facts.Query(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		http.Error(w, "attribute not set on entity", http.StatusNotFound)
		return
	}
	tx := []map[string]any{{
		":db/id":     entityID,
		":db/op":     ":db/retract",
		":db/attr":   attr,
		":db/value":  rows[0][0],
	}}
	if err := a.facts.Transact(r.Context(), tx); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseEntityID parses a positive integer entity id from a URL
// fragment, rejecting anything else (including negatives, zero, and
// non-numeric).
func parseEntityID(s string) (int, error) {
	id := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("entity id must be a positive integer: %q", s)
		}
		id = id*10 + int(c-'0')
		if id > 1<<31 { // arbitrary sanity cap; Datalevin ids are 64-bit but operators won't use these
			return 0, fmt.Errorf("entity id out of range: %q", s)
		}
	}
	if id == 0 {
		return 0, fmt.Errorf("entity id must be a positive integer: %q", s)
	}
	return id, nil
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
		// is bad: invalid Talon source). Everything else is 500.
		if _, ok := err.(*talon.CompileError); ok {
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

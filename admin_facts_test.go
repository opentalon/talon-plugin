package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/talon-language/pkg/talon"
)

// fakeFactStore is a minimal in-memory backend implementing
// talon.FactStore. Lets the /facts/* handler tests run without
// hitting a real Datalevin server.
type fakeFactStore struct {
	mu       sync.Mutex
	tx       []map[string]any
	queries  []string
	respond  func(q string) ([][]any, error)
	failTx   error
}

func (f *fakeFactStore) Query(_ context.Context, q string) ([][]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.respond != nil {
		return f.respond(q)
	}
	return nil, nil
}

func (f *fakeFactStore) Transact(_ context.Context, tx []map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTx != nil {
		return f.failTx
	}
	f.tx = append(f.tx, tx...)
	return nil
}

func (f *fakeFactStore) Schema(_ context.Context, _ map[string]map[string]string) error {
	return nil
}

var _ talon.FactStore = (*fakeFactStore)(nil)

// newFactTestServer wires the admin server with a fake fact store. The
// rule store gets a temp dir too so /rules endpoints still respond
// (the test uses /facts only but the handler needs the field).
func newFactTestServer(t *testing.T) (*httptest.Server, *fakeFactStore) {
	t.Helper()
	fs := &fakeFactStore{}
	a := &adminServer{
		token: "tok",
		rules: &ruleStore{RootDir: t.TempDir()},
		facts: fs,
	}
	srv := httptest.NewServer(a.routes())
	t.Cleanup(srv.Close)
	return srv, fs
}

func TestFacts_PostAddsFact(t *testing.T) {
	srv, fs := newFactTestServer(t)
	body, _ := json.Marshal(map[string]any{
		"entity_id": 808,
		"attrs":     map[string]any{"name": "Cement", "current_stock": 12.0},
	})
	resp := do(t, http.MethodPost, srv.URL+"/facts", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if len(fs.tx) != 1 {
		t.Fatalf("expected 1 Transact call, got %d", len(fs.tx))
	}
	fact := fs.tx[0]
	if fact["db/id"] != 808 {
		t.Errorf("db/id = %v, want 808", fact["db/id"])
	}
	if fact["name"] != "Cement" {
		t.Errorf("name = %v", fact["name"])
	}
}

func TestFacts_PostRejectsMissingEntityID(t *testing.T) {
	srv, _ := newFactTestServer(t)
	body, _ := json.Marshal(map[string]any{"attrs": map[string]any{"name": "x"}})
	resp := do(t, http.MethodPost, srv.URL+"/facts", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestFacts_PostRejectsEmptyAttrs(t *testing.T) {
	srv, _ := newFactTestServer(t)
	body, _ := json.Marshal(map[string]any{"entity_id": 808})
	resp := do(t, http.MethodPost, srv.URL+"/facts", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestFacts_GetReadsEntity(t *testing.T) {
	srv, fs := newFactTestServer(t)
	fs.respond = func(_ string) ([][]any, error) {
		return [][]any{
			{"name", "Cement"},
			{"current_stock", 12.0},
		}, nil
	}
	resp := do(t, http.MethodGet, srv.URL+"/facts/808", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var got struct {
		EntityID int            `json:"entity_id"`
		Attrs    map[string]any `json:"attrs"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.EntityID != 808 {
		t.Errorf("entity_id = %d", got.EntityID)
	}
	if got.Attrs["name"] != "Cement" {
		t.Errorf("name = %v", got.Attrs["name"])
	}
}

func TestFacts_GetNotFound(t *testing.T) {
	srv, fs := newFactTestServer(t)
	fs.respond = func(_ string) ([][]any, error) { return nil, nil }
	resp := do(t, http.MethodGet, srv.URL+"/facts/999", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

func TestFacts_PutPatchesAttrs(t *testing.T) {
	srv, fs := newFactTestServer(t)
	body, _ := json.Marshal(map[string]any{"attrs": map[string]any{"current_stock": 8.0}})
	resp := do(t, http.MethodPut, srv.URL+"/facts/808", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if len(fs.tx) != 1 {
		t.Fatalf("expected 1 Transact, got %d", len(fs.tx))
	}
	if fs.tx[0]["current_stock"] != 8.0 {
		t.Errorf("current_stock = %v", fs.tx[0]["current_stock"])
	}
}

func TestFacts_DeleteEntity(t *testing.T) {
	srv, fs := newFactTestServer(t)
	resp := do(t, http.MethodDelete, srv.URL+"/facts/808", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status %d, want 204", resp.StatusCode)
	}
	if len(fs.tx) != 1 {
		t.Fatalf("expected 1 Transact, got %d", len(fs.tx))
	}
	if fs.tx[0][":db/op"] != ":db.fn/retractEntity" {
		t.Errorf("expected retractEntity op, got %+v", fs.tx[0])
	}
}

func TestFacts_DeleteAttribute(t *testing.T) {
	srv, fs := newFactTestServer(t)
	fs.respond = func(_ string) ([][]any, error) {
		return [][]any{{12.0}}, nil
	}
	resp := do(t, http.MethodDelete, srv.URL+"/facts/808/current_stock", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if len(fs.tx) != 1 {
		t.Fatalf("expected 1 Transact, got %d", len(fs.tx))
	}
	if fs.tx[0][":db/op"] != ":db/retract" {
		t.Errorf("expected retract op, got %+v", fs.tx[0])
	}
	if fs.tx[0][":db/attr"] != "current_stock" {
		t.Errorf("expected attr current_stock, got %v", fs.tx[0][":db/attr"])
	}
}

func TestFacts_DeleteAttributeNotSet(t *testing.T) {
	srv, fs := newFactTestServer(t)
	fs.respond = func(_ string) ([][]any, error) { return nil, nil } // no rows
	resp := do(t, http.MethodDelete, srv.URL+"/facts/808/missing", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	// No Transact issued — the read showed nothing to remove.
	if len(fs.tx) != 0 {
		t.Errorf("unexpected Transact calls: %+v", fs.tx)
	}
}

func TestFacts_DisabledWithoutDatalevin(t *testing.T) {
	a := &adminServer{
		token: "tok",
		rules: &ruleStore{RootDir: t.TempDir()},
		facts: nil, // no datalevin_url configured
	}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	resp := do(t, http.MethodPost, srv.URL+"/facts", "tok", strings.NewReader(`{"entity_id":1,"attrs":{"x":1}}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
}

func TestFacts_AuthRequired(t *testing.T) {
	srv, _ := newFactTestServer(t)
	resp := do(t, http.MethodPost, srv.URL+"/facts", "", strings.NewReader(`{}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
}

func TestFacts_InvalidEntityID(t *testing.T) {
	srv, _ := newFactTestServer(t)
	for _, bad := range []string{"abc", "-1", "0", "999999999999"} {
		resp := do(t, http.MethodGet, srv.URL+"/facts/"+bad, "tok", nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("entity_id=%q: status %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestFacts_BackendError(t *testing.T) {
	srv, fs := newFactTestServer(t)
	fs.failTx = errors.New("backend exploded")
	body, _ := json.Marshal(map[string]any{"entity_id": 1, "attrs": map[string]any{"x": 1}})
	resp := do(t, http.MethodPost, srv.URL+"/facts", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", resp.StatusCode)
	}
}

func TestFacts_MethodNotAllowed(t *testing.T) {
	srv, _ := newFactTestServer(t)
	resp := do(t, http.MethodGet, srv.URL+"/facts", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", resp.StatusCode)
	}
}

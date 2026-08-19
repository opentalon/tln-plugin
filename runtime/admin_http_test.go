package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer spins up the admin server against a fresh temp dir.
// Returns the server, its base URL, and a cleanup. token is the
// expected bearer; pass empty to test the auth-disabled path.
func newTestServer(t *testing.T, token string) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	a := &adminServer{
		token: token,
		rules: &ruleStore{RootDir: dir},
	}
	srv := httptest.NewServer(a.routes())
	t.Cleanup(srv.Close)
	return srv, dir
}

// do issues an HTTP request with a bearer header and returns the
// response — saves boilerplate in every test.
func do(t *testing.T, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAdmin_AuthRequired(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	// No token → 401.
	resp := do(t, http.MethodGet, srv.URL+"/rules", "", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", resp.StatusCode)
	}

	// Wrong token → 401.
	resp = do(t, http.MethodGet, srv.URL+"/rules", "nope", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", resp.StatusCode)
	}

	// Right token → 200.
	resp = do(t, http.MethodGet, srv.URL+"/rules", "secret", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("right token: status %d, want 200", resp.StatusCode)
	}
}

func TestAdmin_DisabledWithoutToken(t *testing.T) {
	// Empty server token = no auth model = refuse to serve.
	srv, _ := newTestServer(t, "")
	resp := do(t, http.MethodGet, srv.URL+"/rules", "anything", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
}

func TestAdmin_RuleCRUD(t *testing.T) {
	srv, dir := newTestServer(t, "tok")

	src := `
workflow "test" {
  step "s1" {
    tool "srv" "tool" {
      x 1
    }
  }
}`

	// POST /rules creates.
	body, _ := json.Marshal(map[string]string{"name": "fleet", "source": src})
	resp := do(t, http.MethodPost, srv.URL+"/rules", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST: status %d, body %s", resp.StatusCode, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "fleet.tln")); err != nil {
		t.Errorf("file not written: %v", err)
	}

	// GET /rules lists.
	resp = do(t, http.MethodGet, srv.URL+"/rules", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	var listed struct {
		Rules []string `json:"rules"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	if len(listed.Rules) != 1 || listed.Rules[0] != "fleet" {
		t.Errorf("list: %+v", listed.Rules)
	}

	// GET /rules/{name} reads.
	resp = do(t, http.MethodGet, srv.URL+"/rules/fleet", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), `workflow "test"`) {
		t.Errorf("GET content: %q", got)
	}

	// PUT /rules/{name} replaces.
	newSrc := strings.Replace(src, "test", "test2", 1)
	resp = do(t, http.MethodPut, srv.URL+"/rules/fleet", "tok", strings.NewReader(newSrc))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: status %d, body %s", resp.StatusCode, b)
	}

	// DELETE removes.
	resp = do(t, http.MethodDelete, srv.URL+"/rules/fleet", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE: status %d, want 204", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "fleet.tln")); !os.IsNotExist(err) {
		t.Errorf("file still present after delete: %v", err)
	}
}

func TestAdmin_RejectsInvalidTln(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	body, _ := json.Marshal(map[string]string{"name": "broken", "source": "workflow \"x\" { step \"s\" {"})
	resp := do(t, http.MethodPost, srv.URL+"/rules", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status %d, want 400; body=%s", resp.StatusCode, b)
	}
}

func TestAdmin_AcceptsDetectRule(t *testing.T) {
	// Detect-bearing rules are valid Tln — the SDK returns
	// ErrRequiresFactStore as the "this is a detect rule, not a
	// workflow" signal during dry-compile. The store must accept it.
	srv, _ := newTestServer(t, "tok")
	detectSrc := `
detect "Low stock" {
  for records where type == "stock_item"
    and attr "current_stock" <= attr "minimum_amount"
  flag matching items
  label "{item.name}: low"
  priority HIGH
}`
	body, _ := json.Marshal(map[string]string{"name": "low_stock", "source": detectSrc})
	resp := do(t, http.MethodPost, srv.URL+"/rules", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status %d, want 201; body=%s", resp.StatusCode, b)
	}
}

func TestAdmin_InvalidRuleName(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	body, _ := json.Marshal(map[string]string{"name": "../escape", "source": "workflow \"x\" {}"})
	resp := do(t, http.MethodPost, srv.URL+"/rules", "tok", bytes.NewReader(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestAdmin_NotFound(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	resp := do(t, http.MethodGet, srv.URL+"/rules/missing", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

func TestAdmin_MethodNotAllowed(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	resp := do(t, http.MethodPatch, srv.URL+"/rules", "tok", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", resp.StatusCode)
	}
}

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newListenerTestServer builds a real server (cert, database, web UI) with
// one dummy agent binary, without opening any sockets.
func newListenerTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	dir := t.TempDir()
	bins := filepath.Join(dir, "agents")
	if err := os.MkdirAll(bins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bins, "backup-agent-linux-amd64"), []byte("ELF"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.StorageDir = filepath.Join(dir, "storage")
	cfg.AgentBins = bins
	if cfg.Listen == "" {
		cfg.Listen = ":8443"
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.store.Close() })
	return s
}

func statusOf(h http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

// Routes that belong to the admin side. On the agent listener every one of
// them must be absent (404/405), so publishing that port exposes no login
// page, no GUI asset and no admin API.
var adminOnly = []struct{ method, path string }{
	{"GET", "/"},
	{"GET", "/login"},
	{"GET", "/setup"},
	{"GET", "/clients"},
	{"GET", "/settings"},
	{"GET", "/static/app.js"},
	{"GET", "/static/app.css"},
	{"POST", "/api/admin/login"},
	{"POST", "/api/admin/setup"},
	{"GET", "/api/admin/overview"},
	{"GET", "/api/admin/storage"},
	{"GET", "/api/admin/storage/oauth/callback"},
}

func TestAgentHandlerServesOnlyAgentRoutes(t *testing.T) {
	s := newListenerTestServer(t, Config{AdminListen: ":8444", ServerName: "backup.example.com"})
	h := s.agentHandler()

	for _, rt := range adminOnly {
		if code := statusOf(h, rt.method, rt.path); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("agent listener %s %s = %d, want 404/405", rt.method, rt.path, code)
		}
	}

	for _, p := range []string{"/static/install-agent.sh", "/static/install-agent.ps1", "/dl/backup-agent-linux-amd64"} {
		if code := statusOf(h, "GET", p); code != http.StatusOK {
			t.Errorf("agent listener GET %s = %d, want 200", p, code)
		}
	}
	// Agent API is present (rejects the bad request, but the route exists).
	if code := statusOf(h, "POST", "/api/agent/enroll"); code == http.StatusNotFound {
		t.Errorf("agent listener POST /api/agent/enroll = 404, want the enroll handler")
	}
	if code := statusOf(h, "POST", "/api/agent/blobs/check"); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Errorf("agent listener unauthenticated blob check = %d, want 401/403", code)
	}
}

func TestAdminHandlerServesNoAgentRoutes(t *testing.T) {
	s := newListenerTestServer(t, Config{AdminListen: ":8444", ServerName: "backup.example.com"})
	h := s.adminHandler()

	for _, p := range []string{"/api/agent/enroll", "/api/agent/blobs/check"} {
		if code := statusOf(h, "POST", p); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("admin listener POST %s = %d, want 404/405", p, code)
		}
	}
	if code := statusOf(h, "GET", "/dl/backup-agent-linux-amd64"); code != http.StatusNotFound {
		t.Errorf("admin listener GET /dl/... = %d, want 404", code)
	}
	for _, p := range []string{"/setup", "/static/app.js", "/static/install-agent.sh"} {
		if code := statusOf(h, "GET", p); code != http.StatusOK {
			t.Errorf("admin listener GET %s = %d, want 200", p, code)
		}
	}
}

func TestCombinedHandlerServesEverything(t *testing.T) {
	s := newListenerTestServer(t, Config{})
	h := s.combinedHandler()
	for _, p := range []string{"/setup", "/static/app.js", "/static/install-agent.sh",
		"/static/install-agent.ps1", "/dl/backup-agent-linux-amd64"} {
		if code := statusOf(h, "GET", p); code != http.StatusOK {
			t.Errorf("combined GET %s = %d, want 200", p, code)
		}
	}
	if code := statusOf(h, "POST", "/api/agent/enroll"); code == http.StatusNotFound {
		t.Errorf("combined POST /api/agent/enroll = 404")
	}
	// The install script must be the real file, not a directory listing.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/install-agent.sh", nil))
	if !strings.HasPrefix(rec.Body.String(), "#!") {
		t.Errorf("install-agent.sh body does not look like the script: %.40q", rec.Body.String())
	}
}

func TestSplitModeURLs(t *testing.T) {
	// Behind a tunnel the admin request's Host is the admin hostname; it must
	// never leak into enrollment commands.
	r := httptest.NewRequest("POST", "/api/admin/tokens", nil)
	r.Host = "backup-admin.example.com"

	s := &Server{cfg: Config{Listen: ":8443", AdminListen: ":8444", ServerName: "backup.example.com"}}
	if got := s.agentBaseURL(r); got != "https://backup.example.com:8443" {
		t.Errorf("split agentBaseURL without CB_PUBLIC_URL = %q, want server name + agent port", got)
	}
	if got := s.adminBaseURL(r); got != "https://backup-admin.example.com" {
		t.Errorf("split adminBaseURL without CB_ADMIN_URL = %q, want the request host", got)
	}
	if got := s.adminBaseURL(nil); got != "https://backup.example.com:8444" {
		t.Errorf("split adminBaseURL(nil) = %q, want server name + admin port", got)
	}

	s.cfg.PublicURL = "https://agents.example.com:8443"
	s.cfg.AdminURL = "https://backup-admin.example.com"
	if got := s.agentBaseURL(r); got != "https://agents.example.com:8443" {
		t.Errorf("agentBaseURL = %q, want CB_PUBLIC_URL", got)
	}
	want := "https://backup-admin.example.com" + oauthCallbackPath
	if got := s.oauthRedirectURL(); got != want {
		t.Errorf("oauthRedirectURL = %q, want %q (CB_ADMIN_URL)", got, want)
	}
	if got := s.oauthRedirectURLFromRequest(r, ""); got != want {
		t.Errorf("oauthRedirectURLFromRequest = %q, want %q", got, want)
	}
}

func TestCombinedModeAdminURLMatchesPublicURL(t *testing.T) {
	// Unchanged behaviour when CB_ADMIN_LISTEN is unset: the OAuth callback
	// still uses CB_PUBLIC_URL, as before the split existed.
	s := &Server{cfg: Config{Listen: ":8443", PublicURL: "https://backup.example.com:8443"}}
	if got := s.adminBaseURL(nil); got != "https://backup.example.com:8443" {
		t.Errorf("combined adminBaseURL = %q, want CB_PUBLIC_URL", got)
	}
}

func TestValidateListeners(t *testing.T) {
	bad := []Config{
		{Listen: ":8443", AdminListen: ":8443", ServerName: "x"},
		{Listen: ":8443", AdminListen: "0.0.0.0:8443", ServerName: "x"},
		{Listen: ":8443", AdminListen: ":8444"}, // no way to know the agent address
	}
	for _, c := range bad {
		if err := validateListeners(c); err == nil {
			t.Errorf("validateListeners(%+v) = nil, want an error", c)
		}
	}
	good := []Config{
		{Listen: ":8443"},
		{Listen: ":8443", AdminListen: "127.0.0.1:8444", ServerName: "backup.example.com"},
		{Listen: ":8443", AdminListen: ":8444", PublicURL: "https://agents.example.com:8443"},
	}
	for _, c := range good {
		if err := validateListeners(c); err != nil {
			t.Errorf("validateListeners(%+v) = %v, want nil", c, err)
		}
	}
}

func TestNewRejectsInvalidAdminURL(t *testing.T) {
	dir := t.TempDir() + "/never-created"
	_, err := New(Config{DataDir: dir, Listen: ":8443", AdminListen: ":8444",
		ServerName: "x", AdminURL: "http://admin.example.com"})
	if err == nil || !strings.Contains(err.Error(), "CB_ADMIN_URL") {
		t.Fatalf("New with http:// CB_ADMIN_URL: err = %v, want a CB_ADMIN_URL error", err)
	}
}

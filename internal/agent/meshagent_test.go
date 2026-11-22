//go:build !windows

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubMesh replaces the platform checks: "installed" means the file the fake
// installer creates exists.
func stubMesh(t *testing.T, flag string) {
	t.Helper()
	oldI, oldC, oldS := meshInstalled, meshInContainer, meshSettle
	meshInstalled = func() bool { _, err := os.Stat(flag); return err == nil }
	meshInContainer = func() bool { return false }
	meshSettle = time.Millisecond
	t.Cleanup(func() { meshInstalled, meshInContainer, meshSettle = oldI, oldC, oldS })
}

func meshTestAgent(t *testing.T, h http.HandlerFunc) (*Agent, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	state := t.TempDir()
	return &Agent{stateDir: state, client: &serverClient{
		base: srv.URL, creds: &Credentials{AgentID: "agent1", Secret: "s"}, http: srv.Client(),
	}}, filepath.Join(state, meshMarker)
}

func TestMeshAgentInstalledFromServer(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "installed")
	stubMesh(t, flag)
	var gotQuery string
	a, marker := meshTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/meshagent" || r.Header.Get("X-Agent-Id") != "agent1" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Mesh-File-Name", "../../evil/meshagent") // must be reduced to a base name
		w.Header().Set("X-Mesh-Install-Args", "-install")
		// The "installer": checks its argument and creates the flag file.
		w.Write([]byte("#!/bin/sh\n[ \"$1\" = -install ] || exit 3\ntouch '" + flag + "'\necho installed-ok\n"))
	})

	done, err := a.meshAgentStep(context.Background(), marker)
	if !done || err != nil {
		t.Fatalf("step = %v, %v", done, err)
	}
	if !strings.Contains(gotQuery, "os=") || !strings.Contains(gotQuery, "arch=") {
		t.Errorf("query = %q", gotQuery)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("marker not written after install")
	}
	if _, err := os.Stat(filepath.Join(a.stateDir, "meshagent-setup")); !os.IsNotExist(err) {
		t.Error("setup directory left behind")
	}

	// Once the marker exists nothing is attempted, even if MeshAgent was
	// removed since (an admin uninstalling it on purpose).
	os.Remove(flag)
	a.client.base = "http://127.0.0.1:1" // would fail if contacted
	if done, err := a.meshAgentStep(context.Background(), marker); !done || err != nil {
		t.Errorf("with marker: %v, %v", done, err)
	}
}

func TestMeshAgentNotConfigured(t *testing.T) {
	stubMesh(t, filepath.Join(t.TempDir(), "never"))
	a, marker := meshTestAgent(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	done, err := a.meshAgentStep(context.Background(), marker)
	if !done || err != nil {
		t.Fatalf("204 should stop quietly: %v, %v", done, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("marker written although nothing was installed (a later server change should still install)")
	}
}

func TestMeshAgentInstallerFailureRetries(t *testing.T) {
	stubMesh(t, filepath.Join(t.TempDir(), "never"))
	a, marker := meshTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Mesh-Install-Args", "-install")
		w.Write([]byte("#!/bin/sh\necho boom >&2\nexit 1\n"))
	})
	done, err := a.meshAgentStep(context.Background(), marker)
	if done || err == nil {
		t.Fatalf("failed installer: done=%v err=%v, want retry", done, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("marker written after a failed install")
	}
}

func TestMeshAgentAlreadyPresentOrOptedOut(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "installed")
	os.WriteFile(flag, nil, 0o600)
	stubMesh(t, flag)
	a, marker := meshTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server contacted although MeshAgent is already installed")
	})
	if done, _ := a.meshAgentStep(context.Background(), marker); !done {
		t.Error("existing MeshAgent not detected")
	}

	stubMesh(t, filepath.Join(t.TempDir(), "never"))
	t.Setenv("CB_NO_REMOTE_ACCESS", "1")
	a2, marker2 := meshTestAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server contacted although opted out")
	})
	if done, _ := a2.meshAgentStep(context.Background(), marker2); !done {
		t.Error("CB_NO_REMOTE_ACCESS not honoured")
	}
}

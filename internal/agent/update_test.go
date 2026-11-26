//go:build !windows

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"centralbackup/internal/proto"
)

func TestSelfUpdateSwapsExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "backup-agent")
	os.WriteFile(exe, []byte("old build"), 0o755)
	newBuild := []byte("new build")
	sum := sha256.Sum256(newBuild)
	want := hex.EncodeToString(sum[:])

	oldExe, oldC := selfExe, updateInContainer
	selfExe = func() (string, error) { return exe, nil }
	updateInContainer = func() bool { return false }
	t.Cleanup(func() { selfExe, updateInContainer = oldExe, oldC })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/update" || r.URL.Query().Get("os") == "" {
			http.NotFound(w, r)
			return
		}
		w.Write(newBuild)
	}))
	defer srv.Close()
	a := &Agent{restart: make(chan struct{}, 1), client: &serverClient{
		base: srv.URL, creds: &Credentials{AgentID: "a", Secret: "s"}, http: srv.Client()}}

	// A wrong hash must leave the executable alone and free the run lock.
	if err := a.selfUpdate(context.Background(), hex.EncodeToString(make([]byte, 32))); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old build" {
		t.Fatalf("executable changed after failed update: %q", b)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Error("temporary file left behind")
	}
	if !a.runs.TryLock() {
		t.Fatal("run lock still held after a failed update")
	}
	a.runs.Unlock()

	a.handleAgentUpdate(protoUpdate(want))
	if b, _ := os.ReadFile(exe); string(b) != "new build" {
		t.Fatalf("executable not replaced: %q", b)
	}
	select {
	case <-a.restart:
	default:
		t.Error("no restart requested after update")
	}
	if a.runs.TryLock() {
		t.Error("run lock released after a successful update (a backup could start on the old binary)")
	}
	if got := exeImageSHA256(); got != want {
		t.Errorf("exeImageSHA256 = %s, want %s", got, want)
	}
}

func TestSelfUpdateOptOut(t *testing.T) {
	t.Setenv("CB_NO_AUTO_UPDATE", "1")
	a := &Agent{restart: make(chan struct{}, 1)}
	a.handleAgentUpdate(protoUpdate("x")) // must return without touching the client (nil)
	select {
	case <-a.restart:
		t.Error("restart requested although opted out")
	default:
	}
}

func protoUpdate(sum string) proto.AgentUpdate { return proto.AgentUpdate{SHA256: sum} }

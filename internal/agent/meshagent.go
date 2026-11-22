package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Remote-access bootstrapper: when the server has MeshCentral configured,
// the backup agent installs MeshAgent itself, so a client needs only the one
// backup-agent installer.
//
// The MeshAgent installer comes from the backup server
// (GET /api/agent/meshagent), over the connection whose certificate this
// agent already pins. The server fetches it from MeshCentral with the device
// group's settings and this agent's ID as the agent tag, so the new
// MeshCentral device is linked to this client exactly.
//
// MeshAgent is installed once. A marker file in the state directory records
// that, so an admin who later uninstalls MeshAgent on purpose doesn't get it
// back on the next restart. Delete the marker to have it reinstalled.

const meshMarker = "meshagent-installed"

// meshRetry is how long to wait after a failed attempt, and meshFirstDelay
// before the first one (lets the network settle after boot). Variables, along
// with the platform checks below, so tests can replace them.
var (
	meshRetry       = 30 * time.Minute
	meshFirstDelay  = 20 * time.Second
	meshInstalled   = meshAgentInstalled
	meshInContainer = runningInContainer
	meshSettle      = 2 * time.Second
)

// ensureMeshAgent runs in the background for the life of the agent.
func (a *Agent) ensureMeshAgent(ctx context.Context) {
	marker := filepath.Join(a.stateDir, meshMarker)
	select {
	case <-ctx.Done():
		return
	case <-time.After(meshFirstDelay):
	}
	for {
		done, err := a.meshAgentStep(ctx, marker)
		if done {
			return
		}
		if err != nil {
			log.Printf("remote access: %v (retrying in %s)", err, meshRetry)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(meshRetry):
		}
	}
}

// meshAgentStep makes one attempt. done means stop trying (installed, not
// wanted, or not applicable here).
func (a *Agent) meshAgentStep(ctx context.Context, marker string) (done bool, err error) {
	if _, err := os.Stat(marker); err == nil {
		return true, nil
	}
	if os.Getenv("CB_NO_REMOTE_ACCESS") != "" || meshInContainer() {
		return true, nil
	}
	if meshInstalled() {
		writeMeshMarker(marker, "found an existing MeshAgent")
		return true, nil
	}

	q := url.Values{"os": {runtime.GOOS}, "arch": {runtime.GOARCH}}
	res, err := a.client.do(ctx, "GET", "/api/agent/meshagent?"+q.Encode(), nil, "")
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode == 204 {
		return true, nil // remote access not configured, or not for this platform
	}
	name := filepath.Base(res.Header.Get("X-Mesh-File-Name"))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "meshagent"
	}
	args := strings.Fields(res.Header.Get("X-Mesh-Install-Args"))

	dir := filepath.Join(a.stateDir, "meshagent-setup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	exe := filepath.Join(dir, name)
	f, err := os.OpenFile(exe, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return false, err
	}
	n, err := io.Copy(f, res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, fmt.Errorf("download MeshAgent: %w", err)
	}
	log.Printf("remote access: installing MeshAgent (%d bytes) with %v", n, args)

	ictx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ictx, exe, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" {
		log.Printf("remote access: installer output: %s", truncate(s, 2000))
	}
	if err != nil {
		return false, fmt.Errorf("MeshAgent installer failed: %w", err)
	}
	// The installers return before the service has fully registered on
	// some systems; give it a moment before checking.
	for i := 0; i < 15 && !meshInstalled(); i++ {
		time.Sleep(meshSettle)
	}
	if !meshInstalled() {
		return false, errors.New("MeshAgent installer ran but no MeshAgent service was found")
	}
	writeMeshMarker(marker, "installed by backup agent")
	log.Printf("remote access: MeshAgent installed")
	return true, nil
}

func writeMeshMarker(path, note string) {
	os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+" "+note+"\n"), 0o600)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// runningInContainer reports whether this agent runs in a container (the
// Docker-host agent). Installing MeshAgent there would put it in the
// container, not on the host, so it is skipped; install MeshAgent on the host
// directly instead.
func runningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	return false
}

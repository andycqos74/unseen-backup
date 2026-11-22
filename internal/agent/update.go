package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"centralbackup/internal/bundle"
	"centralbackup/internal/proto"
)

// Self-update. On connect the agent reports the SHA-256 of its executable
// image; when the server's build for this platform differs, the server sends
// MsgAgentUpdate. The agent then waits until no backup or restore is
// running, downloads the new build over its pinned connection, checks the
// hash, swaps its executable and exits with ErrRestartForUpdate. The service
// manager starts the new version: systemd (Restart=always), launchd
// (KeepAlive) and the Windows service's recovery actions all restart a
// failed agent. Containers and CB_NO_AUTO_UPDATE skip it.

// ErrRestartForUpdate is returned by Run after the executable was replaced.
var ErrRestartForUpdate = errors.New("agent updated; restarting to load the new version")

// Test hooks.
var (
	selfExe           = os.Executable
	updateInContainer = runningInContainer
)

// buildFlavor distinguishes builds that share an OS/arch: the Windows 7
// build is compiled with Go 1.20.
func buildFlavor() string {
	if strings.HasPrefix(runtime.Version(), "go1.20") {
		return "legacy"
	}
	return ""
}

// exeImageSHA256 hashes the running executable without appended enrollment.
func exeImageSHA256() string {
	exe, err := selfExe()
	if err != nil {
		return ""
	}
	n, err := bundle.ImageSize(exe)
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, n)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cleanupOldExe removes the executable a previous Windows update set aside.
func cleanupOldExe() {
	if exe, err := selfExe(); err == nil {
		os.Remove(exe + ".old")
	}
}

// handleAgentUpdate runs in the background after MsgAgentUpdate.
func (a *Agent) handleAgentUpdate(u proto.AgentUpdate) {
	if os.Getenv("CB_NO_AUTO_UPDATE") != "" || updateInContainer() {
		return
	}
	if !a.updating.CompareAndSwap(false, true) {
		return // already in progress
	}
	defer a.updating.Store(false)
	if err := a.selfUpdate(context.Background(), u.SHA256); err != nil {
		log.Printf("self-update: %v", err)
		return
	}
	log.Printf("self-update: installed new agent build; restarting")
	a.requestRestart()
}

// selfUpdate downloads and installs the build with the given hash. It holds
// the run lock, so it waits for a running backup/restore to finish and no
// new one starts until the agent restarts.
func (a *Agent) selfUpdate(ctx context.Context, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if len(want) != 64 {
		return errors.New("bad update hash")
	}
	a.runs.Lock()
	// Deliberately not unlocked on success: the process restarts. On
	// failure, release it so backups continue.
	ok := false
	defer func() {
		if !ok {
			a.runs.Unlock()
		}
	}()

	exe, err := selfExe()
	if err != nil {
		return err
	}
	q := url.Values{"os": {runtime.GOOS}, "arch": {runtime.GOARCH}, "flavor": {buildFlavor()}}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	res, err := a.client.do(dctx, "GET", "/api/agent/update?"+q.Encode(), nil, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("write new build next to %s: %w", exe, err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("download: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("checksum mismatch (got %s, want %s)", got, want)
	}
	if err := replaceExe(exe, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	ok = true
	return nil
}

// replaceExe puts the new build in place of the running executable. On
// Unix a rename over the running file is atomic and safe. Windows can't
// overwrite a running image but can rename it, so the old one is moved
// aside (and removed on the next start).
func replaceExe(exe, newFile string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(newFile, exe)
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("set aside running executable: %w", err)
	}
	if err := os.Rename(newFile, exe); err != nil {
		os.Rename(old, exe) // put it back
		return fmt.Errorf("install new executable: %w", err)
	}
	return nil
}

func (a *Agent) requestRestart() {
	select {
	case a.restart <- struct{}{}:
	default:
	}
}

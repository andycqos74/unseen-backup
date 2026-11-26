package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"centralbackup/internal/kopia"
	"centralbackup/internal/proto"
)

// Kopia engine, agent side. Jobs with Engine "kopia" snapshot through the
// server's Kopia repository server as this client's own (append-only) Kopia
// user. The kopia binary is fetched from the backup server over the pinned
// connection and checked against the SHA-256 the server states; the
// connection settings arrive over the control channel (MsgKopiaConfig).
// Everything lives under <state>/kopia.

// kopiaSupported reports whether the server ships a kopia build for this
// platform: Linux x64/ARM64 and Windows x64, the platforms agents ship for.
// 32-bit Windows (including the Windows 7 build) keeps the built-in engine.
func kopiaSupported() bool {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "linux/arm64", "windows/amd64":
		return true
	}
	return false
}

func helloCaps() []string {
	if kopiaSupported() {
		return []string{proto.CapKopia}
	}
	return nil
}

func (a *Agent) kopiaDir() string { return filepath.Join(a.stateDir, "kopia") }

// saveKopiaConfig stores the connection settings sent by the server.
func (a *Agent) saveKopiaConfig(cfg proto.KopiaConfig) error {
	a.kopiaMu.Lock()
	defer a.kopiaMu.Unlock()
	if err := os.MkdirAll(a.kopiaDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(cfg)
	return writeFileAtomic(filepath.Join(a.kopiaDir(), "conn.json"), b, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// kopiaCLI returns a ready CLI: binary present and current, repository
// connected with the latest settings.
func (a *Agent) kopiaCLI(ctx context.Context) (*kopia.CLI, error) {
	a.kopiaMu.Lock()
	defer a.kopiaMu.Unlock()
	dir := a.kopiaDir()
	raw, err := os.ReadFile(filepath.Join(dir, "conn.json"))
	if err != nil {
		return nil, errors.New("no Kopia connection settings from the server yet (is the Kopia engine enabled there?)")
	}
	var cfg proto.KopiaConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	bin, err := a.ensureKopiaBinary(ctx, dir)
	if err != nil {
		return nil, err
	}
	cli := &kopia.CLI{
		Bin: bin, ConfigFile: filepath.Join(dir, "repository.config"),
		CacheDir: filepath.Join(dir, "cache"), LogDir: filepath.Join(dir, "logs"),
		Password: cfg.Password,
	}
	// Reconnect whenever the settings (or the binary) change.
	sum := sha256.Sum256(append(raw, []byte(bin)...))
	stamp := hex.EncodeToString(sum[:])
	stampFile := filepath.Join(dir, "connected")
	if old, _ := os.ReadFile(stampFile); string(old) == stamp {
		if _, err := os.Stat(cli.ConfigFile); err == nil {
			return cli, nil
		}
	}
	_, err = cli.Run(ctx, "repository", "connect", "server",
		"--url="+cfg.URL, "--server-cert-fingerprint="+cfg.Fingerprint,
		"--override-username="+cfg.Username, "--override-hostname="+cfg.Hostname,
		"--content-cache-size-mb=500", "--metadata-cache-size-mb=200")
	if err != nil {
		return nil, fmt.Errorf("connect to Kopia repository server: %w", err)
	}
	os.WriteFile(stampFile, []byte(stamp), 0o600)
	return cli, nil
}

// ensureKopiaBinary downloads kopia from the server when missing or when the
// server has a different build, verifying the stated SHA-256.
func (a *Agent) ensureKopiaBinary(ctx context.Context, dir string) (string, error) {
	name := "kopia"
	if runtime.GOOS == "windows" {
		name = "kopia.exe"
	}
	bin := filepath.Join(dir, name)
	have := ""
	if b, err := os.ReadFile(bin + ".sha256"); err == nil {
		if _, err := os.Stat(bin); err == nil {
			have = strings.TrimSpace(string(b))
		}
	}
	q := url.Values{"os": {runtime.GOOS}, "arch": {runtime.GOARCH}, "have": {have}}
	res, err := a.client.do(ctx, "GET", "/api/agent/kopia/binary?"+q.Encode(), nil, "")
	if err != nil {
		if have != "" {
			return bin, nil // keep using what we have if the server is unreachable mid-run
		}
		return "", fmt.Errorf("download kopia: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == 304 {
		return bin, nil
	}
	want := strings.ToLower(res.Header.Get("X-Content-SHA256"))
	if len(want) != 64 {
		return "", errors.New("download kopia: server sent no checksum")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := bin + ".download"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("download kopia: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return "", fmt.Errorf("download kopia: checksum mismatch (got %s, want %s)", got, want)
	}
	if err := os.Rename(tmp, bin); err != nil {
		return "", err
	}
	os.WriteFile(bin+".sha256", []byte(want), 0o600)
	return bin, nil
}

// doKopiaBackup snapshots the job's paths and records them on the server.
func (a *Agent) doKopiaBackup(ctx context.Context, cmd proto.RunBackup) (proto.RunStats, string, error) {
	var stats proto.RunStats
	cli, err := a.kopiaCLI(ctx)
	if err != nil {
		return stats, "", err
	}
	args := []string{"snapshot", "create", "--json",
		"--tags=agent:" + a.creds.AgentID, "--tags=job:" + cmd.Job.ID, "--tags=run:" + cmd.RunID,
		"--description=" + cmd.Job.Name}
	args = append(args, cmd.Job.Paths...)
	a.runLog(cmd.RunID, "info", fmt.Sprintf("kopia: snapshotting %d path(s)", len(cmd.Job.Paths)))
	out, runErr := cli.Run(ctx, args...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stats, "", ctxErr
	}
	snaps, decErr := kopia.DecodeSnapshots(out)
	if len(snaps) == 0 {
		if runErr != nil {
			return stats, "", runErr
		}
		if decErr != nil {
			return stats, "", fmt.Errorf("kopia output: %w", decErr)
		}
		return stats, "", errors.New("kopia created no snapshot")
	}
	commit := proto.KopiaCommit{RunID: cmd.RunID, JobID: cmd.Job.ID}
	logged := 0
	for _, s := range snaps {
		sum := s.RootEntry.Summ
		stats.FilesTotal += sum.Files
		stats.BytesTotal += sum.Size
		stats.FilesSkipped += sum.NumFailed
		for _, e := range sum.Errors {
			if logged < 20 {
				a.runLog(cmd.RunID, "warn", fmt.Sprintf("%s: %s", e.Path, e.Error))
			}
			logged++
		}
		commit.Snapshots = append(commit.Snapshots, proto.KopiaSnapshot{
			ID: s.ID, Path: s.Source.Path, Root: s.RootEntry.Obj,
			Files: sum.Files, Bytes: sum.Size, NumFailed: sum.NumFailed,
		})
	}
	if logged > 20 {
		a.runLog(cmd.RunID, "warn", fmt.Sprintf("… and %d more unreadable files", logged-20))
	}
	if runErr != nil && stats.FilesSkipped == 0 {
		// Snapshots exist but kopia still failed: report, don't hide it.
		a.runLog(cmd.RunID, "warn", runErr.Error())
	}
	var resp struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := a.client.doJSON(ctx, "POST", "/api/agent/kopia/snapshots", commit, &resp); err != nil {
		return stats, "", fmt.Errorf("record snapshot on server: %w", err)
	}
	a.runLog(cmd.RunID, "info", fmt.Sprintf("kopia: snapshot %s committed: %d files, %s (%d unreadable)",
		resp.SnapshotID, stats.FilesTotal, fmtBytes(stats.BytesTotal), stats.FilesSkipped))
	return stats, resp.SnapshotID, nil
}

var restoredRe = regexp.MustCompile(`Restored (\d+) files`)

// doKopiaRestore restores selected paths (or everything) of a Kopia
// snapshot to their original locations or under TargetDir, with the same
// path mapping as the built-in engine.
func (a *Agent) doKopiaRestore(ctx context.Context, cmd proto.Restore) (proto.RunStats, error) {
	var stats proto.RunStats
	cli, err := a.kopiaCLI(ctx)
	if err != nil {
		return stats, err
	}
	type item struct{ src, manifestPath string }
	var items []item
	for _, ks := range cmd.Kopia {
		base := strings.TrimRight(strings.ReplaceAll(ks.Path, `\`, "/"), "/")
		trimmedBase := strings.TrimLeft(base, "/")
		if len(cmd.Paths) == 0 {
			items = append(items, item{ks.ID, base})
			continue
		}
		for _, sel := range cmd.Paths {
			sel = strings.Trim(sel, "/")
			switch {
			case sel == "" || sel == trimmedBase || strings.HasPrefix(trimmedBase, sel+"/"):
				items = append(items, item{ks.ID, base}) // the whole source
			case strings.HasPrefix(sel, trimmedBase+"/"):
				rel := sel[len(trimmedBase)+1:]
				mp := sel
				if !(len(sel) > 1 && sel[1] == ':') {
					mp = "/" + sel
				}
				items = append(items, item{ks.ID + "/" + rel, mp})
			}
		}
	}
	if len(items) == 0 {
		return stats, errors.New("nothing in this snapshot matches the selection")
	}
	for _, it := range items {
		target, err := restoreTarget(it.manifestPath, cmd.TargetDir)
		if err != nil {
			return stats, err
		}
		args := []string{"snapshot", "restore", it.src, target, "--ignore-permission-errors"}
		if !cmd.Overwrite {
			args = append(args, "--skip-existing")
		}
		a.runLog(cmd.RunID, "info", "kopia: restoring "+it.manifestPath+" to "+target)
		out, err := cli.Run(ctx, args...)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stats, ctxErr
		}
		if err != nil {
			return stats, err
		}
		if m := restoredRe.FindSubmatch(out); m != nil {
			n, _ := strconv.ParseInt(string(m[1]), 10, 64)
			stats.FilesTotal += n
		}
	}
	a.runLog(cmd.RunID, "info", fmt.Sprintf("kopia: restore finished (%d files)", stats.FilesTotal))
	return stats, nil
}

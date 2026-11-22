package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"centralbackup/internal/kopia"
	"centralbackup/internal/proto"
	"centralbackup/internal/server/store"
)

// Kopia engine, server side.
//
// The backup server owns the Kopia repository (filesystem, under
// CB_KOPIA_REPO) and runs `kopia server` as a supervised child process on
// CB_KOPIA_LISTEN, using the backup server's own TLS certificate — so agents
// pin the same fingerprint they already trust. Each client is a Kopia user
// ("agent@<agent id>") whose access is append-only: it can create snapshots
// but not delete them or change policies. Retention and excludes are Kopia
// policies the server sets from the job before each run; expiry runs here.
//
// Snapshot rows for Kopia runs have backend "kopia" and a manifest key of
// "kopia:" + JSON list of proto.KopiaSnapshot. The existing browse,
// download and zip code reads them through eachManifestEntry/streamFile,
// which walk the Kopia tree instead of a manifest file.

const (
	kopiaBackend     = "kopia"
	kopiaKeyPrefix   = "kopia:"
	kopiaUser        = "agent"
	kopiaControlUser = "controller"
	kopiaSelfUser    = "controller"
	kopiaSelfHost    = "backup-server"
)

type kopiaConfig struct {
	Password string // repository password (CB_KOPIA_PASSWORD); enables the engine
	Bin      string // kopia binary (CB_KOPIA_BIN, default "kopia")
	Listen   string // repository server listen address (CB_KOPIA_LISTEN, default ":51515")
	URL      string // agent-facing URL (CB_KOPIA_URL; default: agent host + listen port)
	Repo     string // filesystem repository path (CB_KOPIA_REPO; default <data>/kopia/repo)
}

func kopiaConfigFromEnv() kopiaConfig {
	get := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	return kopiaConfig{
		Password: os.Getenv("CB_KOPIA_PASSWORD"),
		Bin:      get("CB_KOPIA_BIN", "kopia"),
		Listen:   get("CB_KOPIA_LISTEN", ":51515"),
		URL:      get("CB_KOPIA_URL", ""),
		Repo:     get("CB_KOPIA_REPO", ""),
	}
}

type kopiaEngine struct {
	cfg         kopiaConfig
	cli         *kopia.CLI // direct repository access as controller@backup-server
	dir         string
	certFile    string
	keyFile     string
	fingerprint string
	controlPass string
	agentURL    string

	treeMu    sync.Mutex
	treeCache map[string][]proto.ManifestEntry
	treeOrder []string
}

// initKopia prepares the repository and starts the repository server. It is
// a no-op when CB_KOPIA_PASSWORD is unset.
func (s *Server) initKopia(certFile, keyFile string) error {
	cfg := s.cfg.Kopia
	if cfg.Password == "" {
		return nil
	}
	dir := filepath.Join(s.cfg.DataDir, "kopia")
	if cfg.Repo == "" {
		cfg.Repo = filepath.Join(dir, "repo")
	}
	for _, d := range []string{dir, filepath.Join(dir, "cache"), filepath.Join(dir, "logs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	bin, err := exec.LookPath(cfg.Bin)
	if err != nil {
		return fmt.Errorf("CB_KOPIA_PASSWORD is set but the kopia binary %q was not found: %w", cfg.Bin, err)
	}
	cfg.Bin = bin
	ctl, err := s.store.GetSetting("kopia_control_password")
	if err != nil {
		return err
	}
	if ctl == "" {
		ctl = randomHex(24)
		if err := s.store.SetSetting("kopia_control_password", ctl); err != nil {
			return err
		}
	}
	k := &kopiaEngine{
		cfg: cfg, dir: dir, certFile: certFile, keyFile: keyFile,
		fingerprint: s.fingerprint, controlPass: ctl,
		treeCache: map[string][]proto.ManifestEntry{},
		cli: &kopia.CLI{
			Bin: bin, ConfigFile: filepath.Join(dir, "repository.config"),
			CacheDir: filepath.Join(dir, "cache"), LogDir: filepath.Join(dir, "logs"),
			Password: cfg.Password,
		},
	}
	k.agentURL = cfg.URL
	if k.agentURL == "" {
		u, _ := url.Parse(s.agentBaseURL(nil))
		host := "localhost"
		if u != nil && u.Hostname() != "" {
			host = u.Hostname()
		}
		k.agentURL = "https://" + net.JoinHostPort(host, listenPort(cfg.Listen))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := k.openRepository(ctx); err != nil {
		return err
	}
	// Global defaults, re-applied each start: zstd compression, and Windows
	// shadow copies (VSS) so open and locked files are captured consistently.
	if _, err := k.cli.Run(ctx, "policy", "set", "--global", "--compression=zstd",
		"--enable-volume-shadow-copy=when-available"); err != nil {
		return fmt.Errorf("kopia: set global policy: %w", err)
	}
	if err := k.ensureACLs(ctx); err != nil {
		return err
	}
	s.kopia = k
	go k.superviseServer()
	log.Printf("kopia: repository %s, repository server on %s (agents use %s)", cfg.Repo, cfg.Listen, k.agentURL)
	return nil
}

func listenPort(listen string) string {
	if i := strings.LastIndexByte(listen, ':'); i >= 0 {
		return listen[i+1:]
	}
	return listen
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// openRepository connects to the filesystem repository, creating it (with
// zstd compression as the global policy) on first use.
func (k *kopiaEngine) openRepository(ctx context.Context) error {
	if _, err := k.cli.Run(ctx, "repository", "status"); err == nil {
		return nil
	}
	ident := []string{"--override-username=" + kopiaSelfUser, "--override-hostname=" + kopiaSelfHost}
	if _, err := os.Stat(filepath.Join(k.cfg.Repo, "kopia.repository.f")); err == nil {
		_, err := k.cli.Run(ctx, append([]string{"repository", "connect", "filesystem", "--path=" + k.cfg.Repo}, ident...)...)
		if err != nil {
			return fmt.Errorf("kopia: connect repository (is CB_KOPIA_PASSWORD right?): %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(k.cfg.Repo, 0o700); err != nil {
		return err
	}
	if _, err := k.cli.Run(ctx, append([]string{"repository", "create", "filesystem", "--path=" + k.cfg.Repo}, ident...)...); err != nil {
		return fmt.Errorf("kopia: create repository: %w", err)
	}
	log.Printf("kopia: created repository at %s", k.cfg.Repo)
	return nil
}

// ensureACLs turns on server ACLs and makes clients append-only: their own
// snapshots APPEND (create, never delete), their own policies READ (the
// server sets them). Idempotent.
func (k *kopiaEngine) ensureACLs(ctx context.Context) error {
	out, _ := k.cli.Run(ctx, "server", "acl", "list")
	if strings.TrimSpace(string(out)) == "" {
		if _, err := k.cli.Run(ctx, "server", "acl", "enable"); err != nil {
			return fmt.Errorf("kopia: enable ACLs: %w", err)
		}
		if out, _ = k.cli.Run(ctx, "server", "acl", "list"); len(out) == 0 {
			return fmt.Errorf("kopia: ACLs empty after enabling")
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "access:FULL") || !strings.Contains(line, "OWN_USER") {
			continue
		}
		var target, access string
		switch {
		case strings.Contains(line, "type=snapshot"):
			target, access = "type=snapshot,hostname=OWN_HOST,username=OWN_USER", "APPEND"
		case strings.Contains(line, "type=policy"):
			target, access = "type=policy,hostname=OWN_HOST,username=OWN_USER", "READ"
		default:
			continue
		}
		id := strings.TrimPrefix(strings.Fields(line)[0], "id:")
		if _, err := k.cli.Run(ctx, "server", "acl", "delete", id, "--delete"); err != nil {
			return fmt.Errorf("kopia: tighten ACL: %w", err)
		}
		if _, err := k.cli.Run(ctx, "server", "acl", "add", "--user=*@*", "--access="+access, "--target="+target); err != nil {
			return fmt.Errorf("kopia: tighten ACL: %w", err)
		}
	}
	return nil
}

// superviseServer runs `kopia server start` for the life of the process,
// restarting it if it exits.
func (k *kopiaEngine) superviseServer() {
	backoff := 2 * time.Second
	for {
		start := time.Now()
		cli := *k.cli
		cli.ExtraEnv = map[string]string{"KOPIA_SERVER_CONTROL_PASSWORD": k.controlPass}
		_, err := cli.Run(context.Background(), "server", "start", "--no-ui",
			"--address="+k.cfg.Listen,
			"--tls-cert-file="+k.certFile, "--tls-key-file="+k.keyFile,
			"--server-control-username="+kopiaControlUser)
		log.Printf("kopia: repository server exited: %v", err)
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		} else if backoff < time.Minute {
			backoff *= 2
		}
		time.Sleep(backoff)
	}
}

// refresh makes the running repository server reload users and policies.
func (k *kopiaEngine) refresh(ctx context.Context) error {
	cli := *k.cli
	cli.ExtraEnv = map[string]string{"KOPIA_SERVER_PASSWORD": k.controlPass}
	_, err := cli.Run(ctx, "server", "refresh",
		"--address=https://"+net.JoinHostPort("127.0.0.1", listenPort(k.cfg.Listen)),
		"--server-cert-fingerprint="+k.fingerprint,
		"--server-control-username="+kopiaControlUser)
	return err
}

// sourceFor is the Kopia source (user@host:path) of one job path.
func sourceFor(agentID, path string) string {
	return kopiaUser + "@" + agentID + ":" + cleanSourcePath(path)
}

// cleanSourcePath drops trailing separators (but keeps "/" and "C:\").
func cleanSourcePath(p string) string {
	p = strings.TrimSpace(p)
	for len(p) > 1 && (strings.HasSuffix(p, "/") || strings.HasSuffix(p, `\`)) && !strings.HasSuffix(p, `:\`) {
		p = p[:len(p)-1]
	}
	return p
}

// provisionAgent creates (or re-keys) the client's Kopia user and returns
// its connection settings.
func (s *Server) provisionKopia(ctx context.Context, agentID string) (proto.KopiaConfig, error) {
	k := s.kopia
	pw, err := s.store.GetSetting("kopia_pw:" + agentID)
	if err != nil {
		return proto.KopiaConfig{}, err
	}
	if pw == "" {
		pw = randomHex(24)
		user := kopiaUser + "@" + agentID
		if _, err := k.cli.Run(ctx, "server", "user", "add", user, "--user-password="+pw); err != nil {
			if !strings.Contains(err.Error(), "already exists") {
				return proto.KopiaConfig{}, err
			}
			if _, err := k.cli.Run(ctx, "server", "user", "set", user, "--user-password="+pw); err != nil {
				return proto.KopiaConfig{}, err
			}
		}
		if err := s.store.SetSetting("kopia_pw:"+agentID, pw); err != nil {
			return proto.KopiaConfig{}, err
		}
		if err := k.refresh(ctx); err != nil {
			log.Printf("kopia: refresh after adding %s: %v", user, err)
		}
	}
	return proto.KopiaConfig{
		URL: k.agentURL, Fingerprint: k.fingerprint,
		Username: kopiaUser, Hostname: agentID, Password: pw,
	}, nil
}

// sendKopiaConfig provisions and sends the client its Kopia connection.
func (s *Server) sendKopiaConfig(agentID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, err := s.provisionKopia(ctx, agentID)
	if err != nil {
		log.Printf("kopia: provision %s: %v", agentID, err)
		return
	}
	s.hub.Send(agentID, proto.MsgKopiaConfig, cfg)
}

// applyKopiaPolicy sets each job path's retention and excludes as Kopia
// policy, before every run. Clients can't change policies themselves.
func (s *Server) applyKopiaPolicy(ctx context.Context, job proto.Job) error {
	for _, p := range job.Paths {
		src := sourceFor(job.AgentID, p)
		// Clearing and adding ignore rules must be separate invocations:
		// in one command, --clear-ignore wins over --add-ignore.
		if _, err := s.kopia.cli.Run(ctx, "policy", "set", src, "--clear-ignore"); err != nil {
			return err
		}
		args := []string{"policy", "set", src,
			"--keep-hourly=0", "--keep-weekly=0", "--keep-monthly=0", "--keep-annual=0"}
		switch {
		case job.KeepLast <= 0 && job.KeepDays <= 0:
			// "Keep everything", as the built-in engine does without a policy.
			args = append(args, "--keep-latest=1000000", "--keep-daily=0")
		default:
			args = append(args, fmt.Sprintf("--keep-latest=%d", job.KeepLast), fmt.Sprintf("--keep-daily=%d", job.KeepDays))
		}
		for _, ex := range job.Excludes {
			if ex = strings.TrimSpace(ex); ex != "" {
				args = append(args, "--add-ignore="+ex)
			}
		}
		if _, err := s.kopia.cli.Run(ctx, args...); err != nil {
			return err
		}
	}
	return s.kopia.refresh(ctx)
}

// expireKopia applies retention (clients cannot delete) and drops snapshot
// rows whose Kopia snapshots are gone.
func (s *Server) expireKopia(ctx context.Context, sources []string) error {
	k := s.kopia
	args := []string{"snapshot", "expire", "--delete"}
	if len(sources) == 0 {
		args = append(args, "--all")
	} else {
		args = append(args, sources...)
	}
	if _, err := k.cli.Run(ctx, args...); err != nil {
		return err
	}
	out, err := k.cli.Run(ctx, "snapshot", "list", "--all", "--json")
	if err != nil {
		return err
	}
	snaps, err := kopia.DecodeSnapshots(out)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, sn := range snaps {
		live[sn.ID] = true
	}
	rows, err := s.store.ListSnapshots(kopiaBackend, "", "")
	if err != nil {
		return err
	}
	for _, row := range rows {
		ks, err := decodeKopiaKey(row.ManifestKey)
		if err != nil {
			continue
		}
		alive := false
		for _, x := range ks {
			alive = alive || live[x.ID]
		}
		if !alive {
			s.store.DeleteSnapshot(row.ID)
			log.Printf("kopia: retention removed snapshot %s of job %s", row.ID, row.JobID)
		}
	}
	return nil
}

func decodeKopiaKey(key string) ([]proto.KopiaSnapshot, error) {
	if !strings.HasPrefix(key, kopiaKeyPrefix) {
		return nil, fmt.Errorf("not a kopia snapshot")
	}
	var ks []proto.KopiaSnapshot
	err := json.Unmarshal([]byte(key[len(kopiaKeyPrefix):]), &ks)
	return ks, err
}

// handleKopiaCommit records a Kopia run's snapshots (agent API).
func (s *Server) handleKopiaCommit(w http.ResponseWriter, r *http.Request, agentID string) {
	if s.kopia == nil {
		httpError(w, http.StatusNotFound, "kopia engine not enabled")
		return
	}
	c, err := decodeBody[proto.KopiaCommit](r)
	if err != nil || len(c.Snapshots) == 0 {
		httpError(w, http.StatusBadRequest, "bad request body")
		return
	}
	row, err := s.store.GetJob(c.JobID)
	if err != nil || row.Job.AgentID != agentID {
		httpError(w, http.StatusForbidden, "not your job")
		return
	}
	key, _ := json.Marshal(c.Snapshots)
	sn := store.Snapshot{
		ID: store.NewID(), Backend: kopiaBackend, JobID: c.JobID, AgentID: agentID, RunID: c.RunID,
		Mode: proto.EngineKopia, CreatedAt: time.Now().Unix(), ManifestKey: kopiaKeyPrefix + string(key),
	}
	var sources []string
	for _, x := range c.Snapshots {
		sn.Files += x.Files
		sn.Bytes += x.Bytes
		sources = append(sources, sourceFor(agentID, x.Path))
	}
	if err := s.store.CreateSnapshot(sn); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.expireKopia(ctx, sources); err != nil {
			log.Printf("kopia: expire after run %s: %v", c.RunID, err)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]string{"snapshot_id": sn.ID})
}

// handleKopiaBinary serves the kopia binary for an agent's platform from the
// agent binaries directory (kopia-<os>-<arch>[.exe]). An agent that already
// has the same build (?have=<sha256>) gets 304.
func (s *Server) handleKopiaBinary(w http.ResponseWriter, r *http.Request, agentID string) {
	goos, goarch := r.URL.Query().Get("os"), r.URL.Query().Get("arch")
	if !validPlatformWord(goos) || !validPlatformWord(goarch) {
		httpError(w, http.StatusBadRequest, "bad platform")
		return
	}
	name := "kopia-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	path := filepath.Join(s.cfg.AgentBins, name)
	sum, err := fileSHA256(path)
	if err != nil {
		httpError(w, http.StatusNotFound, "no kopia build for this platform on the server")
		return
	}
	w.Header().Set("X-Content-SHA256", sum)
	if r.URL.Query().Get("have") == sum {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeFile(w, r, path)
}

func validPlatformWord(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

var shaCache sync.Map // path -> [2]string{modtime+size, sum}

func fileSHA256(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stamp := fmt.Sprint(st.ModTime().UnixNano(), st.Size())
	if v, ok := shaCache.Load(path); ok && v.([2]string)[0] == stamp {
		return v.([2]string)[1], nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	shaCache.Store(path, [2]string{stamp, sum})
	return sum, nil
}

// kopiaEntries lists a Kopia snapshot row as manifest entries (cached), so
// browse/download/zip work unchanged. Paths are slash-form source paths
// ("C:/Users/x/file", "/home/x/file"); file hashes are "kopia:<object id>".
func (s *Server) kopiaEntries(key string) ([]proto.ManifestEntry, error) {
	k := s.kopia
	if k == nil {
		return nil, fmt.Errorf("kopia engine not enabled")
	}
	k.treeMu.Lock()
	if es, ok := k.treeCache[key]; ok {
		k.treeMu.Unlock()
		return es, nil
	}
	k.treeMu.Unlock()

	snaps, err := decodeKopiaKey(key)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var out []proto.ManifestEntry
	var walk func(obj, dir string) error
	walk = func(obj, dir string) error {
		entries, err := k.cli.ReadDir(ctx, obj)
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := dir + "/" + e.Name
			me := proto.ManifestEntry{Path: p, Size: e.Size, Mtime: e.MTime.UnixNano()}
			switch e.Type {
			case "d":
				me.Type = "d"
				out = append(out, me)
				if err := walk(e.Obj, p); err != nil {
					return err
				}
				continue
			case "s":
				me.Type = "l"
			default:
				me.Type = "f"
				me.Hash = kopiaKeyPrefix + e.Obj
			}
			out = append(out, me)
		}
		return nil
	}
	for _, sn := range snaps {
		base := strings.TrimRight(strings.ReplaceAll(sn.Path, `\`, "/"), "/")
		out = append(out, proto.ManifestEntry{Type: "d", Path: base})
		if err := walk(sn.Root, base); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	k.treeMu.Lock()
	k.treeCache[key] = out
	k.treeOrder = append(k.treeOrder, key)
	for len(k.treeOrder) > 8 {
		delete(k.treeCache, k.treeOrder[0])
		k.treeOrder = k.treeOrder[1:]
	}
	k.treeMu.Unlock()
	return out, nil
}

// streamKopiaFile writes a file object's content.
func (s *Server) streamKopiaFile(w io.Writer, obj string) error {
	if s.kopia == nil {
		return fmt.Errorf("kopia engine not enabled")
	}
	return s.kopia.cli.Stream(context.Background(), w, "show", obj)
}

// deleteKopiaSnapshot deletes a Kopia snapshot row's snapshots.
func (s *Server) deleteKopiaSnapshot(sn *store.Snapshot) error {
	ks, err := decodeKopiaKey(sn.ManifestKey)
	if err != nil {
		return err
	}
	if s.kopia != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for _, x := range ks {
			if _, err := s.kopia.cli.Run(ctx, "snapshot", "delete", x.ID, "--delete"); err != nil &&
				!strings.Contains(err.Error(), "not found") {
				return err
			}
		}
	}
	return s.store.DeleteSnapshot(sn.ID)
}

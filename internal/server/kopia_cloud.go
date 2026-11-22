package server

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"centralbackup/internal/server/storage"
)

// OneDrive / Google Drive as the Kopia off-site copy, through rclone.
//
// The admin connects an account in Settings with the same OAuth app and
// callback as the built-in engine's cloud storage, but the result is a
// separate record: connecting here never changes the built-in engine's
// active backend. At connect the server writes rclone.conf (0600, in the
// data volume) once; from then on rclone owns the token — Microsoft rotates
// refresh tokens and rclone writes the new one back — so the file is never
// regenerated on restart.
//
// One long-lived `rclone serve webdav` on loopback serves the cloud folder;
// Kopia talks to it with its WebDAV backend. A single process means a
// single owner of the token (no concurrent refreshes racing to rewrite the
// config) and no per-command rclone start-up and re-authentication.

const (
	kopiaCloudKey      = "kopia_cloud"
	rcloneRemoteName   = "cloud"
	rcloneWebDAVUser   = "kopia"
	defaultCloudAddr   = "127.0.0.1:51516"
	defaultCloudFolder = "unseen-backup-kopia"
)

// kopiaCloud is the stored destination (no tokens: those live in rclone.conf).
type kopiaCloud struct {
	Provider storage.Provider `json:"provider"`
	Account  string           `json:"account,omitempty"`
	Folder   string           `json:"folder"`
	Since    int64            `json:"since"`
}

type rcloneRunner struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stop    chan struct{}
	lastErr string
	running bool
}

func (k *kopiaEngine) rcloneConf() string   { return filepath.Join(k.dir, "rclone.conf") }
func (k *kopiaEngine) htpasswdFile() string { return filepath.Join(k.dir, "rclone.htpasswd") }

func cloudAddr() string {
	if v := strings.TrimSpace(os.Getenv("CB_KOPIA_RCLONE_ADDR")); v != "" {
		return v
	}
	return defaultCloudAddr
}

func rcloneBin() (string, error) {
	name := strings.TrimSpace(os.Getenv("CB_RCLONE_BIN"))
	if name == "" {
		name = "rclone"
	}
	return exec.LookPath(name)
}

func (s *Server) loadKopiaCloud() *kopiaCloud {
	raw, _ := s.store.GetSetting(kopiaCloudKey)
	if raw == "" {
		return nil
	}
	var c kopiaCloud
	if json.Unmarshal([]byte(raw), &c) != nil || c.Provider == "" {
		return nil
	}
	return &c
}

// webdavPassword returns (creating once) the loopback WebDAV password and
// keeps rclone's htpasswd file in step with it.
func (s *Server) webdavPassword() (string, error) {
	pw, err := s.store.GetSetting("kopia_webdav_password")
	if err != nil {
		return "", err
	}
	if pw == "" {
		pw = randomHex(24)
		if err := s.store.SetSetting("kopia_webdav_password", pw); err != nil {
			return "", err
		}
	}
	sum := sha1.Sum([]byte(pw)) // {SHA} htpasswd; the password is 192 random bits
	line := rcloneWebDAVUser + ":{SHA}" + base64.StdEncoding.EncodeToString(sum[:]) + "\n"
	if err := os.WriteFile(s.kopia.htpasswdFile(), []byte(line), 0o600); err != nil {
		return "", err
	}
	return pw, nil
}

// startCloudServer runs `rclone serve webdav` for the connected destination,
// restarting it if it exits, until stopCloudServer.
func (s *Server) startCloudServer() error {
	k := s.kopia
	c := s.loadKopiaCloud()
	if k == nil || c == nil {
		return nil
	}
	bin, err := rcloneBin()
	if err != nil {
		return fmt.Errorf("rclone not found (needed for OneDrive/Google Drive): %w", err)
	}
	if _, err := s.webdavPassword(); err != nil {
		return err
	}
	r := &k.rclone
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return nil // already running
	}
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()

	go func() {
		backoff := 2 * time.Second
		for {
			args := []string{"serve", "webdav", rcloneRemoteName + ":" + c.Folder,
				"--config", k.rcloneConf(),
				"--addr", cloudAddr(),
				"--htpasswd", k.htpasswdFile(),
				// No VFS cache: each WebDAV PUT returns only once the blob
				// is in the cloud, so a finished sync-to means the copy is
				// really there. Kopia writes blobs sequentially, which is
				// what uncached mode needs; for providers that need the size
				// up front (OneDrive) rclone spools the upload itself.
				"--vfs-cache-mode", "off",
				"--cache-dir", filepath.Join(k.dir, "rclone-cache"),
				"--log-level", "NOTICE"}
			cmd := exec.Command(bin, args...)
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &out
			r.mu.Lock()
			r.cmd = cmd
			r.mu.Unlock()
			start := time.Now()
			err := cmd.Start()
			if err == nil {
				r.mu.Lock()
				r.running, r.lastErr = true, ""
				r.mu.Unlock()
				err = cmd.Wait()
			}
			r.mu.Lock()
			r.running = false
			if msg := strings.TrimSpace(out.String()); msg != "" {
				r.lastErr = lastLine(msg)
			} else if err != nil {
				r.lastErr = err.Error()
			}
			r.mu.Unlock()
			select {
			case <-stop:
				return
			default:
			}
			log.Printf("kopia: rclone webdav exited: %v (%s)", err, r.lastErr)
			if time.Since(start) > time.Minute {
				backoff = 2 * time.Second
			} else if backoff < time.Minute {
				backoff *= 2
			}
			select {
			case <-stop:
				return
			case <-time.After(backoff):
			}
		}
	}()
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func (s *Server) stopCloudServer() {
	if s.kopia == nil {
		return
	}
	r := &s.kopia.rclone
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	if r.cmd != nil && r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
}

// cloudSyncArgs returns the sync-to target for the connected destination
// and the environment carrying its credentials.
func (s *Server) cloudSyncArgs() ([]string, map[string]string, error) {
	pw, err := s.webdavPassword()
	if err != nil {
		return nil, nil, err
	}
	return []string{"webdav", "--url=http://" + cloudAddr() + "/"},
		map[string]string{"KOPIA_WEBDAV_USERNAME": rcloneWebDAVUser, "KOPIA_WEBDAV_PASSWORD": pw}, nil
}

// rcloneConfigFor builds the rclone remote for a freshly authorized account.
func rcloneConfigFor(cfg storage.Config, tok *oauth2.Token, driveID, driveType string) (string, error) {
	tj, err := json.Marshal(map[string]any{
		"access_token": tok.AccessToken, "token_type": "Bearer",
		"refresh_token": tok.RefreshToken, "expiry": tok.Expiry.Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", rcloneRemoteName)
	switch cfg.Provider {
	case storage.ProviderOneDrive:
		if driveID == "" {
			return "", fmt.Errorf("could not determine the OneDrive drive")
		}
		fmt.Fprintf(&b, "type = onedrive\ndrive_id = %s\ndrive_type = %s\n", driveID, driveType)
	case storage.ProviderGoogleDrive:
		// drive.file: rclone only sees files this OAuth app created, which
		// is exactly the backup folder.
		b.WriteString("type = drive\nscope = drive.file\n")
	default:
		return "", fmt.Errorf("%s is not supported for the Kopia off-site copy", cfg.Provider)
	}
	fmt.Fprintf(&b, "client_id = %s\nclient_secret = %s\ntoken = %s\n", cfg.ClientID, cfg.ClientSecret, tj)
	return b.String(), nil
}

// oneDriveInfoURL is a var so tests can use a fake Graph server.
var oneDriveInfoURL = "https://graph.microsoft.com/v1.0/me/drive"

func fetchOneDrive(ctx context.Context, accessToken string) (id, typ string, err error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", oneDriveInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", "", fmt.Errorf("OneDrive drive lookup: %s", res.Status)
	}
	var d struct {
		ID        string `json:"id"`
		DriveType string `json:"driveType"`
	}
	if err := json.NewDecoder(res.Body).Decode(&d); err != nil {
		return "", "", err
	}
	return d.ID, d.DriveType, nil
}

// finishKopiaCloud completes the OAuth flow for the Kopia destination.
func (s *Server) finishKopiaCloud(ctx context.Context, pending storage.Config, folder string, tok *oauth2.Token) error {
	if s.kopia == nil {
		return fmt.Errorf("the Kopia engine is not enabled")
	}
	var driveID, driveType string
	if pending.Provider == storage.ProviderOneDrive {
		var err error
		if driveID, driveType, err = fetchOneDrive(ctx, tok.AccessToken); err != nil {
			return err
		}
	}
	conf, err := rcloneConfigFor(pending, tok, driveID, driveType)
	if err != nil {
		return err
	}
	s.stopCloudServer()
	if err := writeFileAtomic(s.kopia.rcloneConf(), []byte(conf), 0o600); err != nil {
		return err
	}
	if folder == "" {
		folder = defaultCloudFolder
	}
	rec, _ := json.Marshal(kopiaCloud{Provider: pending.Provider, Account: pending.Account, Folder: folder, Since: time.Now().Unix()})
	if err := s.store.SetSetting(kopiaCloudKey, string(rec)); err != nil {
		return err
	}
	// A new destination starts a fresh copy history.
	for _, k := range []string{"kopia_sync_last_run", "kopia_sync_last_ok", "kopia_sync_error"} {
		s.store.SetSetting(k, "")
	}
	if err := s.startCloudServer(); err != nil {
		return err
	}
	go func() {
		time.Sleep(5 * time.Second)
		s.kopiaSyncOnce()
	}()
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) registerKopiaCloudAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/kopia/cloud/connect", s.adminAuth(s.handleKopiaCloudConnect))
	mux.HandleFunc("POST /api/admin/kopia/cloud/test", s.adminAuth(s.handleKopiaCloudTest))
	mux.HandleFunc("POST /api/admin/kopia/cloud/sync", s.adminAuth(s.handleKopiaCloudSync))
	mux.HandleFunc("DELETE /api/admin/kopia/cloud", s.adminAuth(s.handleKopiaCloudDisconnect))
}

// handleKopiaCloudConnect starts the OAuth flow for the Kopia off-site
// destination. Client ID/secret default to the ones already saved for the
// built-in engine's storage with the same provider.
func (s *Server) handleKopiaCloudConnect(w http.ResponseWriter, r *http.Request) {
	if s.kopia == nil {
		httpError(w, http.StatusBadRequest, "the Kopia engine is not enabled (CB_KOPIA_PASSWORD)")
		return
	}
	req, err := decodeBody[struct {
		Provider     storage.Provider `json:"provider"`
		ClientID     string           `json:"client_id"`
		ClientSecret string           `json:"client_secret"`
		Folder       string           `json:"folder"`
	}](r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad request body")
		return
	}
	if req.Provider != storage.ProviderOneDrive && req.Provider != storage.ProviderGoogleDrive {
		httpError(w, http.StatusBadRequest, "provider must be onedrive or gdrive")
		return
	}
	folder := strings.Trim(strings.TrimSpace(req.Folder), "/")
	if strings.Contains(folder, "..") || strings.ContainsAny(folder, ":\\") {
		httpError(w, http.StatusBadRequest, "folder must be a plain path like backups/kopia")
		return
	}
	cfg := storage.Config{Provider: req.Provider, ClientID: strings.TrimSpace(req.ClientID), ClientSecret: strings.TrimSpace(req.ClientSecret)}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		if cur := s.loadStorageConfig(); cur.Provider == req.Provider && cur.ClientID != "" {
			cfg.ClientID, cfg.ClientSecret = cur.ClientID, cur.ClientSecret
		}
	}
	oc, err := oauthConfig(cfg, s.oauthRedirectURLFromRequest(r, cfg.Provider))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	state := newOAuthState()
	s.oauth.mu.Lock()
	s.oauth.state, s.oauth.pending = state, cfg
	s.oauth.purpose, s.oauth.folder = oauthPurposeKopia, folder
	s.oauth.mu.Unlock()
	opts := append([]oauth2.AuthCodeOption{}, providerOAuthConfig[cfg.Provider].AuthParams...)
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": oc.AuthCodeURL(state, opts...)})
}

// handleKopiaCloudTest lists the account root through rclone.
func (s *Server) handleKopiaCloudTest(w http.ResponseWriter, r *http.Request) {
	if s.kopia == nil || s.loadKopiaCloud() == nil {
		httpError(w, http.StatusBadRequest, "no OneDrive/Google Drive destination connected")
		return
	}
	bin, err := rcloneBin()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "rclone not found on the server"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "lsd", rcloneRemoteName+":", "--config", s.kopia.rcloneConf(), "--log-level", "ERROR").CombinedOutput()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": lastLine(string(out)) + " (" + err.Error() + ")"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleKopiaCloudSync(w http.ResponseWriter, r *http.Request) {
	if s.kopia == nil {
		httpError(w, http.StatusBadRequest, "the Kopia engine is not enabled")
		return
	}
	go s.kopiaSyncOnce()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleKopiaCloudDisconnect forgets the destination. The copy already in
// the cloud is left alone.
func (s *Server) handleKopiaCloudDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.kopia == nil {
		httpError(w, http.StatusBadRequest, "the Kopia engine is not enabled")
		return
	}
	s.stopCloudServer()
	os.Remove(s.kopia.rcloneConf())
	s.store.SetSetting(kopiaCloudKey, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

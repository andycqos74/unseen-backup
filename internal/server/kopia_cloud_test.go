package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"centralbackup/internal/kopia"
	"centralbackup/internal/server/storage"
)

// TestKopiaCloudConnectKeepsStorageBackend drives connect -> callback for the
// Kopia off-site destination against a fake Microsoft token and Graph API,
// and checks the built-in engine's storage is untouched.
func TestKopiaCloudConnectKeepsStorageBackend(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			json.NewEncoder(w).Encode(map[string]any{"access_token": "acc", "refresh_token": "ref-1", "token_type": "Bearer", "expires_in": 3600})
		case strings.HasSuffix(r.URL.Path, "/me/drive"):
			if r.Header.Get("Authorization") != "Bearer acc" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "DRIVE123", "driveType": "personal"})
		case strings.HasSuffix(r.URL.Path, "/me"):
			json.NewEncoder(w).Encode(map[string]any{"userPrincipalName": "andy@example.com"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	orig := providerOAuthConfig[storage.ProviderOneDrive]
	p := orig
	p.Endpoint = oauth2.Endpoint{AuthURL: fake.URL + "/authorize", TokenURL: fake.URL + "/token"}
	providerOAuthConfig[storage.ProviderOneDrive] = p
	origAcct := accountInfoEndpoint[storage.ProviderOneDrive]
	accountInfoEndpoint[storage.ProviderOneDrive] = struct{ URL, Field string }{fake.URL + "/me", "userPrincipalName"}
	origDrive := oneDriveInfoURL
	oneDriveInfoURL = fake.URL + "/me/drive"
	defer func() {
		providerOAuthConfig[storage.ProviderOneDrive] = orig
		accountInfoEndpoint[storage.ProviderOneDrive] = origAcct
		oneDriveInfoURL = origDrive
	}()
	t.Setenv("CB_RCLONE_BIN", "true") // a stand-in that exits at once
	t.Setenv("CB_KOPIA_RCLONE_ADDR", "127.0.0.1:1")

	s, sess := oauthTestServer(t)
	s.kopia = &kopiaEngine{dir: t.TempDir(), cli: &kopia.CLI{Bin: "false"}}
	defer s.stopCloudServer()

	// App credentials saved for the built-in storage are reused when the
	// Kopia form leaves them empty; the active backend stays local.
	if err := s.saveStorageConfig(storage.Config{Provider: storage.ProviderOneDrive, ClientID: "cid", ClientSecret: "csec"}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.store.GetSetting(storageConfigKey)

	req := httptest.NewRequest("POST", "/api/admin/kopia/cloud/connect", strings.NewReader(`{"provider":"onedrive","folder":"backups/kopia"}`))
	req.Header.Set("X-Requested-With", "fetch")
	withSession(req, sess)
	rec := httptest.NewRecorder()
	s.adminAuth(s.handleKopiaCloudConnect)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	var cr struct {
		AuthURL string `json:"auth_url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &cr)
	u, _ := url.Parse(cr.AuthURL)
	if u.Query().Get("client_id") != "cid" {
		t.Fatalf("auth URL client_id = %q, want the saved one", u.Query().Get("client_id"))
	}

	req = httptest.NewRequest("GET", "/api/admin/storage/oauth/callback?code=c&state="+u.Query().Get("state"), nil)
	withSession(req, sess)
	rec = httptest.NewRecorder()
	s.handleStorageOAuthCallback(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "kopia_cloud=connected") {
		t.Fatalf("callback redirect = %q", loc)
	}

	conf, err := os.ReadFile(s.kopia.rcloneConf())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[cloud]", "type = onedrive", "drive_id = DRIVE123", "drive_type = personal",
		"client_id = cid", "client_secret = csec", `"refresh_token":"ref-1"`} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("rclone.conf lacks %q:\n%s", want, conf)
		}
	}
	if st, _ := os.Stat(s.kopia.rcloneConf()); st.Mode().Perm() != 0o600 {
		t.Errorf("rclone.conf mode = %v, want 0600", st.Mode().Perm())
	}
	c := s.loadKopiaCloud()
	if c == nil || c.Folder != "backups/kopia" || c.Account != "andy@example.com" {
		t.Fatalf("kopia cloud record = %+v", c)
	}
	if after, _ := s.store.GetSetting(storageConfigKey); after != before {
		t.Errorf("built-in storage config changed:\n%s\n->\n%s", before, after)
	}
	if st := s.kopiaSyncStatus(); st == nil || !strings.Contains(st.Target, "OneDrive") || !strings.Contains(st.Target, "backups/kopia") {
		t.Errorf("status = %+v", st)
	}

	// Disconnect forgets it.
	req = httptest.NewRequest("DELETE", "/api/admin/kopia/cloud", nil)
	req.Header.Set("X-Requested-With", "fetch")
	withSession(req, sess)
	s.adminAuth(s.handleKopiaCloudDisconnect)(httptest.NewRecorder(), req)
	if s.loadKopiaCloud() != nil {
		t.Error("destination still configured after disconnect")
	}
	if _, err := os.Stat(s.kopia.rcloneConf()); !os.IsNotExist(err) {
		t.Error("rclone.conf kept after disconnect")
	}
}

func TestRcloneConfigGoogleDrive(t *testing.T) {
	tok := &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Unix(0, 0)}
	conf, err := rcloneConfigFor(storage.Config{Provider: storage.ProviderGoogleDrive, ClientID: "g", ClientSecret: "s"}, tok, "", "")
	if err != nil || !strings.Contains(conf, "type = drive") || !strings.Contains(conf, "scope = drive.file") {
		t.Fatalf("gdrive conf = %q, %v", conf, err)
	}
	if _, err := rcloneConfigFor(storage.Config{Provider: storage.ProviderOneDrive}, tok, "", ""); err == nil {
		t.Error("OneDrive without a drive ID accepted")
	}
	if _, err := rcloneConfigFor(storage.Config{Provider: storage.ProviderBox}, tok, "", ""); err == nil {
		t.Error("unsupported provider accepted")
	}
}

// TestKopiaCloudSyncLive copies a real Kopia repository through a real
// `rclone serve webdav` (a local rclone remote stands in for OneDrive).
// Runs only with KOPIA_BIN and RCLONE_BIN set.
func TestKopiaCloudSyncLive(t *testing.T) {
	kb, rb := os.Getenv("KOPIA_BIN"), os.Getenv("RCLONE_BIN")
	if kb == "" || rb == "" {
		t.Skip("KOPIA_BIN / RCLONE_BIN not set")
	}
	dir := t.TempDir()
	cli := &kopia.CLI{Bin: kb, ConfigFile: filepath.Join(dir, "repo.config"), CacheDir: filepath.Join(dir, "cache"), LogDir: filepath.Join(dir, "logs"), Password: "pw"}
	ctx := context.Background()
	if _, err := cli.Run(ctx, "repository", "create", "filesystem", "--path="+filepath.Join(dir, "repo")); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "f.txt"), []byte("hello"), 0o644)
	if _, err := cli.Run(ctx, "snapshot", "create", src); err != nil {
		t.Fatal(err)
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	t.Setenv("CB_RCLONE_BIN", rb)
	t.Setenv("CB_KOPIA_RCLONE_ADDR", addr)
	s := testServer(t)
	s.kopia = &kopiaEngine{dir: dir, cli: cli, syncEvery: time.Hour}
	remote := filepath.Join(dir, "cloud-remote")
	os.MkdirAll(remote, 0o755)
	os.WriteFile(s.kopia.rcloneConf(), []byte("[cloud]\ntype = local\n"), 0o600)
	rec, _ := json.Marshal(kopiaCloud{Provider: storage.ProviderOneDrive, Folder: remote})
	s.store.SetSetting(kopiaCloudKey, string(rec))
	if err := s.startCloudServer(); err != nil {
		t.Fatal(err)
	}
	defer s.stopCloudServer()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rclone webdav didn't start: %+v", s.kopiaSyncStatus())
		}
		time.Sleep(200 * time.Millisecond)
	}

	s.kopiaSyncOnce()
	st := s.kopiaSyncStatus()
	if st.LastOK == 0 || st.LastErr != "" || !st.RcloneRunning {
		t.Fatalf("sync status = %+v", st)
	}
	// The copy is a complete repository.
	mirror := &kopia.CLI{Bin: kb, ConfigFile: filepath.Join(dir, "mirror.config"), CacheDir: filepath.Join(dir, "mcache"), LogDir: filepath.Join(dir, "logs"), Password: "pw"}
	if _, err := mirror.Run(ctx, "repository", "connect", "filesystem", "--path="+remote); err != nil {
		t.Fatalf("open copy: %v", err)
	}
	out, err := mirror.Run(ctx, "snapshot", "list", "--all", "--json")
	snaps, _ := kopia.DecodeSnapshots(out)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("copy has %d snapshots (%v)", len(snaps), err)
	}
}

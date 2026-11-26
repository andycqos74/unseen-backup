// Package server implements the central backup server: HTTPS endpoints
// serving the admin web GUI, the admin REST API, the agent WebSocket
// control channel and the agent data-plane endpoints (blob/manifest
// upload/download).
//
// By default one listener (CB_LISTEN) serves everything. With
// CB_ADMIN_LISTEN set, the GUI and admin API move to that listener and
// CB_LISTEN serves agents only, so the agent port can be published while the
// admin side stays private (e.g. reachable only through a Cloudflare Tunnel).
package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"centralbackup/internal/meshcentral"
	"centralbackup/internal/server/storage"
	"centralbackup/internal/server/store"
)

type Config struct {
	Listen string // e.g. ":8443" — agents (and, unless AdminListen is set, everything)
	// AdminListen, when set, moves the GUI and admin API to their own
	// listener (e.g. "127.0.0.1:8444" or ":8444" on an internal network).
	AdminListen string
	DataDir     string // sqlite db, tls certs
	StorageDir  string // blob/manifest storage root (localfs backend)
	ServerName  string // comma-separated extra SANs for the generated cert
	PublicURL   string // origin agents connect to, e.g. https://backup.example.com:8443
	AdminURL    string // origin browsers use for the GUI (split mode), e.g. https://backup-admin.example.com
	CertFile    string // optional externally provided cert
	KeyFile     string
	AgentBins   string // directory of prebuilt agent binaries served at /dl/

	// Mesh is the MeshCentral integration (remote desktop/terminal/files);
	// nil when CB_MESH_URL is unset. MeshGroup names the device group new
	// clients join. MeshErr records an invalid MeshCentral configuration,
	// reported by New so a typo fails at startup.
	Mesh            *meshcentral.Client
	MeshGroup       string
	MeshAutoInstall bool // backup agents install MeshAgent (CB_MESH_AUTOINSTALL, default on)
	MeshErr         error
}

func ConfigFromEnv() Config {
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	dataDir := get("CB_DATA_DIR", "./data")
	mesh, meshGroup, meshAuto, meshErr := meshConfigFromEnvEx()
	return Config{
		Mesh:            mesh,
		MeshGroup:       meshGroup,
		MeshAutoInstall: meshAuto,
		MeshErr:         meshErr,
		Listen:          get("CB_LISTEN", ":8443"),
		AdminListen:     get("CB_ADMIN_LISTEN", ""),
		AdminURL:        get("CB_ADMIN_URL", ""),
		DataDir:         dataDir,
		StorageDir:      get("CB_STORAGE_DIR", filepath.Join(dataDir, "storage")),
		ServerName:      get("CB_SERVER_NAME", ""),
		PublicURL:       get("CB_PUBLIC_URL", ""),
		CertFile:        get("CB_TLS_CERT", ""),
		KeyFile:         get("CB_TLS_KEY", ""),
		AgentBins:       get("CB_AGENT_BIN_DIR", "./agents"),
	}
}

type Server struct {
	cfg         Config
	store       *store.Store
	hub         *Hub
	fingerprint string
	tlsCert     tls.Certificate

	// storageMu guards the active backend and its ID, which can be swapped
	// at runtime when the admin changes the storage provider. Read them via
	// backend() and backendKey().
	storageMu        sync.RWMutex
	storageActive    storage.Backend
	storageBackendID string

	// commitMu serialises snapshot commits against garbage collection:
	// commits take the read lock, GC takes the write lock.
	commitMu sync.RWMutex

	// oauth holds the in-flight storage "connect" flow state.
	oauth oauthFlow

	// docker correlates in-flight container-discovery requests with the
	// agent replies that answer them.
	docker dockerDiscovery

	// sched correlates scheduled-task list/action requests with their
	// agent replies.
	sched schedTasks
	// browse correlates in-flight directory-listing requests (the file
	// browser) with the agent replies that answer them.
	browse browseRequests

	web *webUI

	// mesh is the MeshCentral integration, nil when not configured.
	mesh            *meshcentral.Client
	meshGroupName   string
	meshAutoInstall bool
}

// backend returns the currently active storage backend.
func (s *Server) backend() storage.Backend {
	s.storageMu.RLock()
	defer s.storageMu.RUnlock()
	return s.storageActive
}

func New(cfg Config) (*Server, error) {
	// Validate before touching disk so a typo fails fast and loudly rather
	// than silently producing unusable enrollment commands.
	publicURL, err := normalizePublicURL(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	cfg.PublicURL = publicURL
	adminURL, err := normalizeOrigin("CB_ADMIN_URL", cfg.AdminURL)
	if err != nil {
		return nil, err
	}
	cfg.AdminURL = adminURL
	if cfg.MeshErr != nil {
		return nil, cfg.MeshErr
	}
	if err := validateListeners(cfg); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "server.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	certFile, keyFile := cfg.CertFile, cfg.KeyFile
	if certFile == "" {
		certFile = filepath.Join(cfg.DataDir, "tls", "cert.pem")
		keyFile = filepath.Join(cfg.DataDir, "tls", "key.pem")
	}
	cert, err := ensureTLSCert(certFile, keyFile, strings.Split(cfg.ServerName, ","))
	if err != nil {
		return nil, fmt.Errorf("tls certificate: %w", err)
	}
	fp, err := certFingerprint(cert)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:         cfg,
		store:       st,
		hub:         newHub(),
		fingerprint: fp,
		tlsCert:     cert,

		mesh:            cfg.Mesh,
		meshGroupName:   cfg.MeshGroup,
		meshAutoInstall: cfg.MeshAutoInstall,
	}
	if err := s.initStorage(); err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	s.web, err = newWebUI(s)
	if err != nil {
		return nil, fmt.Errorf("web ui: %w", err)
	}
	return s, nil
}

// agentHandler serves what agents and the install scripts need, and nothing
// else: no GUI, no admin API, no login page.
func (s *Server) agentHandler() *http.ServeMux {
	mux := http.NewServeMux()
	s.registerAgentRoutes(mux)
	return mux
}

// adminHandler serves the GUI and admin API (split mode).
func (s *Server) adminHandler() *http.ServeMux {
	mux := http.NewServeMux()
	s.registerAdminRoutes(mux)
	return mux
}

// combinedHandler serves everything on one listener (the default).
func (s *Server) combinedHandler() *http.ServeMux {
	mux := http.NewServeMux()
	s.registerAgentRoutes(mux)
	s.registerAdminRoutes(mux)
	return mux
}

func (s *Server) registerAgentRoutes(mux *http.ServeMux) {
	// Agent endpoints (authenticated by agent id + secret).
	mux.HandleFunc("POST /api/agent/enroll", s.handleEnroll)
	mux.HandleFunc("GET /api/agent/ws", s.handleAgentWS)
	mux.HandleFunc("POST /api/agent/blobs/check", s.agentAuth(s.handleBlobCheck))
	mux.HandleFunc("PUT /api/agent/blobs/{hash}", s.agentAuth(s.handleBlobPut))
	mux.HandleFunc("GET /api/agent/blobs/{hash}", s.agentAuth(s.handleBlobGet))
	mux.HandleFunc("POST /api/agent/snapshots", s.agentAuth(s.handleSnapshotCommit))
	mux.HandleFunc("GET /api/agent/manifests/{id}", s.agentAuth(s.handleManifestGet))
	mux.HandleFunc("GET /api/agent/transfers/{id}/content", s.agentAuth(s.handleTransferContent))
	mux.HandleFunc("PUT /api/agent/transfers/{id}/content", s.agentAuth(s.handleTransferUpload))
	mux.HandleFunc("GET /api/agent/meshagent", s.agentAuth(s.handleAgentMeshInstaller))

	// Prebuilt agent binaries for the install scripts (not secret).
	mux.Handle("GET /dl/", http.StripPrefix("/dl/", http.FileServer(http.Dir(s.cfg.AgentBins))))

	// The paste-in install commands fetch these from the agent address.
	s.web.registerInstallScripts(mux)
}

func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	// Admin API (session cookie).
	s.registerAdminAPI(mux)
	s.registerStorageAPI(mux)
	s.registerRemoteAPI(mux)

	// Web GUI.
	s.web.register(mux)
}

func (s *Server) newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{s.tlsCert},
			MinVersion:   tls.VersionTLS12,
			// Capped at 1.2: .NET Framework's HttpWebRequest (used by
			// Windows PowerShell 5.1's Invoke-WebRequest) fails the
			// handshake against TLS 1.3's post-handshake NewSessionTicket
			// message ("underlying connection was closed: unexpected error
			// on a send"), even though every other client handles it fine.
			// TLS 1.2 with modern cipher suites has no practical security
			// downside here and sidesteps that whole bug class.
			MaxVersion: tls.VersionTLS12,
		},
	}
}

func (s *Server) Run() error {
	go s.runScheduler()
	go s.runMaintenance()

	log.Printf("TLS certificate SHA-256 fingerprint: %s", s.fingerprint)
	if s.mesh != nil {
		log.Printf("MeshCentral remote access enabled: %s (acting as %q)", s.mesh.WebURL, s.mesh.User)
		if s.mesh.APIFingerprint == "" {
			log.Printf("note: CB_MESH_API_FINGERPRINT is not set; MeshCentral's API certificate is not verified " +
				"(acceptable only when CB_MESH_API_URL is on a private Docker network)")
		}
	}

	if !s.cfg.splitMode() {
		log.Printf("central backup server listening on https://%s (GUI, admin API and agents)", s.cfg.Listen)
		return s.newHTTPServer(s.cfg.Listen, s.combinedHandler()).ListenAndServeTLS("", "")
	}

	// Split mode: both listeners share the certificate, so agents' pinned
	// fingerprint is unchanged and cloudflared can reach the admin side
	// over HTTPS (with origin TLS verification off, or this cert pinned).
	if s.cfg.AdminURL == "" {
		log.Printf("note: CB_ADMIN_URL is not set; the storage OAuth callback will use the " +
			"browser's host, and background token refresh falls back to CB_SERVER_NAME")
	}
	errc := make(chan error, 2)
	go func() {
		log.Printf("agent listener on https://%s (agents only; agent URL %s)", s.cfg.Listen, s.agentBaseURL(nil))
		errc <- fmt.Errorf("agent listener: %w", s.newHTTPServer(s.cfg.Listen, s.agentHandler()).ListenAndServeTLS("", ""))
	}()
	go func() {
		log.Printf("admin listener on https://%s (GUI and admin API)", s.cfg.AdminListen)
		errc <- fmt.Errorf("admin listener: %w", s.newHTTPServer(s.cfg.AdminListen, s.adminHandler()).ListenAndServeTLS("", ""))
	}()
	// Either listener failing is fatal: a half-running server (agents up,
	// no GUI, or the reverse) is worse than a clean restart.
	return <-errc
}

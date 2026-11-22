package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// oauthCallbackPath is the storage OAuth redirect path, registered verbatim
// in the provider's developer console.
const oauthCallbackPath = "/api/admin/storage/oauth/callback"

// normalizePublicURL validates and canonicalises CB_PUBLIC_URL — the
// externally reachable origin of this server, e.g.
// "https://backup.example.com:8443". An empty value is valid and means
// "derive it from the incoming request".
func normalizePublicURL(raw string) (string, error) {
	return normalizeOrigin("CB_PUBLIC_URL", raw)
}

// normalizeOrigin validates an https origin setting named name.
func normalizeOrigin(name, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	// A bare host:port is a natural thing to type; accept it rather than
	// failing on the missing scheme.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s is not a valid URL: %w", name, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("%s must use https (got %q): agents pin the server "+
			"certificate and the server has no plain-HTTP listener", name, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s has no host: %q", name, raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("%s must be a bare origin with no path (got %q)", name, u.Path)
	}
	return "https://" + u.Host, nil
}

// splitMode reports whether the admin GUI/API has its own listener
// (CB_ADMIN_LISTEN). In split mode CB_LISTEN serves agents only, so it can
// be published to the internet while the admin side sits behind a tunnel.
func (c Config) splitMode() bool { return c.AdminListen != "" }

// validateListeners checks the listener/URL combination at startup.
func validateListeners(c Config) error {
	if !c.splitMode() {
		return nil
	}
	if sameListenAddr(c.AdminListen, c.Listen) {
		return fmt.Errorf("CB_ADMIN_LISTEN (%s) must differ from CB_LISTEN (%s)", c.AdminListen, c.Listen)
	}
	// The admin side may be reached through a tunnel under a different
	// hostname, so the agent address can't be learned from the browser.
	if c.PublicURL == "" && firstServerName(c.ServerName) == "" {
		return fmt.Errorf("with CB_ADMIN_LISTEN set, CB_PUBLIC_URL (or CB_SERVER_NAME) is required: " +
			"it is the address agents connect to, and it can't be derived from the admin request")
	}
	return nil
}

func sameListenAddr(a, b string) bool {
	norm := func(s string) string {
		if strings.HasPrefix(s, "0.0.0.0:") || strings.HasPrefix(s, "[::]:") {
			return s[strings.LastIndexByte(s, ':'):]
		}
		return s
	}
	return norm(a) == norm(b)
}

// agentBaseURL is the origin agents connect to: baked into enrollment
// commands and installers. It always points at the CB_LISTEN listener.
//
// A configured CB_PUBLIC_URL always wins. Deriving it from the request means
// that browsing the GUI by IP (or by any alternate name) silently mints
// enrollment commands pointing at that address, and the agent then fails
// certificate-fingerprint verification far from the cause. In split mode the
// request is never used: it arrived on the admin listener (often through a
// Cloudflare Tunnel), whose host is not where agents should go.
func (s *Server) agentBaseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	if !s.cfg.splitMode() && r != nil && r.Host != "" {
		return "https://" + r.Host
	}
	return originFor(s.cfg.ServerName, s.cfg.Listen)
}

// adminBaseURL is the origin a browser uses for the admin GUI. It is the base
// of the storage OAuth callback, which identity providers redirect the
// admin's browser to.
//
// Combined mode: same as the agent address (CB_PUBLIC_URL), as before.
// Split mode: CB_ADMIN_URL, else the request's own host (the tunnel
// hostname), else CB_SERVER_NAME with the admin port.
func (s *Server) adminBaseURL(r *http.Request) string {
	if s.cfg.AdminURL != "" {
		return s.cfg.AdminURL
	}
	if !s.cfg.splitMode() {
		return s.agentBaseURL(r)
	}
	if r != nil && r.Host != "" {
		return "https://" + r.Host
	}
	return originFor(s.cfg.ServerName, s.cfg.AdminListen)
}

// publicBaseURL is the combined-mode origin (kept for callers and tests that
// predate the split); it equals agentBaseURL.
func (s *Server) publicBaseURL(r *http.Request) string { return s.agentBaseURL(r) }

// originFor builds the request-free fallback from CB_SERVER_NAME and a
// listen address. Background work (OAuth token refresh) has no request to
// learn the host from.
func originFor(serverName, listen string) string {
	host := firstServerName(serverName)
	if host == "" {
		host = "localhost"
	}
	return "https://" + host + portSuffix(listen)
}

func firstServerName(serverName string) string {
	if i := indexByte(serverName, ','); i >= 0 {
		serverName = serverName[:i]
	}
	return strings.TrimSpace(serverName)
}

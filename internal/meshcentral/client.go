package meshcentral

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// View is a device tab to open in MeshCentral.
type View string

const (
	ViewDesktop  View = "desktop"
	ViewTerminal View = "terminal"
	ViewFiles    View = "files"
)

// viewModes are MeshCentral's device page tab ids (default.handlebars go()).
var viewModes = map[View]int{ViewDesktop: 11, ViewTerminal: 12, ViewFiles: 13}

// ParseView validates a view name.
func ParseView(s string) (View, bool) {
	v := View(strings.ToLower(s))
	_, ok := viewModes[v]
	return v, ok
}

// Node is a MeshCentral device.
type Node struct {
	ID       string `json:"id"`       // "node//<base64>"
	Name     string `json:"name"`     // display name (editable in MeshCentral)
	Computer string `json:"computer"` // computer name reported by the agent
	MeshID   string `json:"mesh_id"`  // "mesh//<base64>"
	Online   bool   `json:"online"`
}

// ShortID is the node id without the "node/<domain>/" prefix, as used in
// MeshCentral URLs (?gotonode=).
func (n Node) ShortID() string { return ShortID(n.ID) }

// ShortID strips the "node/<domain>/" (or "mesh/<domain>/") prefix.
func ShortID(id string) string {
	if parts := strings.SplitN(id, "/", 3); len(parts) == 3 {
		return parts[2]
	}
	return id
}

// Mesh is a MeshCentral device group.
type Mesh struct {
	ID   string `json:"id"` // "mesh//<base64>"
	Name string `json:"name"`
}

// Client talks to one MeshCentral server.
type Client struct {
	// WebURL is the origin admins' browsers use for MeshCentral (through the
	// tunnel), e.g. https://mesh.example.com.
	WebURL string
	// APIURL is the origin this server uses for control.ashx, e.g.
	// https://meshcentral:443 on the internal Docker network. Defaults to
	// WebURL.
	APIURL string
	// APIFingerprint, when set, pins the SHA-256 of MeshCentral's TLS
	// certificate on the API connection (lowercase hex). When empty the
	// certificate is not verified, which is only acceptable on a private
	// network.
	APIFingerprint string
	// AgentURL is the origin MeshAgents connect to (MeshCentral's agent-only
	// port), e.g. https://agents.example.com:4443. Used for installer links.
	AgentURL string
	User     string // MeshCentral account the integration acts as
	Key      []byte // LoginCookieEncryptionKey
	// TokenMinutes is the lifetime of browser login tokens (default 2).
	TokenMinutes int
}

// DeepLink returns a URL that signs the browser in to MeshCentral with a
// fresh single-use token and opens the given tab of the given device.
func (c *Client) DeepLink(nodeID string, view View, now time.Time) (string, error) {
	mode, ok := viewModes[view]
	if !ok {
		return "", fmt.Errorf("unknown view %q", view)
	}
	mins := c.TokenMinutes
	if mins <= 0 {
		mins = 2
	}
	tok, err := LoginToken(c.Key, c.User, mins, now)
	if err != nil {
		return "", err
	}
	node, err := hexNodeID(nodeID)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("login", tok)
	q.Set("gotonode", node)
	q.Set("viewmode", fmt.Sprint(mode))
	return strings.TrimRight(c.WebURL, "/") + "/?" + q.Encode(), nil
}

// hexNodeID converts a node id to the 96-character hex form ?gotonode=
// accepts. MeshCentral's page reads URL arguments without percent-decoding,
// so the base64 form (with its '$' and '@') would only work unescaped; hex is
// URL-safe as is.
func hexNodeID(nodeID string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.NewReplacer("@", "+", "$", "/").Replace(ShortID(nodeID)))
	if err != nil || len(raw) != 48 {
		return "", fmt.Errorf("not a MeshCentral node id: %q", nodeID)
	}
	return strings.ToUpper(hex.EncodeToString(raw)), nil
}

// WindowsInstallerURL is the MeshAgent download for Windows x64 with the
// device group's settings embedded (agent type 4, MeshService64.exe).
func (c *Client) WindowsInstallerURL(meshID string) string {
	q := url.Values{}
	q.Set("id", "4")
	q.Set("meshid", ShortID(meshID))
	q.Set("installflags", "0")
	return strings.TrimRight(c.agentBase(), "/") + "/meshagents?" + q.Encode()
}

// LinuxInstallCommand is a one-liner that installs MeshAgent on Linux into
// the device group. It downloads the agent with the group settings and
// MeshCentral's installer embedded (meshagents?meshinstall=<archid>), which,
// unlike meshinstall.sh, is served on MeshCentral's agent-only port. Agent
// type 6 is Linux x86-64, 32 is Linux ARM64. --no-check-certificate is
// needed for MeshCentral's self-signed certificate; the agent itself then
// authenticates the server by the hash embedded in its settings.
func (c *Client) LinuxInstallCommand(meshID string) string {
	base := strings.TrimRight(c.agentBase(), "/")
	id := strings.NewReplacer("$", "%24", "@", "%40").Replace(ShortID(meshID))
	return fmt.Sprintf(`case "$(uname -m)" in aarch64|arm64) T=32;; *) T=6;; esac; `+
		`wget --no-check-certificate -O meshagent "%s/meshagents?id=%s&installflags=0&meshinstall=$T" && `+
		`chmod +x meshagent && sudo ./meshagent -install`, base, id)
}

func (c *Client) agentBase() string {
	if c.AgentURL != "" {
		return c.AgentURL
	}
	return c.WebURL
}

func (c *Client) apiBase() string {
	if c.APIURL != "" {
		return c.APIURL
	}
	return c.WebURL
}

// Inventory is a snapshot of the device groups and devices the integration
// account can see.
type Inventory struct {
	Meshes []Mesh
	Nodes  []Node
}

// Inventory opens a control.ashx session and fetches device groups and
// devices.
func (c *Client) Inventory(ctx context.Context) (Inventory, error) {
	var inv Inventory
	conn, err := c.dial(ctx)
	if err != nil {
		return inv, err
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		conn.SetReadDeadline(dl)
		conn.SetWriteDeadline(dl)
	}
	for _, action := range []string{"meshes", "nodes"} {
		if err := conn.WriteJSON(map[string]any{"action": action, "responseid": "cb-" + action}); err != nil {
			// MeshCentral closes a session it refuses right after saying why;
			// surface that reason rather than the broken pipe.
			if cause := closeCause(conn); cause != "" {
				return inv, refused(cause)
			}
			return inv, fmt.Errorf("MeshCentral: send %s: %w", action, err)
		}
	}

	gotMeshes, gotNodes := false, false
	for !(gotMeshes && gotNodes) {
		var msg struct {
			Action string `json:"action"`
			Cause  string `json:"cause"`
			Result string `json:"result"`
			Nodes  map[string][]struct {
				ID    string `json:"_id"`
				Name  string `json:"name"`
				RName string `json:"rname"`
				Conn  int    `json:"conn"`
			} `json:"nodes"`
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return inv, fmt.Errorf("MeshCentral: read: %w", err)
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue // not every server message is an object we care about
		}
		switch msg.Action {
		case "close":
			return inv, refused(msg.Cause)
		case "meshes":
			// Field names in MeshCentral are _id/name; decode them separately.
			var m struct {
				Meshes []struct {
					ID   string `json:"_id"`
					Name string `json:"name"`
				} `json:"meshes"`
			}
			json.Unmarshal(data, &m)
			for _, x := range m.Meshes {
				inv.Meshes = append(inv.Meshes, Mesh{ID: x.ID, Name: x.Name})
			}
			gotMeshes = true
		case "nodes":
			if msg.Nodes == nil && msg.Result != "" {
				return inv, fmt.Errorf("MeshCentral: nodes: %s", msg.Result)
			}
			for meshID, list := range msg.Nodes {
				for _, n := range list {
					inv.Nodes = append(inv.Nodes, Node{
						ID: n.ID, Name: n.Name, Computer: n.RName, MeshID: meshID,
						Online: n.Conn&1 != 0,
					})
				}
			}
			gotNodes = true
		}
	}
	sort.Slice(inv.Meshes, func(i, j int) bool { return inv.Meshes[i].Name < inv.Meshes[j].Name })
	sort.Slice(inv.Nodes, func(i, j int) bool { return strings.ToLower(inv.Nodes[i].Name) < strings.ToLower(inv.Nodes[j].Name) })
	return inv, nil
}

func refused(cause string) error {
	return fmt.Errorf("MeshCentral refused the API session (%s); check CB_MESH_LOGIN_KEY and CB_MESH_USER", cause)
}

// closeCause reads any messages already sent by the server, briefly, and
// returns the cause of a "close" among them.
func closeCause(conn *websocket.Conn) string {
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return ""
		}
		var m struct{ Action, Cause string }
		if json.Unmarshal(data, &m) == nil && m.Action == "close" {
			if m.Cause == "" {
				m.Cause = "closed"
			}
			return m.Cause
		}
	}
}

func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	u, err := url.Parse(c.apiBase())
	if err != nil {
		return nil, fmt.Errorf("MeshCentral API URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	cookie, err := apiCookie(c.Key, c.User, time.Now())
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/control.ashx"
	u.RawQuery = url.Values{"auth": {cookie}}.Encode()

	d := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  c.tlsConfig(),
	}
	// No Origin header is sent: MeshCentral treats that as a non-browser
	// client and skips its origin check.
	conn, res, err := d.DialContext(ctx, u.String(), nil)
	if err != nil {
		if res != nil {
			return nil, fmt.Errorf("MeshCentral: connect %s: %s", redact(u), res.Status)
		}
		return nil, fmt.Errorf("MeshCentral: connect %s: %w", redact(u), err)
	}
	return conn, nil
}

func (c *Client) tlsConfig() *tls.Config {
	fp := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(c.APIFingerprint), ":", ""))
	cfg := &tls.Config{InsecureSkipVerify: true} // verified below, or deliberately not at all
	if fp != "" {
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("MeshCentral presented no certificate")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != fp {
				return errors.New("MeshCentral certificate fingerprint mismatch")
			}
			return nil
		}
	}
	return cfg
}

// redact drops the auth cookie from a URL for error messages and logs.
func redact(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	return c.String()
}

// Match returns the devices whose computer name or display name matches
// hostname (case-insensitive; a bare name also matches an FQDN's first label
// and vice versa). The agent-reported computer name is preferred: if any
// device matches on it, display-name-only matches are dropped.
func Match(nodes []Node, hostname string) []Node {
	h := strings.ToLower(strings.TrimSpace(hostname))
	if h == "" {
		return nil
	}
	short := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.IndexByte(s, '.'); i > 0 {
			return s[:i]
		}
		return s
	}
	eq := func(a string) bool {
		a = strings.ToLower(strings.TrimSpace(a))
		return a != "" && (a == h || short(a) == short(h))
	}
	var byComputer, byName []Node
	for _, n := range nodes {
		switch {
		case eq(n.Computer):
			byComputer = append(byComputer, n)
		case eq(n.Name):
			byName = append(byName, n)
		}
	}
	if len(byComputer) > 0 {
		return byComputer
	}
	return byName
}

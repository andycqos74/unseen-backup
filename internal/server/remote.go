package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"centralbackup/internal/meshcentral"
	"centralbackup/internal/server/store"
)

// Remote access through MeshCentral (desktop, terminal, files).
//
// The controller never relays remote sessions. It only:
//   - maps each backup client to its MeshCentral device (by hostname, or as
//     picked by the admin), using MeshCentral's control API;
//   - mints a short-lived, single-use MeshCentral login token per click and
//     redirects the admin's browser to that device's tab in MeshCentral;
//   - shows MeshAgent install links for the configured device group.
// The feature is off unless CB_MESH_URL and CB_MESH_LOGIN_KEY are set.

// meshConfigFromEnv reads the MeshCentral settings. It returns nil when the
// integration is not configured.
func meshConfigFromEnv() (*meshcentral.Client, string, error) {
	c, g, _, err := meshConfigFromEnvEx()
	return c, g, err
}

// meshConfigFromEnvEx also reports CB_MESH_AUTOINSTALL (default on): whether
// backup agents install MeshAgent themselves.
func meshConfigFromEnvEx() (*meshcentral.Client, string, bool, error) {
	webURL := strings.TrimSpace(os.Getenv("CB_MESH_URL"))
	keyHex := strings.TrimSpace(os.Getenv("CB_MESH_LOGIN_KEY"))
	if webURL == "" && keyHex == "" {
		return nil, "", false, nil
	}
	if webURL == "" || keyHex == "" {
		return nil, "", false, fmt.Errorf("MeshCentral integration needs both CB_MESH_URL and CB_MESH_LOGIN_KEY")
	}
	web, err := normalizeOrigin("CB_MESH_URL", webURL)
	if err != nil {
		return nil, "", false, err
	}
	key, err := meshcentral.ParseKey(keyHex)
	if err != nil {
		return nil, "", false, fmt.Errorf("CB_MESH_LOGIN_KEY: %w", err)
	}
	api := strings.TrimSpace(os.Getenv("CB_MESH_API_URL"))
	agentURL := strings.TrimSpace(os.Getenv("CB_MESH_AGENT_URL"))
	if agentURL != "" {
		if agentURL, err = normalizeOrigin("CB_MESH_AGENT_URL", agentURL); err != nil {
			return nil, "", false, err
		}
	}
	user := strings.TrimSpace(os.Getenv("CB_MESH_USER"))
	if user == "" {
		user = "admin"
	}
	mins := 2
	if v := os.Getenv("CB_MESH_TOKEN_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 60 {
			mins = n
		}
	}
	autoInstall := !strings.EqualFold(strings.TrimSpace(os.Getenv("CB_MESH_AUTOINSTALL")), "false")
	return &meshcentral.Client{
		WebURL:         web,
		APIURL:         api,
		APIFingerprint: os.Getenv("CB_MESH_API_FINGERPRINT"),
		AgentURL:       agentURL,
		User:           user,
		Key:            key,
		TokenMinutes:   mins,
	}, strings.TrimSpace(os.Getenv("CB_MESH_GROUP")), autoInstall, nil
}

func meshNodeKey(agentID string) string { return "mesh_node:" + agentID }

// meshServedKey records when this server handed a client its MeshAgent
// installer (unix seconds). The device whose first MeshCentral connection
// follows that moment, among those matching the client's hostname, is the
// one the bootstrapper installed.
func meshServedKey(agentID string) string { return "mesh_served:" + agentID }

// meshInstallWindow bounds how long after the download the new device must
// first connect to count as this client's.
const meshInstallWindow = 2 * time.Hour

// installedBy picks, among candidates, the single device that first
// connected within the window after served. nil if none or ambiguous.
func installedBy(cands []meshcentral.Node, served time.Time) *meshcentral.Node {
	var hit *meshcentral.Node
	for i := range cands {
		fc := cands[i].FirstConnect
		if fc.IsZero() || fc.Before(served.Add(-time.Minute)) || fc.After(served.Add(meshInstallWindow)) {
			continue
		}
		if hit != nil {
			return nil
		}
		hit = &cands[i]
	}
	return hit
}

// meshUnlinked is stored when the admin explicitly unlinks a client, so the
// hostname match does not immediately link it again.
const meshUnlinked = "-"

func (s *Server) registerRemoteAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/mesh", s.adminAuth(s.handleMeshInfo))
	mux.HandleFunc("GET /api/admin/agents/{id}/remote", s.adminAuth(s.handleRemoteGet))
	mux.HandleFunc("POST /api/admin/agents/{id}/remote/link", s.adminAuth(s.handleRemoteLink))
	mux.HandleFunc("GET /api/admin/agents/{id}/remote/open", s.adminAuth(s.handleRemoteOpen))
}

func (s *Server) meshInventory(r *http.Request) (meshcentral.Inventory, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	return s.mesh.Inventory(ctx)
}

// meshGroup picks the device group for installer links: CB_MESH_GROUP by
// name, or the only group if there is exactly one.
func (s *Server) meshGroup(inv meshcentral.Inventory) (*meshcentral.Mesh, string) {
	if s.meshGroupName != "" {
		for _, m := range inv.Meshes {
			if strings.EqualFold(m.Name, s.meshGroupName) {
				m := m
				return &m, ""
			}
		}
		return nil, fmt.Sprintf("device group %q (CB_MESH_GROUP) not found in MeshCentral", s.meshGroupName)
	}
	switch len(inv.Meshes) {
	case 0:
		return nil, "MeshCentral has no device groups yet: create one in MeshCentral"
	case 1:
		return &inv.Meshes[0], ""
	}
	return nil, "MeshCentral has several device groups: set CB_MESH_GROUP to the one new clients should join"
}

// handleMeshInfo backs the enroll dialog: is remote access configured, and
// how to install MeshAgent into the device group.
func (s *Server) handleMeshInfo(w http.ResponseWriter, r *http.Request) {
	if s.mesh == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	out := map[string]any{"enabled": true, "web_url": s.mesh.WebURL}
	inv, err := s.meshInventory(r)
	if err != nil {
		log.Printf("meshcentral: %v", err)
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	g, problem := s.meshGroup(inv)
	if g == nil {
		out["error"] = problem
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["group"] = g.Name
	out["windows_url"] = s.mesh.WindowsBrowserURL(g.ID)
	out["windows_direct_url"] = s.mesh.WindowsInstallerURL(g.ID)
	out["linux_cmd"] = s.mesh.LinuxInstallCommand(g.ID)
	writeJSON(w, http.StatusOK, out)
}

// meshLink is a client's stored MeshCentral link: a node id, "m:"+node id
// when the admin picked it by hand, or meshUnlinked.
type meshLink struct {
	nodeID   string
	manual   bool // picked by hand: never replaced automatically
	unlinked bool // explicitly unlinked: nothing is linked automatically
}

func (s *Server) getMeshLink(agentID string) meshLink {
	v, _ := s.store.GetSetting(meshNodeKey(agentID))
	switch {
	case v == meshUnlinked:
		return meshLink{unlinked: true}
	case strings.HasPrefix(v, "m:"):
		return meshLink{nodeID: v[2:], manual: true}
	}
	return meshLink{nodeID: v}
}

// exactMatch finds the device the bootstrapper installed for this client:
// by agent tag if MeshCentral reports one, else the hostname match that first
// connected right after the installer was served.
func (s *Server) exactMatch(agentID string, nodes, hostMatches []meshcentral.Node) *meshcentral.Node {
	for i := range nodes {
		if nodes[i].Tag == agentID {
			return &nodes[i]
		}
	}
	if v, _ := s.store.GetSetting(meshServedKey(agentID)); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			return installedBy(hostMatches, time.Unix(sec, 0))
		}
	}
	return nil
}

// handleRemoteGet reports which MeshCentral device a client is linked to.
// Automatic linking, strongest first: the device the bootstrapper installed
// (exact), then a single hostname match. An admin's manual pick or explicit
// unlink is never overridden.
func (s *Server) handleRemoteGet(w http.ResponseWriter, r *http.Request) {
	if s.mesh == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	agent, err := s.store.GetAgent(r.PathValue("id"))
	if err == store.ErrNotFound {
		httpError(w, http.StatusNotFound, "no such client")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := map[string]any{"enabled": true}
	inv, err := s.meshInventory(r)
	if err != nil {
		log.Printf("meshcentral: %v", err)
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	link := s.getMeshLink(agent.ID)
	matches := meshcentral.Match(inv.Nodes, agent.Hostname)

	if !link.unlinked && !link.manual {
		if n := s.exactMatch(agent.ID, inv.Nodes, matches); n != nil {
			if n.ID != link.nodeID {
				s.store.SetSetting(meshNodeKey(agent.ID), n.ID)
			}
			out["linked"] = *n
			out["exact"] = true
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	if link.nodeID != "" {
		for _, n := range inv.Nodes {
			if n.ID == link.nodeID {
				out["linked"] = n
				out["manual"] = link.manual
				writeJSON(w, http.StatusOK, out)
				return
			}
		}
		out["stale"] = true // linked device no longer exists in MeshCentral
	}
	if !link.unlinked && len(matches) == 1 {
		if err := s.store.SetSetting(meshNodeKey(agent.ID), matches[0].ID); err == nil {
			out["linked"] = matches[0]
			out["auto"] = true
			delete(out, "stale")
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	// Ambiguous or no match: let the admin pick, matches first.
	cands := append([]meshcentral.Node{}, matches...)
	if len(matches) == 0 || link.unlinked {
		seen := map[string]bool{}
		for _, n := range matches {
			seen[n.ID] = true
		}
		for _, n := range inv.Nodes {
			if !seen[n.ID] {
				cands = append(cands, n)
			}
		}
	}
	if len(cands) > 500 {
		cands = cands[:500]
	}
	out["candidates"] = cands
	out["matched"] = len(matches) > 0
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRemoteLink(w http.ResponseWriter, r *http.Request) {
	if s.mesh == nil {
		httpError(w, http.StatusNotFound, "remote access is not configured")
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.GetAgent(id); err != nil {
		httpError(w, http.StatusNotFound, "no such client")
		return
	}
	req, err := decodeBody[struct {
		NodeID string `json:"node_id"`
	}](r)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	nodeID := strings.TrimSpace(req.NodeID)
	if nodeID == "" {
		nodeID = meshUnlinked
	} else if strings.HasPrefix(nodeID, "node/") {
		nodeID = "m:" + nodeID // picked by hand
	} else {
		httpError(w, http.StatusBadRequest, "invalid MeshCentral device id")
		return
	}
	if err := s.store.SetSetting(meshNodeKey(id), nodeID); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRemoteOpen redirects the admin's browser into MeshCentral, signed in
// with a fresh single-use token, on the linked device's requested tab. The
// token is minted per click and never placed in page HTML.
func (s *Server) handleRemoteOpen(w http.ResponseWriter, r *http.Request) {
	if s.mesh == nil {
		httpError(w, http.StatusNotFound, "remote access is not configured")
		return
	}
	view, ok := meshcentral.ParseView(r.URL.Query().Get("view"))
	if !ok {
		httpError(w, http.StatusBadRequest, "view must be desktop, terminal or files")
		return
	}
	nodeID := s.getMeshLink(r.PathValue("id")).nodeID
	if nodeID == "" {
		httpError(w, http.StatusConflict, "this client is not linked to a MeshCentral device yet")
		return
	}
	link, err := s.mesh.DeepLink(nodeID, view, time.Now())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not create MeshCentral login")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, link, http.StatusFound)
}

// handleAgentMeshInstaller serves MeshAgent to a backup agent (the
// bootstrapper): the installer for the agent's platform, fetched from
// MeshCentral server-side with the device group's settings and the backup
// agent's ID as the agent tag, so the new MeshCentral device links to this
// client exactly. Serving it from here means clients download it over the
// connection whose certificate they already pin, and never need to trust
// MeshCentral's self-signed certificate.
//
// 204 means "nothing to install" (integration off or auto-install disabled);
// the agent stops asking until its next start.
func (s *Server) handleAgentMeshInstaller(w http.ResponseWriter, r *http.Request, agentID string) {
	if s.mesh == nil || !s.meshAutoInstall {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	goos, goarch := r.URL.Query().Get("os"), r.URL.Query().Get("arch")
	if _, _, ok := meshcentral.InstallerFor(goos, goarch); !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	inv, err := s.meshInventory(r)
	if err != nil {
		log.Printf("meshcentral: installer for %s: %v", agentID, err)
		httpError(w, http.StatusServiceUnavailable, "MeshCentral unavailable")
		return
	}
	g, problem := s.meshGroup(inv)
	if g == nil {
		log.Printf("meshcentral: installer for %s: %s", agentID, problem)
		httpError(w, http.StatusServiceUnavailable, problem)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	body, inst, err := s.mesh.FetchInstaller(ctx, g.ID, goos, goarch, agentID)
	if err != nil {
		log.Printf("meshcentral: installer for %s: %v", agentID, err)
		httpError(w, http.StatusBadGateway, "could not fetch MeshAgent from MeshCentral")
		return
	}
	defer body.Close()
	s.store.SetSetting(meshServedKey(agentID), strconv.FormatInt(time.Now().Unix(), 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Mesh-File-Name", inst.FileName)
	w.Header().Set("X-Mesh-Install-Args", strings.Join(inst.Args, " "))
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, body); err != nil {
		log.Printf("meshcentral: installer for %s: copy: %v", agentID, err)
	}
}

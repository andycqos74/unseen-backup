package server

import (
	"context"
	"fmt"
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
	webURL := strings.TrimSpace(os.Getenv("CB_MESH_URL"))
	keyHex := strings.TrimSpace(os.Getenv("CB_MESH_LOGIN_KEY"))
	if webURL == "" && keyHex == "" {
		return nil, "", nil
	}
	if webURL == "" || keyHex == "" {
		return nil, "", fmt.Errorf("MeshCentral integration needs both CB_MESH_URL and CB_MESH_LOGIN_KEY")
	}
	web, err := normalizeOrigin("CB_MESH_URL", webURL)
	if err != nil {
		return nil, "", err
	}
	key, err := meshcentral.ParseKey(keyHex)
	if err != nil {
		return nil, "", fmt.Errorf("CB_MESH_LOGIN_KEY: %w", err)
	}
	api := strings.TrimSpace(os.Getenv("CB_MESH_API_URL"))
	agentURL := strings.TrimSpace(os.Getenv("CB_MESH_AGENT_URL"))
	if agentURL != "" {
		if agentURL, err = normalizeOrigin("CB_MESH_AGENT_URL", agentURL); err != nil {
			return nil, "", err
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
	return &meshcentral.Client{
		WebURL:         web,
		APIURL:         api,
		APIFingerprint: os.Getenv("CB_MESH_API_FINGERPRINT"),
		AgentURL:       agentURL,
		User:           user,
		Key:            key,
		TokenMinutes:   mins,
	}, strings.TrimSpace(os.Getenv("CB_MESH_GROUP")), nil
}

func meshNodeKey(agentID string) string { return "mesh_node:" + agentID }

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

// handleRemoteGet reports which MeshCentral device a client is linked to,
// linking it automatically when exactly one device matches its hostname.
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

	linkedID, _ := s.store.GetSetting(meshNodeKey(agent.ID))
	manual := linkedID == meshUnlinked
	if manual {
		linkedID = ""
	}
	if linkedID != "" {
		for _, n := range inv.Nodes {
			if n.ID == linkedID {
				out["linked"] = n
				writeJSON(w, http.StatusOK, out)
				return
			}
		}
		out["stale"] = true // linked device no longer exists in MeshCentral
	}

	matches := meshcentral.Match(inv.Nodes, agent.Hostname)
	if linkedID == "" && !manual && len(matches) == 1 {
		if err := s.store.SetSetting(meshNodeKey(agent.ID), matches[0].ID); err == nil {
			out["linked"] = matches[0]
			out["auto"] = true
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	// Ambiguous or no match: let the admin pick, matches first.
	cands := matches
	if len(cands) == 0 || manual {
		// Matches first, then everything else.
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
	} else if !strings.HasPrefix(nodeID, "node/") {
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
	nodeID, _ := s.store.GetSetting(meshNodeKey(r.PathValue("id")))
	if nodeID == "" || nodeID == meshUnlinked {
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

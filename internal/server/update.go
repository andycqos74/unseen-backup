package server

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"centralbackup/internal/proto"
)

// Agent self-update: when an agent's executable differs from the build this
// server ships for its platform, the server offers the update on connect
// (MsgAgentUpdate) and serves it from GET /api/agent/update. On by default;
// CB_AGENT_AUTOUPDATE=false turns it off.

func autoUpdateEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("CB_AGENT_AUTOUPDATE")), "false")
}

// agentBuildFile maps a platform to the build shipped in the agent
// binaries directory (the same files the installers are made from).
func agentBuildFile(goos, goarch, flavor string) string {
	if !validPlatformWord(goos) || !validPlatformWord(goarch) {
		return ""
	}
	name := "backup-agent-" + goos + "-" + goarch
	if flavor == "legacy" {
		if goos != "windows" || goarch != "386" {
			return ""
		}
		name += "-legacy"
	} else if flavor != "" {
		return ""
	}
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// offerAgentUpdate runs on hello.
func (s *Server) offerAgentUpdate(agentID string, h proto.Hello) {
	if !s.autoUpdate || h.ExeSHA256 == "" {
		return // disabled, or an agent too old to update itself
	}
	name := agentBuildFile(h.OS, h.Arch, h.Flavor)
	if name == "" {
		return
	}
	sum, err := fileSHA256(filepath.Join(s.cfg.AgentBins, name))
	if err != nil || sum == h.ExeSHA256 {
		return
	}
	log.Printf("agent %s: offering update to %s (%s)", agentID, name, sum[:12])
	s.hub.Send(agentID, proto.MsgAgentUpdate, proto.AgentUpdate{SHA256: sum})
}

func (s *Server) handleAgentUpdateDownload(w http.ResponseWriter, r *http.Request, agentID string) {
	q := r.URL.Query()
	name := agentBuildFile(q.Get("os"), q.Get("arch"), q.Get("flavor"))
	if name == "" {
		httpError(w, http.StatusBadRequest, "no build for this platform")
		return
	}
	path := filepath.Join(s.cfg.AgentBins, name)
	sum, err := fileSHA256(path)
	if err != nil {
		httpError(w, http.StatusNotFound, "no build for this platform on the server")
		return
	}
	w.Header().Set("X-Content-SHA256", sum)
	http.ServeFile(w, r, path)
}

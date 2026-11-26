//go:build !windows

package agent

import (
	"os"
	"runtime"
)

// meshAgentPaths are where MeshAgent's Linux installers put the agent or its
// service definition (service name "meshagent").
var meshAgentPaths = []string{
	"/etc/systemd/system/meshagent.service",
	"/lib/systemd/system/meshagent.service",
	"/usr/lib/systemd/system/meshagent.service",
	"/etc/init.d/meshagent",
	"/usr/local/mesh_services/meshagent/meshagent",
	"/usr/local/mesh/meshagent",
	// Systems without systemd/init.d (containers, some appliances).
	"/usr/local/mesh_daemons/meshagent/meshagent",
}

// meshAgentInstalled reports whether MeshAgent is installed. On platforms
// the bootstrapper doesn't handle (macOS, BSD) it reports true, so nothing is
// attempted there.
func meshAgentInstalled() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	for _, p := range meshAgentPaths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

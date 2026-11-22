//go:build windows

package agent

import (
	"golang.org/x/sys/windows/svc/mgr"
)

// meshAgentInstalled reports whether the MeshAgent service ("Mesh Agent",
// MeshAgent's default service name) exists.
func meshAgentInstalled() bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer m.Disconnect()
	s, err := m.OpenService("Mesh Agent")
	if err != nil {
		return false
	}
	s.Close()
	return true
}

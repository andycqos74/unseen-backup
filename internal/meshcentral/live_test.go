package meshcentral

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveInventory runs against a real MeshCentral when MC_LIVE_URL (its
// web origin, e.g. https://localhost:19443) and MC_LIVE_KEY (its
// LoginCookieEncryptionKey) are set. MC_LIVE_USER defaults to "admin".
func TestLiveInventory(t *testing.T) {
	u, k := os.Getenv("MC_LIVE_URL"), os.Getenv("MC_LIVE_KEY")
	if u == "" || k == "" {
		t.Skip("MC_LIVE_URL / MC_LIVE_KEY not set")
	}
	key, err := ParseKey(k)
	if err != nil {
		t.Fatal(err)
	}
	user := os.Getenv("MC_LIVE_USER")
	if user == "" {
		user = "admin"
	}
	c := &Client{WebURL: u, User: user, Key: key}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inv, err := c.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("groups: %+v", inv.Meshes)
	t.Logf("devices: %+v", inv.Nodes)
	if len(inv.Meshes) > 0 {
		t.Logf("WINDOWS %s", c.WindowsInstallerURL(inv.Meshes[0].ID))
		t.Logf("LINUX %s", c.LinuxInstallCommand(inv.Meshes[0].ID))
	}
	if len(inv.Nodes) > 0 {
		link, _ := c.DeepLink(inv.Nodes[0].ID, ViewTerminal, time.Now())
		t.Logf("LINK %s", link)
	}

	bad := append([]byte{}, key...)
	bad[1] ^= 1
	if _, err := (&Client{WebURL: u, User: user, Key: bad}).Inventory(ctx); err == nil {
		t.Error("wrong key accepted")
	} else {
		t.Logf("wrong key: %v", err)
	}
}

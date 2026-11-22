package meshcentral

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Produced by MeshCentral 1.3.1's own encodeCookie (meshcentral.js) under
// node with this key, for payload {"u":"user//admin","a":3,"expire":0}.
const (
	fixtureKey   = "bff352d80e862d7f59c7f9cb8852baeef70a03b8c51dd37a408201039076ae803d6d506758aa1849ed559669156d47da786bcaaf52de433584fb4b56bf671ddba5ad3ed6d8766d0c9364cb06e88e966d"
	fixtureToken = "ECfml8uKBzs9dRin0MQKmIosFd0tXrsWoR55x@5bGOqQzXA9ZIJonZzNkQVsKYUpxcOOG8XT$jFQda8mg83whjTn4hhs82$iuxseTAVaRwzm2w=="
)

func TestDecodeMeshCentralProducedCookie(t *testing.T) {
	key, err := ParseKey(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	o, err := DecodeCookie(key, fixtureToken)
	if err != nil {
		t.Fatalf("decode MeshCentral cookie: %v", err)
	}
	if o["u"] != "user//admin" || o["a"] != float64(3) {
		t.Errorf("payload = %v", o)
	}
	if _, ok := o["time"].(float64); !ok {
		t.Errorf("time missing: %v", o)
	}
}

func TestLoginTokenRoundTrip(t *testing.T) {
	key, _ := ParseKey(fixtureKey)
	now := time.Unix(1_800_000_000, 0)
	tok, err := LoginToken(key, "Admin", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(tok, "+/") {
		t.Errorf("token uses raw base64 characters: %s", tok)
	}
	o, err := DecodeCookie(key, tok)
	if err != nil {
		t.Fatal(err)
	}
	if o["u"] != "user//admin" || o["a"] != float64(3) || o["expire"] != float64(2) ||
		o["time"] != float64(now.Unix()) || o["once"] == "" {
		t.Errorf("payload = %v", o)
	}
	// Two tokens must differ (fresh IV and once value).
	tok2, _ := LoginToken(key, "admin", 2, now)
	if tok2 == tok {
		t.Error("tokens repeat")
	}
}

// TestPrintTokenForInterop prints a Go-made login token and API cookie for
// checking against MeshCentral's decodeCookie (see docs). Skipped unless
// MC_INTEROP_KEY is set.
func TestPrintTokenForInterop(t *testing.T) {
	k := os.Getenv("MC_INTEROP_KEY")
	if k == "" {
		t.Skip("MC_INTEROP_KEY not set")
	}
	key, err := ParseKey(k)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := LoginToken(key, "admin", 2, time.Now())
	api, _ := apiCookie(key, "admin", time.Now())
	t.Logf("LOGIN %s", tok)
	t.Logf("API %s", api)
}

func TestParseKey(t *testing.T) {
	if _, err := ParseKey(strings.Repeat("ab", 79)); err == nil {
		t.Error("short key accepted")
	}
	if _, err := ParseKey(strings.Repeat("zz", 80)); err == nil {
		t.Error("non-hex key accepted")
	}
	if _, err := ParseKey(" " + fixtureKey + "\n"); err != nil {
		t.Errorf("valid key rejected: %v", err)
	}
}

func TestDeepLinkAndInstallers(t *testing.T) {
	key, _ := ParseKey(fixtureKey)
	c := &Client{WebURL: "https://mesh.example.com/", AgentURL: "https://agents.example.com:4443", User: "admin", Key: key}
	link, err := c.DeepLink("node//ePGDXZJeMdu2NT0$KVo$n1$0tL87@pkng6UL73Mvl6uWlU2TLSJ1dC9@OhohL0yK", ViewTerminal, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "https://mesh.example.com/?") ||
		!strings.Contains(link, "gotonode=78F1835D925E31DBB6353D3F295A3F9F5FF4B4BF3BFA992783A50BEF732F97AB96954D932D2275742F7E3A1A212F4C8A&") || !strings.Contains(link, "viewmode=12") ||
		!strings.Contains(link, "login=") {
		t.Errorf("deep link = %s", link)
	}
	if _, err := c.DeepLink("node//x", View("bogus"), time.Now()); err == nil {
		t.Error("bogus view accepted")
	}
	if _, err := c.DeepLink("node//not-a-node", ViewDesktop, time.Now()); err == nil {
		t.Error("malformed node id accepted")
	}
	win := c.WindowsInstallerURL("mesh//G$1@")
	if win != "https://agents.example.com:4443/meshagents?id=4&installflags=0&meshid=G%241%40" {
		t.Errorf("windows installer = %s", win)
	}
	lin := c.LinuxInstallCommand("mesh//G$1@")
	if !strings.Contains(lin, "https://agents.example.com:4443/meshagents?id=G%241%40&installflags=0&meshinstall=$T") ||
		!strings.Contains(lin, "sudo ./meshagent -install") {
		t.Errorf("linux command = %s", lin)
	}
}

func TestMatch(t *testing.T) {
	nodes := []Node{
		{ID: "node//1", Name: "Accounts PC", Computer: "ACC-01"},
		{ID: "node//2", Name: "acc-01", Computer: "OTHER"},
		{ID: "node//3", Name: "web", Computer: "web.example.com"},
	}
	if m := Match(nodes, "acc-01"); len(m) != 1 || m[0].ID != "node//1" {
		t.Errorf("computer-name match should win over display name: %v", m)
	}
	if m := Match(nodes, "WEB"); len(m) != 1 || m[0].ID != "node//3" {
		t.Errorf("short name should match FQDN: %v", m)
	}
	if m := Match(nodes, "nope"); len(m) != 0 {
		t.Errorf("unexpected match: %v", m)
	}
	if m := Match(nodes, ""); len(m) != 0 {
		t.Errorf("empty hostname matched: %v", m)
	}
}

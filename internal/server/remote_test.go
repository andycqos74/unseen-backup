package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"centralbackup/internal/meshcentral"
	"centralbackup/internal/proto"
)

var testMeshKey = func() []byte {
	k := make([]byte, meshcentral.KeyLen)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}()

// fakeMeshCentral serves control.ashx like MeshCentral: it authenticates the
// ?auth= cookie with the shared key and answers "meshes" and "nodes".
func fakeMeshCentral(t *testing.T, nodes map[string][]map[string]any, meshes []map[string]any) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/control.ashx" {
			http.NotFound(w, r)
			return
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		o, err := meshcentral.DecodeCookie(testMeshKey, r.URL.Query().Get("auth"))
		if err != nil || o["userid"] != "user//admin" || o["domainid"] != "" {
			ws.WriteJSON(map[string]any{"action": "close", "cause": "noauth", "msg": "noauth"})
			return
		}
		ws.WriteJSON(map[string]any{"action": "serverinfo"}) // unrelated chatter first
		for {
			var cmd map[string]any
			if err := ws.ReadJSON(&cmd); err != nil {
				return
			}
			switch cmd["action"] {
			case "meshes":
				ws.WriteJSON(map[string]any{"action": "meshes", "meshes": meshes})
			case "nodes":
				ws.WriteJSON(map[string]any{"action": "nodes", "responseid": cmd["responseid"], "nodes": nodes})
			}
		}
	}))
}

func meshTestServer(t *testing.T, mc *httptest.Server, group string) (*Server, string) {
	t.Helper()
	s := testServer(t)
	s.mesh = &meshcentral.Client{
		WebURL:   "https://mesh.example.com",
		APIURL:   mc.URL,
		AgentURL: "https://agents.example.com:4443",
		User:     "admin",
		Key:      testMeshKey,
	}
	s.meshGroupName = group
	id, _, err := s.store.CreateAgent("pc", proto.EnrollRequest{Hostname: "ACC-01", OS: "windows", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	return s, id
}

func serveAdmin(s *Server, method, path, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	// Bypass the session check: these tests are about the handlers.
	mux.HandleFunc("GET /api/admin/mesh", s.handleMeshInfo)
	mux.HandleFunc("GET /api/admin/agents/{id}/remote", s.handleRemoteGet)
	mux.HandleFunc("POST /api/admin/agents/{id}/remote/link", s.handleRemoteLink)
	mux.HandleFunc("GET /api/admin/agents/{id}/remote/open", s.handleRemoteOpen)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON %q: %v", rec.Body.String(), err)
	}
	return m
}

var testNodes = map[string][]map[string]any{
	"mesh//G1": {
		{"_id": "node//ePGDXZJeMdu2NT0$KVo$n1$0tL87@pkng6UL73Mvl6uWlU2TLSJ1dC9@OhohL0yK", "name": "Accounts PC", "rname": "ACC-01", "conn": 1},
		{"_id": "node//N2", "name": "web", "rname": "web01", "conn": 0},
	},
}
var testMeshes = []map[string]any{{"_id": "mesh//G1", "name": "Office"}}

func TestRemoteAutoLinkAndOpen(t *testing.T) {
	mc := fakeMeshCentral(t, testNodes, testMeshes)
	defer mc.Close()
	s, id := meshTestServer(t, mc, "")

	rec := serveAdmin(s, "GET", "/api/admin/agents/"+id+"/remote", "")
	m := decodeJSON(t, rec)
	linked, _ := m["linked"].(map[string]any)
	if linked == nil || linked["id"] != "node//ePGDXZJeMdu2NT0$KVo$n1$0tL87@pkng6UL73Mvl6uWlU2TLSJ1dC9@OhohL0yK" || m["auto"] != true || linked["online"] != true {
		t.Fatalf("remote = %v, want auto-link to node//ePGDXZJeMdu2NT0$KVo$n1$0tL87@pkng6UL73Mvl6uWlU2TLSJ1dC9@OhohL0yK", m)
	}

	rec = serveAdmin(s, "GET", "/api/admin/agents/"+id+"/remote/open?view=desktop", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("open = %d %s", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || u.Host != "mesh.example.com" {
		t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}
	q := u.Query()
	if q.Get("gotonode") != "78F1835D925E31DBB6353D3F295A3F9F5FF4B4BF3BFA992783A50BEF732F97AB96954D932D2275742F7E3A1A212F4C8A" || q.Get("viewmode") != "11" {
		t.Errorf("redirect query = %v", q)
	}
	tok, err := meshcentral.DecodeCookie(testMeshKey, q.Get("login"))
	if err != nil || tok["u"] != "user//admin" || tok["a"] != float64(3) || tok["expire"] != float64(2) || tok["once"] == nil {
		t.Errorf("login token = %v, %v", tok, err)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("redirect carrying a login token must not be cacheable")
	}

	if rec := serveAdmin(s, "GET", "/api/admin/agents/"+id+"/remote/open?view=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bogus view = %d", rec.Code)
	}
}

func TestRemoteUnlinkStopsAutoLink(t *testing.T) {
	mc := fakeMeshCentral(t, testNodes, testMeshes)
	defer mc.Close()
	s, id := meshTestServer(t, mc, "")
	serveAdmin(s, "GET", "/api/admin/agents/"+id+"/remote", "") // auto-links

	if rec := serveAdmin(s, "POST", "/api/admin/agents/"+id+"/remote/link", `{"node_id":""}`); rec.Code != http.StatusOK {
		t.Fatalf("unlink = %d", rec.Code)
	}
	m := decodeJSON(t, serveAdmin(s, "GET", "/api/admin/agents/"+id+"/remote", ""))
	if m["linked"] != nil {
		t.Fatalf("unlinked client was linked again: %v", m)
	}
	cands, _ := m["candidates"].([]any)
	if len(cands) != 2 || cands[0].(map[string]any)["id"] != "node//ePGDXZJeMdu2NT0$KVo$n1$0tL87@pkng6UL73Mvl6uWlU2TLSJ1dC9@OhohL0yK" {
		t.Errorf("candidates = %v, want the match first then the rest", cands)
	}
	if rec := serveAdmin(s, "GET", "/api/admin/agents/"+id+"/remote/open?view=terminal", ""); rec.Code != http.StatusConflict {
		t.Errorf("open while unlinked = %d, want 409", rec.Code)
	}

	// Manual link to another device.
	serveAdmin(s, "POST", "/api/admin/agents/"+id+"/remote/link", `{"node_id":"node//N2"}`)
	m = decodeJSON(t, serveAdmin(s, "GET", "/api/admin/agents/"+id+"/remote", ""))
	if l, _ := m["linked"].(map[string]any); l == nil || l["id"] != "node//N2" {
		t.Errorf("manual link = %v", m)
	}
	if rec := serveAdmin(s, "POST", "/api/admin/agents/"+id+"/remote/link", `{"node_id":"javascript:alert(1)"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid node id accepted: %d", rec.Code)
	}
}

func TestMeshInfoInstallers(t *testing.T) {
	mc := fakeMeshCentral(t, testNodes, testMeshes)
	defer mc.Close()
	s, _ := meshTestServer(t, mc, "")
	m := decodeJSON(t, serveAdmin(s, "GET", "/api/admin/mesh", ""))
	if m["group"] != "Office" || m["error"] != nil {
		t.Fatalf("mesh info = %v", m)
	}
	if !strings.HasPrefix(m["windows_url"].(string), "https://mesh.example.com/meshagents?id=4&") {
		t.Errorf("windows_url = %v, want the UI origin", m["windows_url"])
	}
	if !strings.HasPrefix(m["windows_direct_url"].(string), "https://agents.example.com:4443/meshagents?id=4&") {
		t.Errorf("windows_direct_url = %v, want the agent port", m["windows_direct_url"])
	}

	s.meshGroupName = "Nope"
	m = decodeJSON(t, serveAdmin(s, "GET", "/api/admin/mesh", ""))
	if m["error"] == nil {
		t.Errorf("unknown CB_MESH_GROUP not reported: %v", m)
	}
}

func TestRemoteWrongKeyReportsError(t *testing.T) {
	mc := fakeMeshCentral(t, testNodes, testMeshes)
	defer mc.Close()
	s, id := meshTestServer(t, mc, "")
	bad := append([]byte{}, testMeshKey...)
	bad[0] ^= 0xff
	s.mesh.Key = bad
	m := decodeJSON(t, serveAdmin(s, "GET", "/api/admin/agents/"+id+"/remote", ""))
	if e, _ := m["error"].(string); !strings.Contains(e, "CB_MESH_LOGIN_KEY") {
		t.Errorf("error = %v, want a hint about the key", m)
	}
}

func TestRemoteDisabled(t *testing.T) {
	s := testServer(t)
	m := decodeJSON(t, serveAdmin(s, "GET", "/api/admin/agents/x/remote", ""))
	if m["enabled"] != false {
		t.Errorf("remote without config = %v", m)
	}
	if rec := serveAdmin(s, "GET", "/api/admin/agents/x/remote/open?view=desktop", ""); rec.Code != http.StatusNotFound {
		t.Errorf("open without config = %d", rec.Code)
	}
}

func TestMeshConfigFromEnv(t *testing.T) {
	t.Setenv("CB_MESH_URL", "")
	t.Setenv("CB_MESH_LOGIN_KEY", "")
	if c, _, err := meshConfigFromEnv(); c != nil || err != nil {
		t.Errorf("unset: %v %v", c, err)
	}
	t.Setenv("CB_MESH_URL", "https://mesh.example.com")
	if _, _, err := meshConfigFromEnv(); err == nil {
		t.Error("URL without key accepted")
	}
	t.Setenv("CB_MESH_LOGIN_KEY", strings.Repeat("ab", 80))
	t.Setenv("CB_MESH_USER", "")
	c, _, err := meshConfigFromEnv()
	if err != nil || c.User != "admin" || c.TokenMinutes != 2 {
		t.Errorf("valid config: %+v %v", c, err)
	}
	t.Setenv("CB_MESH_URL", "http://mesh.example.com")
	if _, _, err := meshConfigFromEnv(); err == nil {
		t.Error("http:// URL accepted")
	}
}

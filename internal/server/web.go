package server

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
)

//go:embed templates static
var webFS embed.FS

// webUI serves the admin GUI: thin server-rendered shells whose data is
// loaded from the admin API by static/app.js.
type webUI struct {
	s     *Server
	pages map[string]*template.Template
}

func newWebUI(s *Server) (*webUI, error) {
	ui := &webUI{s: s, pages: map[string]*template.Template{}}
	names := []string{
		"login", "setup", "dashboard", "clients", "client",
		"job_edit", "run", "snapshot", "settings",
	}
	for _, name := range names {
		t, err := template.ParseFS(webFS, "templates/base.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		ui.pages[name] = t
	}
	return ui, nil
}

type pageData struct {
	Page string
	ID   string
}

func (ui *webUI) render(w http.ResponseWriter, name string, data pageData) {
	data.Page = name
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.pages[name].ExecuteTemplate(w, "base.html", data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// page wraps authenticated GUI pages: redirects to first-run setup when no
// admin user exists yet, and to the login page when not signed in.
func (ui *webUI) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if n, err := ui.s.store.CountUsers(); err == nil && n == 0 {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		if ui.s.sessionUser(r) == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ui.render(w, name, pageData{ID: r.PathValue("id")})
	}
}

// installScripts are the only static files the agent listener serves: the
// paste-in install commands fetch them from the agent address.
var installScripts = []string{"install-agent.sh", "install-agent.ps1"}

// registerInstallScripts serves exactly the install scripts, by exact path,
// so the agent listener exposes no other GUI asset.
func (ui *webUI) registerInstallScripts(mux *http.ServeMux) {
	for _, name := range installScripts {
		name := name
		mux.HandleFunc("GET /static/"+name, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, webFS, "static/"+name)
		})
	}
}

func (ui *webUI) register(mux *http.ServeMux) {
	static, _ := fs.Sub(webFS, "static")
	// GET /static/ is the less specific pattern, so in combined mode the
	// install-script routes registered by registerInstallScripts win for
	// those two files and this serves everything else.
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if n, err := ui.s.store.CountUsers(); err == nil && n == 0 {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		ui.render(w, "login", pageData{})
	})
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		if n, err := ui.s.store.CountUsers(); err == nil && n > 0 {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ui.render(w, "setup", pageData{})
	})

	mux.HandleFunc("GET /{$}", ui.page("dashboard"))
	mux.HandleFunc("GET /clients", ui.page("clients"))
	mux.HandleFunc("GET /clients/{id}", ui.page("client"))
	mux.HandleFunc("GET /jobs/new", ui.page("job_edit"))
	mux.HandleFunc("GET /jobs/{id}", ui.page("job_edit"))
	mux.HandleFunc("GET /runs/{id}", ui.page("run"))
	mux.HandleFunc("GET /snapshots/{id}", ui.page("snapshot"))
	mux.HandleFunc("GET /settings", ui.page("settings"))
}

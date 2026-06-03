package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:static
var staticFS embed.FS

// staticHandler serves the embedded Web UI. It strips the "/" prefix and
// falls back to index.html for SPA routing.
func (s *Server) staticHandler() http.Handler {
	// Try to open the embedded static directory. If it doesn't exist (e.g.
	// during development before the UI is built), return a no-op handler.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(indexHTMLFallback))
				return
			}
			http.NotFound(w, r)
		})
	}

	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API routes should never reach here (mux routes them first), but
		// guard defensively so a missing API route doesn't fall through to
		// the SPA fallback.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}

		// Resolve the requested file. The root and any path that doesn't map
		// to an embedded file are served the SPA entrypoint.
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" {
			if f, err := sub.Open(name); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// Serve index.html directly rather than rewriting the path to
		// "/index.html" and delegating to fileServer: http.FileServer
		// canonicalises any "/index.html" request with a 301 redirect to
		// "./", which resolves back to "/" and loops forever. ServeFileFS
		// keys its redirect off r.URL.Path (here "/" or an SPA route), so it
		// serves the file without redirecting.
		http.ServeFileFS(w, r, sub, "index.html")
	})
}

// indexHTMLFallback is a minimal placeholder served when the embedded static/
// directory is absent. It lets the server start and respond to / even before
// the Web UI assets are built.
const indexHTMLFallback = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Octo Agent</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:800px;margin:40px auto;padding:0 20px;line-height:1.6}
h1{color:#333}code{background:#f4f4f4;padding:2px 6px;border-radius:3px}
</style>
</head>
<body>
<h1>🐙 Octo Agent Server</h1>
<p>The Web UI assets are not embedded yet. Build them into <code>internal/server/static/</code> and recompile.</p>
<p>API endpoints available:</p>
<ul>
<li><code>POST /api/chat</code> — create a new session and send a message</li>
<li><code>POST /api/chat/:id/turn</code> — send a message to an existing session</li>
<li><code>GET  /api/sessions</code> — list recent sessions</li>
<li><code>GET  /api/sessions/:id</code> — get session details</li>
<li><code>GET  /api/tools</code> — list available tools</li>
<li><code>GET  /api/skills</code> — list available skills</li>
<li><code>GET  /api/health</code> — health check</li>
</ul>
</body>
</html>
`

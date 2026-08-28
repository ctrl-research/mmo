package gateway

import (
	"net/http"
	"os"
	"path"
	"strings"
)

// staticHandler serves the built client.
//
// Serving the client from the game server keeps a deployment to one binary,
// which is what the self-hosting goal asks for: no separate web server to
// configure, and no cross-origin setup, so the gateway's same-origin WebSocket
// check works without an allowlist.
func staticHandler(dir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)

		// Vite emits content-hashed asset filenames, so those are immutable and
		// safe to cache indefinitely. index.html must never be cached, or a
		// deploy leaves browsers loading an old bundle against a new protocol
		// -- which the version check would then reject at the handshake,
		// turning a stale cache into a hard failure.
		switch {
		case strings.HasPrefix(clean, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}

		// The WebAssembly module must be served with the right type or
		// instantiateStreaming refuses it, and the failure message does not
		// point at the content type.
		if strings.HasSuffix(clean, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
		}

		// Anything that is not a real file falls back to index.html. http.Dir
		// already rejects paths that escape the root, so this cannot serve
		// anything outside dir.
		if clean != "/" {
			if _, err := os.Stat(path.Join(dir, clean)); err != nil {
				http.ServeFile(w, r, path.Join(dir, "index.html"))
				return
			}
		}

		fileServer.ServeHTTP(w, r)
	})
}

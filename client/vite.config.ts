import { defineConfig } from "vite";
import { fileURLToPath, URL } from "node:url";

// The dev server proxies the game API and WebSocket to the Go server, so the
// browser sees a single origin. That matters beyond convenience: the gateway
// defaults to same-origin WebSocket checks, and developing against a split
// origin would mean either weakening that or discovering the difference only
// in production.
const SERVER = process.env.MMO_SERVER ?? "http://localhost:8080";

export default defineConfig({
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: SERVER },
      "/healthz": { target: SERVER },

      // changeOrigin stays off so the Host header keeps the browser's own
      // origin. The gateway checks that a WebSocket's Origin matches its Host,
      // which is the right default -- a browser will otherwise open a socket
      // from any page. Rewriting Host here would break that match and force
      // the server to be started with an origin allowlist just to develop
      // against it, which is the wrong direction to fix a dev-only problem.
      "/ws": { target: SERVER, ws: true },
    },
  },
  build: {
    target: "es2022",
    sourcemap: true,
  },
});

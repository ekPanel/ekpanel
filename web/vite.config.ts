import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// In development the Vite server proxies /api to the Go panel. The panel only accepts
// same-origin requests, so the proxy presents the panel's own origin.
const panel = process.env.EKPANEL_DEV_TARGET ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      "/api": {
        target: panel,
        changeOrigin: true,
        ws: true,
        configure: (proxy) => {
          proxy.on("proxyReq", (req) => req.setHeader("origin", panel));
          proxy.on("proxyReqWs", (req) => req.setHeader("origin", panel));
        },
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // Never inline assets as data: URIs; the panel's Content-Security-Policy only allows
    // fonts and scripts from its own origin.
    assetsInlineLimit: 0,
    chunkSizeWarningLimit: 800,
  },
});

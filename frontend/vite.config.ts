import tailwindcss from "@tailwindcss/vite";
import babel from "@rolldown/plugin-babel";
import { defineConfig } from "vite";
import react, { reactCompilerPreset } from "@vitejs/plugin-react";

const proxyTarget = process.env.VITE_PROXY_TARGET ?? "http://api:8080";
const wsTarget = process.env.VITE_WS_TARGET ?? "http://wsserver:8081";
const hmrHost = process.env.VITE_HMR_HOST ?? "localhost";
const hmrClientPort = Number(process.env.VITE_HMR_CLIENT_PORT ?? 5173);

export default defineConfig({
  plugins: [react(), babel({ presets: [reactCompilerPreset()] }), tailwindcss()],
  server: {
    host: "0.0.0.0",
    port: 5173,
    strictPort: true,
    watch: {
      usePolling: true,
      interval: 1000,
    },
    proxy: {
      "/healthz": {
        target: proxyTarget,
        changeOrigin: true,
      },
      "/images": {
        target: proxyTarget,
        changeOrigin: true,
      },
      "/repos": {
        target: proxyTarget,
        changeOrigin: true,
      },
      "/threads": {
        target: proxyTarget,
        changeOrigin: true,
      },
      "/stream": {
        target: wsTarget,
        changeOrigin: true,
        ws: true,
        rewrite: (path: string) => path.replace(/^\/stream/, ""),
      },
    },
    hmr: {
      host: hmrHost,
      clientPort: hmrClientPort,
    },
  },
});

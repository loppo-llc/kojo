/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const backend = process.env.KOJO_BACKEND || "localhost:8080";

// Baked into the bundle so the running frontend knows its own build
// version and can detect a stale tab after a deploy (see
// lib/versionCheck.ts). Fed from the same `git describe` value the Go
// binary is stamped with — the Makefile passes it as KOJO_VERSION. Left
// empty in `vite dev` (define resolves to ""), which suppresses the check.
const kojoVersion = process.env.KOJO_VERSION || "";

export default defineConfig({
  define: {
    __KOJO_VERSION__: JSON.stringify(kojoVersion),
  },
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      "/api": `http://${backend}`,
      "/ws": {
        target: `ws://${backend}`,
        ws: true,
      },
    },
  },
  build: {
    outDir: "dist",
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
  },
});

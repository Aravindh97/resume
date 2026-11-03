/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build lands in internal/site/static/ui, which the Go server embeds and
// serves; index.html is served at "/" (server) or copied to the site root
// (GitHub Pages); its assets/ and shots/ are served at the root next to it.
export default defineConfig({
  base: "./",
  plugins: [react()],
  build: { outDir: "../internal/site/static/ui", emptyOutDir: true },
  test: { environment: "jsdom" },
});

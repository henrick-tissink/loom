import { defineConfig } from "vite";
import { writeFileSync } from "node:fs";

export default defineConfig({
  // emptyOutDir clears stale hashed chunks, which //go:embed all:frontend/dist
  // would otherwise bake into every binary. The committed dist/.gitkeep keeps
  // the embed target present on checkouts that have not run a frontend build,
  // so it is written back after each build.
  build: { outDir: "dist", emptyOutDir: true },
  plugins: [{ name: "keep-gitkeep", closeBundle() { writeFileSync("dist/.gitkeep", ""); } }],
});

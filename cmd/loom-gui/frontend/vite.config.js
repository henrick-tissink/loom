import { defineConfig } from "vite";

export default defineConfig({
  // emptyOutDir stays false so the committed dist/.gitkeep survives builds —
  // it keeps the //go:embed target present on fresh checkouts that haven't run
  // a frontend build. dist/* is gitignored, so stale hashed chunks never reach git.
  build: { outDir: "dist", emptyOutDir: false },
});

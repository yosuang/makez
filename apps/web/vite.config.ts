import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite-plus";

export default defineConfig(({ mode }) => ({
  // Dev and preview history fallback. Cloudflare Pages does the same when dist has no 404.html.
  appType: "spa",
  server: { host: "127.0.0.1", port: 3000, strictPort: true },
  preview: { host: "127.0.0.1", port: 4173, strictPort: true },
  resolve: { tsconfigPaths: true },
  plugins: mode === "test" ? [] : [tanstackRouter({ target: "react" }), tailwindcss(), react()],
  test: { include: ["src/**/*.test.ts"] },
  fmt: { ignorePatterns: ["src/routeTree.gen.ts"] },
  lint: { ignorePatterns: ["src/routeTree.gen.ts"] },
}));

import { copyFileSync } from "node:fs";
import { resolve } from "node:path";
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite-plus";

// Unknown paths must serve the shell so a refresh on a client route still boots the router.
function spaFallback(): Plugin {
  return {
    name: "spa-fallback",
    apply: "build",
    writeBundle(options) {
      const dir = options.dir ?? resolve("dist");
      copyFileSync(resolve(dir, "index.html"), resolve(dir, "404.html"));
    },
  };
}

export default defineConfig(({ mode }) => ({
  appType: "spa",
  server: { host: "127.0.0.1", port: 3000, strictPort: true },
  preview: { host: "127.0.0.1", port: 4173, strictPort: true },
  resolve: { tsconfigPaths: true },
  plugins:
    mode === "test"
      ? []
      : [tanstackRouter({ target: "react" }), tailwindcss(), react(), spaFallback()],
  test: { include: ["src/**/*.test.ts"] },
  fmt: { ignorePatterns: ["src/routeTree.gen.ts"] },
  lint: { ignorePatterns: ["src/routeTree.gen.ts"] },
}));

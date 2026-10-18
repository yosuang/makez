import { cloudflare } from "@cloudflare/vite-plugin";
import tailwindcss from "@tailwindcss/vite";
import { tanstackStart } from "@tanstack/react-start/plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite-plus";

export default defineConfig(({ mode }) => ({
  server: { host: "127.0.0.1", port: 3000, strictPort: true },
  resolve: { tsconfigPaths: true },
  plugins:
    mode === "test"
      ? []
      : [cloudflare({ viteEnvironment: { name: "ssr" } }), tailwindcss(), tanstackStart(), react()],
  test: { include: ["src/**/*.test.ts"] },
  fmt: { ignorePatterns: ["src/routeTree.gen.ts", "worker-configuration.d.ts"] },
  lint: { ignorePatterns: ["src/routeTree.gen.ts", "worker-configuration.d.ts"] },
}));

import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite-plus";

export default defineConfig({
  // Dev and preview history fallback. Production uses assets.not_found_handling in wrangler.jsonc.
  appType: "spa",
  server: { host: "127.0.0.1", port: 3000, strictPort: true },
  preview: { host: "127.0.0.1", port: 4173, strictPort: true },
  resolve: { tsconfigPaths: true },
  plugins: [tanstackRouter({ target: "react" }), tailwindcss(), react()],
  fmt: { ignorePatterns: ["src/routeTree.gen.ts"] },
  lint: { ignorePatterns: ["src/routeTree.gen.ts"] },
});

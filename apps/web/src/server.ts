import handler from "@tanstack/react-start/server-entry";
import { proxyAPI } from "./server/proxy";

export default {
  async fetch(request: Request, env: { MAKEZ_API_ORIGIN: string }) {
    const path = new URL(request.url).pathname;
    const response = path.startsWith("/v1/")
      ? await proxyAPI(request, env.MAKEZ_API_ORIGIN)
      : path === "/healthz"
        ? new Response("ok")
        : await handler.fetch(request);
    const secured = new Response(response.body, response);
    secured.headers.set("X-Content-Type-Options", "nosniff");
    secured.headers.set("X-Frame-Options", "DENY");
    secured.headers.set("Referrer-Policy", "no-referrer");
    secured.headers.set("Cache-Control", "no-store");
    return secured;
  },
} satisfies ExportedHandler<{ MAKEZ_API_ORIGIN: string }>;

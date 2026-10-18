const routes = [
  ["GET", /^\/v1\/agents$/],
  ["GET", /^\/v1\/tasks(?:\/[\w-]+(?:\/(?:events|artifacts))?)?$/],
  ["POST", /^\/v1\/tasks(?:\/[\w-]+\/cancel)?$/],
  ["GET", /^\/v1\/artifacts\/[\w-]+\/content$/],
] as const;

export async function proxyAPI(request: Request, origin: string): Promise<Response> {
  const url = new URL(request.url);
  if (!routes.some(([method, path]) => request.method === method && path.test(url.pathname))) {
    return Response.json({ error: "接口不存在" }, { status: 404 });
  }
  if (!/^Bearer \S+$/.test(request.headers.get("Authorization") ?? "")) {
    return Response.json({ error: "请先连接工作区" }, { status: 401 });
  }
  let upstream: URL;
  try {
    upstream = new URL(origin);
    const local = ["127.0.0.1", "localhost", "[::1]"].includes(upstream.hostname);
    if (
      (upstream.protocol !== "https:" && !(local && upstream.protocol === "http:")) ||
      upstream.username ||
      upstream.password ||
      upstream.pathname !== "/" ||
      upstream.search ||
      upstream.hash ||
      upstream.origin === url.origin
    ) {
      throw new Error("Invalid API origin");
    }
  } catch {
    return Response.json({ error: "Web 服务尚未配置有效的 Go API 地址" }, { status: 503 });
  }
  upstream.pathname = url.pathname;
  upstream.search = url.search;
  const headers = new Headers();
  for (const name of ["Authorization", "Content-Type", "Idempotency-Key", "Last-Event-ID"]) {
    const value = request.headers.get(name);
    if (value !== null) headers.set(name, value);
  }
  try {
    const init = {
      method: request.method,
      headers,
      body: request.method === "POST" ? request.body : null,
      signal: request.signal,
      redirect: "manual" as const,
      duplex: "half" as const,
    };
    const response = await fetch(upstream, init);
    // Credentials must never follow an upstream redirect to another origin.
    if (response.status >= 300 && response.status < 400) {
      await response.body?.cancel();
      return Response.json({ error: "Go API 返回了意外的重定向" }, { status: 502 });
    }
    const outgoing = new Headers({
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
    });
    for (const name of [
      "Content-Type",
      "Content-Disposition",
      "Content-Length",
      "X-Accel-Buffering",
    ]) {
      const value = response.headers.get(name);
      if (value !== null) outgoing.set(name, value);
    }
    return new Response(response.body, { status: response.status, headers: outgoing });
  } catch {
    return Response.json({ error: "暂时无法连接 Go 服务，请稍后重试" }, { status: 502 });
  }
}

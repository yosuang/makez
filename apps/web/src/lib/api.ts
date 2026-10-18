export interface Agent {
  ID: string;
  Version: string;
}
export interface Task {
  id: string;
  workspace_id: string;
  prompt: string;
  agent_id: string;
  agent_version: string;
  state: string;
  reason?: string;
  dispatch_error?: string;
  created_at: string;
  deadline: string;
  cancel_requested: boolean;
  run?: { id: string; state: string; cleanup_state: string; verified_stopped: boolean };
}
export interface Artifact {
  id: string;
  path: string;
  size: number;
  sha256: string;
  media_type: string;
}
export interface RunEvent {
  seq: number;
  type: string;
  payload: unknown;
  created_at: string;
}

export const terminal = (state: string) => ["SUCCEEDED", "FAILED", "CANCELED"].includes(state);

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

export async function api(token: string, path: string, options: RequestInit = {}) {
  const headers = new Headers(options.headers);
  headers.set("Authorization", `Bearer ${token}`);
  if (options.body) headers.set("Content-Type", "application/json");
  const response = await fetch(path, { ...options, headers });
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as { error?: string };
    throw new APIError(body.error || `请求失败（${response.status}）`, response.status);
  }
  return response;
}

export function eventText(payload: unknown): string {
  if (typeof payload === "string") return payload;
  if (payload && typeof payload === "object") {
    const data = payload as Record<string, unknown>;
    for (const key of ["text", "message", "data"]) {
      if (typeof data[key] === "string") return data[key];
    }
  }
  return JSON.stringify(payload) ?? "";
}

// One parser for chunk boundaries, UTF-8, and SSE replay in both browser and tests.
export async function readEvents(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: { type: string; id: string; data: string }) => void,
) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let match: RegExpExecArray | null;
      while ((match = /\r?\n\r?\n/.exec(buffer))) {
        const message = buffer.slice(0, match.index);
        buffer = buffer.slice(match.index + match[0].length);
        let type = "message",
          id = "";
        const data: string[] = [];
        for (const line of message.split(/\r?\n/)) {
          const colon = line.indexOf(":");
          const field = colon < 0 ? line : line.slice(0, colon);
          const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
          if (field === "event") type = value;
          if (field === "id") id = value;
          if (field === "data") data.push(value);
        }
        if (data.length) onEvent({ type, id, data: data.join("\n") });
      }
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

import { expect, it } from "vite-plus/test";
import { eventText, readEvents, terminal } from "./api";

it("parses UTF-8 and CRLF frames split at every byte, ignoring comments and incomplete frames", async () => {
  const bytes = new TextEncoder().encode(
    ': heartbeat\r\nid: 9\r\nevent: log\r\ndata: {"text":"你好"}\r\n\r\nevent: status\ndata: first\ndata: second\n\ndata: unfinished',
  );
  const received: unknown[] = [];
  await readEvents(
    new ReadableStream({
      start(controller) {
        for (const byte of bytes) controller.enqueue(Uint8Array.of(byte));
        controller.close();
      },
    }),
    (event) => received.push(event),
  );
  expect(received).toEqual([
    { type: "log", id: "9", data: '{"text":"你好"}' },
    { type: "status", id: "", data: "first\nsecond" },
  ]);
});

it("keeps structured logs visible and recognizes only terminal task states", () => {
  expect(eventText({ text: "hello" })).toBe("hello");
  expect(eventText({ tool: "shell", args: ["ls"] })).toBe('{"tool":"shell","args":["ls"]}');
  expect(eventText(null)).toBe("null");
  expect(terminal("CANCELING")).toBe(false);
  expect(terminal("FAILED")).toBe(true);
});

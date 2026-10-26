import { useEffect, useState } from "react";
import {
  api,
  APIError,
  readEvents,
  terminal,
  type Artifact,
  type RunEvent,
  type Task,
} from "./api";

export function useTask(token: string, id: string) {
  const [task, setTask] = useState<Task>();
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [artifacts, setArtifacts] = useState<Artifact[]>([]);
  const [error, setError] = useState("");
  const [streamState, setStreamState] = useState("Waiting to connect");

  useEffect(() => {
    setTask(undefined);
    setEvents([]);
    setArtifacts([]);
    setError("");
    if (!token || !id) return;
    const controller = new AbortController();
    const options = { signal: controller.signal };
    let cursor = "0";
    let ended = false;
    let reconnect: ReturnType<typeof setTimeout>;
    let poll: ReturnType<typeof setTimeout>;

    async function refresh() {
      try {
        const current: Task = await (await api(token, `/v1/tasks/${id}`, options)).json();
        const files: Artifact[] = await (
          await api(token, `/v1/tasks/${id}/artifacts`, options)
        ).json();
        if (controller.signal.aborted) return;
        setTask(current);
        setArtifacts(files);
        setError("");
      } catch (err) {
        if (!controller.signal.aborted)
          setError(err instanceof Error ? err.message : "Could not load the task");
      } finally {
        if (!controller.signal.aborted) poll = setTimeout(refresh, 5000);
      }
    }

    async function observe() {
      try {
        setStreamState(cursor === "0" ? "Connecting" : "Reconnecting");
        const response = await api(token, `/v1/tasks/${id}/events`, {
          ...options,
          headers: { "Last-Event-ID": cursor },
        });
        if (!response.body) throw new Error("Event stream is unavailable");
        if (controller.signal.aborted) {
          await response.body.cancel();
          return;
        }
        setStreamState("Live");
        await readEvents(response.body, (message) => {
          if (controller.signal.aborted) return;
          if (message.type === "status") {
            const current = JSON.parse(message.data) as Task;
            setTask(current);
            ended = terminal(current.state);
          } else if (
            message.type === "log" &&
            /^\d+$/.test(message.id) &&
            BigInt(message.id) > BigInt(cursor)
          ) {
            const event = JSON.parse(message.data) as RunEvent;
            cursor = message.id;
            // shortcut: retain 1000 events in the UI, add paged history when longer sessions need it.
            setEvents((previous) => [...previous, event].slice(-1000));
          }
        });
        if (controller.signal.aborted) return;
        if (ended) {
          setStreamState("Log synced");
          const files: Artifact[] = await (
            await api(token, `/v1/tasks/${id}/artifacts`, options)
          ).json();
          if (!controller.signal.aborted) setArtifacts(files);
          return;
        }
      } catch (err) {
        if (controller.signal.aborted) return;
        if (err instanceof APIError && [401, 403, 404].includes(err.status)) {
          setStreamState("Connection unavailable");
          setError(err.message);
          return;
        }
      }
      if (!controller.signal.aborted) {
        setStreamState("Connection lost. Reconnecting");
        reconnect = setTimeout(observe, 2000);
      }
    }
    void refresh();
    void observe();
    return () => {
      controller.abort();
      clearTimeout(reconnect);
      clearTimeout(poll);
    };
  }, [token, id]);

  return { task, events, artifacts, error, streamState, setTask };
}

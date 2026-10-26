import { useEffect, useRef, useState } from "react";
import {
  AlertCircle,
  ArrowDown,
  ArrowDownToLine,
  Check,
  ChevronRight,
  CircleDot,
  File,
  Info,
  LoaderCircle,
  Play,
  Square,
} from "lucide-react";
import { Button } from "@makez/ui/components/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@makez/ui/components/collapsible";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@makez/ui/components/tabs";
import { api, eventText, terminal, type Artifact, type RunEvent, type Task } from "@/lib/api";
import {
  formatBytes,
  formatClock,
  formatTimestamp,
  formatWorked,
  message,
  stateLabel,
} from "@/lib/format";
import { StatusPill } from "@/components/status-pill";

const tabTriggerClass =
  "!flex-none bg-transparent px-2.5 shadow-none data-active:bg-transparent dark:data-active:border-transparent dark:data-active:bg-transparent";

export function TaskDetail({
  token,
  task,
  events,
  artifacts,
  error,
  streamState,
  onTask,
}: {
  token: string;
  task?: Task;
  events: RunEvent[];
  artifacts: Artifact[];
  error: string;
  streamState: string;
  onTask: (task: Task) => void;
}) {
  const [canceling, setCanceling] = useState(false);
  const [actionError, setActionError] = useState("");
  const [downloading, setDownloading] = useState("");
  const [rawOpen, setRawOpen] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const active = !task || !terminal(task.state);

  useEffect(() => {
    if (!active) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [active]);

  useEffect(() => {
    setCanceling(false);
    setActionError("");
  }, [task?.id]);

  async function cancel() {
    if (!task) return;
    setCanceling(true);
    setActionError("");
    try {
      const updated: Task = await (
        await api(token, `/v1/tasks/${task.id}/cancel`, { method: "POST" })
      ).json();
      onTask(updated);
    } catch (err) {
      setActionError(message(err));
    } finally {
      setCanceling(false);
    }
  }

  async function download(file: Artifact) {
    setDownloading(file.id);
    setActionError("");
    try {
      const blob = await (await api(token, `/v1/artifacts/${file.id}/content`)).blob();
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = file.path.split("/").pop() || "artifact";
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 5000);
    } catch (err) {
      setActionError(message(err));
    } finally {
      setDownloading("");
    }
  }

  const banner =
    actionError || error || task?.dispatch_error || (task?.state === "FAILED" ? task.reason : "");
  const lines = summaryLines(events);

  return (
    <div className="grid min-h-0 flex-1 grid-cols-1 overflow-auto lg:grid-cols-[minmax(0,1fr)_300px] lg:grid-rows-[minmax(0,1fr)] lg:overflow-hidden">
      <section className="min-w-0 px-6 py-5 lg:min-h-0 lg:overflow-auto" aria-label="Run">
        <h1 className="text-lg font-medium wrap-anywhere">{task?.prompt ?? "Loading task…"}</h1>
        {task && (
          <div className="mt-3 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
            <span className="rounded border border-border-strong px-1.5 py-0.5">
              {task.agent_id}@{task.agent_version}
            </span>
            <StatusPill state={task.state} />
            <span>
              {terminal(task.state)
                ? formatTimestamp(task.created_at)
                : `Worked for ${formatWorked(task.created_at, now)}`}
            </span>
          </div>
        )}
        {banner && (
          <p
            className="mt-4 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            role="alert"
          >
            {banner}
          </p>
        )}
        {events.length ? (
          <ol className="mt-6 space-y-3">
            {events.map((event) => (
              <Step key={event.seq} event={event} />
            ))}
          </ol>
        ) : (
          <p className="mt-6 text-sm text-muted-foreground">
            {task && terminal(task.state)
              ? "This task has no events."
              : "Waiting for the first update."}
          </p>
        )}
        <section className="mt-6 rounded-lg border border-border bg-card p-4">
          <h2 className="text-sm font-medium">Summary</h2>
          {lines.length ? (
            <ul className="mt-2 list-disc space-y-1 pl-4 text-sm text-muted-foreground">
              {lines.map((line) => (
                <li key={line} className="wrap-anywhere">
                  {line}
                </li>
              ))}
            </ul>
          ) : (
            <p className="mt-2 text-sm text-muted-foreground">No summary yet.</p>
          )}
        </section>
        <Collapsible open={rawOpen} onOpenChange={setRawOpen} className="mt-3">
          <CollapsibleTrigger className="group flex w-full items-center gap-2 rounded-lg border border-border px-3 py-2 text-left text-sm">
            <ChevronRight className="size-4 text-muted-foreground transition-transform group-data-panel-open:rotate-90" />
            Raw log
            <span className="ml-auto text-xs text-muted-foreground">{events.length}</span>
          </CollapsibleTrigger>
          <CollapsibleContent className="mt-2 max-h-80 overflow-auto rounded-lg border border-border px-3 py-2">
            <LogLines events={events} />
          </CollapsibleContent>
        </Collapsible>
      </section>
      <aside
        className="flex flex-col border-t border-border bg-surface lg:min-h-0 lg:overflow-hidden lg:border-t-0 lg:border-l"
        aria-label="Task details"
      >
        <Tabs defaultValue="artifacts" className="flex w-full flex-col gap-0 lg:min-h-0 lg:flex-1">
          <TabsList
            variant="line"
            className="h-10 w-full justify-start gap-1 rounded-none border-b bg-transparent px-2"
          >
            <TabsTrigger value="artifacts" className={tabTriggerClass}>
              Artifacts
            </TabsTrigger>
            <TabsTrigger value="log" className={tabTriggerClass}>
              Log
            </TabsTrigger>
            <TabsTrigger value="details" className={tabTriggerClass}>
              Details
            </TabsTrigger>
          </TabsList>
          <TabsContent value="artifacts" className="overflow-auto p-3 lg:min-h-0 lg:flex-1">
            {artifacts.length ? (
              <div className="space-y-2">
                {artifacts.map((file) => (
                  <div
                    key={file.id}
                    className="flex items-center gap-2 rounded-md border border-border px-2 py-2"
                  >
                    <File className="size-4 shrink-0 text-muted-foreground" />
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-sm" title={file.path}>
                        {file.path}
                      </p>
                      <p className="text-xs text-text-faint">{formatBytes(file.size)}</p>
                    </div>
                    <Button
                      size="icon-sm"
                      variant="ghost"
                      aria-label={`Download ${file.path}`}
                      disabled={Boolean(downloading)}
                      onClick={() => void download(file)}
                    >
                      {downloading === file.id ? (
                        <LoaderCircle className="animate-spin" />
                      ) : (
                        <ArrowDownToLine />
                      )}
                    </Button>
                  </div>
                ))}
              </div>
            ) : (
              <p className="px-1 py-6 text-center text-sm text-muted-foreground">
                {task && terminal(task.state)
                  ? "No archived artifacts."
                  : "Artifacts show up here after the task is archived."}
              </p>
            )}
          </TabsContent>
          <TabsContent value="log" className="flex flex-col overflow-hidden lg:min-h-0 lg:flex-1">
            <LogPanel events={events} streamState={streamState} />
          </TabsContent>
          <TabsContent value="details" className="overflow-auto p-3 lg:min-h-0 lg:flex-1">
            {task ? (
              <dl className="space-y-3 text-sm">
                <Detail label="Task ID" value={task.id} mono />
                <Detail label="Agent" value={`${task.agent_id}@${task.agent_version}`} mono />
                <Detail label="Created" value={formatTimestamp(task.created_at)} />
                <Detail label="Deadline" value={formatTimestamp(task.deadline)} />
                <Detail label="Stage" value={stateLabel(task.run?.state)} />
                <Detail label="Cleanup" value={stateLabel(task.run?.cleanup_state)} />
                <Detail label="Stream" value={streamState} />
                {task.reason && <Detail label="Reason" value={task.reason} />}
              </dl>
            ) : (
              <p className="text-sm text-muted-foreground">Loading task…</p>
            )}
          </TabsContent>
        </Tabs>
        <div className="shrink-0 border-t border-border p-3">
          <p className="text-xs text-muted-foreground">
            Task {stateLabel(task?.state)} / Stage {stateLabel(task?.run?.state)}
          </p>
          {task && !terminal(task.state) && (
            <Button
              variant="outline"
              className="mt-2 w-full"
              disabled={canceling || task.cancel_requested}
              onClick={() => void cancel()}
            >
              {canceling ? <LoaderCircle className="animate-spin" /> : <Square />}
              {task.cancel_requested ? "Canceling…" : "Cancel"}
            </Button>
          )}
        </div>
      </aside>
    </div>
  );
}

function Detail({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="text-xs text-text-faint">{label}</dt>
      <dd className={mono ? "mt-0.5 font-mono text-xs wrap-anywhere" : "mt-0.5 wrap-anywhere"}>
        {value}
      </dd>
    </div>
  );
}

function Step({ event }: { event: RunEvent }) {
  const step = stepOf(event);
  const Icon =
    step.tone === "error"
      ? AlertCircle
      : step.tone === "start"
        ? Play
        : step.tone === "done"
          ? Check
          : step.label === "QUEUED"
            ? CircleDot
            : Info;
  return (
    <li className="flex gap-3">
      <span className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full border border-border bg-muted text-muted-foreground">
        <Icon className="size-3.5" />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-baseline gap-2">
          <span className="text-[11px] font-medium tracking-wide text-muted-foreground">
            {step.label}
          </span>
          <time className="ml-auto shrink-0 text-[11px] text-text-faint">
            {formatClock(event.created_at)}
          </time>
        </div>
        <p className="text-sm">{step.title}</p>
        {step.body && (
          <p className="mt-0.5 line-clamp-3 text-xs wrap-anywhere whitespace-pre-wrap text-muted-foreground">
            {step.body}
          </p>
        )}
      </div>
    </li>
  );
}

function LogPanel({ events, streamState }: { events: RunEvent[]; streamState: string }) {
  const logs = useRef<HTMLDivElement>(null);
  const follow = useRef(true);
  const [autoScroll, setAutoScroll] = useState(true);

  useEffect(() => {
    if (follow.current && logs.current) logs.current.scrollTop = logs.current.scrollHeight;
  }, [events]);

  return (
    <>
      <div className="flex items-center justify-between px-3 py-2 text-xs text-text-faint">
        <span>{streamState}</span>
        <span>
          {events.length >= 1000 ? "Showing the latest 1,000 entries" : `${events.length} entries`}
        </span>
      </div>
      <div
        ref={logs}
        className="max-h-80 overflow-auto px-3 pb-3 lg:max-h-none lg:min-h-0 lg:flex-1"
        role="log"
        aria-label="Run log"
        tabIndex={0}
        onScroll={() => {
          const element = logs.current;
          if (!element) return;
          follow.current = element.scrollHeight - element.scrollTop - element.clientHeight < 60;
          setAutoScroll(follow.current);
        }}
      >
        <LogLines events={events} />
      </div>
      <div className="flex items-center justify-between gap-2 border-t border-border px-3 py-2 text-xs text-text-faint">
        <span>Leaving this page does not stop the task</span>
        {!autoScroll && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              follow.current = true;
              setAutoScroll(true);
              if (logs.current) logs.current.scrollTop = logs.current.scrollHeight;
            }}
          >
            <ArrowDown />
            Jump to latest
          </Button>
        )}
      </div>
    </>
  );
}

function LogLines({ events }: { events: RunEvent[] }) {
  if (!events.length) {
    return <p className="text-sm text-muted-foreground">No log entries yet.</p>;
  }
  return (
    <div className="space-y-2 font-mono text-xs">
      {events.map((event) => (
        <div key={event.seq} className="flex gap-3">
          <time className="shrink-0 text-text-faint">{formatClock(event.created_at)}</time>
          <pre className="min-w-0 wrap-anywhere whitespace-pre-wrap text-text-secondary">
            {eventText(event.payload)}
          </pre>
        </div>
      ))}
    </div>
  );
}

function summaryLines(events: RunEvent[]) {
  const lines: string[] = [];
  for (const event of events) {
    if (!isRecord(event.payload) || typeof event.payload.summary !== "string") continue;
    for (const line of event.payload.summary.split(/\r?\n/)) {
      const trimmed = line.replace(/^[-*]\s*/, "").trim();
      if (trimmed && !lines.includes(trimmed)) lines.push(trimmed);
    }
  }
  return lines;
}

function stepOf(event: RunEvent) {
  const record = isRecord(event.payload) ? event.payload : undefined;
  const embedded = typeof record?.type === "string" ? record.type : "";
  const raw = eventText(event.payload).trim();
  const source = `${event.type} ${embedded}`.toLowerCase();
  let label = "INFO";
  let tone: "info" | "error" | "start" | "done" = "info";
  if (/fail|error/.test(source)) {
    label = "ERROR";
    tone = "error";
  } else if (/queue/.test(source)) {
    label = "QUEUED";
  } else if (/start|prepar/.test(source)) {
    label = "STARTED";
    tone = "start";
  } else if (event.type === "usage" || /complete|succeed/.test(source)) {
    label = "DONE";
    tone = "done";
  } else if (event.type === "status") {
    label = "STATUS";
  }
  const title = embedded ? humanize(embedded) : label === "INFO" ? "Log" : label;
  const body = raw && raw !== title ? raw : "";
  return { label, tone, title, body };
}

function humanize(type: string) {
  const known: Record<string, string> = {
    "thread.started": "Thread started",
    "turn.started": "Turn started",
    "turn.completed": "Turn completed",
    "turn.failed": "Turn failed",
    "makez.error": "Agent error",
    error: "Error",
  };
  return known[type] ?? type.replace(/[._]/g, " ").replace(/\b\w/g, (char) => char.toUpperCase());
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

import { useEffect, useState } from "react";
import { Plus, Search, Settings, UserRound } from "lucide-react";
import { Button } from "@makez/ui/components/button";
import { Input } from "@makez/ui/components/input";
import { cn } from "@makez/ui/lib/utils";
import { terminal, type Task } from "@/lib/api";
import { formatAgo } from "@/lib/format";

export function TaskSidebar({
  tasks,
  active,
  search,
  onSearch,
  onSelect,
  connected,
  savedKey,
  loading,
  error,
  open,
  onClose,
  onOpenSettings,
}: {
  tasks: Task[];
  active: string;
  search: string;
  onSearch: (value: string) => void;
  onSelect: (id: string) => void;
  connected: boolean;
  savedKey: boolean;
  loading: boolean;
  error: string;
  open: boolean;
  onClose: () => void;
  onOpenSettings: () => void;
}) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, []);

  const filtered = tasks.filter(
    (item) => item.prompt.toLowerCase().includes(search.toLowerCase()) || item.id.includes(search),
  );
  const working = filtered.filter((item) => !terminal(item.state));
  const settled = filtered.filter((item) => terminal(item.state));

  return (
    <>
      {open && (
        <button
          className="fixed inset-0 z-20 bg-black/50 md:hidden"
          aria-label="Close task list"
          onClick={onClose}
        />
      )}
      <aside
        className={cn(
          "z-30 flex w-[260px] shrink-0 flex-col border-r border-border bg-surface",
          "max-md:fixed max-md:inset-y-0 max-md:left-0",
          open ? "max-md:translate-x-0" : "max-md:invisible max-md:-translate-x-full",
        )}
        aria-label="Tasks"
      >
        <div className="flex items-center gap-2 px-3 pt-3">
          <img src="/favicon.svg" width="22" height="22" alt="" />
          <span className="text-sm font-medium">makez</span>
        </div>
        <div className="px-2 pt-3">
          <div className="relative">
            <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-text-faint" />
            <Input
              aria-label="Search tasks"
              placeholder="Search"
              value={search}
              onChange={(event) => onSearch(event.target.value)}
              className="h-8 border-transparent bg-transparent pl-8 shadow-none"
            />
          </div>
        </div>
        <div className="px-2 pt-2">
          <Button
            variant="outline"
            className="h-8 w-full justify-start bg-surface-hover"
            onClick={() => onSelect("")}
          >
            <Plus />
            New task
          </Button>
        </div>
        <nav className="min-h-0 flex-1 overflow-auto px-2 pt-2" aria-label="Tasks">
          {loading ? (
            <p className="px-2 py-3 text-xs text-muted-foreground">Loading tasks…</p>
          ) : !connected ? (
            <p className="px-2 py-3 text-xs text-muted-foreground">
              {savedKey
                ? "The saved API key was rejected. Update it in Settings."
                : "Add a workspace API key in Settings."}
            </p>
          ) : !filtered.length ? (
            <p className="px-2 py-3 text-xs text-muted-foreground">
              {search ? "No matching tasks" : "No tasks yet."}
            </p>
          ) : (
            <>
              <TaskSection
                title="Working"
                count={working.length}
                tasks={working}
                active={active}
                now={now}
                onSelect={onSelect}
              />
              <TaskSection
                title="Settled"
                count={settled.length}
                tasks={settled}
                active={active}
                now={now}
                onSelect={onSelect}
              />
            </>
          )}
          {error && (
            <p className="px-2 py-2 text-xs text-destructive" role="alert">
              {error}
            </p>
          )}
        </nav>
        <div className="flex items-center gap-2 border-t border-border px-2 py-2">
          <div className="flex min-w-0 flex-1 items-center gap-2 px-1">
            <span className="flex size-7 shrink-0 items-center justify-center rounded-full border border-border bg-muted text-muted-foreground">
              <UserRound className="size-3.5" />
            </span>
            <span className="truncate text-sm">User</span>
          </div>
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="Settings"
            title="Settings"
            onClick={onOpenSettings}
          >
            <Settings />
          </Button>
        </div>
      </aside>
    </>
  );
}

function TaskSection({
  title,
  count,
  tasks,
  active,
  now,
  onSelect,
}: {
  title: string;
  count: number;
  tasks: Task[];
  active: string;
  now: number;
  onSelect: (id: string) => void;
}) {
  return (
    <section>
      <div className="flex items-center justify-between px-2 pt-3 pb-1 text-[11px] text-text-faint">
        <span>{title}</span>
        <span>{count}</span>
      </div>
      {tasks.map((item) => (
        <button
          key={item.id}
          className={cn(
            "flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left",
            active === item.id ? "bg-surface-active" : "hover:bg-surface-hover",
          )}
          aria-current={active === item.id ? "page" : undefined}
          onClick={() => onSelect(item.id)}
        >
          <span className="min-w-0 flex-1">
            <span className="block truncate text-[13px] text-text-secondary">{item.prompt}</span>
            <span className="mt-0.5 flex items-center justify-between gap-2 text-[11px] text-text-faint">
              <span className="truncate">{item.agent_id}</span>
              <span className="shrink-0">{formatAgo(item.created_at, now)}</span>
            </span>
          </span>
          <span className={cn("mt-1.5 size-1.5 shrink-0 rounded-full", dotClass(item.state))} />
        </button>
      ))}
    </section>
  );
}

function dotClass(state: string) {
  if (state === "FAILED" || state === "BLOCKED") return "bg-destructive";
  if (
    state === "RUNNING" ||
    state === "PREPARING" ||
    state === "STARTING" ||
    state === "STOPPING" ||
    state === "FINALIZING" ||
    state === "CANCELING"
  ) {
    return "bg-muted-foreground";
  }
  if (state === "SUCCEEDED" || state === "DONE") return "bg-foreground";
  return "bg-text-faint";
}

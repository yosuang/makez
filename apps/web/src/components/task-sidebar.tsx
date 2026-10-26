import { useEffect, useState } from "react";
import { PanelLeft, Plus, Search, Settings, UserRound } from "lucide-react";
import { Button } from "@makez/ui/components/button";
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@makez/ui/components/command";
import { cn } from "@makez/ui/lib/utils";
import { terminal, type Task } from "@/lib/api";
import { formatAgo } from "@/lib/format";

export function TaskSidebar({
  tasks,
  active,
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
  const [collapsed, setCollapsed] = useState(false);
  const [searchOpen, setSearchOpen] = useState(false);
  const [query, setQuery] = useState("");

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setQuery("");
        setSearchOpen((current) => !current);
      }
    }
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, []);

  const working = tasks.filter((item) => !terminal(item.state));
  const settled = tasks.filter((item) => terminal(item.state));

  function openSearch() {
    setQuery("");
    setSearchOpen(true);
  }

  function choose(id: string) {
    setSearchOpen(false);
    onSelect(id);
  }

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
          collapsed && "md:w-12",
          open ? "max-md:translate-x-0" : "max-md:invisible max-md:-translate-x-full",
        )}
        aria-label="Tasks"
      >
        <div
          className={cn(
            "flex h-12 items-center gap-0.5 px-1.5",
            collapsed && "md:h-auto md:flex-col md:items-center md:gap-1 md:px-1 md:pt-2",
          )}
        >
          <img
            src="/favicon.svg"
            width="22"
            height="22"
            alt=""
            className={cn("size-[22px] shrink-0", collapsed ? "md:my-1" : "ml-1.5")}
          />
          <div
            className={cn(
              "flex items-center",
              collapsed ? "md:flex-col md:items-center md:gap-1" : "ml-auto",
            )}
          >
            <Button
              variant="ghost"
              size="icon-sm"
              className="max-md:hidden"
              aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
              aria-expanded={!collapsed}
              title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
              onClick={() => setCollapsed((value) => !value)}
            >
              <PanelLeft />
            </Button>
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label="Search tasks"
              title="Search tasks"
              onClick={openSearch}
            >
              <Search />
            </Button>
          </div>
        </div>
        <div className={cn("px-2 pt-2", collapsed && "md:flex md:justify-center md:px-1")}>
          <Button
            variant="outline"
            className={cn(
              "h-8 w-full justify-start bg-surface-hover",
              collapsed && "md:w-8 md:justify-center md:px-0",
            )}
            aria-label="New task"
            onClick={() => onSelect("")}
          >
            <Plus />
            <span className={cn(collapsed && "md:sr-only")}>New task</span>
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-auto">
          <nav className={cn("px-2 pt-2", collapsed && "md:hidden")} aria-label="Tasks">
            {loading ? (
              <p className="px-2 py-3 text-xs text-muted-foreground">Loading tasks…</p>
            ) : !connected ? (
              <p className="px-2 py-3 text-xs text-muted-foreground">
                {savedKey
                  ? "The saved API key was rejected. Update it in Settings."
                  : "Add a workspace API key in Settings."}
              </p>
            ) : !tasks.length ? (
              <p className="px-2 py-3 text-xs text-muted-foreground">No tasks yet.</p>
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
        </div>
        <div
          className={cn(
            "flex items-center gap-2 border-t border-border px-2 py-2",
            collapsed && "md:flex-col md:gap-1 md:px-1",
          )}
        >
          <div
            className={cn(
              "flex min-w-0 flex-1 items-center gap-2 px-1",
              collapsed && "md:flex-none md:justify-center md:px-0",
            )}
          >
            <span className="flex size-7 shrink-0 items-center justify-center rounded-full border border-border bg-muted text-muted-foreground">
              <UserRound className="size-3.5" />
            </span>
            <span className={cn("truncate text-sm", collapsed && "md:sr-only")}>User</span>
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
      <CommandDialog
        open={searchOpen}
        onOpenChange={(next) => {
          if (next) setQuery("");
          setSearchOpen(next);
        }}
        title="Search tasks"
        description="Search tasks and jump to one."
      >
        <CommandInput
          autoFocus
          placeholder="Search tasks…"
          value={query}
          onValueChange={setQuery}
          aria-label="Search tasks"
        />
        <CommandList>
          <CommandEmpty>No matching tasks</CommandEmpty>
          <CommandGroup heading="Actions">
            <CommandItem value="new task" onSelect={() => choose("")}>
              <Plus />
              New task
            </CommandItem>
          </CommandGroup>
          {working.length > 0 && (
            <CommandGroup heading="Working">
              {working.map((item) => (
                <TaskCommandItem key={item.id} task={item} now={now} onSelect={choose} />
              ))}
            </CommandGroup>
          )}
          {settled.length > 0 && (
            <CommandGroup heading="Settled">
              {settled.map((item) => (
                <TaskCommandItem key={item.id} task={item} now={now} onSelect={choose} />
              ))}
            </CommandGroup>
          )}
        </CommandList>
      </CommandDialog>
    </>
  );
}

function TaskCommandItem({
  task,
  now,
  onSelect,
}: {
  task: Task;
  now: number;
  onSelect: (id: string) => void;
}) {
  return (
    <CommandItem
      value={task.id}
      keywords={[task.prompt, task.agent_id]}
      onSelect={() => onSelect(task.id)}
    >
      <span className={cn("size-1.5 shrink-0 rounded-full", dotClass(task.state))} />
      <span className="min-w-0 flex-1 truncate">{task.prompt}</span>
      <span className="max-w-28 shrink-0 truncate text-xs text-muted-foreground">
        {task.agent_id}
      </span>
      <span className="w-8 shrink-0 text-right text-xs text-muted-foreground">
        {formatAgo(task.created_at, now)}
      </span>
    </CommandItem>
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

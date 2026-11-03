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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@makez/ui/components/dropdown-menu";
import { cn } from "@makez/ui/lib/utils";
import { terminal, type Task } from "@/lib/api";
import markSvg from "@/assets/mark.svg?raw";

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
  accountName,
  plan,
  onSignOut,
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
  accountName: string;
  plan: string;
  onSignOut: (all: boolean) => void;
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

  const groups = groupTasks(tasks, now);

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
          type="button"
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
          <Mark className={cn("size-[22px] shrink-0", collapsed ? "md:my-1" : "ml-1.5")} />
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
            className={cn(
              "w-full justify-start",
              collapsed && "md:size-8 md:justify-center md:px-0",
            )}
            aria-label="New task"
            onClick={() => onSelect("")}
          >
            <Plus data-icon="inline-start" />
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
              groups.map((group) => (
                <TaskSection
                  key={group.title}
                  title={group.title}
                  tasks={group.tasks}
                  active={active}
                  onSelect={onSelect}
                />
              ))
            )}
            {error && (
              <p className="px-2 py-2 text-xs text-destructive" role="alert">
                {error}
              </p>
            )}
          </nav>
        </div>
        <div className={cn("border-t border-border p-2", collapsed && "md:px-1")}>
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  variant="ghost"
                  aria-label="Account"
                  className={cn(
                    "h-auto w-full justify-start px-2 py-1.5",
                    collapsed && "md:size-8 md:justify-center md:px-0",
                  )}
                />
              }
            >
              <span className="flex size-7 shrink-0 items-center justify-center rounded-full border border-border bg-muted text-muted-foreground">
                <UserRound />
              </span>
              <span className={cn("min-w-0 flex-1 text-left", collapsed && "md:sr-only")}>
                <span className="block truncate text-sm">{accountName}</span>
                {plan && (
                  <span className="block truncate text-xs text-muted-foreground">{plan}</span>
                )}
              </span>
            </DropdownMenuTrigger>
            <DropdownMenuContent side="top" align="start" className="min-w-56">
              <DropdownMenuGroup>
                <DropdownMenuItem onClick={onOpenSettings}>
                  <Settings />
                  Settings
                </DropdownMenuItem>
              </DropdownMenuGroup>
              <DropdownMenuSeparator />
              <DropdownMenuGroup>
                <DropdownMenuItem onClick={() => onSignOut(false)}>Sign out</DropdownMenuItem>
                <DropdownMenuItem onClick={() => onSignOut(true)}>Sign out all</DropdownMenuItem>
              </DropdownMenuGroup>
            </DropdownMenuContent>
          </DropdownMenu>
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
          {groups.map((group) => (
            <CommandGroup key={group.title} heading={group.title}>
              {group.tasks.map((item) => (
                <TaskCommandItem key={item.id} task={item} onSelect={choose} />
              ))}
            </CommandGroup>
          ))}
        </CommandList>
      </CommandDialog>
    </>
  );
}

function TaskCommandItem({ task, onSelect }: { task: Task; onSelect: (id: string) => void }) {
  const running = !terminal(task.state);
  return (
    <CommandItem
      value={task.id}
      keywords={[task.prompt, task.agent_id]}
      onSelect={() => onSelect(task.id)}
    >
      <span className="min-w-0 flex-1 truncate">{task.prompt}</span>
      {running && <RunningDot />}
    </CommandItem>
  );
}

function TaskSection({
  title,
  tasks,
  active,
  onSelect,
}: {
  title: string;
  tasks: Task[];
  active: string;
  onSelect: (id: string) => void;
}) {
  return (
    <section>
      <h2 className="px-2 pt-3 pb-1 text-[11px] text-text-faint">{title}</h2>
      {tasks.map((item) => {
        const running = !terminal(item.state);
        return (
          <button
            key={item.id}
            type="button"
            className={cn(
              "flex h-8 w-full items-center gap-2 rounded-md px-2 text-left",
              active === item.id ? "bg-surface-active" : "hover:bg-surface-hover",
            )}
            aria-current={active === item.id ? "page" : undefined}
            title={item.prompt}
            onClick={() => onSelect(item.id)}
          >
            <span className="min-w-0 flex-1 truncate text-[13px] text-text-secondary">
              {item.prompt}
            </span>
            {running && <RunningDot />}
          </button>
        );
      })}
    </section>
  );
}

function RunningDot() {
  return (
    <span className="size-1.5 shrink-0 rounded-full bg-foreground" title="In progress">
      <span className="sr-only">In progress</span>
    </span>
  );
}

function groupTasks(tasks: Task[], now: number) {
  const start = new Date(now);
  start.setHours(0, 0, 0, 0);
  const todayStart = start.getTime();
  const weekStart = todayStart - 6 * 24 * 60 * 60 * 1000;
  const groups = [
    { title: "Today", tasks: [] as Task[] },
    { title: "Last 7 days", tasks: [] as Task[] },
    { title: "Older", tasks: [] as Task[] },
  ];
  const sorted = [...tasks].sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
  for (const task of sorted) {
    const created = Date.parse(task.created_at);
    if (created >= todayStart) groups[0].tasks.push(task);
    else if (created >= weekStart) groups[1].tasks.push(task);
    else groups[2].tasks.push(task);
  }
  return groups.filter((group) => group.tasks.length > 0);
}

function Mark({ className }: { className?: string }) {
  return (
    <span
      aria-hidden="true"
      className={cn("inline-block text-foreground [&_svg]:block [&_svg]:size-full", className)}
      dangerouslySetInnerHTML={{ __html: markSvg }}
    />
  );
}

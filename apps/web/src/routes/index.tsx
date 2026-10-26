import { createFileRoute } from "@tanstack/react-router";
import { useCallback, useEffect, useRef, useState } from "react";
import { Menu } from "lucide-react";
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@makez/ui/components/breadcrumb";
import { Button } from "@makez/ui/components/button";
import { api, isAbort, readToken, writeToken, type Agent, type Task } from "@/lib/api";
import { message } from "@/lib/format";
import { useTask } from "@/lib/use-task";
import { NewTask } from "@/components/new-task";
import { SettingsDialog, type SettingsPage } from "@/components/settings-dialog";
import { TaskDetail } from "@/components/task-detail";
import { TaskSidebar } from "@/components/task-sidebar";

export const Route = createFileRoute("/")({ component: Console });

function Console() {
  const storedToken = useRef(readToken());
  const [savedToken, setSavedToken] = useState(storedToken.current);
  const [token, setToken] = useState("");
  const [agents, setAgents] = useState<Agent[]>([]);
  const [connectError, setConnectError] = useState("");
  const [booting, setBooting] = useState(Boolean(storedToken.current.trim()));
  const [draft, setDraft] = useState("");
  const [tasks, setTasks] = useState<Task[]>([]);
  const [active, setActive] = useState("");
  const [search, setSearch] = useState("");
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [listError, setListError] = useState("");
  const [loading, setLoading] = useState(Boolean(storedToken.current.trim()));
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [settingsPage, setSettingsPage] = useState<SettingsPage>("appearance");
  const connection = useRef<AbortController | null>(null);
  const previousToken = useRef("");

  const connect = useCallback(async (next: string) => {
    connection.current?.abort();
    const controller = new AbortController();
    connection.current = controller;
    try {
      const list: Agent[] | null = await (
        await api(next, "/v1/agents", { signal: controller.signal })
      ).json();
      if (controller.signal.aborted) throw new DOMException("Aborted", "AbortError");
      writeToken(next);
      setSavedToken(next);
      setToken(next);
      setAgents(list ?? []);
      setConnectError("");
    } catch (err) {
      if (controller.signal.aborted || isAbort(err)) {
        throw new DOMException("Aborted", "AbortError");
      }
      throw err;
    }
  }, []);

  useEffect(() => {
    const initial = storedToken.current.trim();
    if (!initial) return;
    let cancelled = false;
    void (async () => {
      try {
        await connect(initial);
      } catch (err) {
        if (!cancelled && !isAbort(err)) setConnectError(message(err));
      } finally {
        if (!cancelled) setBooting(false);
      }
    })();
    return () => {
      cancelled = true;
      connection.current?.abort();
    };
  }, [connect]);

  useEffect(() => {
    if (previousToken.current && previousToken.current !== token) {
      setActive("");
      setTasks([]);
      setListError("");
    }
    previousToken.current = token;
  }, [token]);

  useEffect(() => {
    if (!token) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function refresh() {
      try {
        const data: Task[] = await (
          await api(token, "/v1/tasks", { signal: controller.signal })
        ).json();
        if (!controller.signal.aborted) {
          setTasks(data);
          setListError("");
        }
      } catch (err) {
        if (!controller.signal.aborted) setListError(message(err));
      } finally {
        if (!controller.signal.aborted) {
          setLoading(false);
          timer = setTimeout(refresh, 5000);
        }
      }
    }
    void refresh();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [token]);

  const { task, events, artifacts, error, streamState, setTask } = useTask(token, active);

  function select(id: string) {
    setActive(id);
    setSidebarOpen(false);
  }

  function openSettings(page: SettingsPage) {
    setSettingsPage(page);
    setSettingsOpen(true);
    setSidebarOpen(false);
  }

  async function saveKey(next: string) {
    if (!next) {
      connection.current?.abort();
      writeToken("");
      setSavedToken("");
      setToken("");
      setAgents([]);
      setConnectError("");
      setTasks([]);
      setActive("");
      setLoading(false);
      return;
    }
    await connect(next);
    setLoading(true);
  }

  const current = task ?? tasks.find((item) => item.id === active);

  return (
    <div className="flex h-dvh min-h-[500px] overflow-hidden bg-background text-foreground">
      <TaskSidebar
        tasks={tasks}
        active={active}
        search={search}
        onSearch={setSearch}
        onSelect={select}
        connected={Boolean(token)}
        savedKey={Boolean(savedToken.trim())}
        loading={booting || (Boolean(token) && loading)}
        error={listError}
        open={sidebarOpen}
        onClose={() => setSidebarOpen(false)}
        onOpenSettings={() => openSettings("appearance")}
      />
      <main className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-12 shrink-0 items-center gap-2 border-b border-border px-3 md:px-4">
          <Button
            variant="ghost"
            size="icon-sm"
            className="md:hidden"
            aria-label="Open task list"
            onClick={() => setSidebarOpen(true)}
          >
            <Menu />
          </Button>
          <Breadcrumb>
            <BreadcrumbList>
              <BreadcrumbItem>
                <span className="text-muted-foreground">Workspace</span>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{active ? "Task" : "New task"}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        </header>
        {active ? (
          <TaskDetail
            key={active}
            token={token}
            task={current}
            events={events}
            artifacts={artifacts}
            error={error}
            streamState={streamState}
            onTask={(updated) => {
              setTask(updated);
              setTasks((previous) =>
                previous.map((item) => (item.id === updated.id ? { ...item, ...updated } : item)),
              );
            }}
          />
        ) : (
          <NewTask
            token={token}
            agents={agents}
            draft={draft}
            onDraftChange={setDraft}
            connectError={connectError}
            onAccess={() => openSettings("advanced")}
            onCreated={(created) => {
              setDraft("");
              setTasks((previous) => [
                created,
                ...previous.filter((item) => item.id !== created.id),
              ]);
              select(created.id);
            }}
          />
        )}
      </main>
      <SettingsDialog
        open={settingsOpen}
        page={settingsPage}
        savedToken={savedToken}
        onOpenChange={setSettingsOpen}
        onPageChange={setSettingsPage}
        onSaveKey={saveKey}
      />
    </div>
  );
}

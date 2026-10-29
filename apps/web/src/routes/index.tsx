import { createFileRoute } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { Menu } from "lucide-react";
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@makez/ui/components/breadcrumb";
import { Button } from "@makez/ui/components/button";
import {
  api,
  isAbort,
  signIn,
  logout,
  sessionProfile,
  selectWorkspace,
  onSessionChange,
  sessionScope,
  currentWorkspace,
  canWrite,
  type User,
  type Workspace,
  type Agent,
  type Task,
} from "@/lib/api";
import { message } from "@/lib/format";
import { useTask } from "@/lib/use-task";
import { NewTask } from "@/components/new-task";
import { SettingsDialog } from "@/components/settings-dialog";
import { TaskDetail } from "@/components/task-detail";
import { TaskSidebar } from "@/components/task-sidebar";

export const Route = createFileRoute("/")({ component: Console });

function Console() {
  const [token, setToken] = useState("");
  const [user, setUser] = useState<User>();
  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const [workspaceID, setWorkspaceID] = useState("");
  const [agents, setAgents] = useState<Agent[]>([]);
  const [connectError, setConnectError] = useState("");
  const [booting, setBooting] = useState(true);
  const [draft, setDraft] = useState("");
  const [tasks, setTasks] = useState<Task[]>([]);
  const [active, setActive] = useState("");
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [listError, setListError] = useState("");
  const [loading, setLoading] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);

  useEffect(
    () =>
      onSessionChange(() => {
        setToken(sessionScope());
        setWorkspaceID(currentWorkspace()?.id ?? "");
        setActive("");
        setTasks([]);
        setAgents([]);
        setListError("");
        if (!sessionScope()) {
          setUser(undefined);
          setWorkspaces([]);
        }
      }),
    [],
  );

  useEffect(() => {
    let cancelled = false;
    void sessionProfile()
      .then(({ user, workspaces }) => {
        if (cancelled) return;
        setUser(user);
        setWorkspaces(workspaces);
        if (workspaces[0]) {
          selectWorkspace(workspaces[0]);
          setWorkspaceID(workspaces[0].id);
        }
      })
      .catch((err) => {
        if (!cancelled) setConnectError(message(err));
      })
      .finally(() => {
        if (!cancelled) setBooting(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!token) return;
    const controller = new AbortController();
    void api(token, "/v1/agents", { signal: controller.signal })
      .then((r) => r.json())
      .then((list: Agent[]) => {
        if (!controller.signal.aborted) setAgents(list ?? []);
      })
      .catch((err) => {
        if (!isAbort(err) && !controller.signal.aborted) setConnectError(message(err));
      });
    return () => controller.abort();
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

  useEffect(() => {
    if (!token) return;
    let cancelled = false;
    const timer = setInterval(() => {
      void sessionProfile()
        .then(({ user, workspaces }) => {
          if (cancelled) return;
          setUser(user);
          setWorkspaces(workspaces);
          const current = currentWorkspace();
          const next = workspaces.find((w) => w.id === current?.id) ?? workspaces[0];
          if (
            next &&
            (next.id !== current?.id ||
              next.role !== current.role ||
              next.authz_version !== current.authz_version)
          )
            selectWorkspace(next);
        })
        .catch((err) => {
          if (!cancelled) setConnectError(message(err));
        });
    }, 5000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [token]);

  const { task, events, artifacts, error, streamState, setTask } = useTask(token, active);

  function select(id: string) {
    setActive(id);
    setSidebarOpen(false);
  }

  async function signOut(all: boolean) {
    try {
      await logout(all);
    } catch (err) {
      setConnectError(message(err));
    }
  }

  const current = task ?? tasks.find((item) => item.id === active);

  return (
    <div className="flex h-dvh min-h-[500px] overflow-hidden bg-background text-foreground">
      <TaskSidebar
        tasks={tasks}
        active={active}
        onSelect={select}
        connected={Boolean(token)}
        savedKey={Boolean(user)}
        loading={booting || (Boolean(token) && loading)}
        error={listError}
        open={sidebarOpen}
        onClose={() => setSidebarOpen(false)}
        onOpenSettings={() => setSettingsOpen(true)}
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
          <div className="ml-auto flex items-center gap-2">
            {user ? (
              <>
                <span className="text-sm">{user.display_name}</span>
                <select
                  aria-label="Workspace"
                  value={workspaceID}
                  onChange={(event) => {
                    const next = workspaces.find((w) => w.id === event.target.value);
                    if (next) {
                      selectWorkspace(next);
                      setWorkspaceID(next.id);
                    }
                  }}
                >
                  {workspaces.map((w) => (
                    <option key={w.id} value={w.id}>
                      {w.slug} · {w.role}
                    </option>
                  ))}
                </select>
                <Button variant="ghost" onClick={() => void signOut(false)}>
                  Sign out
                </Button>
                <Button variant="ghost" onClick={() => void signOut(true)}>
                  Sign out all
                </Button>
              </>
            ) : (
              <Button onClick={signIn} disabled={booting}>
                Sign in with Google
              </Button>
            )}
          </div>
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
            token={canWrite() ? token : ""}
            readOnly={Boolean(token) && !canWrite()}
            agents={agents}
            draft={draft}
            onDraftChange={setDraft}
            connectError={connectError}
            onAccess={signIn}
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
      <SettingsDialog open={settingsOpen} onOpenChange={setSettingsOpen} />
    </div>
  );
}

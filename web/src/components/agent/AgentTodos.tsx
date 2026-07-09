import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router";
import { agentApi, type AgentInfo, type AgentTask } from "../../lib/agentApi";
import { PreconditionFailedError } from "../../lib/httpClient";
import { errMsg, localRFC3339 } from "../../lib/utils";
import { useT } from "../../lib/i18n";
import { AgentAvatar } from "./AgentAvatar";

function formatCreatedAt(createdAt: string): string {
  const d = new Date(createdAt);
  if (Number.isNaN(d.getTime())) {
    return createdAt;
  }
  return localRFC3339(d);
}

export function AgentTodos() {
  const t = useT();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const navigateRef = useRef(navigate);
  navigateRef.current = navigate;

  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [tasks, setTasks] = useState<AgentTask[]>([]);
  const [tasksLoaded, setTasksLoaded] = useState(false);
  const [error, setError] = useState("");
  const [newTitle, setNewTitle] = useState("");
  const [doneExpanded, setDoneExpanded] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editTitle, setEditTitle] = useState("");
  // Live mirror of the route id so async mutation completions can tell
  // whether the user has navigated to a different agent meanwhile — a
  // stale response must not touch the new page's state.
  const idRef = useRef(id);
  idRef.current = id;

  // Best-effort refetch used after a failed mutation: the optimistic
  // local state may have diverged from the server (and a full-array
  // rollback would clobber other concurrent mutations' successes), so
  // resync from the source of truth instead.
  const reload = useCallback((forId: string) => {
    agentApi.tasks
      .list(forId)
      .then((list) => {
        if (idRef.current === forId) setTasks(list);
      })
      .catch(() => {});
  }, []);

  // Shared failure path for mutations: ignore if the user navigated
  // away; otherwise surface the error (412 gets a friendlier message)
  // and resync the list from the server.
  const failMutation = useCallback(
    (forId: string, err: unknown, conflictMsg: string) => {
      if (idRef.current !== forId) return;
      setError(err instanceof PreconditionFailedError ? conflictMsg : errMsg(err));
      reload(forId);
    },
    [reload],
  );

  useEffect(() => {
    if (!id) return;
    let cancelled = false;
    setAgent(null);
    setTasks([]);
    setTasksLoaded(false);
    setError("");
    setEditingId(null);

    agentApi
      .get(id)
      .then((a) => {
        if (!cancelled) setAgent(a);
      })
      .catch(() => {
        if (!cancelled) navigateRef.current("/");
      });

    agentApi.tasks
      .list(id)
      .then((list) => {
        if (!cancelled) {
          setTasks(list);
          setTasksLoaded(true);
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setTasksLoaded(true);
          setError(errMsg(err));
        }
      });

    return () => {
      cancelled = true;
    };
  }, [id]);

  const openTasks = tasks.filter((task) => task.status === "open");
  const doneTasks = tasks.filter((task) => task.status === "done");
  const openCount = openTasks.length;

  const handleAdd = async () => {
    if (!id) return;
    const reqId = id;
    const title = newTitle.trim();
    if (!title) return;
    const now = localRFC3339();
    const tempId = `optimistic_${Date.now()}`;
    const optimistic: AgentTask = {
      id: tempId,
      title,
      status: "open",
      createdAt: now,
      updatedAt: now,
    };
    setTasks((ts) => [...ts, optimistic]);
    setNewTitle("");
    setError("");
    try {
      const created = await agentApi.tasks.create(reqId, title);
      if (idRef.current !== reqId) return;
      setTasks((ts) => ts.map((x) => (x.id === tempId ? created : x)));
    } catch (err) {
      failMutation(reqId, err, errMsg(err));
    }
  };

  const handleStatus = async (task: AgentTask, status: "open" | "done") => {
    if (!id) return;
    const reqId = id;
    setTasks((ts) => ts.map((x) => (x.id === task.id ? { ...x, status } : x)));
    setError("");
    try {
      const updated = await agentApi.tasks.update(reqId, task.id, { status }, task.etag);
      if (idRef.current !== reqId) return;
      setTasks((ts) => ts.map((x) => (x.id === task.id ? updated : x)));
    } catch (err) {
      failMutation(reqId, err, t("todos.conflict"));
    }
  };

  const handleDelete = async (task: AgentTask) => {
    if (!id) return;
    const reqId = id;
    setTasks((ts) => ts.filter((x) => x.id !== task.id));
    if (editingId === task.id) setEditingId(null);
    setError("");
    try {
      await agentApi.tasks.delete(reqId, task.id, task.etag);
    } catch (err) {
      failMutation(reqId, err, t("todos.conflict"));
    }
  };

  const startEdit = (task: AgentTask) => {
    setEditingId(task.id);
    setEditTitle(task.title);
  };

  const cancelEdit = () => {
    setEditingId(null);
    setEditTitle("");
  };

  const saveEdit = async (taskId: string) => {
    if (!id) return;
    const trimmed = editTitle.trim();
    const original = tasks.find((x) => x.id === taskId);
    setEditingId(null);
    setEditTitle("");
    if (!original || !trimmed || trimmed === original.title) return;
    const reqId = id;
    setTasks((ts) => ts.map((x) => (x.id === taskId ? { ...x, title: trimmed } : x)));
    setError("");
    try {
      const updated = await agentApi.tasks.update(reqId, taskId, { title: trimmed }, original.etag);
      if (idRef.current !== reqId) return;
      setTasks((ts) => ts.map((x) => (x.id === taskId ? updated : x)));
    } catch (err) {
      failMutation(reqId, err, t("todos.conflict"));
    }
  };

  if (!id) return null;

  const renderRow = (task: AgentTask, done: boolean) => {
    const isEditing = editingId === task.id;
    // Row is an optimistic placeholder until create resolves and swaps in
    // the server row — its id doesn't exist on the server yet, so status /
    // edit / delete actions are disabled until then.
    const pending = task.id.startsWith("optimistic_");
    return (
      <div
        key={task.id}
        className={`flex items-start gap-3 border-b border-hairline px-4 py-3 ${pending ? "opacity-60" : ""}`}
      >
        <button
          type="button"
          disabled={pending}
          onClick={() => void handleStatus(task, done ? "open" : "done")}
          className={`mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full border-2 transition-colors ${
            done
              ? "border-copper bg-copper"
              : "border-ink-faint hover:border-copper"
          }`}
          title={done ? t("todos.reopen") : t("todos.markDone")}
          aria-label={done ? t("todos.reopen") : t("todos.markDone")}
        >
          {done && (
            <svg viewBox="0 0 12 12" className="h-3 w-3 text-[#14100b]" aria-hidden>
              <path
                d="M2.5 6.5l2.5 2.5 4.5-5"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.75"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          )}
        </button>

        <div className="min-w-0 flex-1">
          {isEditing ? (
            <input
              autoFocus
              value={editTitle}
              onChange={(e) => setEditTitle(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  void saveEdit(task.id);
                } else if (e.key === "Escape") {
                  e.preventDefault();
                  cancelEdit();
                }
              }}
              onBlur={() => void saveEdit(task.id)}
              className="w-full rounded-xl border border-hairline bg-transparent px-2 py-1 text-[14px] text-ink focus:border-copper/50 focus:outline-none"
              aria-label={t("todos.editTitle")}
            />
          ) : (
            <button
              type="button"
              disabled={pending}
              onClick={() => startEdit(task)}
              className={`block w-full break-words text-left text-[14px] ${
                done ? "text-ink-faint line-through" : "text-ink"
              }`}
              title={t("todos.editTitle")}
            >
              {task.title}
            </button>
          )}
          <div className="mt-0.5 font-mono text-[11px] text-ink-faint">
            {formatCreatedAt(task.createdAt)}
          </div>
        </div>

        <button
          type="button"
          disabled={pending}
          onClick={() => void handleDelete(task)}
          className="shrink-0 p-2 text-ink-faint transition-colors hover:text-lamp-err"
          title={t("todos.delete")}
          aria-label={t("todos.delete")}
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="h-4 w-4">
            <path
              fillRule="evenodd"
              d="M8.75 1A2.75 2.75 0 006 3.75v.443c-.795.077-1.584.176-2.365.298a.75.75 0 10.23 1.482l.149-.022.841 10.518A2.75 2.75 0 007.596 19h4.807a2.75 2.75 0 002.742-2.53l.841-10.52.149.023a.75.75 0 00.23-1.482A41.03 41.03 0 0014 4.193V3.75A2.75 2.75 0 0011.25 1h-2.5zM10 4c.84 0 1.673.025 2.5.075V3.75c0-.69-.56-1.25-1.25-1.25h-2.5c-.69 0-1.25.56-1.25 1.25v.325C8.327 4.025 9.16 4 10 4zM8.58 7.72a.75.75 0 00-1.5.06l.3 7.5a.75.75 0 101.5-.06l-.3-7.5zm4.34.06a.75.75 0 10-1.5-.06l-.3 7.5a.75.75 0 101.5.06l.3-7.5z"
              clipRule="evenodd"
            />
          </svg>
        </button>
      </div>
    );
  };

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden bg-app text-ink">
      <header className="flex h-[52px] shrink-0 items-center gap-2 border-b border-hairline px-3">
        <button
          type="button"
          onClick={() => navigate(`/agents/${id}`, { replace: true })}
          className="-ml-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-[10px] text-ink-dim transition-colors hover:bg-hover hover:text-ink"
          aria-label={t("common.back")}
        >
          <svg
            viewBox="0 0 20 20"
            fill="none"
            stroke="currentColor"
            strokeWidth={2}
            strokeLinecap="round"
            strokeLinejoin="round"
            className="h-5 w-5"
          >
            <path d="M12.5 15l-5-5 5-5" />
          </svg>
        </button>
        {agent ? (
          <AgentAvatar agentId={agent.id} name={agent.name} size="sm" cacheBust={agent.avatarHash} />
        ) : (
          <div className="h-12 w-12 shrink-0 rounded-full bg-surface" />
        )}
        <div className="min-w-0 flex-1">
          <div className="truncate font-semibold text-[14px] text-ink">
            {agent?.name ?? " "}
          </div>
          <div className="text-[11px] text-ink-faint">{t("todos.title")}</div>
        </div>
        {openCount > 0 && (
          <span className="font-mono text-[11px] text-ink-faint">
            {openCount} {t("todos.openCount")}
          </span>
        )}
      </header>

      <div className="mx-auto w-full max-w-2xl shrink-0 px-4 py-3">
        <div className="flex items-center gap-2">
          <div className="min-w-0 flex-1 rounded-xl border border-hairline bg-raised focus-within:border-copper/50">
            <input
              type="text"
              value={newTitle}
              onChange={(e) => setNewTitle(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  void handleAdd();
                }
              }}
              placeholder={t("todos.addPlaceholder")}
              className="w-full bg-transparent px-3 py-2 text-[14px] text-ink placeholder:text-ink-faint focus:outline-none"
              aria-label={t("todos.add")}
            />
          </div>
          <button
            type="button"
            onClick={() => void handleAdd()}
            className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-copper text-[#14100b] transition-colors hover:bg-copper-bright"
            title={t("todos.add")}
            aria-label={t("todos.add")}
          >
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="h-5 w-5">
              <path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
            </svg>
          </button>
        </div>
      </div>

      {error && (
        <div className="mx-auto flex w-full max-w-2xl shrink-0 items-center justify-between bg-lamp-err/10 px-4 py-2 text-[12px] text-lamp-err">
          <span className="min-w-0 break-words">{error}</span>
          <button
            type="button"
            onClick={() => setError("")}
            className="ml-2 shrink-0 p-1 text-lamp-err"
            aria-label={t("common.close")}
          >
            &times;
          </button>
        </div>
      )}

      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto w-full max-w-2xl">
          {tasksLoaded && !error && tasks.length === 0 && (
            <div className="py-16 text-center">
              <p className="text-[13px] text-ink-faint">{t("todos.empty")}</p>
              <p className="mt-1 text-[12px] text-ink-faint">{t("todos.emptyHint")}</p>
            </div>
          )}

          {tasksLoaded && openTasks.map((task) => renderRow(task, false))}

          {tasksLoaded && doneTasks.length > 0 && (
            <div>
              <button
                type="button"
                onClick={() => setDoneExpanded((v) => !v)}
                aria-expanded={doneExpanded}
                aria-controls="todos-done-list"
                className="flex w-full items-center gap-2 px-4 py-3 text-left text-[13px] text-ink-faint transition-colors hover:text-ink-dim"
              >
                <svg
                  viewBox="0 0 20 20"
                  fill="currentColor"
                  className={`h-4 w-4 transition-transform ${doneExpanded ? "rotate-90" : ""}`}
                  aria-hidden
                >
                  <path
                    fillRule="evenodd"
                    d="M7.21 14.77a.75.75 0 01.02-1.06L11.168 10 7.23 6.29a.75.75 0 111.04-1.08l4.5 4.25a.75.75 0 010 1.08l-4.5 4.25a.75.75 0 01-1.06-.02z"
                    clipRule="evenodd"
                  />
                </svg>
                <span>{t("todos.doneSection", { count: doneTasks.length })}</span>
              </button>
              {doneExpanded && (
                <div id="todos-done-list">{doneTasks.map((task) => renderRow(task, true))}</div>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

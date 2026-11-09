import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { useQuery } from "@tanstack/react-query";
import { CheckCircle2, Clock, Loader2, XCircle } from "lucide-react";
import { api, type Task } from "../lib/api";
import { formatRelative } from "../lib/format";
import { Badge, Card, type Tone } from "./ui";

/** Agent errors arrive as "<code>: <detail>"; show the translated sentence for known codes. */
function taskError(t: TFunction, error: string): string {
  const code = /^([a-z_]+): /.exec(error)?.[1];
  return code ? t(`errors.agent_${code}`, { defaultValue: error }) : error;
}

const statusTone: Record<Task["status"], Tone> = { queued: "neutral", running: "accent", succeeded: "ok", failed: "crit" };

function StatusIcon({ status }: { status: Task["status"] }) {
  const cls = "size-4 shrink-0";
  switch (status) {
    case "queued":
      return <Clock className={`${cls} text-muted`} aria-hidden />;
    case "running":
      return <Loader2 className={`${cls} animate-spin text-accent`} aria-hidden />;
    case "succeeded":
      return <CheckCircle2 className={`${cls} text-ok`} aria-hidden />;
    case "failed":
      return <XCircle className={`${cls} text-crit`} aria-hidden />;
  }
}

/** "Restart caddy", in the user's language. Unknown kinds fall back to their code. */
export function useTaskLabel() {
  const { t } = useTranslation();
  return (task: Task) => t(`tasks.kinds.${task.kind.replace(".", "_")}`, { defaultValue: task.kind, ...task.params });
}

/** Active-polling list: fast while something is running, slow otherwise. */
export function useTasks(query: string, enabled = true) {
  return useQuery({
    queryKey: ["tasks", query],
    queryFn: () => api.get<Task[]>(`/tasks?${query}`),
    enabled,
    refetchInterval: (q) => (q.state.data?.some((t) => t.status === "queued" || t.status === "running") ? 1500 : 10000),
  });
}

export function TaskRow({ task, showServer }: { task: Task; showServer?: boolean }) {
  const { t, i18n } = useTranslation();
  const label = useTaskLabel();
  return (
    <li className="grid gap-0.5 px-4 py-2.5">
      <div className="flex items-center gap-2">
        <StatusIcon status={task.status} />
        <span className="min-w-0 truncate font-medium">{label(task)}</span>
        {showServer && task.server_name && <span className="truncate text-sm text-muted">· {task.server_name}</span>}
        <span className="ms-auto shrink-0">
          <Badge tone={statusTone[task.status]}>{t(`tasks.status.${task.status}`)}</Badge>
        </span>
      </div>
      <div className="ps-6 text-xs text-muted">
        {formatRelative(task.finished_at ?? task.started_at ?? task.created_at, i18n.language)}
        {task.status === "failed" && task.error && (
          <p className="mt-0.5 text-crit" dir="auto" title={task.error}>
            {taskError(t, task.error)}
          </p>
        )}
      </div>
    </li>
  );
}

export function RecentTasks({ serverId }: { serverId: string }) {
  const { t } = useTranslation();
  const q = useTasks(`server_id=${encodeURIComponent(serverId)}&limit=8`);
  return (
    <Card className="overflow-hidden">
      <h2 className="border-b border-line px-4 py-3 font-semibold">{t("tasks.recent")}</h2>
      {q.data && q.data.length === 0 && <p className="px-4 py-6 text-center text-sm text-muted">{t("tasks.none")}</p>}
      <ul className="divide-y divide-line">
        {q.data?.map((task) => <TaskRow key={task.id} task={task} />)}
      </ul>
    </Card>
  );
}

/** Floating list of work in progress, visible on every page while something runs. */
export function TaskTray() {
  const { t } = useTranslation();
  const q = useTasks("active=1&limit=5");
  const active = q.data ?? [];
  if (active.length === 0) return null;
  return (
    <div
      role="status"
      aria-live="polite"
      className="fixed end-4 bottom-4 z-40 w-[min(22rem,calc(100vw-2rem))] overflow-hidden rounded-xl border border-line bg-surface shadow-lg"
    >
      <p className="border-b border-line bg-sunk px-4 py-2 text-sm font-semibold">{t("tasks.in_progress", { count: active.length })}</p>
      <ul className="divide-y divide-line">
        {active.map((task) => <TaskRow key={task.id} task={task} showServer />)}
      </ul>
    </div>
  );
}

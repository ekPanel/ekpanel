import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, Terminal } from "lucide-react";
import { api, errorMessage, type EnrollResult, type Metrics, type Server } from "../lib/api";
import { formatBytes, formatPercent, formatRelative } from "../lib/format";
import { Badge, Button, CopyButton, Dialog, ErrorAlert, Field, Meter, StatusDot, type Tone } from "../components/ui";

export type ServerState = "online" | "offline" | "pending";

export function serverState(s: Pick<Server, "online" | "enrolled_at">): ServerState {
  if (s.online) return "online";
  return s.enrolled_at ? "offline" : "pending";
}

const stateTone: Record<ServerState, Tone> = { online: "ok", offline: "crit", pending: "warn" };

export function ServerStatus({ server }: { server: Pick<Server, "online" | "enrolled_at" | "last_seen_at"> }) {
  const { t, i18n } = useTranslation();
  const state = serverState(server);
  return (
    <span className="inline-flex items-center gap-2 text-sm" title={server.last_seen_at ? t("servers.last_seen", { when: formatRelative(server.last_seen_at, i18n.language) }) : undefined}>
      <StatusDot tone={stateTone[state]} pulse={state === "online"} />
      {t(`servers.state.${state}`)}
    </span>
  );
}

export function ServerStateBadge({ server }: { server: Pick<Server, "online" | "enrolled_at"> }) {
  const { t } = useTranslation();
  const state = serverState(server);
  return <Badge tone={stateTone[state]}>{t(`servers.state.${state}`)}</Badge>;
}

/** CPU / RAM / disk bars used on server cards and rows. */
export function UsageBars({ m }: { m: Metrics }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const row = (label: string, percent: number, detail: string) => (
    <div className="grid gap-1">
      <div className="flex justify-between gap-2 text-sm">
        <span className="text-muted">{label}</span>
        <span className="tabular-nums">{detail}</span>
      </div>
      <Meter percent={percent} label={label} />
    </div>
  );
  const memPct = m.mem_total ? (m.mem_used / m.mem_total) * 100 : 0;
  const diskPct = m.disk_total ? (m.disk_used / m.disk_total) * 100 : 0;
  return (
    <div className="grid gap-3">
      {row(t("metrics.cpu"), m.cpu_percent, formatPercent(m.cpu_percent, lng))}
      {row(t("metrics.memory"), memPct, `${formatBytes(m.mem_used, lng)} / ${formatBytes(m.mem_total, lng)}`)}
      {row(t("metrics.disk"), diskPct, `${formatBytes(m.disk_used, lng)} / ${formatBytes(m.disk_total, lng)}`)}
    </div>
  );
}

function CommandBlock({ label, command }: { label: string; command: string }) {
  return (
    <div className="grid gap-1.5">
      <p className="text-sm font-medium">{label}</p>
      <div className="flex items-start gap-2 rounded-lg bg-term-bg p-3 text-term-fg">
        <code dir="ltr" className="min-w-0 flex-1 overflow-x-auto font-mono text-[13px] leading-6 whitespace-pre">
          {command}
        </code>
        <CopyButton text={command} className="shrink-0 text-term-fg hover:bg-white/10" />
      </div>
    </div>
  );
}

/** The commands an admin runs on a new server, with a live "waiting for the agent" indicator. */
export function EnrollInstructions({ result }: { result: EnrollResult }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const live = useQuery({
    queryKey: ["server", result.server.id],
    queryFn: () => api.get<Server>(`/servers/${result.server.id}`),
    refetchInterval: (q) => (q.state.data?.online ? false : 2000),
    initialData: result.server,
  });
  const connected = live.data?.online;
  useEffect(() => {
    if (connected) qc.invalidateQueries({ queryKey: ["servers"] });
  }, [connected, qc]);

  const panel = window.location.origin;
  const enroll = `sudo ekpanel-agent enroll \\\n  --panel ${panel} \\\n  --token ${result.enroll_token}`;
  const expires = result.server.enroll_expires_at ? formatRelative(result.server.enroll_expires_at, i18n.language) : "";

  return (
    <div className="grid gap-5">
      <p className="text-muted">{t("servers.enroll.intro")}</p>
      <CommandBlock label={t("servers.enroll.step1")} command={enroll} />
      <CommandBlock label={t("servers.enroll.step2")} command="sudo ekpanel-agent run" />
      <p className="text-sm text-muted">{t("servers.enroll.token_note", { when: expires })}</p>
      <div className={`flex items-center gap-3 rounded-lg px-3 py-3 ${connected ? "bg-ok-soft text-ok" : "bg-sunk text-muted"}`} role="status">
        {connected ? <CheckCircle2 className="size-5 shrink-0" aria-hidden /> : <Terminal className="size-5 shrink-0 animate-pulse" aria-hidden />}
        <span className="font-medium">{connected ? t("servers.enroll.connected") : t("servers.enroll.waiting")}</span>
      </div>
    </div>
  );
}

export function AddServerDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<EnrollResult | null>(null);

  useEffect(() => {
    if (open) {
      setName("");
      setError("");
      setResult(null);
    }
  }, [open]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      setResult(await api.post<EnrollResult>("/servers", { name }));
      qc.invalidateQueries({ queryKey: ["servers"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} title={result ? t("servers.enroll.title", { name: result.server.name }) : t("servers.add")} wide={!!result}>
      {result ? (
        <>
          <EnrollInstructions result={result} />
          <div className="flex justify-end">
            <Button variant="primary" onClick={onClose}>
              {t("common.done")}
            </Button>
          </div>
        </>
      ) : (
        <form onSubmit={submit} className="grid gap-5">
          <ErrorAlert>{error}</ErrorAlert>
          <Field label={t("servers.name")} hint={t("servers.name_hint")} value={name} onChange={(e) => setName(e.target.value)} required autoFocus maxLength={100} />
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!name.trim()}>
              {t("common.continue")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

export function EmptyState({ icon, title, body, action }: { icon: ReactNode; title: string; body: string; action?: ReactNode }) {
  return (
    <div className="grid justify-items-center gap-3 rounded-xl border border-dashed border-line bg-surface px-6 py-14 text-center">
      <div className="grid size-12 place-items-center rounded-full bg-accent-soft text-accent [&>svg]:size-6">{icon}</div>
      <h2 className="text-lg font-semibold">{title}</h2>
      <p className="max-w-md text-muted">{body}</p>
      {action && <div className="mt-2">{action}</div>}
    </div>
  );
}

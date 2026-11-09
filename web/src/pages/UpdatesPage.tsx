import { useRef, useState } from "react";
import { Link } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleArrowUp, ExternalLink, RefreshCw, RotateCcw } from "lucide-react";
import { api, ApiError, errorMessage, type UpdateState, type UpdatesInfo } from "../lib/api";
import { formatDateTime, formatRelative } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, ErrorAlert, Spinner, StatusDot, type Tone } from "../components/ui";

export const updatesKey = ["updates"];

/** Settings → Updates: the panel's version, new releases, and every server's agent. */
export function UpdatesPage() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState<"check" | "panel" | "agents" | null>(null);
  const [error, setError] = useState("");
  // The version this page was loaded with: after an update the browser holds the old UI.
  const loadedVersion = useRef<string | null>(null);

  const q = useQuery({
    queryKey: updatesKey,
    queryFn: () => api.get<UpdatesInfo>("/updates"),
    // While an update runs, the panel restarts: keep asking, and keep the last answer meanwhile.
    refetchInterval: (query) => (query.state.data?.state && running(query.state.data.state) ? 3000 : 60000),
    // Keep trying while the panel restarts (no answer, or the web server's 502); give up on
    // other errors.
    retry: (count, err) => (err instanceof ApiError && (err.code === "network" || err.status >= 502)) || count < 1,
    retryDelay: 3000,
  });
  if (q.data && loadedVersion.current === null) loadedVersion.current = q.data.current;

  const run = async (what: "check" | "panel" | "agents") => {
    setBusy(what);
    setError("");
    try {
      if (what === "check") {
        qc.setQueryData(updatesKey, await api.post<UpdatesInfo>("/updates/check"));
      } else {
        await api.post(what === "panel" ? "/updates/panel" : "/updates/agents", what === "panel" ? { version: q.data?.latest.version } : {});
        await qc.invalidateQueries({ queryKey: updatesKey });
      }
    } catch (e) {
      setError(errorMessage(t, e));
    } finally {
      setBusy(null);
    }
  };

  if (q.isPending) return <Spinner />;
  if (q.error && !q.data) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;
  const d = q.data!;
  const lng = i18n.language;
  const st = d.state;
  const isRunning = !!st && running(st);
  const restarting = isRunning && q.isError; // the panel is restarting right now
  const newUI = loadedVersion.current !== null && d.current !== loadedVersion.current;

  return (
    <div className="grid gap-6">
      <PageHeader title={t("updates.title")} description={t("updates.description")} />
      {error && <ErrorAlert>{error}</ErrorAlert>}
      {newUI && (
        <div className="flex flex-wrap items-center gap-3 rounded-lg bg-ok-soft px-4 py-3 text-ok" role="status">
          <span className="font-medium">{t("updates.reload_hint", { version: d.current })}</span>
          <Button size="sm" onClick={() => window.location.reload()}>
            <RotateCcw className="size-4" aria-hidden /> {t("updates.reload")}
          </Button>
        </div>
      )}

      <Card className="grid gap-4 p-5">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="grid gap-1">
            <p className="text-sm text-muted">{t("updates.this_panel")}</p>
            <p className="font-mono text-2xl font-semibold">{d.current}</p>
            {!d.release && <p className="max-w-xl text-sm text-muted">{t("updates.not_release")}</p>}
          </div>
          <Button onClick={() => run("check")} loading={busy === "check"} disabled={isRunning}>
            <RefreshCw className="size-4" aria-hidden /> {t("updates.check")}
          </Button>
        </div>
        <LatestLine d={d} lng={lng} />
        {d.update_available && !isRunning && (
          <div className="grid gap-3 rounded-lg border border-line bg-sunk p-4">
            <p className="font-semibold">{t("updates.available", { version: d.latest.version })}</p>
            <ol className="grid list-decimal gap-1 ps-5 text-sm text-muted">
              <li>{t("updates.steps.download")}</li>
              <li>{t("updates.steps.backup")}</li>
              <li>{t("updates.steps.restart")}</li>
              <li>{t("updates.steps.agents")}</li>
            </ol>
            {!d.can_update && <p className="text-sm text-warn">{t("updates.panel_agent_offline")}</p>}
            <div className="flex flex-wrap items-center gap-3">
              <Button
                variant="primary"
                loading={busy === "panel"}
                disabled={!d.can_update}
                onClick={() => window.confirm(t("updates.confirm", { version: d.latest.version })) && run("panel")}
              >
                <CircleArrowUp className="size-4" aria-hidden /> {t("updates.update_to", { version: d.latest.version })}
              </Button>
              {d.latest.notes_url && (
                <a href={d.latest.notes_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-sm text-accent hover:underline">
                  {t("updates.notes")} <ExternalLink className="size-3.5" aria-hidden />
                </a>
              )}
            </div>
          </div>
        )}
      </Card>

      {st && <Progress st={st} restarting={restarting} lng={lng} />}

      <Card className="grid gap-4 p-5">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h2 className="text-lg font-semibold">{t("updates.agents.title")}</h2>
            <p className="text-sm text-muted">{t("updates.agents.body")}</p>
          </div>
          {d.outdated > 0 && !isRunning && (
            <Button loading={busy === "agents"} onClick={() => run("agents")}>
              <CircleArrowUp className="size-4" aria-hidden /> {t("updates.agents.update", { count: d.outdated })}
            </Button>
          )}
        </div>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead className="text-start text-muted">
              <tr className="border-b border-line">
                <th className="py-2 pe-4 text-start font-medium">{t("updates.agents.server")}</th>
                <th className="py-2 pe-4 text-start font-medium">{t("updates.agents.version")}</th>
                <th className="py-2 text-start font-medium">{t("updates.agents.status")}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {d.servers.map((s) => (
                <tr key={s.id}>
                  <td className="py-2.5 pe-4">
                    <Link to={`/servers/${s.id}`} className="font-medium hover:underline">
                      {s.name}
                    </Link>
                    {s.panel_host && <span className="ms-2 text-xs text-muted">{t("updates.agents.panel_host")}</span>}
                  </td>
                  <td className="py-2.5 pe-4 font-mono">{s.agent_version || "—"}</td>
                  <td className="py-2.5">
                    {!s.online ? (
                      <Badge tone="crit">{t("servers.state.offline")}</Badge>
                    ) : s.outdated ? (
                      <Badge tone="warn">{t("updates.agents.outdated")}</Badge>
                    ) : (
                      <Badge tone="ok">{t("updates.agents.current")}</Badge>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
}

function running(st: UpdateState) {
  return st.phase === "panel" || st.phase === "agents";
}

function LatestLine({ d, lng }: { d: UpdatesInfo; lng: string }) {
  const { t } = useTranslation();
  const l = d.latest;
  if (!l.checked_at) {
    return <p className="text-sm text-muted">{d.check_enabled ? t("updates.never_checked") : t("updates.check_off")}</p>;
  }
  return (
    <div className="grid gap-1 text-sm">
      {l.version && (
        <p>
          {t("updates.latest", { version: l.version })}
          {l.published_at && <span className="text-muted"> · {formatDateTime(l.published_at, lng)}</span>}
          {!d.update_available && <span className="ms-2 text-ok">{t("updates.up_to_date")}</span>}
        </p>
      )}
      <p className="text-muted">{t("updates.checked", { when: formatRelative(l.checked_at, lng) })}</p>
      {l.error && <p className="text-crit">{t("updates.check_failed", { message: l.error })}</p>}
    </div>
  );
}

const statusTone: Record<string, Tone> = { updated: "ok", current: "neutral", offline: "warn", failed: "crit" };

function Progress({ st, restarting, lng }: { st: UpdateState; restarting: boolean; lng: string }) {
  const { t } = useTranslation();
  const tone: Tone = st.phase === "failed" ? "crit" : st.phase === "done" ? "ok" : "accent";
  return (
    <Card className="grid gap-3 p-5" aria-live="polite">
      <div className="flex flex-wrap items-center gap-3">
        <StatusDot tone={tone} pulse={running(st)} />
        <h2 className="font-semibold">{t(`updates.phase.${st.phase}`, { version: st.target })}</h2>
        <span className="text-sm text-muted">
          {st.finished_at ? formatDateTime(st.finished_at, lng) : formatRelative(st.started_at, lng)}
        </span>
      </div>
      {restarting && <p className="text-sm text-muted">{t("updates.restarting")}</p>}
      {st.error && <ErrorAlert>{st.error}</ErrorAlert>}
      {st.backup && <p className="text-sm text-muted">{t("updates.backup", { path: st.backup })}</p>}
      {st.servers && st.servers.length > 0 && (
        <ul className="grid gap-1.5 text-sm">
          {st.servers.map((s) => (
            <li key={s.server_id} className="flex flex-wrap items-center gap-2">
              <Badge tone={statusTone[s.status] ?? "neutral"}>{t(`updates.server_status.${s.status}`)}</Badge>
              <span className="font-medium">{s.name}</span>
              {s.from && <span className="font-mono text-muted">{s.from}</span>}
              {s.error && <span className="text-crit">{s.error}</span>}
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

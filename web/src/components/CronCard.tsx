import { useEffect, useMemo, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CalendarClock, Pencil, Play, Plus, ScrollText, Trash2 } from "lucide-react";
import { api, errorMessage, type CronJob, type CronStatus } from "../lib/api";
import { formatDateTime, formatRelative } from "../lib/format";
import { Badge, Button, Card, Dialog, ErrorAlert, Field, Spinner, cx } from "./ui";

const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";
/** Time zones to offer: UTC first (Intl does not list it), then every zone the browser knows. */
const allZones = (() => {
  let zones: string[] = [];
  try {
    zones = Intl.supportedValuesOf("timeZone");
  } catch {
    // older browsers: UTC only
  }
  return ["UTC", ...zones.filter((z) => z !== "UTC")];
})();
/** The browser's own zone, if it is a real one ("Etc/Unknown" happens). */
const browserZone = (() => {
  const z = Intl.DateTimeFormat().resolvedOptions().timeZone;
  return z && allZones.includes(z) ? z : "UTC";
})();

/** Schedules most people want, as cron expressions. "custom" lets them type one. */
type Preset = "minute" | "5min" | "15min" | "hourly" | "daily" | "weekly" | "monthly" | "custom";

function presetOf(expr: string): { preset: Preset; time: string; weekday: string } {
  const f = expr.trim().split(/\s+/);
  const fixed: Record<string, Preset> = { "* * * * *": "minute", "*/5 * * * *": "5min", "*/15 * * * *": "15min", "0 * * * *": "hourly" };
  if (fixed[f.join(" ")]) return { preset: fixed[f.join(" ")], time: "03:00", weekday: "0" };
  const [m, h, dom, mon, dow] = f;
  const isNum = (x: string | undefined) => !!x && /^\d+$/.test(x);
  if (f.length === 5 && isNum(m) && isNum(h) && mon === "*") {
    const time = `${h.padStart(2, "0")}:${m.padStart(2, "0")}`;
    if (dom === "*" && dow === "*") return { preset: "daily", time, weekday: "0" };
    if (dom === "*" && /^[0-6]$/.test(dow)) return { preset: "weekly", time, weekday: dow };
    if (dom === "1" && dow === "*") return { preset: "monthly", time, weekday: "0" };
  }
  return { preset: "custom", time: "03:00", weekday: "0" };
}

function exprOf(preset: Preset, time: string, weekday: string, custom: string): string {
  const [h, m] = time.split(":").map((x) => String(Number(x) || 0));
  switch (preset) {
    case "minute":
      return "* * * * *";
    case "5min":
      return "*/5 * * * *";
    case "15min":
      return "*/15 * * * *";
    case "hourly":
      return "0 * * * *";
    case "daily":
      return `${m} ${h} * * *`;
    case "weekly":
      return `${m} ${h} * * ${weekday}`;
    case "monthly":
      return `${m} ${h} 1 * *`;
    default:
      return custom;
  }
}

function CronDialog({ open, job, target, onClose }: { open: boolean; job: CronJob | null; target: { siteId?: string; appId?: string }; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [command, setCommand] = useState("");
  const [preset, setPreset] = useState<Preset>("daily");
  const [time, setTime] = useState("03:00");
  const [weekday, setWeekday] = useState("0");
  const [custom, setCustom] = useState("");
  const [zone, setZone] = useState(browserZone);
  const [timeout, setTimeout_] = useState("60");
  const [service, setService] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [preview, setPreview] = useState<{ next?: string[]; error?: string }>({});
  const zones = useMemo(() => (job && !allZones.includes(job.timezone) ? [job.timezone, ...allZones] : allZones), [job]);
  const weekdays = useMemo(() => {
    const f = new Intl.DateTimeFormat(i18n.language, { weekday: "long" });
    return Array.from({ length: 7 }, (_, d) => f.format(new Date(2026, 10, 1 + d))); // 1 Nov 2026 is a Sunday
  }, [i18n.language]);

  useEffect(() => {
    if (!open) return;
    const p = presetOf(job?.schedule ?? "0 3 * * *");
    setName(job?.name ?? "");
    setCommand(job?.command ?? "");
    setPreset(p.preset);
    setTime(p.time);
    setWeekday(p.weekday);
    setCustom(job?.schedule ?? "0 3 * * *");
    setZone(job?.timezone ?? browserZone);
    setTimeout_(String(Math.round((job?.timeout_seconds ?? 3600) / 60)));
    setService(job?.service ?? "");
    setError("");
  }, [open, job]);

  const schedule = exprOf(preset, time, weekday, custom);
  useEffect(() => {
    if (!open) return;
    const timer = window.setTimeout(async () => {
      try {
        const r = await api.post<{ next: string[] }>("/cron/preview", { schedule, timezone: zone });
        setPreview({ next: r.next });
      } catch (err) {
        setPreview({ error: errorMessage(t, err) });
      }
    }, 300);
    return () => window.clearTimeout(timer);
  }, [open, schedule, zone, t]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const body = {
      site_id: target.siteId ?? "",
      app_id: target.appId ?? "",
      service,
      name,
      schedule,
      timezone: zone,
      command,
      timeout_seconds: Math.max(10, Math.round(Number(timeout) * 60) || 3600),
      enabled: job ? job.enabled : true,
    };
    try {
      if (job) await api.put(`/cron/${job.id}`, body);
      else await api.post("/cron", body);
      qc.invalidateQueries({ queryKey: ["cron"] });
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  const zoneTime = (iso: string) => new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short", timeZone: zone }).format(new Date(iso));

  return (
    <Dialog open={open} onClose={onClose} title={job ? t("cron.edit") : t("cron.add")} wide>
      <form onSubmit={submit} className="grid gap-5">
        <ErrorAlert>{error}</ErrorAlert>
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("cron.command")}</span>
          <textarea
            value={command}
            onChange={(e) => setCommand(e.target.value)}
            rows={3}
            dir="ltr"
            spellCheck={false}
            required
            className="rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm text-ink"
            placeholder={target.appId ? "php artisan schedule:run" : "php /home/…/public_html/cron.php"}
          />
          <span className="text-sm text-muted">{target.appId ? t("cron.command_hint_app") : t("cron.command_hint_site")}</span>
        </label>
        <div className="grid gap-4 sm:grid-cols-2">
          <label className="grid gap-1.5">
            <span className="text-sm font-medium">{t("cron.when")}</span>
            <select value={preset} onChange={(e) => setPreset(e.target.value as Preset)} className={select}>
              {(["minute", "5min", "15min", "hourly", "daily", "weekly", "monthly", "custom"] as const).map((p) => (
                <option key={p} value={p}>
                  {t(`cron.presets.${p}`)}
                </option>
              ))}
            </select>
          </label>
          {(preset === "daily" || preset === "weekly" || preset === "monthly") && (
            <div className="grid grid-cols-2 gap-3">
              {preset === "weekly" && (
                <label className="grid gap-1.5">
                  <span className="text-sm font-medium">{t("cron.weekday")}</span>
                  <select value={weekday} onChange={(e) => setWeekday(e.target.value)} className={select}>
                    {weekdays.map((d, i) => (
                      <option key={i} value={String(i)}>
                        {d}
                      </option>
                    ))}
                  </select>
                </label>
              )}
              <Field
                label={t("cron.at")}
                type="time"
                value={time}
                onChange={(e) => setTime(e.target.value)}
                required
                className={preset === "weekly" ? "" : "col-span-2"}
              />
            </div>
          )}
          {preset === "custom" && (
            <Field
              label={t("cron.expression")}
              hint={t("cron.expression_hint")}
              value={custom}
              onChange={(e) => setCustom(e.target.value)}
              dir="ltr"
              className="[&_input]:font-mono"
              required
            />
          )}
        </div>
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("cron.timezone")}</span>
          <select value={zone} onChange={(e) => setZone(e.target.value)} className={select}>
            {zones.map((z) => (
              <option key={z} value={z}>
                {z}
              </option>
            ))}
          </select>
        </label>
        <div className="rounded-lg bg-sunk p-3 text-sm">
          <p className="mb-1 font-medium">
            {t("cron.next_runs")}{" "}
            <span className="font-mono text-xs text-muted" dir="ltr">
              {schedule}
            </span>
          </p>
          {preview.error ? (
            <p className="text-crit">{preview.error}</p>
          ) : (
            <ul className="grid gap-0.5 text-muted tabular-nums">
              {preview.next?.map((n) => (
                <li key={n}>{zoneTime(n)}</li>
              ))}
            </ul>
          )}
        </div>
        <div className="grid gap-4 sm:grid-cols-3">
          <Field
            label={t("cron.name")}
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={100}
            placeholder={t("cron.name_placeholder")}
            className="sm:col-span-2"
          />
          <Field label={t("cron.timeout")} value={timeout} onChange={(e) => setTimeout_(e.target.value.replace(/\D/g, ""))} inputMode="numeric" dir="ltr" />
        </div>
        {target.appId && (
          <Field
            label={t("cron.service")}
            hint={t("cron.service_hint")}
            value={service}
            onChange={(e) => setService(e.target.value)}
            dir="ltr"
            placeholder="app"
          />
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!!preview.error}>
            {job ? t("sites.save") : t("cron.create")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function CronOutput({ job, onClose }: { job: CronJob; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const q = useQuery({ queryKey: ["cron-status", job.id], queryFn: () => api.get<CronStatus>(`/cron/${job.id}/status`), refetchInterval: 3000, retry: false });
  const s = q.data;
  return (
    <Dialog open onClose={onClose} title={job.name} wide>
      {q.isPending && <Spinner />}
      {q.error && <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>}
      {s && (
        <div className="grid gap-3">
          <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1 text-sm">
            <dt className="text-muted">{t("cron.last_run")}</dt>
            <dd className="flex flex-wrap items-center gap-2">
              {s.running ? (
                <Badge tone="accent">{t("cron.running")}</Badge>
              ) : s.last_run ? (
                <>
                  <span>{formatDateTime(s.last_run, i18n.language)}</span>
                  <Badge tone={s.result === "success" ? "ok" : "crit"}>
                    {s.result === "success" ? t("cron.ok") : t("cron.failed", { code: s.exit_code ?? "?", result: s.result ?? "" })}
                  </Badge>
                </>
              ) : (
                t("cron.never")
              )}
            </dd>
            <dt className="text-muted">{t("cron.next_run")}</dt>
            <dd>{s.next_run ? formatDateTime(s.next_run, i18n.language) : "—"}</dd>
          </dl>
          <pre className="max-h-96 overflow-auto rounded-lg bg-sunk p-3 font-mono text-xs whitespace-pre-wrap" dir="ltr">
            {s.output.trim() || t("cron.no_output")}
          </pre>
        </div>
      )}
    </Dialog>
  );
}

/** Scheduled commands of a website or an app. */
export function CronCard({ siteId, appId }: { siteId?: string; appId?: string }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const query = siteId ? `site_id=${siteId}` : `app_id=${appId}`;
  const jobs = useQuery({
    queryKey: ["cron", query],
    queryFn: () => api.get<CronJob[]>(`/cron?${query}`),
    refetchInterval: (q) => (q.state.data?.some((j) => j.state === "pending" || j.state === "deleting") ? 2000 : 30000),
  });
  const [editing, setEditing] = useState<CronJob | null>(null);
  const [adding, setAdding] = useState(false);
  const [viewing, setViewing] = useState<CronJob | null>(null);
  const [error, setError] = useState("");

  const act = async (run: () => Promise<unknown>) => {
    setError("");
    try {
      await run();
      qc.invalidateQueries({ queryKey: ["cron"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };

  return (
    <Card className="overflow-hidden">
      <div className="flex items-center gap-3 border-b border-line px-4 py-3">
        <CalendarClock className="size-4 text-muted" aria-hidden />
        <h2 className="font-semibold">{t("cron.title")}</h2>
        <Button size="sm" className="ms-auto" onClick={() => setAdding(true)}>
          <Plus className="size-4" aria-hidden />
          {t("cron.add")}
        </Button>
      </div>
      {error && (
        <div className="px-4 pt-3">
          <ErrorAlert>{error}</ErrorAlert>
        </div>
      )}
      {jobs.isPending && <Spinner />}
      {jobs.data?.length === 0 && <p className="px-4 py-6 text-center text-sm text-muted">{t("cron.none")}</p>}
      <ul className="divide-y divide-line">
        {jobs.data?.map((j) => (
          <li key={j.id} className={cx("grid gap-1 px-4 py-3 text-sm", !j.enabled && "opacity-60")}>
            <div className="flex flex-wrap items-center gap-2">
              <span className="min-w-0 truncate font-medium" dir="auto">
                {j.name}
              </span>
              {j.state === "pending" && <Badge tone="warn">{t("sites.state.pending")}</Badge>}
              {j.state === "error" && <Badge tone="crit">{t("sites.state.error")}</Badge>}
              {!j.enabled && <Badge>{t("cron.paused")}</Badge>}
              <span className="ms-auto flex gap-1">
                <label className="me-1 flex items-center gap-1.5 text-xs text-muted">
                  <input
                    type="checkbox"
                    checked={j.enabled}
                    onChange={(e) =>
                      act(() =>
                        api.put(`/cron/${j.id}`, {
                          name: j.name,
                          schedule: j.schedule,
                          timezone: j.timezone,
                          command: j.command,
                          timeout_seconds: j.timeout_seconds,
                          service: j.service ?? "",
                          enabled: e.target.checked,
                        }),
                      )
                    }
                    className="accent-[var(--accent)]"
                  />
                  {t("cron.on")}
                </label>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => act(() => api.post(`/cron/${j.id}/run`))}
                  disabled={j.applied_generation === 0}
                  aria-label={t("cron.run_now")}
                  title={t("cron.run_now")}
                >
                  <Play className="size-4" aria-hidden />
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setViewing(j)}
                  disabled={j.applied_generation === 0}
                  aria-label={t("cron.output")}
                  title={t("cron.output")}
                >
                  <ScrollText className="size-4" aria-hidden />
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setEditing(j)} aria-label={t("cron.edit")} title={t("cron.edit")}>
                  <Pencil className="size-4" aria-hidden />
                </Button>
                <Button size="sm" variant="ghost" onClick={() => act(() => api.del(`/cron/${j.id}`))} aria-label={t("cron.delete")} title={t("cron.delete")}>
                  <Trash2 className="size-4" aria-hidden />
                </Button>
              </span>
            </div>
            <div className="flex flex-wrap gap-x-3 text-muted">
              <span className="font-mono text-xs" dir="ltr">
                {j.schedule}
              </span>
              <span className="text-xs">{j.timezone}</span>
              {j.next_run && j.enabled && <span className="text-xs">{t("cron.next", { when: formatRelative(j.next_run, i18n.language) })}</span>}
            </div>
            {j.state === "error" && j.message && <p className="text-xs break-words text-crit">{j.message}</p>}
          </li>
        ))}
      </ul>
      <CronDialog open={adding || editing !== null} job={editing} target={{ siteId, appId }} onClose={() => (setAdding(false), setEditing(null))} />
      {viewing && <CronOutput job={viewing} onClose={() => setViewing(null)} />}
    </Card>
  );
}

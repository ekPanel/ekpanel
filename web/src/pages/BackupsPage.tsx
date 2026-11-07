import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArchiveRestore, ArrowLeft, CloudUpload, Database as DatabaseIcon, Globe, HardDrive, Loader2, Play, ShieldAlert, Trash2 } from "lucide-react";
import { api, ApiError, errorMessage, type BackupRepo, type BackupScheduleKind, type BackupSnapshot, type Database, type Server, type Site } from "../lib/api";
import { formatDateTime, formatRelative } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, Dialog, ErrorAlert, Field, Spinner, cx, type Tone } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { useTasks } from "../components/Tasks";

const statusTone: Record<NonNullable<BackupRepo["last_status"]>, Tone> = {
  succeeded: "ok",
  partial: "warn",
  failed: "crit",
};

/** The UTC hour for a local hour today (time zones with half hours round down). */
const localToUTCHour = (h: number) => {
  const d = new Date();
  d.setHours(h, 0, 0, 0);
  return d.getUTCHours();
};
const utcToLocalHour = (h: number) => {
  const d = new Date();
  d.setUTCHours(h, 0, 0, 0);
  return d.getHours();
};

function useScheduleText() {
  const { t, i18n } = useTranslation();
  return (r: Pick<BackupRepo, "schedule" | "hour_utc">) => {
    const at = new Intl.DateTimeFormat(i18n.language, {
      hour: "numeric",
      minute: "2-digit",
    }).format(new Date(new Date().setUTCHours(r.hour_utc, 0, 0, 0)));
    return t(`backups.schedule_text.${r.schedule}`, { time: at });
  };
}

interface ScheduleValue {
  schedule: BackupScheduleKind;
  localHour: number;
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
}

function ScheduleFields({ value, onChange }: { value: ScheduleValue; onChange: (v: ScheduleValue) => void }) {
  const { t, i18n } = useTranslation();
  const hourLabel = (h: number) =>
    new Intl.DateTimeFormat(i18n.language, {
      hour: "numeric",
      minute: "2-digit",
    }).format(new Date(2000, 0, 1, h));
  return (
    <div className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("backups.frequency")}</span>
          <select
            value={value.schedule}
            onChange={(e) =>
              onChange({
                ...value,
                schedule: e.target.value as BackupScheduleKind,
              })
            }
            className="h-10 rounded-lg border border-line bg-surface px-3 text-ink"
          >
            {(["daily", "hourly", "weekly", "off"] as const).map((s) => (
              <option key={s} value={s}>
                {t(`backups.frequency_options.${s}`)}
              </option>
            ))}
          </select>
        </label>
        {(value.schedule === "daily" || value.schedule === "weekly") && (
          <label className="grid gap-1.5">
            <span className="text-sm font-medium">{t("backups.time")}</span>
            <select
              value={value.localHour}
              onChange={(e) => onChange({ ...value, localHour: Number(e.target.value) })}
              className="h-10 rounded-lg border border-line bg-surface px-3 text-ink"
            >
              {Array.from({ length: 24 }, (_, h) => (
                <option key={h} value={h}>
                  {hourLabel(h)}
                </option>
              ))}
            </select>
          </label>
        )}
      </div>
      <fieldset className="grid gap-2">
        <legend className="mb-1 text-sm font-medium">{t("backups.keep")}</legend>
        <div className="grid grid-cols-3 gap-3">
          {(["keep_daily", "keep_weekly", "keep_monthly"] as const).map((k) => (
            <Field
              key={k}
              label={t(`backups.${k}`)}
              type="number"
              min={0}
              max={365}
              value={String(value[k])}
              onChange={(e) =>
                onChange({
                  ...value,
                  [k]: Math.max(0, Number(e.target.value) || 0),
                })
              }
            />
          ))}
        </div>
        <p className="text-sm text-muted">{t("backups.keep_hint")}</p>
      </fieldset>
    </div>
  );
}

function RecoveryKeyBox({ keyValue }: { keyValue: string }) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-2 rounded-lg bg-warn-soft p-4 text-warn">
      <p className="flex items-start gap-2 text-sm font-medium">
        <ShieldAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
        {t("backups.recovery_once")}
      </p>
      <div className="flex items-center gap-2">
        <code className="rounded bg-surface px-2 py-1 font-mono text-sm break-all text-ink" dir="ltr">
          {keyValue}
        </code>
        <CopyButton text={keyValue} />
      </div>
    </div>
  );
}

// ---- Add ----

function AddBackupDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const servers = useQuery({
    queryKey: ["servers"],
    queryFn: () => api.get<Server[]>("/servers"),
    enabled: open,
  });
  const ready = (servers.data ?? []).filter((s) => s.enrolled_at);
  const [server, setServer] = useState("");
  const [name, setName] = useState("");
  const [kind, setKind] = useState<"local" | "s3">("local");
  const [s3, setS3] = useState({
    endpoint: "",
    bucket: "",
    prefix: "",
    region: "",
    access_key: "",
    secret_key: "",
  });
  const [connect, setConnect] = useState(false);
  const [recoveryKey, setRecoveryKey] = useState("");
  const [schedule, setSchedule] = useState<ScheduleValue>({
    schedule: "daily",
    localHour: 2,
    keep_daily: 7,
    keep_weekly: 4,
    keep_monthly: 6,
  });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<{
    repo: BackupRepo;
    recovery_key?: string;
  } | null>(null);

  useEffect(() => {
    if (!open) return;
    setCreated(null);
    setError("");
    setRecoveryKey("");
    setConnect(false);
  }, [open]);
  useEffect(() => {
    if (!server && ready.length) setServer(ready[0].id);
  }, [ready, server]);
  useEffect(() => {
    setName((n) => (n && n !== t("backups.default_name.local") && n !== t("backups.default_name.s3") ? n : t(`backups.default_name.${kind}`)));
  }, [kind, t]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const r = await api.post<{ repo: BackupRepo; recovery_key?: string }>("/backups", {
        server_id: server,
        name,
        kind,
        s3: kind === "s3" ? s3 : null,
        recovery_key: connect && kind === "s3" ? recoveryKey : "",
        schedule: schedule.schedule,
        hour_utc: localToUTCHour(schedule.localHour),
        keep_daily: schedule.keep_daily,
        keep_weekly: schedule.keep_weekly,
        keep_monthly: schedule.keep_monthly,
      });
      qc.invalidateQueries({ queryKey: ["backups"] });
      if (r.recovery_key) setCreated(r);
      else {
        onClose();
        navigate(`/backups/${r.repo.id}`);
      }
    } catch (err) {
      // Setup failures (an unreachable endpoint, refused keys) are only useful with restic's own words.
      setError(errorMessage(t, err) + (err instanceof ApiError && err.code === "agent_failed" ? ` ${err.message}` : ""));
    } finally {
      setBusy(false);
    }
  };
  const set = (k: keyof typeof s3) => (e: React.ChangeEvent<HTMLInputElement>) => setS3({ ...s3, [k]: e.target.value });

  return (
    <Dialog open={open} onClose={onClose} title={created ? t("backups.created_title") : t("backups.add")} wide>
      {created ? (
        <div className="grid gap-4">
          <RecoveryKeyBox keyValue={created.recovery_key!} />
          <p className="text-sm text-muted">{t("backups.recovery_hint")}</p>
          <div className="flex justify-end">
            <Button variant="primary" onClick={() => (onClose(), navigate(`/backups/${created.repo.id}`))}>
              {t("backups.saved_it")}
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-5">
          <ErrorAlert>{error}</ErrorAlert>
          {ready.length > 1 && (
            <label className="grid gap-1.5">
              <span className="text-sm font-medium">{t("sites.server")}</span>
              <select value={server} onChange={(e) => setServer(e.target.value)} className="h-10 rounded-lg border border-line bg-surface px-3 text-ink">
                {ready.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
            </label>
          )}
          <fieldset className="grid gap-2">
            <legend className="mb-1.5 text-sm font-medium">{t("backups.where")}</legend>
            <div className="grid gap-2 sm:grid-cols-2">
              {(["local", "s3"] as const).map((k) => (
                <label
                  key={k}
                  className={cx("flex cursor-pointer gap-3 rounded-lg border p-3", kind === k ? "border-accent bg-accent-soft" : "border-line hover:bg-sunk")}
                >
                  <input type="radio" name="kind" checked={kind === k} onChange={() => setKind(k)} className="mt-1 accent-[var(--accent)]" />
                  <span className="grid gap-0.5">
                    <span className="font-medium">{t(`backups.kind.${k}`)}</span>
                    <span className="text-sm text-muted">{t(`backups.kind_hint.${k}`)}</span>
                  </span>
                </label>
              ))}
            </div>
          </fieldset>
          {kind === "s3" && (
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label={t("backups.s3.endpoint")} hint={t("backups.s3.endpoint_hint")} value={s3.endpoint} onChange={set("endpoint")} dir="ltr" required />
              <Field label={t("backups.s3.bucket")} value={s3.bucket} onChange={set("bucket")} dir="ltr" required />
              <Field label={t("backups.s3.prefix")} value={s3.prefix} onChange={set("prefix")} dir="ltr" placeholder="ekpanel/web-1" />
              <Field label={t("backups.s3.region")} value={s3.region} onChange={set("region")} dir="ltr" placeholder="us-east-1" />
              <Field label={t("backups.s3.access_key")} value={s3.access_key} onChange={set("access_key")} dir="ltr" autoComplete="off" required />
              <Field
                label={t("backups.s3.secret_key")}
                type="password"
                value={s3.secret_key}
                onChange={set("secret_key")}
                dir="ltr"
                autoComplete="new-password"
                required
              />
              <p className="text-sm text-muted sm:col-span-2">{t("backups.s3.secret_note")}</p>
            </div>
          )}
          <Field label={t("backups.name")} value={name} onChange={(e) => setName(e.target.value)} maxLength={100} required />
          <ScheduleFields value={schedule} onChange={setSchedule} />
          {kind === "s3" && (
            <label className="flex items-start gap-2 text-sm">
              <input type="checkbox" checked={connect} onChange={(e) => setConnect(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
              <span>{t("backups.connect_existing")}</span>
            </label>
          )}
          {connect && kind === "s3" && (
            <Field
              label={t("backups.recovery_key")}
              value={recoveryKey}
              onChange={(e) => setRecoveryKey(e.target.value)}
              dir="ltr"
              className="[&_input]:font-mono"
              required
            />
          )}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!server || !name}>
              {connect && kind === "s3" ? t("backups.connect") : t("backups.create")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

// ---- List ----

function LastRun({ repo }: { repo: BackupRepo }) {
  const { t, i18n } = useTranslation();
  if (!repo.last_run_at || !repo.last_status) return <span className="text-sm text-muted">{t("backups.never_run")}</span>;
  return (
    <span className="flex flex-wrap items-center gap-2 text-sm">
      <Badge tone={statusTone[repo.last_status]}>{t(`backups.status.${repo.last_status}`)}</Badge>
      <span className="text-muted">{formatRelative(repo.last_run_at, i18n.language)}</span>
    </span>
  );
}

export function BackupsPage() {
  const { t, i18n } = useTranslation();
  const scheduleText = useScheduleText();
  const repos = useQuery({
    queryKey: ["backups"],
    queryFn: () => api.get<BackupRepo[]>("/backups"),
    refetchInterval: 15000,
  });
  const [params, setParams] = useSearchParams();
  const adding = params.get("add") === "1";
  const setAdding = (on: boolean) => setParams(on ? { add: "1" } : {}, { replace: true });
  const addButton = (
    <Button variant="primary" onClick={() => setAdding(true)}>
      <CloudUpload className="size-4" aria-hidden />
      {t("backups.add")}
    </Button>
  );
  return (
    <>
      <PageHeader title={t("nav.backups")} description={t("backups.intro")} actions={repos.data?.length ? addButton : undefined} />
      {repos.isPending && <Spinner />}
      {repos.error && <ErrorAlert>{errorMessage(t, repos.error)}</ErrorAlert>}
      {repos.data?.length === 0 && <EmptyState icon={<ArchiveRestore />} title={t("backups.empty.title")} body={t("backups.empty.body")} action={addButton} />}
      <div className="grid gap-4 lg:grid-cols-2">
        {repos.data?.map((r) => (
          <Link key={r.id} to={`/backups/${r.id}`} className="group">
            <Card className="grid h-full gap-3 p-5 transition-colors group-hover:border-accent">
              <div className="flex items-start gap-3">
                <span className="grid size-10 shrink-0 place-items-center rounded-lg bg-accent-soft text-accent">
                  {r.kind === "s3" ? <CloudUpload className="size-5" /> : <HardDrive className="size-5" />}
                </span>
                <div className="min-w-0">
                  <h2 className="truncate font-semibold">{r.name}</h2>
                  <p className="truncate text-sm text-muted">
                    {r.server_name} · {scheduleText(r)}
                  </p>
                </div>
              </div>
              <LastRun repo={r} />
              {r.next_run_at && (
                <p className="text-xs text-muted">
                  {t("backups.next", {
                    when: formatDateTime(r.next_run_at, i18n.language),
                  })}
                </p>
              )}
            </Card>
          </Link>
        ))}
      </div>
      <AddBackupDialog open={adding} onClose={() => setAdding(false)} />
    </>
  );
}

// ---- Repository page ----

export function BackupRepoPage() {
  const { id = "" } = useParams();
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const scheduleText = useScheduleText();
  const repo = useQuery({
    queryKey: ["backups", id],
    queryFn: () => api.get<BackupRepo>(`/backups/${id}`),
    refetchInterval: 10000,
    retry: (n, err) => !(err instanceof ApiError && err.status === 404) && n < 2,
  });
  const snapshots = useQuery({
    queryKey: ["snapshots", id],
    queryFn: () => api.get<{ snapshots: BackupSnapshot[] }>(`/backups/${id}/snapshots`),
    retry: false,
  });
  const serverId = repo.data?.server_id;
  const sites = useQuery({
    queryKey: ["sites", serverId],
    queryFn: () => api.get<Site[]>(`/sites?server_id=${serverId}`),
    enabled: !!serverId,
  });
  const dbs = useQuery({
    queryKey: ["databases", serverId],
    queryFn: () => api.get<Database[]>(`/databases?server_id=${serverId}`),
    enabled: !!serverId,
  });
  const tasks = useTasks(`server_id=${encodeURIComponent(serverId ?? "")}&limit=20`, !!serverId);
  const running = tasks.data?.find((x) => (x.kind === "backup.run" || x.kind === "backup.restore") && (x.status === "queued" || x.status === "running"));
  const [error, setError] = useState("");
  const [editing, setEditing] = useState(false);
  const [restore, setRestore] = useState<BackupSnapshot | null>(null);
  const [removing, setRemoving] = useState(false);

  // Refresh the snapshot list when a backup or restore finishes.
  const runningId = running?.id;
  useEffect(() => {
    if (!runningId) qc.invalidateQueries({ queryKey: ["snapshots", id] });
  }, [runningId, qc, id]);

  const groups = useMemo(() => {
    const g = new Map<
      string,
      {
        kind: BackupSnapshot["kind"];
        name: string;
        itemId: string;
        snaps: BackupSnapshot[];
      }
    >();
    for (const s of snapshots.data?.snapshots ?? []) {
      const key = s.kind + s.item_id;
      if (!g.has(key))
        g.set(key, {
          kind: s.kind,
          name: s.name,
          itemId: s.item_id,
          snaps: [],
        });
      g.get(key)!.snaps.push(s);
    }
    return [...g.values()].sort((a, b) => (a.kind === b.kind ? a.name.localeCompare(b.name) : a.kind === "site" ? -1 : 1));
  }, [snapshots.data]);

  if (repo.isPending) return <Spinner />;
  if (repo.error || !repo.data) return <ErrorAlert>{errorMessage(t, repo.error)}</ErrorAlert>;
  const r = repo.data;
  const itemExists = (kind: string, itemId: string) => (kind === "site" ? sites.data?.some((s) => s.id === itemId) : dbs.data?.some((d) => d.id === itemId));

  const runNow = async () => {
    setError("");
    try {
      await api.post(`/backups/${id}/run`);
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };

  return (
    <>
      <Link to="/backups" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("nav.backups")}
      </Link>
      <PageHeader
        title={r.name}
        description={`${r.server_name} · ${scheduleText(r)}`}
        actions={
          <>
            <Button size="sm" variant="primary" onClick={runNow} disabled={!!running}>
              {running ? <Loader2 className="size-4 animate-spin" aria-hidden /> : <Play className="size-4" aria-hidden />}
              {running ? t(`backups.running.${running.kind === "backup.run" ? "backup" : "restore"}`) : t("backups.run_now")}
            </Button>
            <Button size="sm" onClick={() => setEditing(true)}>
              {t("backups.edit_schedule")}
            </Button>
          </>
        }
      />
      <div className="grid gap-6">
        <ErrorAlert>{error}</ErrorAlert>
        <Card className="grid gap-2 p-5 text-sm">
          <div className="flex flex-wrap items-center gap-3">
            <LastRun repo={r} />
            {r.next_run_at && (
              <span className="text-muted">
                {t("backups.next", {
                  when: formatDateTime(r.next_run_at, i18n.language),
                })}
              </span>
            )}
          </div>
          {r.last_message && <p className="break-words text-warn">{r.last_message}</p>}
          <p className="text-muted">
            {t("backups.location")}:{" "}
            <span className="font-mono" dir="ltr">
              {r.location}
            </span>
          </p>
        </Card>

        <Card className="overflow-hidden">
          <h2 className="border-b border-line px-4 py-3 font-semibold">{t("backups.snapshots")}</h2>
          {snapshots.isPending && <Spinner />}
          {snapshots.error && (
            <div className="p-4">
              <ErrorAlert>{errorMessage(t, snapshots.error)}</ErrorAlert>
            </div>
          )}
          {snapshots.data && groups.length === 0 && <p className="px-4 py-8 text-center text-muted">{t("backups.no_snapshots")}</p>}
          <div className="divide-y divide-line">
            {groups.map((g) => (
              <details key={g.kind + g.itemId} className="group px-4 py-3" open={groups.length <= 4}>
                <summary className="flex cursor-pointer items-center gap-2">
                  {g.kind === "site" ? <Globe className="size-4 text-muted" aria-hidden /> : <DatabaseIcon className="size-4 text-muted" aria-hidden />}
                  <span className="font-medium" dir="auto">
                    {g.name}
                  </span>
                  <span className="text-sm text-muted">{t("backups.count", { count: g.snaps.length })}</span>
                  {!itemExists(g.kind, g.itemId) && <Badge>{t("backups.item_gone")}</Badge>}
                </summary>
                <ul className="mt-2 grid gap-1">
                  {g.snaps.map((s) => (
                    <li key={s.id} className="flex flex-wrap items-center gap-3 rounded-lg px-2 py-1.5 text-sm hover:bg-sunk">
                      <span className="tabular-nums">{formatDateTime(s.time, i18n.language)}</span>
                      <span className="text-muted">{formatRelative(s.time, i18n.language)}</span>
                      <span className="font-mono text-xs text-muted" dir="ltr">
                        {s.id.slice(0, 8)}
                      </span>
                      {itemExists(g.kind, g.itemId) && (
                        <Button size="sm" variant="ghost" className="ms-auto" onClick={() => setRestore(s)} disabled={!!running}>
                          <ArchiveRestore className="size-4" aria-hidden />
                          {t("backups.restore")}
                        </Button>
                      )}
                    </li>
                  ))}
                </ul>
              </details>
            ))}
          </div>
        </Card>

        <Card className="grid gap-3 p-6">
          <h2 className="text-lg font-semibold">{t("backups.remove_title")}</h2>
          <p className="text-sm text-muted">{t(r.kind === "s3" ? "backups.remove_body" : "backups.remove_body_local", { path: r.location })}</p>
          <div>
            <Button variant="danger" onClick={() => setRemoving(true)}>
              <Trash2 className="size-4" aria-hidden />
              {t("backups.remove")}
            </Button>
          </div>
        </Card>
      </div>

      <EditScheduleDialog open={editing} repo={r} onClose={() => setEditing(false)} />
      {restore && <RestoreDialog repoId={id} snapshot={restore} onClose={() => setRestore(null)} />}
      <Dialog open={removing} onClose={() => setRemoving(false)} title={t("backups.remove_title")}>
        <p className="text-muted">{t(r.kind === "s3" ? "backups.remove_confirm" : "backups.remove_confirm_local", { path: r.location })}</p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => setRemoving(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            variant="danger"
            onClick={async () => {
              await api.del(`/backups/${id}`);
              qc.invalidateQueries({ queryKey: ["backups"] });
              navigate("/backups", { replace: true });
            }}
          >
            {t("backups.remove")}
          </Button>
        </div>
      </Dialog>
    </>
  );
}

function EditScheduleDialog({ open, repo, onClose }: { open: boolean; repo: BackupRepo; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [value, setValue] = useState<ScheduleValue>({
    schedule: repo.schedule,
    localHour: utcToLocalHour(repo.hour_utc),
    keep_daily: repo.keep_daily,
    keep_weekly: repo.keep_weekly,
    keep_monthly: repo.keep_monthly,
  });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <Dialog open={open} onClose={onClose} title={t("backups.edit_schedule")}>
      <form
        className="grid gap-5"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            const r = await api.patch<BackupRepo>(`/backups/${repo.id}`, {
              schedule: value.schedule,
              hour_utc: localToUTCHour(value.localHour),
              keep_daily: value.keep_daily,
              keep_weekly: value.keep_weekly,
              keep_monthly: value.keep_monthly,
            });
            qc.setQueryData(["backups", repo.id], r);
            qc.invalidateQueries({ queryKey: ["backups"] });
            onClose();
          } catch (err) {
            setError(errorMessage(t, err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <ErrorAlert>{error}</ErrorAlert>
        <ScheduleFields value={value} onChange={setValue} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy}>
            {t("sites.save")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function RestoreDialog({ repoId, snapshot, onClose }: { repoId: string; snapshot: BackupSnapshot; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [mode, setMode] = useState<"folder" | "replace">("folder");
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const isSite = snapshot.kind === "site";
  const confirmed = isSite ? true : typed.trim() === snapshot.name;
  return (
    <Dialog open onClose={onClose} title={t("backups.restore_title", { name: snapshot.name })}>
      <form
        className="grid gap-5"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            await api.post(
              `/backups/${repoId}/restore`,
              isSite ? { snapshot_id: snapshot.id, site_id: snapshot.item_id, mode } : { snapshot_id: snapshot.id, database_id: snapshot.item_id },
            );
            qc.invalidateQueries({ queryKey: ["tasks"] });
            onClose();
          } catch (err) {
            setError(errorMessage(t, err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <p className="text-sm text-muted">
          {t("backups.restore_from", {
            when: formatDateTime(snapshot.time, i18n.language),
          })}
        </p>
        <ErrorAlert>{error}</ErrorAlert>
        {isSite ? (
          <div className="grid gap-2">
            {(["folder", "replace"] as const).map((m) => (
              <label
                key={m}
                className={cx("flex cursor-pointer gap-3 rounded-lg border p-3", mode === m ? "border-accent bg-accent-soft" : "border-line hover:bg-sunk")}
              >
                <input type="radio" name="mode" checked={mode === m} onChange={() => setMode(m)} className="mt-1 accent-[var(--accent)]" />
                <span className="grid gap-0.5">
                  <span className="font-medium">{t(`backups.mode.${m}`)}</span>
                  <span className="text-sm text-muted">{t(`backups.mode_hint.${m}`)}</span>
                </span>
              </label>
            ))}
          </div>
        ) : (
          <>
            <p className="rounded-lg bg-warn-soft p-3 text-sm text-warn">{t("backups.db_restore_warning")}</p>
            <Field
              label={t("databases.delete.confirm", { name: snapshot.name })}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              className="[&_input]:font-mono"
              autoComplete="off"
            />
          </>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant={isSite && mode === "folder" ? "primary" : "danger"} loading={busy} disabled={!confirmed}>
            {t("backups.restore")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

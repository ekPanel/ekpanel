import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Database as DatabaseIcon, KeyRound, Loader2, Plus, ShieldAlert, Trash2 } from "lucide-react";
import { api, ApiError, errorMessage, type Database, type DatabaseWithPassword, type Server } from "../lib/api";
import { formatBytes, formatDateTime } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, Dialog, ErrorAlert, Field, Spinner, cx, type Tone } from "../components/ui";
import { EmptyState } from "./ServerParts";

const engineLabel: Record<Database["engine"], string> = { mariadb: "MariaDB", postgresql: "PostgreSQL" };
const stateTone: Record<Database["state"], Tone> = { pending: "warn", active: "ok", error: "crit", deleting: "neutral" };

/** How applications on the same server connect to this database. */
export function connectionInfo(d: Pick<Database, "engine" | "name" | "username">) {
  return d.engine === "mariadb"
    ? { host: "localhost", port: 3306, url: `mysql://${d.username}:PASSWORD@localhost:3306/${d.name}` }
    : { host: "127.0.0.1", port: 5432, url: `postgresql://${d.username}:PASSWORD@127.0.0.1:5432/${d.name}` };
}

export function DatabaseStateBadge({ db }: { db: Pick<Database, "state"> }) {
  const { t } = useTranslation();
  const busy = db.state === "pending" || db.state === "deleting";
  return (
    <Badge tone={stateTone[db.state]}>
      {busy && <Loader2 className="size-3 animate-spin" aria-hidden />}
      {t(`databases.state.${db.state}`)}
    </Badge>
  );
}

export function useDatabases(serverId?: string) {
  return useQuery({
    queryKey: ["databases", serverId ?? "all"],
    queryFn: () => api.get<Database[]>(`/databases${serverId ? `?server_id=${encodeURIComponent(serverId)}` : ""}`),
    refetchInterval: (q) => (q.state.data?.some((d) => d.state === "pending" || d.state === "deleting") ? 2000 : 15000),
  });
}

function InfoRow({ label, value, copy = value, mono = true }: { label: string; value: ReactNode; copy?: ReactNode; mono?: boolean }) {
  return (
    <div className="contents">
      <dt className="text-muted">{label}</dt>
      <dd className="flex min-w-0 items-center gap-1">
        <span className={cx("truncate", mono && "font-mono")} dir="ltr">
          {value}
        </span>
        {typeof copy === "string" && <CopyButton text={copy} className="-my-1 shrink-0" />}
      </dd>
    </div>
  );
}

/** The one moment a password is visible: after creating a database or resetting it. */
export function PasswordReveal({ result }: { result: DatabaseWithPassword }) {
  const { t } = useTranslation();
  const d = result.database;
  const c = connectionInfo(d);
  return (
    <div className="grid gap-4">
      <div className="flex items-start gap-3 rounded-lg bg-warn-soft px-4 py-3 text-warn">
        <ShieldAlert className="mt-0.5 size-5 shrink-0" aria-hidden />
        <p className="text-sm">{t("databases.password_once")}</p>
      </div>
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
        <InfoRow label={t("databases.host")} value={c.host} />
        <InfoRow label={t("databases.port")} value={String(c.port)} />
        <InfoRow label={t("databases.name")} value={d.name} />
        <InfoRow label={t("databases.username")} value={d.username} />
        <InfoRow label={t("databases.password")} value={result.password} />
      </dl>
    </div>
  );
}

// ---- Add dialog ----

export function AddDatabaseDialog({ open, onClose, serverId }: { open: boolean; onClose: () => void; serverId?: string }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers"), enabled: open });
  const ready = (servers.data ?? []).filter((s) => s.enrolled_at);
  const [server, setServer] = useState("");
  const [engine, setEngine] = useState<Database["engine"]>("mariadb");
  const [name, setName] = useState("");
  const [username, setUsername] = useState("");
  const [userTouched, setUserTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<DatabaseWithPassword | null>(null);

  useEffect(() => {
    if (!open) return;
    setName("");
    setUsername("");
    setUserTouched(false);
    setError("");
    setResult(null);
  }, [open]);
  useEffect(() => {
    if (serverId) setServer(serverId);
    else if (!server && ready.length > 0) setServer(ready[0].id);
  }, [serverId, ready, server]);

  const clean = (v: string) => v.toLowerCase().replace(/[^a-z0-9_]/g, "_").slice(0, 32);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      setResult(await api.post<DatabaseWithPassword>("/databases", { server_id: server, engine, name, username: username || name }));
      qc.invalidateQueries({ queryKey: ["databases"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} title={result ? t("databases.created_title", { name: result.database.name }) : t("databases.add")} wide>
      {result ? (
        <>
          <PasswordReveal result={result} />
          <div className="flex justify-end gap-2">
            <Link to={`/databases/${result.database.id}`} onClick={onClose} className="inline-flex h-10 items-center rounded-lg px-4 font-medium text-accent hover:bg-sunk">
              {t("databases.open")}
            </Link>
            <Button variant="primary" onClick={onClose}>
              {t("databases.saved_it")}
            </Button>
          </div>
        </>
      ) : servers.isSuccess && ready.length === 0 ? (
        <p className="text-muted">{t("sites.need_server")}</p>
      ) : (
        <form onSubmit={submit} className="grid gap-5">
          <ErrorAlert>{error}</ErrorAlert>
          <fieldset className="grid gap-2">
            <legend className="mb-1.5 text-sm font-medium">{t("databases.engine")}</legend>
            <div className="grid gap-2 sm:grid-cols-2">
              {(["mariadb", "postgresql"] as const).map((e) => (
                <label key={e} className={cx("flex cursor-pointer gap-3 rounded-lg border p-3", engine === e ? "border-accent bg-accent-soft" : "border-line hover:bg-sunk")}>
                  <input type="radio" name="engine" checked={engine === e} onChange={() => setEngine(e)} className="mt-1 accent-[var(--accent)]" />
                  <span className="grid gap-0.5">
                    <span className="font-medium">{engineLabel[e]}</span>
                    <span className="text-sm text-muted">{t(`databases.engine_hint.${e}`)}</span>
                  </span>
                </label>
              ))}
            </div>
          </fieldset>
          <Field
            label={t("databases.name")}
            hint={t("databases.name_hint")}
            value={name}
            onChange={(e) => {
              setName(clean(e.target.value));
              if (!userTouched) setUsername(clean(e.target.value));
            }}
            className="[&_input]:font-mono"
            required
            autoComplete="off"
            spellCheck={false}
          />
          <Field
            label={t("databases.username")}
            value={username}
            onChange={(e) => {
              setUserTouched(true);
              setUsername(clean(e.target.value));
            }}
            className="[&_input]:font-mono"
            autoComplete="off"
            spellCheck={false}
          />
          {ready.length > 1 && !serverId && (
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
          <p className="text-sm text-muted">{t("databases.password_note")}</p>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!name || !server}>
              {t("databases.create")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

// ---- List ----

export function DatabasesPage() {
  const { t, i18n } = useTranslation();
  const dbs = useDatabases();
  const [params, setParams] = useSearchParams();
  const adding = params.get("add") === "1";
  const setAdding = (on: boolean) => setParams(on ? { add: "1" } : {}, { replace: true });
  const addButton = (
    <Button variant="primary" onClick={() => setAdding(true)}>
      <Plus className="size-4" aria-hidden />
      {t("databases.add")}
    </Button>
  );
  return (
    <>
      <PageHeader title={t("nav.databases")} description={t("databases.intro")} actions={dbs.data?.length ? addButton : undefined} />
      {dbs.isPending && <Spinner />}
      {dbs.error && <ErrorAlert>{errorMessage(t, dbs.error)}</ErrorAlert>}
      {dbs.data?.length === 0 && <EmptyState icon={<DatabaseIcon />} title={t("databases.empty.title")} body={t("databases.empty.body")} action={addButton} />}
      {!!dbs.data?.length && (
        <Card className="overflow-x-auto">
          <table className="w-full min-w-[680px] text-sm">
            <thead>
              <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                <th className="px-4 py-3 text-start font-semibold">{t("databases.name")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("databases.engine")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("databases.username")}</th>
                <th className="px-4 py-3 text-end font-semibold">{t("databases.size")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("sites.col.server")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("sites.col.status")}</th>
              </tr>
            </thead>
            <tbody>
              {dbs.data.map((d) => (
                <tr key={d.id} className="border-b border-line last:border-0 hover:bg-sunk/60">
                  <td className="px-4 py-3">
                    <Link to={`/databases/${d.id}`} className="font-mono font-semibold hover:text-accent">
                      {d.name}
                    </Link>
                  </td>
                  <td className="px-4 py-3">{engineLabel[d.engine]}</td>
                  <td className="px-4 py-3 font-mono text-muted">{d.username}</td>
                  <td className="px-4 py-3 text-end tabular-nums">{d.size_bytes != null ? formatBytes(d.size_bytes, i18n.language) : "—"}</td>
                  <td className="px-4 py-3 text-muted">{d.server_name}</td>
                  <td className="px-4 py-3">
                    <DatabaseStateBadge db={d} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      <AddDatabaseDialog open={adding} onClose={() => setAdding(false)} />
    </>
  );
}

// ---- Detail ----

export function DatabaseDetailPage() {
  const { id = "" } = useParams();
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["database", id],
    queryFn: () => api.get<Database>(`/databases/${id}`),
    refetchInterval: (x) => (x.state.data && ["pending", "deleting"].includes(x.state.data.state) ? 1500 : 15000),
    retry: (n, err) => !(err instanceof ApiError && err.status === 404) && n < 2,
  });
  const [reset, setReset] = useState<DatabaseWithPassword | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const gone = q.error instanceof ApiError && q.error.status === 404;
  useEffect(() => {
    if (gone) {
      qc.invalidateQueries({ queryKey: ["databases"] });
      navigate("/databases", { replace: true });
    }
  }, [gone, navigate, qc]);

  if (q.isPending || gone) return <Spinner />;
  if (q.error || !q.data) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;
  const d = q.data;
  const c = connectionInfo(d);

  const resetPassword = async () => {
    setBusy(true);
    setError("");
    try {
      const r = await api.post<DatabaseWithPassword>(`/databases/${d.id}/password`);
      qc.setQueryData(["database", id], r.database);
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setReset(r);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <Link to="/databases" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("nav.databases")}
      </Link>
      <PageHeader title={d.name} description={`${engineLabel[d.engine]} · ${d.server_name}`} actions={<DatabaseStateBadge db={d} />} />
      <div className="grid gap-6">
        <ErrorAlert>{error}</ErrorAlert>
        {d.state === "error" && (
          <div className="grid gap-1 rounded-lg bg-crit-soft p-4 text-crit" role="alert">
            <p className="font-semibold">{t("databases.error_title")}</p>
            <p className="text-sm break-words">{d.message}</p>
          </div>
        )}
        {d.state === "pending" && (
          <div className="flex items-center gap-3 rounded-lg bg-warn-soft px-4 py-3 text-warn" role="status">
            <Loader2 className="size-5 shrink-0 animate-spin" aria-hidden />
            {t("databases.applying")}
          </div>
        )}
        <div className="grid gap-6 xl:grid-cols-2">
          <Card className="grid content-start gap-4 p-6">
            <h2 className="text-lg font-semibold">{t("databases.connect")}</h2>
            <p className="text-sm text-muted">{t("databases.connect_hint")}</p>
            <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
              <InfoRow label={t("databases.host")} value={c.host} />
              <InfoRow label={t("databases.port")} value={String(c.port)} />
              <InfoRow label={t("databases.name")} value={d.name} />
              <InfoRow label={t("databases.username")} value={d.username} />
              <InfoRow label={t("databases.url")} value={c.url} />
            </dl>
          </Card>
          <div className="grid content-start gap-6">
            <Card className="grid gap-3 p-6">
              <h2 className="text-lg font-semibold">{t("databases.password")}</h2>
              {reset ? (
                <PasswordReveal result={reset} />
              ) : (
                <>
                  <p className="text-sm text-muted">{t("databases.reset_hint")}</p>
                  <div>
                    <Button onClick={resetPassword} loading={busy} disabled={d.state === "deleting"}>
                      <KeyRound className="size-4" aria-hidden />
                      {t("databases.reset")}
                    </Button>
                  </div>
                </>
              )}
            </Card>
            <Card className="grid gap-2 p-6 text-sm">
              <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2">
                <InfoRow label={t("databases.size")} value={d.size_bytes != null ? formatBytes(d.size_bytes, i18n.language) : "—"} copy={null} mono={false} />
                <InfoRow label={t("sites.created")} value={formatDateTime(d.created_at, i18n.language)} copy={null} mono={false} />
              </dl>
            </Card>
            <Card className="grid gap-3 p-6">
              <h2 className="text-lg font-semibold">{t("databases.delete.title_short")}</h2>
              <p className="text-sm text-muted">{t("databases.delete.body")}</p>
              <div>
                <Button variant="danger" onClick={() => setConfirmDelete(true)} disabled={d.state === "deleting"}>
                  <Trash2 className="size-4" aria-hidden />
                  {t("databases.delete.button")}
                </Button>
              </div>
            </Card>
          </div>
        </div>
      </div>
      <DeleteDatabaseDialog open={confirmDelete} db={d} onClose={() => setConfirmDelete(false)} />
    </>
  );
}

function DeleteDatabaseDialog({ open, db, onClose }: { open: boolean; db: Database; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <Dialog open={open} onClose={onClose} title={t("databases.delete.title", { name: db.name })}>
      <form
        className="grid gap-5"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          try {
            await api.del(`/databases/${db.id}`);
            qc.setQueryData(["database", db.id], { ...db, state: "deleting" });
            qc.invalidateQueries({ queryKey: ["tasks"] });
            onClose();
          } catch (err) {
            setError(errorMessage(t, err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <p className="text-muted">{t("databases.delete.warning")}</p>
        <ErrorAlert>{error}</ErrorAlert>
        <Field label={t("databases.delete.confirm", { name: db.name })} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" className="[&_input]:font-mono" />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="danger" loading={busy} disabled={typed.trim() !== db.name}>
            {t("databases.delete.button")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

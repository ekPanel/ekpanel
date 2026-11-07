import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Boxes, Download, ExternalLink, Lock, Play, Plus, RefreshCw, RotateCw, Square, Trash2 } from "lucide-react";
import { api, ApiError, errorMessage, type App, type AppContainer, type Server, type Task } from "../lib/api";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, Dialog, ErrorAlert, Field, Spinner, cx, type Tone } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { SiteStateBadge } from "./SitesPage";
import { useTasks } from "../components/Tasks";
import { useMe } from "../lib/session";
import { CronCard } from "../components/CronCard";
import { OwnerField } from "./CustomersPage";

const textarea = "rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm text-ink";
const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

/** The editable part of an app, as the form holds it. */
interface AppForm {
  image: string;
  compose: string;
  service: string;
  port: string;
  domain: string;
  tls: "auto" | "internal";
  env: string; // KEY=value lines, like a .env file
  volumes: string; // one container path per line
  memory: string;
  cpus: string;
  publicPorts: boolean;
}

const emptyForm: AppForm = {
  image: "",
  compose: "",
  service: "",
  port: "",
  domain: "",
  tls: "auto",
  env: "",
  volumes: "",
  memory: "",
  cpus: "",
  publicPorts: false,
};

function formFromApp(a: App): AppForm {
  return {
    image: a.image ?? "",
    compose: a.compose ?? "",
    service: a.service ?? "",
    port: a.container_port ? String(a.container_port) : "",
    domain: a.domain_display || "",
    tls: a.tls,
    env: a.env.map((e) => `${e.key}=${e.value}`).join("\n"),
    volumes: a.volumes.join("\n"),
    memory: a.memory_mb ? String(a.memory_mb) : "",
    cpus: a.cpus ? String(a.cpus) : "",
    publicPorts: a.public_ports,
  };
}

/** Reads KEY=value lines; blank lines and # comments are skipped. */
function parseEnv(text: string) {
  return text
    .split("\n")
    .map((l) => l.replace(/\r$/, ""))
    .filter((l) => l.trim() && !l.trimStart().startsWith("#"))
    .map((l) => {
      const i = l.indexOf("=");
      return i < 0 ? { key: l.trim(), value: "" } : { key: l.slice(0, i).trim(), value: l.slice(i + 1) };
    });
}

function requestBody(kind: App["kind"], f: AppForm) {
  const num = (s: string) => (s.trim() ? Number(s) : null);
  return {
    image: f.image,
    compose: f.compose,
    service: f.service,
    container_port: num(f.port),
    domain: f.domain,
    tls: f.tls,
    env: parseEnv(f.env),
    volumes:
      kind === "image"
        ? f.volumes
            .split("\n")
            .map((v) => v.trim())
            .filter(Boolean)
        : [],
    memory_mb: num(f.memory),
    cpus: num(f.cpus),
    public_ports: kind === "compose" && f.publicPorts,
  };
}

/** Ready-made settings for a few popular images; a full app catalogue comes later. */
const presets: { name: string; label: string; form: Partial<AppForm> }[] = [
  { name: "uptime-kuma", label: "Uptime Kuma", form: { image: "louislam/uptime-kuma:1", port: "3001", volumes: "/app/data" } },
  { name: "n8n", label: "n8n", form: { image: "docker.n8n.io/n8nio/n8n:latest", port: "5678", volumes: "/home/node/.n8n" } },
  { name: "web", label: "Nginx", form: { image: "nginx:alpine", port: "80" } },
];

function AppFields({ kind, value, onChange }: { kind: App["kind"]; value: AppForm; onChange: (v: AppForm) => void }) {
  const { t } = useTranslation();
  const set = <K extends keyof AppForm>(k: K, v: AppForm[K]) => onChange({ ...value, [k]: v });
  return (
    <div className="grid gap-4">
      {kind === "image" ? (
        <Field
          label={t("apps.image")}
          hint={t("apps.image_hint")}
          value={value.image}
          onChange={(e) => set("image", e.target.value)}
          dir="ltr"
          className="[&_input]:font-mono"
          required
        />
      ) : (
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("apps.compose")}</span>
          <textarea
            value={value.compose}
            onChange={(e) => set("compose", e.target.value)}
            rows={12}
            dir="ltr"
            spellCheck={false}
            className={textarea}
            required
            placeholder={"services:\n  web:\n    image: nginx:alpine"}
          />
          <span className="text-sm text-muted">{t("apps.compose_hint")}</span>
        </label>
      )}
      <div className={cx("grid gap-4", kind === "compose" ? "sm:grid-cols-2" : "")}>
        {kind === "compose" && (
          <Field label={t("apps.service")} hint={t("apps.service_hint")} value={value.service} onChange={(e) => set("service", e.target.value)} dir="ltr" />
        )}
        <Field
          label={t("apps.port")}
          hint={t("apps.port_hint")}
          value={value.port}
          onChange={(e) => set("port", e.target.value.replace(/\D/g, ""))}
          inputMode="numeric"
          dir="ltr"
          placeholder="8080"
        />
      </div>
      <div className="grid gap-4 sm:grid-cols-[1fr_auto]">
        <Field
          label={t("apps.domain")}
          hint={t("apps.domain_hint")}
          value={value.domain}
          onChange={(e) => set("domain", e.target.value)}
          dir="ltr"
          placeholder="app.example.com"
          disabled={!value.port}
        />
        <label className="grid content-start gap-1.5">
          <span className="text-sm font-medium">{t("sites.tls.label")}</span>
          <select value={value.tls} onChange={(e) => set("tls", e.target.value as AppForm["tls"])} className={select} disabled={!value.domain}>
            <option value="auto">{t("sites.tls.auto")}</option>
            <option value="internal">{t("sites.tls.internal")}</option>
          </select>
        </label>
      </div>
      <label className="grid gap-1.5">
        <span className="text-sm font-medium">{t("apps.env")}</span>
        <textarea
          value={value.env}
          onChange={(e) => set("env", e.target.value)}
          rows={4}
          dir="ltr"
          spellCheck={false}
          className={textarea}
          placeholder={"TZ=Asia/Dhaka\nAPP_SECRET=…"}
        />
        <span className="text-sm text-muted">{kind === "image" ? t("apps.env_hint_image") : t("apps.env_hint_compose")}</span>
      </label>
      {kind === "image" && (
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("apps.volumes")}</span>
          <textarea
            value={value.volumes}
            onChange={(e) => set("volumes", e.target.value)}
            rows={2}
            dir="ltr"
            spellCheck={false}
            className={textarea}
            placeholder="/data"
          />
          <span className="text-sm text-muted">{t("apps.volumes_hint")}</span>
        </label>
      )}
      <details className="rounded-lg border border-line px-3 py-2">
        <summary className="cursor-pointer text-sm font-medium">{t("apps.advanced")}</summary>
        <div className="grid gap-4 pt-3">
          <div className="grid gap-4 sm:grid-cols-2">
            <Field
              label={t("apps.memory")}
              value={value.memory}
              onChange={(e) => set("memory", e.target.value.replace(/\D/g, ""))}
              inputMode="numeric"
              dir="ltr"
              placeholder={t("apps.no_limit")}
            />
            <Field
              label={t("apps.cpus")}
              value={value.cpus}
              onChange={(e) => set("cpus", e.target.value.replace(/[^\d.]/g, ""))}
              inputMode="decimal"
              dir="ltr"
              placeholder={t("apps.no_limit")}
            />
          </div>
          {kind === "compose" && (
            <label className="flex items-start gap-2 text-sm">
              <input
                type="checkbox"
                checked={value.publicPorts}
                onChange={(e) => set("publicPorts", e.target.checked)}
                className="mt-1 accent-[var(--accent)]"
              />
              <span>
                <span className="font-medium">{t("apps.public_ports")}</span>
                <span className="block text-muted">{t("apps.public_ports_hint")}</span>
              </span>
            </label>
          )}
        </div>
      </details>
    </div>
  );
}

export function AppStateBadge({ app }: { app: App }) {
  const { t } = useTranslation();
  if (app.state === "active" && app.stopped) return <Badge>{t("apps.stopped")}</Badge>;
  return <SiteStateBadge site={app} />;
}

// ---- Add ----

function AddAppDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers"), enabled: open });
  // Connected servers first, so the default choice is one that can act now.
  const ready = (servers.data ?? []).filter((s) => s.enrolled_at).sort((a, b) => Number(b.online) - Number(a.online));
  const [server, setServer] = useState("");
  const [kind, setKind] = useState<App["kind"]>("image");
  const [owner, setOwner] = useState("");
  const me = useMe();
  // Compose files can do anything Docker can, so only administrators run them, and only for themselves.
  const composeAllowed = me.data?.role === "admin" && owner === "";
  useEffect(() => {
    if (!composeAllowed) setKind("image");
  }, [composeAllowed]);
  const [name, setName] = useState("");
  const [form, setForm] = useState<AppForm>(emptyForm);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!server && ready.length) setServer(ready[0].id);
  }, [ready, server]);
  useEffect(() => {
    if (open) setError("");
  }, [open]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const app = await api.post<App>("/apps", { server_id: server, name, kind, owner_id: owner, ...requestBody(kind, form) });
      qc.invalidateQueries({ queryKey: ["apps"] });
      onClose();
      setForm(emptyForm);
      setName("");
      navigate(`/apps/${app.id}`);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} title={t("apps.add")} wide>
      <form onSubmit={submit} className="grid gap-5">
        <ErrorAlert>{error}</ErrorAlert>
        <div className="grid grid-cols-2 gap-2" role="radiogroup" aria-label={t("apps.kind_label")}>
          {(composeAllowed ? (["image", "compose"] as const) : (["image"] as const)).map((k) => (
            <label
              key={k}
              className={cx("flex cursor-pointer gap-3 rounded-lg border p-3", kind === k ? "border-accent bg-accent-soft" : "border-line hover:bg-sunk")}
            >
              <input type="radio" name="kind" checked={kind === k} onChange={() => setKind(k)} className="mt-1 accent-[var(--accent)]" />
              <span className="grid gap-0.5">
                <span className="font-medium">{t(`apps.kind.${k}`)}</span>
                <span className="text-sm text-muted">{t(`apps.kind_hint.${k}`)}</span>
              </span>
            </label>
          ))}
        </div>
        {kind === "image" && (
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <span className="text-muted">{t("apps.presets")}</span>
            {presets.map((p) => (
              <button
                key={p.name}
                type="button"
                className="rounded-full border border-line px-3 py-1 hover:border-accent hover:text-accent"
                onClick={() => {
                  setForm({ ...emptyForm, ...p.form });
                  setName(p.name);
                }}
              >
                {p.label}
              </button>
            ))}
          </div>
        )}
        <div className={cx("grid gap-4", ready.length > 1 && "sm:grid-cols-2")}>
          <Field
            label={t("apps.name")}
            hint={t("apps.name_hint")}
            value={name}
            onChange={(e) => setName(e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, ""))}
            dir="ltr"
            maxLength={32}
            required
          />
          {ready.length > 1 && (
            <label className="grid content-start gap-1.5">
              <span className="text-sm font-medium">{t("sites.server")}</span>
              <select value={server} onChange={(e) => setServer(e.target.value)} className={select}>
                {ready.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
        <OwnerField value={owner} onChange={setOwner} />
        <AppFields kind={kind} value={form} onChange={setForm} />
        <p className="text-sm text-muted">{t("apps.first_run")}</p>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!server || !name}>
            {t("apps.create")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

// ---- List ----

export function AppsPage() {
  const { t } = useTranslation();
  const apps = useQuery({
    queryKey: ["apps"],
    queryFn: () => api.get<App[]>("/apps"),
    refetchInterval: (q) => (q.state.data?.some((a) => a.state === "pending" || a.state === "deleting") ? 3000 : 15000),
  });
  const [params, setParams] = useSearchParams();
  const adding = params.get("add") === "1";
  const setAdding = (on: boolean) => setParams(on ? { add: "1" } : {}, { replace: true });
  const addButton = (
    <Button variant="primary" onClick={() => setAdding(true)}>
      <Plus className="size-4" aria-hidden />
      {t("apps.add")}
    </Button>
  );
  return (
    <>
      <PageHeader title={t("nav.apps")} description={t("apps.intro")} actions={apps.data?.length ? addButton : undefined} />
      {apps.isPending && <Spinner />}
      {apps.error && <ErrorAlert>{errorMessage(t, apps.error)}</ErrorAlert>}
      {apps.data?.length === 0 && <EmptyState icon={<Boxes />} title={t("apps.empty.title")} body={t("apps.empty.body")} action={addButton} />}
      {!!apps.data?.length && (
        <Card className="overflow-x-auto">
          <table className="w-full min-w-[680px] text-sm">
            <thead>
              <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                <th className="px-4 py-3 text-start font-semibold">{t("apps.col.app")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("apps.col.source")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("sites.col.server")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("sites.col.status")}</th>
              </tr>
            </thead>
            <tbody>
              {apps.data.map((a) => (
                <tr key={a.id} className="border-b border-line last:border-0 hover:bg-sunk/60">
                  <td className="px-4 py-3">
                    <Link to={`/apps/${a.id}`} className="font-semibold hover:text-accent" dir="ltr">
                      {a.name}
                    </Link>
                    {a.domain && (
                      <div className="flex items-center gap-1 truncate text-muted">
                        {a.tls === "auto" && <Lock className="size-3 text-ok" aria-hidden />}
                        <bdi>{a.domain_display}</bdi>
                      </div>
                    )}
                  </td>
                  <td className="px-4 py-3">
                    <span className="font-mono text-xs" dir="ltr">
                      {a.kind === "image" ? a.image : t("apps.kind.compose")}
                    </span>
                  </td>
                  <td className="px-4 py-3 text-muted">
                    {a.server_name}
                    {a.owner_name && <div className="text-xs">{a.owner_name}</div>}
                  </td>
                  <td className="px-4 py-3">
                    <AppStateBadge app={a} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      <AddAppDialog open={adding} onClose={() => setAdding(false)} />
    </>
  );
}

// ---- Detail ----

const containerTone = (c: AppContainer): Tone =>
  c.health === "unhealthy" || c.state === "restarting" || c.state === "dead"
    ? "crit"
    : c.state === "running"
      ? "ok"
      : c.state === "exited"
        ? "neutral"
        : "warn";

function Containers({ app }: { app: App }) {
  const { t } = useTranslation();
  const q = useQuery({
    queryKey: ["app-status", app.id, app.applied_generation],
    queryFn: () => api.get<{ containers: AppContainer[] }>(`/apps/${app.id}/status`),
    enabled: app.applied_generation > 0,
    refetchInterval: 10000,
    retry: false,
  });
  return (
    <Card className="overflow-hidden">
      <h2 className="border-b border-line px-4 py-3 font-semibold">{t("apps.containers")}</h2>
      {app.applied_generation === 0 && <p className="px-4 py-6 text-center text-sm text-muted">{t("apps.not_ready")}</p>}
      {q.isFetching && !q.data && app.applied_generation > 0 && <Spinner />}
      {q.error && (
        <div className="p-4">
          <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>
        </div>
      )}
      {q.data && q.data.containers.length === 0 && <p className="px-4 py-6 text-center text-sm text-muted">{t("apps.no_containers")}</p>}
      <ul className="divide-y divide-line">
        {q.data?.containers.map((c) => (
          <li key={c.name} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 text-sm">
            <Badge tone={containerTone(c)}>{c.health ? `${c.state} · ${c.health}` : c.state}</Badge>
            <span className="font-medium" dir="ltr">
              {c.service}
            </span>
            <span className="font-mono text-xs text-muted" dir="ltr">
              {c.image}
            </span>
            <span className="ms-auto text-xs text-muted" dir="ltr">
              {c.status}
            </span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function Logs({ app }: { app: App }) {
  const { t } = useTranslation();
  const [tail, setTail] = useState(200);
  const q = useQuery({
    queryKey: ["app-logs", app.id, tail],
    queryFn: () => api.get<{ logs: string; truncated?: boolean }>(`/apps/${app.id}/logs?tail=${tail}`),
    enabled: app.applied_generation > 0,
    retry: false,
  });
  return (
    <Card className="overflow-hidden">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2.5">
        <h2 className="font-semibold">{t("apps.logs")}</h2>
        <select
          value={tail}
          onChange={(e) => setTail(Number(e.target.value))}
          className="ms-auto h-8 rounded-lg border border-line bg-surface px-2 text-sm"
          aria-label={t("apps.lines")}
        >
          {[100, 200, 1000, 5000].map((n) => (
            <option key={n} value={n}>
              {t("apps.last_lines", { count: n })}
            </option>
          ))}
        </select>
        <Button size="sm" variant="ghost" onClick={() => q.refetch()} disabled={app.applied_generation === 0} aria-label={t("apps.refresh")}>
          <RefreshCw className={cx("size-4", q.isFetching && "animate-spin")} aria-hidden />
        </Button>
      </div>
      {q.error && (
        <div className="p-4">
          <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>
        </div>
      )}
      <pre className="max-h-[28rem] overflow-auto bg-sunk p-4 font-mono text-xs leading-relaxed whitespace-pre-wrap" dir="ltr">
        {q.data ? q.data.logs || t("apps.no_logs") : app.applied_generation === 0 ? t("apps.not_ready") : "…"}
      </pre>
    </Card>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 truncate">{children}</dd>
    </div>
  );
}

export function AppDetailPage() {
  const { id = "" } = useParams();
  const { t } = useTranslation();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["apps", id],
    queryFn: () => api.get<App>(`/apps/${id}`),
    refetchInterval: (q) => (q.state.data?.state === "pending" || q.state.data?.state === "deleting" ? 2000 : 15000),
    retry: (n, err) => !(err instanceof ApiError && err.status === 404) && n < 2,
  });
  const tasks = useTasks(`server_id=${encodeURIComponent(q.data?.server_id ?? "")}&limit=10`, !!q.data);
  const updating = tasks.data?.find((x: Task) => x.kind === "app.update" && x.params.app_id === id && (x.status === "queued" || x.status === "running"));
  const [form, setForm] = useState<AppForm | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [deleting, setDeleting] = useState(false);
  const app = q.data;
  useEffect(() => {
    if (app && form === null) setForm(formFromApp(app));
  }, [app, form]);

  if (q.isPending) return <Spinner />;
  if (q.error || !app) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;

  const act = async (what: string, run: () => Promise<unknown>) => {
    setBusy(what);
    setError("");
    try {
      const r = await run();
      if (r && typeof r === "object" && "generation" in r) qc.setQueryData(["apps", id], r);
      qc.invalidateQueries({ queryKey: ["apps"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
      qc.invalidateQueries({ queryKey: ["app-status", id] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };
  const ready = app.applied_generation > 0 && app.state !== "deleting";
  const url = app.domain ? `https://${app.domain_display}` : null;

  return (
    <>
      <Link to="/apps" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("nav.apps")}
      </Link>
      <PageHeader
        title={app.name}
        description={app.kind === "image" ? (app.image ?? "") : t("apps.kind.compose")}
        actions={
          <>
            {url && (
              <a
                href={url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-line px-3 text-sm hover:bg-sunk"
              >
                <ExternalLink className="size-4" aria-hidden />
                {t("apps.open")}
              </a>
            )}
            {app.stopped ? (
              <Button
                size="sm"
                variant="primary"
                disabled={!!busy || app.state === "deleting"}
                loading={busy === "start"}
                onClick={() => act("start", () => api.post<App>(`/apps/${id}/start`))}
              >
                <Play className="size-4" aria-hidden />
                {t("apps.start")}
              </Button>
            ) : (
              <>
                <Button
                  size="sm"
                  disabled={!ready || !!busy}
                  loading={busy === "restart"}
                  onClick={() => act("restart", () => api.post(`/apps/${id}/restart`))}
                >
                  <RotateCw className="size-4" aria-hidden />
                  {t("apps.restart")}
                </Button>
                <Button
                  size="sm"
                  disabled={!!busy || app.state === "deleting"}
                  loading={busy === "stop"}
                  onClick={() => act("stop", () => api.post<App>(`/apps/${id}/stop`))}
                >
                  <Square className="size-4" aria-hidden />
                  {t("apps.stop")}
                </Button>
              </>
            )}
            <Button
              size="sm"
              disabled={!ready || !!busy || !!updating}
              loading={busy === "update" || !!updating}
              onClick={() => act("update", () => api.post(`/apps/${id}/update`))}
            >
              <Download className="size-4" aria-hidden />
              {t("apps.pull")}
            </Button>
          </>
        }
      />
      <div className="grid gap-6">
        <ErrorAlert>{error}</ErrorAlert>
        <Card className="grid gap-3 p-5">
          <div className="flex flex-wrap items-center gap-3">
            <AppStateBadge app={app} />
            {app.state === "error" && app.message && <span className="text-sm break-words text-crit">{app.message}</span>}
          </div>
          <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
            <Fact label={t("sites.server")}>{app.server_name}</Fact>
            {app.domain && (
              <Fact label={t("apps.domain")}>
                <bdi>{app.domain_display}</bdi>
              </Fact>
            )}
            {app.container_port && (
              <Fact label={t("apps.listens")}>
                <span dir="ltr" className="font-mono">
                  {app.service}:{app.container_port} → 127.0.0.1:{app.host_port}
                </span>
              </Fact>
            )}
            <Fact label={t("apps.folder")}>
              <span dir="ltr" className="font-mono">
                /srv/ekpanel/apps/{app.name}
              </span>
            </Fact>
          </dl>
        </Card>

        <div className="grid gap-6 xl:grid-cols-[2fr_3fr]">
          <Containers app={app} />
          <Logs app={app} />
        </div>
        {app.state !== "deleting" && <CronCard appId={app.id} />}

        {form && (
          <Card className="p-6">
            <form
              className="grid gap-5"
              onSubmit={(e) => {
                e.preventDefault();
                act("save", () => api.put<App>(`/apps/${id}`, requestBody(app.kind, form)));
              }}
            >
              <h2 className="text-lg font-semibold">{t("apps.settings")}</h2>
              <AppFields kind={app.kind} value={form} onChange={setForm} />
              <div className="flex justify-end gap-2">
                <Button type="button" variant="ghost" onClick={() => setForm(formFromApp(app))}>
                  {t("apps.reset")}
                </Button>
                <Button type="submit" variant="primary" loading={busy === "save"} disabled={app.state === "deleting"}>
                  {t("apps.save")}
                </Button>
              </div>
            </form>
          </Card>
        )}

        <Card className="grid gap-3 p-6">
          <h2 className="text-lg font-semibold">{t("apps.delete.title")}</h2>
          <p className="text-sm text-muted">{t("apps.delete.body")}</p>
          <div>
            <Button variant="danger" onClick={() => setDeleting(true)} disabled={app.state === "deleting"}>
              <Trash2 className="size-4" aria-hidden />
              {t("apps.delete.button")}
            </Button>
          </div>
        </Card>
      </div>
      <DeleteAppDialog open={deleting} app={app} onClose={() => setDeleting(false)} />
    </>
  );
}

function DeleteAppDialog({ open, app, onClose }: { open: boolean; app: App; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [withData, setWithData] = useState(false);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <Dialog open={open} onClose={onClose} title={t("apps.delete.confirm_title", { name: app.name })}>
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            await api.del(`/apps/${app.id}${withData ? "?data=delete" : ""}`);
            qc.invalidateQueries({ queryKey: ["apps"] });
            navigate("/apps", { replace: true });
          } catch (err) {
            setError(errorMessage(t, err));
            setBusy(false);
          }
        }}
      >
        <ErrorAlert>{error}</ErrorAlert>
        <p className="text-muted">{t("apps.delete.keeps")}</p>
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" checked={withData} onChange={(e) => setWithData(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
          <span>
            <span className="font-medium">{t("apps.delete.with_data")}</span>
            <span className="block text-muted">{t("apps.delete.with_data_hint", { name: app.name })}</span>
          </span>
        </label>
        <Field
          label={t("databases.delete.confirm", { name: app.name })}
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          dir="ltr"
          autoComplete="off"
          className="[&_input]:font-mono"
        />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="danger" loading={busy} disabled={typed.trim() !== app.name}>
            {withData ? t("apps.delete.button_data") : t("apps.delete.button")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

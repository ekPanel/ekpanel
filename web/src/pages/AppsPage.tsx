import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  Boxes,
  Download,
  Eye,
  EyeOff,
  SquareTerminal,
  ExternalLink,
  Hammer,
  LayoutGrid,
  Lock,
  Play,
  Plus,
  RefreshCw,
  RotateCw,
  Square,
  Trash2,
} from "lucide-react";
import { api, ApiError, errorMessage, nodeVersions, type App, type AppContainer, type Package, type Server, type Site, type Task } from "../lib/api";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, Dialog, ErrorAlert, Field, Spinner, cx, type Tone } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { SiteStateBadge } from "./SitesPage";
import { useTasks } from "../components/Tasks";
import { useMe } from "../lib/session";
import { MoveCard } from "../components/SiteMove";
import { CronCard } from "../components/CronCard";
import { LogsCard, TrafficCard } from "../components/WebInsights";
import { OwnerField } from "./CustomersPage";
import { TemplateMark, useCatalog } from "./CatalogPage";
import { DeployButton, DeploymentsCard, GitCard, PreviewOfNote, PreviewsCard } from "../components/Deployments";

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
  gitUrl: string;
  gitBranch: string;
  dockerfile: string;
  buildContext: string;
  autoDeploy: boolean;
  builder: App["builder"];
  previews: boolean;
  previewDomain: string;
  // Service apps.
  siteId: string;
  directory: string;
  command: string;
  buildCommand: string;
  runtime: App["runtime"];
  runtimeVersion: string;
  listens: boolean;
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
  gitUrl: "",
  gitBranch: "main",
  dockerfile: "Dockerfile",
  buildContext: ".",
  autoDeploy: true,
  builder: "auto",
  previews: false,
  previewDomain: "",
  siteId: "",
  directory: "",
  command: "",
  buildCommand: "",
  runtime: "node",
  runtimeVersion: nodeVersions[1],
  listens: true,
};

/** A port in the panel's own range was chosen by the panel, not asked for. */
const panelPort = (p: number | null) => p !== null && p >= 20000 && p <= 29999;

function formFromApp(a: App): AppForm {
  return {
    image: a.image ?? "",
    compose: a.compose ?? "",
    service: a.service ?? "",
    port: a.container_port && !(a.kind === "service" && panelPort(a.container_port)) ? String(a.container_port) : "",
    domain: a.domain_display || "",
    tls: a.tls,
    env: a.env.map((e) => `${e.key}=${e.value}`).join("\n"),
    volumes: a.volumes.join("\n"),
    memory: a.memory_mb ? String(a.memory_mb) : "",
    cpus: a.cpus ? String(a.cpus) : "",
    publicPorts: a.public_ports,
    gitUrl: a.git_url ?? "",
    gitBranch: a.git_branch ?? "",
    dockerfile: a.dockerfile ?? "Dockerfile",
    buildContext: a.build_context ?? ".",
    autoDeploy: a.auto_deploy,
    builder: a.builder,
    previews: a.previews,
    previewDomain: a.preview_domain ?? "",
    siteId: a.site_id ?? "",
    directory: a.directory ?? "",
    command: a.command ?? "",
    buildCommand: a.build_command ?? "",
    runtime: a.runtime,
    runtimeVersion: a.runtime_version ?? nodeVersions[1],
    listens: a.host_port !== null,
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
      kind !== "compose"
        ? f.volumes
            .split("\n")
            .map((v) => v.trim())
            .filter(Boolean)
        : [],
    memory_mb: num(f.memory),
    cpus: num(f.cpus),
    public_ports: kind === "compose" && f.publicPorts,
    git_url: f.gitUrl,
    git_branch: f.gitBranch,
    dockerfile: f.dockerfile,
    build_context: f.buildContext,
    auto_deploy: f.autoDeploy,
    builder: f.builder,
    previews: f.previews,
    preview_domain: f.previewDomain,
    ...(kind === "service" && {
      site_id: f.siteId,
      directory: f.directory,
      command: f.command,
      build_command: f.buildCommand,
      runtime: f.runtime,
      runtime_version: f.runtime ? f.runtimeVersion : "",
      listens: f.listens,
      container_port: f.listens ? num(f.port) : null,
      domain: f.listens ? f.domain : "",
      volumes: [],
    }),
  };
}

/** The settings of an app. An app from the catalogue keeps the template's Compose file and port. */
function AppFields({
  kind,
  value,
  onChange,
  fromTemplate,
  preview,
  serverId,
  ownerId,
}: {
  kind: App["kind"];
  value: AppForm;
  onChange: (v: AppForm) => void;
  fromTemplate?: boolean;
  preview?: boolean;
  /** Service apps: the server and owner whose websites the app may run from. */
  serverId?: string;
  ownerId?: string | null;
}) {
  const { t } = useTranslation();
  const set = <K extends keyof AppForm>(k: K, v: AppForm[K]) => onChange({ ...value, [k]: v });
  return (
    <div className="grid gap-4">
      {fromTemplate ? (
        <p className="rounded-lg bg-sunk px-3 py-2 text-sm text-muted">{t("catalog.compose_fixed")}</p>
      ) : kind === "service" ? (
        <ServiceFields value={value} set={set} serverId={serverId ?? ""} ownerId={ownerId ?? null} />
      ) : kind === "git" ? (
        <div className="grid gap-4">
          <div className="grid gap-4 sm:grid-cols-[2fr_1fr]">
            <Field
              label={t("deploys.repo")}
              hint={t("deploys.repo_hint")}
              value={value.gitUrl}
              onChange={(e) => set("gitUrl", e.target.value.trim())}
              dir="ltr"
              className="[&_input]:font-mono"
              placeholder="https://github.com/owner/app.git"
              required
            />
            <Field label={t("deploys.branch")} value={value.gitBranch} onChange={(e) => set("gitBranch", e.target.value.trim())} dir="ltr" required />
          </div>
          <div className="grid gap-4 sm:grid-cols-3">
            <label className="grid content-start gap-1.5">
              <span className="text-sm font-medium">{t("deploys.builder")}</span>
              <select value={value.builder} onChange={(e) => set("builder", e.target.value as AppForm["builder"])} className={select}>
                {(["auto", "dockerfile", "railpack"] as const).map((b) => (
                  <option key={b} value={b}>
                    {t(`deploys.builders.${b}`)}
                  </option>
                ))}
              </select>
              <span className="text-sm text-muted">{t(`deploys.builder_hint.${value.builder}`)}</span>
            </label>
            {value.builder !== "railpack" && (
              <Field
                label={t("deploys.dockerfile")}
                hint={t("deploys.dockerfile_hint")}
                value={value.dockerfile}
                onChange={(e) => set("dockerfile", e.target.value.trim())}
                dir="ltr"
                className="[&_input]:font-mono"
              />
            )}
            <Field
              label={t("deploys.context")}
              hint={t("deploys.context_hint")}
              value={value.buildContext}
              onChange={(e) => set("buildContext", e.target.value.trim())}
              dir="ltr"
              className="[&_input]:font-mono"
            />
          </div>
          {!preview && (
            <label className="flex items-start gap-2 text-sm">
              <input type="checkbox" checked={value.autoDeploy} onChange={(e) => set("autoDeploy", e.target.checked)} className="mt-1 accent-[var(--accent)]" />
              <span>
                <span className="font-medium">{t("deploys.auto")}</span>
                <span className="block text-muted">{t("deploys.auto_hint")}</span>
              </span>
            </label>
          )}
          {!preview && (
            <div className="grid gap-3 rounded-lg border border-line p-3">
              <label className="flex items-start gap-2 text-sm">
                <input type="checkbox" checked={value.previews} onChange={(e) => set("previews", e.target.checked)} className="mt-1 accent-[var(--accent)]" />
                <span>
                  <span className="font-medium">{t("previews.enable")}</span>
                  <span className="block text-muted">{t("previews.enable_hint")}</span>
                </span>
              </label>
              {value.previews && (
                <Field
                  label={t("previews.domain")}
                  hint={t("previews.domain_hint", {
                    domain: value.previewDomain || "preview.example.com",
                  })}
                  value={value.previewDomain}
                  onChange={(e) => set("previewDomain", e.target.value.trim())}
                  dir="ltr"
                  placeholder="preview.example.com"
                  required
                />
              )}
            </div>
          )}
        </div>
      ) : kind === "image" ? (
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
      {!fromTemplate && kind !== "service" && (
        <div className={cx("grid gap-4", kind === "compose" ? "sm:grid-cols-2" : "")}>
          {kind === "compose" && (
            <Field label={t("apps.service")} hint={t("apps.service_hint")} value={value.service} onChange={(e) => set("service", e.target.value)} dir="ltr" />
          )}
          <Field
            label={t("apps.port")}
            hint={kind === "git" ? t("deploys.port_hint") : t("apps.port_hint")}
            required={kind === "git"}
            value={value.port}
            onChange={(e) => set("port", e.target.value.replace(/\D/g, ""))}
            inputMode="numeric"
            dir="ltr"
            placeholder="8080"
          />
        </div>
      )}
      <div className="grid gap-4 sm:grid-cols-[1fr_auto]">
        <Field
          label={t("apps.domain")}
          hint={t("apps.domain_hint")}
          value={value.domain}
          onChange={(e) => set("domain", e.target.value)}
          dir="ltr"
          placeholder="app.example.com"
          disabled={kind === "service" ? !value.listens : !value.port}
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
        <span className="text-sm text-muted">
          {kind === "git"
            ? t("deploys.env_hint")
            : kind === "image"
              ? t("apps.env_hint_image")
              : kind === "service"
                ? t("service_apps.env_hint")
                : t("apps.env_hint_compose")}
        </span>
      </label>
      {kind !== "compose" && kind !== "service" && (
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
          {kind === "compose" && !fromTemplate && (
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

/** Where a service app runs from and how it starts. */
function ServiceFields({
  value,
  set,
  serverId,
  ownerId,
}: {
  value: AppForm;
  set: <K extends keyof AppForm>(k: K, v: AppForm[K]) => void;
  serverId: string;
  ownerId: string | null;
}) {
  const { t } = useTranslation();
  const sites = useQuery({
    queryKey: ["sites"],
    queryFn: () => api.get<Site[]>("/sites"),
  });
  // The app runs as its website's account, so the website is on its server and its owner's.
  const choices = (sites.data ?? []).filter((s) => s.server_id === serverId && (s.owner_id ?? null) === ownerId && s.state !== "deleting");
  const site = choices.find((s) => s.id === value.siteId);
  useEffect(() => {
    if (sites.data && !site && choices.length && !value.siteId) set("siteId", choices[0].id);
  }, [sites.data, site, choices, value.siteId, set]);
  const home = site ? `/home/${site.linux_user}` : "/home/…";
  return (
    <div className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <label className="grid content-start gap-1.5">
          <span className="text-sm font-medium">{t("service_apps.site")}</span>
          <select value={value.siteId} onChange={(e) => set("siteId", e.target.value)} className={select} required>
            {!site && <option value="">{t("service_apps.choose_site")}</option>}
            {choices.map((s) => (
              <option key={s.id} value={s.id}>
                {s.domain_display}
              </option>
            ))}
          </select>
          <span className="text-sm text-muted">{choices.length || sites.isPending ? t("service_apps.site_hint") : t("service_apps.no_sites")}</span>
        </label>
        <Field
          label={t("service_apps.directory")}
          hint={t("service_apps.directory_hint", {
            path: `${home}/${value.directory.replace(/^\/+/, "")}`,
          })}
          value={value.directory}
          onChange={(e) => set("directory", e.target.value)}
          dir="ltr"
          className="[&_input]:font-mono"
          placeholder="app"
        />
      </div>
      <div className="grid gap-4 sm:grid-cols-[1fr_2fr]">
        <label className="grid content-start gap-1.5">
          <span className="text-sm font-medium">{t("service_apps.runtime")}</span>
          <select
            value={value.runtime ? `node-${value.runtimeVersion}` : ""}
            onChange={(e) => {
              const v = e.target.value;
              set("runtime", v ? "node" : "");
              if (v) set("runtimeVersion", v.slice(5));
            }}
            className={select}
          >
            {nodeVersions.map((v) => (
              <option key={v} value={`node-${v}`}>
                Node.js {v}
              </option>
            ))}
            <option value="">{t("service_apps.runtime_none")}</option>
          </select>
          <span className="text-sm text-muted">{value.runtime ? t("service_apps.runtime_node_hint") : t("service_apps.runtime_none_hint")}</span>
        </label>
        <Field
          label={t("service_apps.command")}
          hint={t("service_apps.command_hint")}
          value={value.command}
          onChange={(e) => set("command", e.target.value)}
          dir="ltr"
          className="[&_input]:font-mono"
          placeholder={value.runtime ? "pnpm start" : "./server"}
          required
        />
      </div>
      <Field
        label={t("service_apps.build_command")}
        hint={t("service_apps.build_command_hint")}
        value={value.buildCommand}
        onChange={(e) => set("buildCommand", e.target.value)}
        dir="ltr"
        className="[&_input]:font-mono"
        placeholder={value.runtime ? "pnpm install --frozen-lockfile && pnpm build" : ""}
      />
      <div className="grid gap-3 rounded-lg border border-line p-3">
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" checked={value.listens} onChange={(e) => set("listens", e.target.checked)} className="mt-1 accent-[var(--accent)]" />
          <span>
            <span className="font-medium">{t("service_apps.listens")}</span>
            <span className="block text-muted">{t("service_apps.listens_hint")}</span>
          </span>
        </label>
        {value.listens && (
          <Field
            label={t("service_apps.port")}
            hint={t("service_apps.port_hint")}
            value={value.port}
            onChange={(e) => set("port", e.target.value.replace(/\D/g, ""))}
            inputMode="numeric"
            dir="ltr"
            placeholder={t("service_apps.port_auto")}
          />
        )}
      </div>
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
  const servers = useQuery({
    queryKey: ["servers"],
    queryFn: () => api.get<Server[]>("/servers"),
    enabled: open,
  });
  // Connected servers first, so the default choice is one that can act now.
  const ready = (servers.data ?? []).filter((s) => s.enrolled_at).sort((a, b) => Number(b.online) - Number(a.online));
  const [server, setServer] = useState("");
  const [kind, setKind] = useState<App["kind"]>("image");
  const [owner, setOwner] = useState("");
  const me = useMe();
  const isAdmin = me.data?.role === "admin";
  const myPackage = useQuery({
    queryKey: ["me", "usage"],
    queryFn: () => api.get<{ package: Package | null }>("/me/usage"),
    enabled: open && me.data?.role === "customer",
  });
  // Compose files can do anything Docker can, so only administrators run them, and only for
  // themselves. Git apps are for administrators, and for customers whose package allows them
  // (the panel checks the package of the customer an administrator picks).
  // Service apps run a command on the server as a website's account: administrators only.
  const kinds: App["kind"][] =
    isAdmin && owner === ""
      ? ["image", "compose", "git", "service"]
      : isAdmin
        ? ["image", "git", "service"]
        : me.data?.role === "reseller" || myPackage.data?.package?.git_apps
          ? ["image", "git"]
          : ["image"];
  useEffect(() => {
    if (!kinds.includes(kind)) setKind("image");
  }, [kinds, kind]);
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
      const app = await api.post<App>("/apps", {
        server_id: server,
        name,
        kind,
        owner_id: owner,
        ...requestBody(kind, form),
      });
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
        <div
          className={cx("grid gap-2", kinds.length === 3 ? "sm:grid-cols-3" : kinds.length % 2 === 0 && kinds.length > 0 && "sm:grid-cols-2")}
          role="radiogroup"
          aria-label={t("apps.kind_label")}
        >
          {kinds.map((k) => (
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
        <p className="text-sm text-muted">
          {t("catalog.or")}{" "}
          <Link to="/apps/catalog" className="font-medium text-accent hover:underline">
            {t("catalog.browse")}
          </Link>
        </p>
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
        <AppFields kind={kind} value={form} onChange={setForm} serverId={server} ownerId={owner || null} />
        <p className="text-sm text-muted">{kind === "service" ? t("service_apps.first_run") : t("apps.first_run")}</p>
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
  const addButtons = (
    <div className="flex flex-wrap justify-center gap-2">
      <Link to="/apps/catalog" className="inline-flex h-10 items-center gap-2 rounded-lg bg-accent px-4 text-sm font-medium text-accent-ink hover:opacity-90">
        <LayoutGrid className="size-4" aria-hidden />
        {t("catalog.open")}
      </Link>
      <Button onClick={() => setAdding(true)}>
        <Plus className="size-4" aria-hidden />
        {t("apps.add")}
      </Button>
    </div>
  );
  return (
    <>
      <PageHeader title={t("nav.apps")} description={t("apps.intro")} actions={apps.data?.length ? addButtons : undefined} />
      {apps.isPending && <Spinner />}
      {apps.error && <ErrorAlert>{errorMessage(t, apps.error)}</ErrorAlert>}
      {apps.data?.length === 0 && <EmptyState icon={<Boxes />} title={t("apps.empty.title")} body={t("apps.empty.body")} action={addButtons} />}
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
                    {a.preview_number && <span className="ms-2 text-xs text-muted">PR #{a.preview_number}</span>}
                    {a.domain && (
                      <div className="flex items-center gap-1 truncate text-muted">
                        {a.tls === "auto" && <Lock className="size-3 text-ok" aria-hidden />}
                        <bdi>{a.domain_display}</bdi>
                      </div>
                    )}
                  </td>
                  <td className="px-4 py-3">
                    <span className="font-mono text-xs" dir="ltr">
                      {a.kind === "image" ? a.image : a.kind === "git" ? a.git_url : (a.template ?? t("apps.kind.compose"))}
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
      <h2 className="border-b border-line px-4 py-3 font-semibold">{app.kind === "service" ? t("service_apps.process") : t("apps.containers")}</h2>
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

/** A service app's last build: its state and what it printed. */
function BuildCard({ app }: { app: App }) {
  const { t, i18n } = useTranslation();
  const tone: Record<App["build_state"], Tone> = {
    "": "neutral",
    running: "warn",
    ok: "ok",
    failed: "crit",
  };
  return (
    <Card className="grid gap-3 p-5">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="font-semibold">{t("service_apps.build_title")}</h2>
        <Badge tone={tone[app.build_state]}>{t(`service_apps.build_state.${app.build_state || "none"}`)}</Badge>
        {app.built_at && app.build_state !== "running" && <span className="text-sm text-muted">{new Date(app.built_at).toLocaleString(i18n.language)}</span>}
      </div>
      <p className="text-sm text-muted">
        {t("service_apps.build_hint")} <code dir="ltr">{app.build_command}</code>
      </p>
      {app.build_log && (
        <pre dir="ltr" className="max-h-96 overflow-auto rounded-lg bg-sunk p-3 font-mono text-xs whitespace-pre-wrap">
          {app.build_log}
        </pre>
      )}
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
  const me = useMe();
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
        description={
          app.kind === "image"
            ? (app.image ?? "")
            : app.kind === "git"
              ? (app.git_url ?? "")
              : app.kind === "service"
                ? (app.command ?? "")
                : app.template
                  ? t("catalog.from")
                  : t("apps.kind.compose")
        }
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
            {app.kind === "git" ? (
              <DeployButton app={app} onError={setError} />
            ) : app.kind === "service" ? (
              app.build_command && (
                <Button
                  size="sm"
                  disabled={!ready || !!busy || app.build_state === "running"}
                  loading={busy === "build" || app.build_state === "running"}
                  onClick={() => act("build", () => api.post(`/apps/${id}/build`))}
                >
                  <Hammer className="size-4" aria-hidden />
                  {t("service_apps.build")}
                </Button>
              )
            ) : (
              <Button
                size="sm"
                disabled={!ready || !!busy || !!updating}
                loading={busy === "update" || !!updating}
                onClick={() => act("update", () => api.post(`/apps/${id}/update`))}
              >
                <Download className="size-4" aria-hidden />
                {t("apps.pull")}
              </Button>
            )}
            {ready && !app.stopped && app.kind !== "service" && (app.kind !== "git" || app.image) && (
              <Link to={`/apps/${id}/terminal`} className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-line px-3 text-sm hover:bg-sunk">
                <SquareTerminal className="size-4" aria-hidden />
                {t("terminal.shell")}
              </Link>
            )}
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
            {app.kind === "service" ? (
              <>
                <Fact label={t("service_apps.site")}>
                  <Link to={`/sites/${app.site_id}`} className="text-accent hover:underline">
                    <bdi>{app.site_domain}</bdi>
                  </Link>
                </Fact>
                <Fact label={t("apps.folder")}>
                  <span dir="ltr" className="font-mono break-all">
                    /home/{app.site_user}
                    {app.directory ? `/${app.directory}` : ""}
                  </span>
                </Fact>
                <Fact label={t("service_apps.runs_as")}>
                  <span dir="ltr" className="font-mono">
                    {app.site_user} · ekpanel-app-{app.name}.service
                  </span>
                </Fact>
                <Fact label={t("service_apps.runtime")}>{app.runtime ? `Node.js ${app.runtime_version}` : t("service_apps.runtime_none")}</Fact>
                {app.host_port && (
                  <Fact label={t("apps.listens")}>
                    <span dir="ltr" className="font-mono">
                      127.0.0.1:{app.host_port} (PORT)
                    </span>
                  </Fact>
                )}
              </>
            ) : (
              <>
                {app.container_port && (
                  <Fact label={t("apps.listens")}>
                    <span dir="ltr" className="font-mono">
                      {app.service}:{app.container_port} → 127.0.0.1:
                      {app.host_port}
                    </span>
                  </Fact>
                )}
                <Fact label={t("apps.folder")}>
                  <span dir="ltr" className="font-mono">
                    /srv/ekpanel/apps/{app.name}
                  </span>
                </Fact>
              </>
            )}
          </dl>
        </Card>

        {app.template && <TemplateCard app={app} />}
        {app.preview_of && <PreviewOfNote app={app} />}
        {app.kind === "git" && app.state !== "deleting" && (
          <>
            {!app.preview_of && <GitCard app={app} />}
            <DeploymentsCard app={app} />
            {app.previews && <PreviewsCard app={app} />}
          </>
        )}
        {app.kind === "service" && app.build_command && <BuildCard app={app} />}
        <div className="grid gap-6 xl:grid-cols-[2fr_3fr]">
          <Containers app={app} />
          <Logs app={app} />
        </div>
        {app.state !== "deleting" && me.data?.role === "admin" && !app.preview_of && app.kind !== "service" && (
          <MoveCard
            thing={{ kind: "app", id: app.id, name: app.name, server_id: app.server_id, server_name: app.server_name, state: app.state, owner_id: app.owner_id }}
          />
        )}
        {app.state !== "deleting" && app.kind !== "service" && <CronCard appId={app.id} />}
        {app.state !== "deleting" && app.domain && (
          <div className="grid gap-6 xl:grid-cols-[2fr_3fr]">
            <TrafficCard base={`/apps/${app.id}`} />
            <LogsCard base={`/apps/${app.id}`} php={false} />
          </div>
        )}

        {form && (app.kind !== "service" || me.data?.role === "admin") && (
          <Card className="p-6">
            <form
              className="grid gap-5"
              onSubmit={(e) => {
                e.preventDefault();
                act("save", () => api.put<App>(`/apps/${id}`, requestBody(app.kind, form)));
              }}
            >
              <h2 className="text-lg font-semibold">{t("apps.settings")}</h2>
              <AppFields
                kind={app.kind}
                value={form}
                onChange={setForm}
                fromTemplate={!!app.template}
                preview={!!app.preview_of}
                serverId={app.server_id}
                ownerId={app.owner_id}
              />
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

/** What a catalogue app needs to be used: its notes and sign-in details. */
function TemplateCard({ app }: { app: App }) {
  const { t } = useTranslation();
  const catalog = useCatalog();
  const [shown, setShown] = useState(false);
  const tpl = catalog.data?.find((c) => c.id === app.template);
  if (!tpl) return null;
  const logins = tpl.vars
    .filter((v) => v.login)
    .map((v) => ({
      key: v.name,
      value: app.env.find((e) => e.key === v.name)?.value ?? "",
    }));
  const notes = t(`catalog.apps.${tpl.id}.notes`, {
    defaultValue: "",
    port: app.host_port ?? "",
    domain: app.domain_display,
  });
  return (
    <Card className="grid gap-4 p-5">
      <div className="flex items-center gap-3">
        <TemplateMark template={tpl} />
        <div className="min-w-0">
          <h2 className="font-semibold">{tpl.name}</h2>
          <p className="text-sm text-muted">{t("catalog.version", { version: app.template_version })}</p>
        </div>
        <a href={tpl.website} target="_blank" rel="noreferrer" className="ms-auto inline-flex items-center gap-1 text-sm text-accent hover:underline">
          {t("catalog.website")}
          <ExternalLink className="size-3.5" aria-hidden />
        </a>
      </div>
      {notes && <p className="text-sm">{notes}</p>}
      {logins.length > 0 && (
        <div className="grid gap-2">
          <div className="flex items-center gap-2">
            <h3 className="text-sm font-semibold">{t("catalog.sign_in")}</h3>
            <Button size="sm" variant="ghost" onClick={() => setShown(!shown)} aria-pressed={shown}>
              {shown ? <EyeOff className="size-4" aria-hidden /> : <Eye className="size-4" aria-hidden />}
              {shown ? t("catalog.hide") : t("catalog.show")}
            </Button>
          </div>
          <dl className="grid gap-2 text-sm">
            {logins.map((l) => (
              <div key={l.key} className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg bg-sunk px-3 py-2">
                <dt className="font-mono text-xs text-muted" dir="ltr">
                  {l.key}
                </dt>
                <dd className="min-w-0 flex-1 truncate font-mono" dir="ltr">
                  {shown || !/PASSWORD|TOKEN/.test(l.key) ? l.value : "•".repeat(12)}
                </dd>
                <CopyButton text={l.value} className="-my-1 shrink-0" />
              </div>
            ))}
          </dl>
          <p className="text-xs text-muted">{t("catalog.sign_in_hint")}</p>
        </div>
      )}
    </Card>
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
        <p className="text-muted">{app.kind === "service" ? t("service_apps.delete_keeps", { site: app.site_domain ?? "" }) : t("apps.delete.keeps")}</p>
        {app.kind !== "service" && (
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" checked={withData} onChange={(e) => setWithData(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
            <span>
              <span className="font-medium">{t("apps.delete.with_data")}</span>
              <span className="block text-muted">{t("apps.delete.with_data_hint", { name: app.name })}</span>
            </span>
          </label>
        )}
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

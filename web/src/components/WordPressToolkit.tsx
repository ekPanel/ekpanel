import { useState, type ReactNode } from "react";
import { Link } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ArrowDownToLine, ArrowUpFromLine, CopyPlus, ExternalLink, Loader2, RefreshCw, ShieldCheck } from "lucide-react";
import { api, errorMessage, type Site, type Task, type WordPressAuto, type WordPressItem } from "../lib/api";
import { formatRelative } from "../lib/format";
import { Badge, Button, Card, Dialog, ErrorAlert, cx } from "./ui";
import { useTasks } from "./Tasks";

const select = "h-9 rounded-lg border border-line bg-surface px-3 text-sm text-ink";

/** The WordPress toolkit of one site: updates, plugins, themes, settings, hardening, staging. */
export function WordPressToolkit({ site, children }: { site: Site; children?: ReactNode }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const tasks = useTasks(`server_id=${encodeURIComponent(site.server_id)}&limit=30`);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const wp = site.wordpress;
  const mine = (x: Task) => x.params.site_id === site.id || x.params.staging_id === site.id || x.params.live_id === site.id;
  const active = (x: Task) => x.status === "queued" || x.status === "running";
  const running = tasks.data?.filter((x) => mine(x) && active(x) && x.kind.startsWith("site.wordpress")) ?? [];
  const lastUpdate = tasks.data?.find((x) => mine(x) && x.kind === "site.wordpress_update" && !active(x));

  const setSite = (s: Site) => qc.setQueryData(["site", site.id], s);
  const call = async (key: string, fn: () => Promise<unknown>) => {
    setBusy(key);
    setError("");
    try {
      await fn();
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };
  const refresh = () => call("refresh", async () => setSite(await api.post<Site>(`/sites/${site.id}/wordpress/refresh`)));
  const manage = (action: string, name = "") =>
    call(action + name, async () => setSite(await api.post<Site>(`/sites/${site.id}/wordpress/manage`, { action, name })));
  const update = (body: { core?: string; plugins?: string[]; themes?: string[] }, key: string) =>
    call(key, () => api.post(`/sites/${site.id}/wordpress/update`, body));
  const setAuto = (auto: WordPressAuto) => call("auto", async () => setSite(await api.put<Site>(`/sites/${site.id}/wordpress/auto`, auto)));
  const setWeb = (part: Partial<Site["web"]>) => call("web", async () => setSite(await api.put<Site>(`/sites/${site.id}/web`, { ...site.web, ...part })));

  const updating = running.some((x) => x.kind === "site.wordpress_update");
  const pluginUpdates = wp?.plugins.filter((p) => p.update) ?? [];
  const themeUpdates = wp?.themes.filter((p) => p.update) ?? [];

  return (
    <Card className="grid gap-5 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="text-lg font-semibold">WordPress {wp?.version ?? site.app_version}</h2>
          <p className="text-sm text-muted">
            {site.wp_scanned_at ? t("wptools.scanned", { when: formatRelative(site.wp_scanned_at, i18n.language) }) : t("wptools.not_scanned")}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="ghost" onClick={refresh} loading={busy === "refresh"}>
            <RefreshCw className="size-4" aria-hidden />
            {t("wptools.check")}
          </Button>
          <a
            href={`https://${site.domain}/wp-admin/`}
            target="_blank"
            rel="noreferrer"
            className="inline-flex h-8 items-center gap-2 rounded-lg bg-accent px-3 text-sm font-medium text-accent-ink hover:brightness-110"
          >
            {t("wordpress.open_admin")}
            <ExternalLink className="size-4 rtl:-scale-x-100" aria-hidden />
          </a>
        </div>
      </div>
      {children}
      <ErrorAlert>{error}</ErrorAlert>
      {wp?.error && <ErrorAlert>{wp.error}</ErrorAlert>}
      {running.length > 0 && (
        <div className="flex items-center gap-3 rounded-lg bg-accent-soft px-4 py-3 text-sm text-accent" role="status">
          <Loader2 className="size-4 shrink-0 animate-spin" aria-hidden />
          {t(`wptools.running.${running[0].kind.replace("site.wordpress_", "")}`, { defaultValue: t("wptools.running.update") })}
        </div>
      )}
      {lastUpdate && <LastUpdate task={lastUpdate} />}

      {wp?.installed && (
        <>
          {/* Core */}
          <Section title={t("wptools.core")}>
            {wp.core_update ? (
              <div className="flex flex-wrap items-center gap-2">
                <Badge tone="warn">{t("wptools.core_available", { version: wp.core_update })}</Badge>
                {wp.core_minor && (
                  <Button size="sm" onClick={() => update({ core: "minor" }, "core-minor")} disabled={updating} loading={busy === "core-minor"}>
                    {t("wptools.update_to", { version: wp.core_minor })}
                  </Button>
                )}
                {wp.core_update !== wp.core_minor && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => update({ core: "latest" }, "core-latest")}
                    disabled={updating}
                    loading={busy === "core-latest"}
                  >
                    {t("wptools.update_to", { version: wp.core_update })}
                  </Button>
                )}
              </div>
            ) : (
              <p className="text-sm text-muted">{t("wptools.core_current")}</p>
            )}
          </Section>

          <ItemList
            title={t("wptools.plugins")}
            items={wp.plugins}
            kind="plugins"
            busy={busy}
            disabled={updating}
            onUpdate={(names, key) => update({ plugins: names }, key)}
            onToggle={(p) => manage(p.status === "active" ? "plugin_deactivate" : "plugin_activate", p.name)}
            updates={pluginUpdates.length}
          />
          <ItemList
            title={t("wptools.themes")}
            items={wp.themes}
            kind="themes"
            busy={busy}
            disabled={updating}
            onUpdate={(names, key) => update({ themes: names }, key)}
            updates={themeUpdates.length}
          />

          <Section title={t("wptools.auto.title")} hint={t("wptools.auto.hint")}>
            <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-sm">
              <label className="flex items-center gap-2">
                {t("wptools.auto.core")}
                <select
                  value={site.wp_auto.core}
                  onChange={(e) => setAuto({ ...site.wp_auto, core: e.target.value as WordPressAuto["core"] })}
                  className={select}
                >
                  <option value="off">{t("wptools.auto.core_off")}</option>
                  <option value="minor">{t("wptools.auto.core_minor")}</option>
                  <option value="all">{t("wptools.auto.core_all")}</option>
                </select>
              </label>
              <Check checked={site.wp_auto.plugins} onChange={(v) => setAuto({ ...site.wp_auto, plugins: v })} label={t("wptools.plugins")} />
              <Check checked={site.wp_auto.themes} onChange={(v) => setAuto({ ...site.wp_auto, themes: v })} label={t("wptools.themes")} />
            </div>
          </Section>

          <Section title={t("wptools.settings")}>
            <div className="grid gap-2 text-sm">
              <Check
                checked={wp.maintenance}
                onChange={(v) => manage(v ? "maintenance_on" : "maintenance_off")}
                label={t("wptools.maintenance")}
                hint={t("wptools.maintenance_hint")}
              />
              {wp.debug && (
                <p className="flex items-center gap-2 text-warn">
                  <AlertTriangle className="size-4 shrink-0" aria-hidden />
                  {t("wptools.debug_on")}
                </p>
              )}
            </div>
          </Section>

          <Section title={t("wptools.hardening.title")} hint={t("wptools.hardening.hint")}>
            <div className="grid gap-2 text-sm">
              <Check
                checked={wp.file_edit_disabled}
                onChange={(v) => manage(v ? "file_edit_off" : "file_edit_on")}
                label={t("wptools.hardening.file_edit")}
                hint={t("wptools.hardening.file_edit_hint")}
              />
              <Check
                checked={!!site.web.block_xmlrpc}
                onChange={(v) => setWeb({ block_xmlrpc: v })}
                label={t("wptools.hardening.xmlrpc")}
                hint={t("wptools.hardening.xmlrpc_hint")}
              />
              <Check
                checked={!!site.web.no_uploads_php}
                onChange={(v) => setWeb({ no_uploads_php: v })}
                label={t("wptools.hardening.uploads")}
                hint={t("wptools.hardening.uploads_hint")}
              />
              <div className="flex flex-wrap items-center gap-2 pt-1">
                {wp.loose_permissions > 0 ? (
                  <>
                    <AlertTriangle className="size-4 text-warn" aria-hidden />
                    <span>{t("wptools.hardening.loose", { count: wp.loose_permissions })}</span>
                    <Button size="sm" onClick={() => manage("fix_permissions")} loading={busy === "fix_permissions"}>
                      {t("wptools.hardening.fix")}
                    </Button>
                  </>
                ) : (
                  <>
                    <ShieldCheck className="size-4 text-ok" aria-hidden />
                    <span>{t("wptools.hardening.permissions_ok")}</span>
                  </>
                )}
              </div>
            </div>
          </Section>

          <Staging site={site} running={running.some((x) => x.kind === "site.wordpress_staging" || x.kind === "site.wordpress_copy")} />
        </>
      )}
    </Card>
  );
}

function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section className="grid gap-2 border-t border-line pt-4">
      <h3 className="font-semibold">{title}</h3>
      {hint && <p className="-mt-1 text-sm text-muted">{hint}</p>}
      {children}
    </section>
  );
}

function Check({ checked, onChange, label, hint }: { checked: boolean; onChange: (v: boolean) => void; label: string; hint?: string }) {
  return (
    <label className="flex items-start gap-2">
      <input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
      <span className="grid">
        <span>{label}</span>
        {hint && <span className="text-muted">{hint}</span>}
      </span>
    </label>
  );
}

function ItemList({
  title,
  items,
  kind,
  busy,
  disabled,
  updates,
  onUpdate,
  onToggle,
}: {
  title: string;
  items: WordPressItem[];
  kind: "plugins" | "themes";
  busy: string;
  disabled: boolean;
  updates: number;
  onUpdate: (names: string[], key: string) => void;
  onToggle?: (p: WordPressItem) => void;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const shown = open ? items : items.filter((p) => p.update || p.status === "active").slice(0, 8);
  return (
    <Section title={`${title} (${items.length})`}>
      <div className="flex flex-wrap items-center gap-2">
        {updates > 0 ? (
          <>
            <Badge tone="warn">{t("wptools.updates", { count: updates })}</Badge>
            <Button size="sm" onClick={() => onUpdate(["*"], kind + "-all")} disabled={disabled} loading={busy === kind + "-all"}>
              {t("wptools.update_all")}
            </Button>
          </>
        ) : (
          <span className="text-sm text-muted">{t("wptools.all_current")}</span>
        )}
      </div>
      {items.length > 0 && (
        <ul className="divide-y divide-line rounded-lg border border-line text-sm">
          {shown.map((p) => (
            <li key={p.name} className="flex flex-wrap items-center justify-between gap-2 px-3 py-2">
              <div className="grid min-w-0">
                <span className="truncate font-medium">{p.title || p.name}</span>
                <span className="text-xs text-muted" dir="ltr">
                  {p.name} · {p.version}
                  {p.update && ` → ${p.update}`}
                </span>
              </div>
              <div className="flex items-center gap-2">
                <Badge tone={p.status === "active" ? "ok" : "neutral"}>{t(`wptools.status.${p.status}`, { defaultValue: p.status })}</Badge>
                {p.update && (
                  <Button size="sm" variant="ghost" onClick={() => onUpdate([p.name], kind + p.name)} disabled={disabled} loading={busy === kind + p.name}>
                    {t("wptools.update")}
                  </Button>
                )}
                {onToggle && (p.status === "active" || p.status === "inactive") && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => onToggle(p)}
                    loading={busy === (p.status === "active" ? "plugin_deactivate" : "plugin_activate") + p.name}
                  >
                    {p.status === "active" ? t("wptools.deactivate") : t("wptools.activate")}
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
      {items.length > shown.length || open ? (
        <button className="justify-self-start text-sm font-medium text-accent hover:underline" onClick={() => setOpen(!open)}>
          {open ? t("wptools.show_less") : t("wptools.show_all", { count: items.length })}
        </button>
      ) : null}
    </Section>
  );
}

interface UpdateResult {
  items?: { kind: string; name: string; from: string; to?: string; outcome: string; error?: string }[];
}

function LastUpdate({ task }: { task: Task }) {
  const { t, i18n } = useTranslation();
  const items = (task.result as UpdateResult | null)?.items ?? [];
  const bad = items.filter((x) => x.outcome === "rolled_back" || x.outcome === "failed");
  if (task.status === "failed") return <ErrorAlert>{t("wptools.update_failed", { reason: task.error })}</ErrorAlert>;
  if (items.length === 0) return null;
  return (
    <div className={cx("grid gap-1 rounded-lg px-4 py-3 text-sm", bad.length ? "bg-warn-soft text-warn" : "bg-ok-soft text-ok")}>
      <p className="font-medium">
        {t("wptools.last_update", { when: formatRelative(task.finished_at ?? task.created_at, i18n.language) })}
        {task.params.auto ? ` · ${t("wptools.automatic")}` : ""}
      </p>
      <ul className="grid gap-0.5">
        {items.map((x) => (
          <li key={x.kind + x.name}>
            <span dir="ltr">
              {x.name} {x.from}
              {x.to ? ` → ${x.to}` : ""}
            </span>
            : {t(`wptools.outcome.${x.outcome}`)}
          </li>
        ))}
      </ul>
    </div>
  );
}

function Staging({ site, running }: { site: Site; running: boolean }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [direction, setDirection] = useState<"push" | "pull" | null>(null);
  const [made, setMade] = useState<{ staging_id: string; domain: string } | null>(null);
  const all = useQuery({ queryKey: ["wordpress"], queryFn: () => api.get<Site[]>("/wordpress"), enabled: !site.staging_of });
  const existing = all.data?.find((x) => x.staging_of === site.id);

  const create = async () => {
    setBusy(true);
    setError("");
    try {
      setMade(await api.post<{ staging_id: string; domain: string }>(`/sites/${site.id}/wordpress/staging`));
      qc.invalidateQueries({ queryKey: ["tasks"] });
      qc.invalidateQueries({ queryKey: ["sites"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  if (site.staging_of) {
    return (
      <Section title={t("wptools.staging.title")} hint={t("wptools.staging.is_copy")}>
        <ErrorAlert>{error}</ErrorAlert>
        <div className="flex flex-wrap gap-2">
          <Link to={`/sites/${site.staging_of}`} className="inline-flex h-8 items-center rounded-lg border border-line px-3 text-sm font-medium hover:bg-sunk">
            {t("wptools.staging.open_live")}
          </Link>
          <Button size="sm" variant="primary" onClick={() => setDirection("push")} disabled={running}>
            <ArrowUpFromLine className="size-4" aria-hidden />
            {t("wptools.staging.push")}
          </Button>
          <Button size="sm" onClick={() => setDirection("pull")} disabled={running}>
            <ArrowDownToLine className="size-4" aria-hidden />
            {t("wptools.staging.pull")}
          </Button>
        </div>
        {direction && <CopyDialog site={site} direction={direction} onClose={() => setDirection(null)} />}
      </Section>
    );
  }
  return (
    <Section title={t("wptools.staging.title")} hint={t("wptools.staging.hint", { domain: `staging.${site.domain}` })}>
      <ErrorAlert>{error}</ErrorAlert>
      {existing ? (
        <p className="text-sm">
          {t("wptools.staging.exists")}{" "}
          <Link to={`/sites/${existing.id}`} className="font-medium text-accent hover:underline" dir="ltr">
            {existing.domain_display}
          </Link>
        </p>
      ) : made ? (
        <p className="text-sm">
          {t("wptools.staging.started")}{" "}
          <Link to={`/sites/${made.staging_id}`} className="font-medium text-accent hover:underline" dir="ltr">
            {made.domain}
          </Link>
        </p>
      ) : (
        <div>
          <Button size="sm" onClick={create} loading={busy} disabled={running}>
            <CopyPlus className="size-4" aria-hidden />
            {t("wptools.staging.create")}
          </Button>
        </div>
      )}
    </Section>
  );
}

function CopyDialog({ site, direction, onClose }: { site: Site; direction: "push" | "pull"; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [files, setFiles] = useState(true);
  const [database, setDatabase] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await api.post(`/sites/${site.id}/wordpress/copy`, { direction, files, database });
      qc.invalidateQueries({ queryKey: ["tasks"] });
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} title={t(`wptools.staging.${direction}_title`)}>
      <div className="grid gap-4">
        <p className="text-muted">{t(`wptools.staging.${direction}_body`)}</p>
        <ErrorAlert>{error}</ErrorAlert>
        <div className="grid gap-2 text-sm">
          <Check checked={files} onChange={setFiles} label={t("wptools.staging.files")} hint={t("wptools.staging.files_hint")} />
          <Check checked={database} onChange={setDatabase} label={t("wptools.staging.database")} hint={t("wptools.staging.database_hint")} />
        </div>
        <p className="text-sm text-muted">{t("wptools.staging.kept")}</p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button variant={direction === "push" ? "danger" : "primary"} onClick={submit} loading={busy} disabled={!files && !database}>
            {t(`wptools.staging.${direction}`)}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

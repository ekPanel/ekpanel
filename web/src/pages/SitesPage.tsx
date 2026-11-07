import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Globe, Loader2, Lock, Plus } from "lucide-react";
import { api, errorMessage, type Server, type Site } from "../lib/api";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, Dialog, ErrorAlert, Field, Spinner, cx, type Tone } from "../components/ui";
import { EmptyState } from "./ServerParts";

// ---- Shared pieces ----

const stateTone: Record<Site["state"], Tone> = { pending: "warn", active: "ok", error: "crit", deleting: "neutral" };

export function SiteStateBadge({ site }: { site: Pick<Site, "state"> }) {
  const { t } = useTranslation();
  const busy = site.state === "pending" || site.state === "deleting";
  return (
    <Badge tone={stateTone[site.state]}>
      {busy && <Loader2 className="size-3 animate-spin" aria-hidden />}
      {t(`sites.state.${site.state}`)}
    </Badge>
  );
}

export function useSites(serverId?: string) {
  return useQuery({
    queryKey: ["sites", serverId ?? "all"],
    queryFn: () => api.get<Site[]>(`/sites${serverId ? `?server_id=${encodeURIComponent(serverId)}` : ""}`),
    refetchInterval: (q) => (q.state.data?.some((s) => s.state === "pending" || s.state === "deleting") ? 2000 : 15000),
  });
}

export function usePHPVersions() {
  return useQuery({
    queryKey: ["php-versions"],
    queryFn: () => api.get<{ versions: string[]; default: string }>("/php-versions"),
    staleTime: Infinity,
  });
}

export interface SiteSettings {
  kind: Site["kind"];
  php_version: string;
  tls: Site["tls"];
}

function Choice({ name, value, current, onChange, title, hint }: { name: string; value: string; current: string; onChange: (v: string) => void; title: string; hint: string }) {
  return (
    <label
      className={cx(
        "flex cursor-pointer gap-3 rounded-lg border p-3 transition-colors",
        current === value ? "border-accent bg-accent-soft" : "border-line hover:bg-sunk",
      )}
    >
      <input type="radio" name={name} value={value} checked={current === value} onChange={() => onChange(value)} className="mt-1 accent-[var(--accent)]" />
      <span className="grid gap-0.5">
        <span className="font-medium">{title}</span>
        <span className="text-sm text-muted">{hint}</span>
      </span>
    </label>
  );
}

/** Type, PHP version and SSL fields, used when adding and when editing a site. */
export function SiteSettingsFields({ value, onChange }: { value: SiteSettings; onChange: (v: SiteSettings) => void }) {
  const { t } = useTranslation();
  const php = usePHPVersions();
  return (
    <>
      <fieldset className="grid gap-2">
        <legend className="mb-1.5 text-sm font-medium">{t("sites.kind.label")}</legend>
        <div className="grid gap-2 sm:grid-cols-2">
          <Choice name="kind" value="php" current={value.kind} onChange={() => onChange({ ...value, kind: "php" })} title={t("sites.kind.php")} hint={t("sites.kind.php_hint")} />
          <Choice name="kind" value="static" current={value.kind} onChange={() => onChange({ ...value, kind: "static" })} title={t("sites.kind.static")} hint={t("sites.kind.static_hint")} />
        </div>
      </fieldset>
      {value.kind === "php" && (
        <label className="grid max-w-56 gap-1.5">
          <span className="text-sm font-medium">{t("sites.php_version")}</span>
          <select
            value={value.php_version}
            onChange={(e) => onChange({ ...value, php_version: e.target.value })}
            className="h-10 rounded-lg border border-line bg-surface px-3 text-ink"
          >
            {(php.data?.versions ?? [value.php_version]).map((v) => (
              <option key={v} value={v}>
                PHP {v}
                {v === php.data?.default ? ` (${t("sites.recommended")})` : ""}
              </option>
            ))}
          </select>
          <span className="text-sm text-muted">{t("sites.php_install_note")}</span>
        </label>
      )}
      <fieldset className="grid gap-2">
        <legend className="mb-1.5 text-sm font-medium">{t("sites.tls.label")}</legend>
        <div className="grid gap-2 sm:grid-cols-2">
          <Choice name="tls" value="auto" current={value.tls} onChange={() => onChange({ ...value, tls: "auto" })} title={t("sites.tls.auto")} hint={t("sites.tls.auto_hint")} />
          <Choice name="tls" value="internal" current={value.tls} onChange={() => onChange({ ...value, tls: "internal" })} title={t("sites.tls.internal")} hint={t("sites.tls.internal_hint")} />
        </div>
      </fieldset>
    </>
  );
}

/** One host name per line, blank lines ignored. */
export function parseLines(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((l) => l.trim())
    .filter(Boolean);
}

// ---- Add dialog ----

export function AddSiteDialog({ open, onClose, serverId }: { open: boolean; onClose: () => void; serverId?: string }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const php = usePHPVersions();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers"), enabled: open });
  const ready = (servers.data ?? []).filter((s) => s.enrolled_at);

  const [server, setServer] = useState("");
  const [domain, setDomain] = useState("");
  const [www, setWww] = useState(true);
  const [extra, setExtra] = useState("");
  const [settings, setSettings] = useState<SiteSettings>({ kind: "php", php_version: "", tls: "auto" });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!open) return;
    setDomain("");
    setExtra("");
    setWww(true);
    setError("");
    setSettings((s) => ({ ...s, kind: "php", tls: "auto" }));
  }, [open]);
  useEffect(() => {
    if (php.data && !settings.php_version) setSettings((s) => ({ ...s, php_version: php.data.default }));
  }, [php.data, settings.php_version]);
  useEffect(() => {
    if (serverId) setServer(serverId);
    else if (!server && ready.length > 0) setServer(ready[0].id);
  }, [serverId, ready, server]);

  const bareDomain = domain.trim().toLowerCase();
  const offerWww = bareDomain !== "" && !bareDomain.startsWith("www.");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const aliases = [...(offerWww && www ? [`www.${bareDomain}`] : []), ...parseLines(extra)];
      const site = await api.post<Site>("/sites", {
        server_id: server,
        domain,
        aliases,
        kind: settings.kind,
        php_version: settings.kind === "php" ? settings.php_version : null,
        tls: settings.tls,
      });
      qc.invalidateQueries({ queryKey: ["sites"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
      onClose();
      navigate(`/sites/${site.id}`);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} title={t("sites.add")} wide>
      {servers.isSuccess && ready.length === 0 ? (
        <div className="grid gap-4">
          <p className="text-muted">{t("sites.need_server")}</p>
          <div>
            <Link to="/servers?add=1" onClick={onClose} className="font-medium text-accent hover:underline">
              {t("servers.add")}
            </Link>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-5">
          <ErrorAlert>{error}</ErrorAlert>
          <Field label={t("sites.domain")} hint={t("sites.domain_hint")} value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="example.com" required autoComplete="off" spellCheck={false} />
          {offerWww && (
            <label className="-mt-2 flex items-center gap-2 text-sm">
              <input type="checkbox" checked={www} onChange={(e) => setWww(e.target.checked)} className="accent-[var(--accent)]" />
              {t("sites.add_www", { domain: `www.${bareDomain}` })}
            </label>
          )}
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
          <SiteSettingsFields value={settings} onChange={setSettings} />
          <details className="group">
            <summary className="cursor-pointer text-sm font-medium text-accent">{t("sites.more_domains")}</summary>
            <label className="mt-3 grid gap-1.5">
              <span className="text-sm text-muted">{t("sites.aliases_hint")}</span>
              <textarea value={extra} onChange={(e) => setExtra(e.target.value)} rows={3} dir="ltr" className="rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm text-ink" />
            </label>
          </details>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!domain.trim() || !server}>
              {t("sites.create")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

// ---- List page ----

export function SitesPage() {
  const { t } = useTranslation();
  const sites = useSites();
  const [params, setParams] = useSearchParams();
  const adding = params.get("add") === "1";
  const setAdding = (on: boolean) => setParams(on ? { add: "1" } : {}, { replace: true });
  const addButton = (
    <Button variant="primary" onClick={() => setAdding(true)}>
      <Plus className="size-4" aria-hidden />
      {t("sites.add")}
    </Button>
  );

  return (
    <>
      <PageHeader title={t("nav.sites")} description={t("sites.intro")} actions={sites.data?.length ? addButton : undefined} />
      {sites.isPending && <Spinner />}
      {sites.error && <ErrorAlert>{errorMessage(t, sites.error)}</ErrorAlert>}
      {sites.data?.length === 0 && <EmptyState icon={<Globe />} title={t("sites.empty.title")} body={t("sites.empty.body")} action={addButton} />}
      {!!sites.data?.length && (
        <Card className="overflow-x-auto">
          <table className="w-full min-w-[680px] text-sm">
            <thead>
              <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                <th className="px-4 py-3 text-start font-semibold">{t("sites.col.site")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("sites.col.type")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("sites.col.server")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("sites.col.status")}</th>
              </tr>
            </thead>
            <tbody>
              {sites.data.map((s) => (
                <tr key={s.id} className="border-b border-line last:border-0 hover:bg-sunk/60">
                  <td className="px-4 py-3">
                    <Link to={`/sites/${s.id}`} className="inline-flex items-center gap-1.5 font-semibold hover:text-accent">
                      {s.tls === "auto" && <Lock className="size-3.5 text-ok" aria-label={t("sites.tls.auto")} />}
                      <bdi>{s.domain_display}</bdi>
                    </Link>
                    {s.aliases_display.length > 0 && (
                      <div className="truncate text-muted">
                        <bdi>{s.aliases_display.join(", ")}</bdi>
                      </div>
                    )}
                  </td>
                  <td className="px-4 py-3">{s.kind === "php" ? `PHP ${s.php_version}` : t("sites.kind.static")}</td>
                  <td className="px-4 py-3 text-muted">{s.server_name}</td>
                  <td className="px-4 py-3">
                    <SiteStateBadge site={s} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      <AddSiteDialog open={adding} onClose={() => setAdding(false)} />
    </>
  );
}

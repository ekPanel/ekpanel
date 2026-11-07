import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, ExternalLink, FolderOpen, Loader2, RotateCw, Trash2 } from "lucide-react";
import { useMe } from "../lib/session";
import { CronCard } from "../components/CronCard";
import { LogsCard, TrafficCard } from "../components/WebInsights";
import { CertificateCard, HotlinkCard, PHPSettingsCard, RedirectsCard } from "../components/SiteRules";
import { SiteFTP, SiteSFTP } from "../components/SiteSFTP";
import { WordPressCard } from "../components/WordPressCard";
import { api, ApiError, errorMessage, type Server, type Site } from "../lib/api";
import { formatDateTime } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Button, Card, CopyButton, Dialog, ErrorAlert, Field, Spinner } from "../components/ui";
import { SiteSettingsFields, SiteStateBadge, parseLines, usePHPVersions, type SiteSettings } from "./SitesPage";

function Fact({ label, children, copy }: { label: string; children: ReactNode; copy?: string }) {
  return (
    <div className="contents">
      <dt className="text-muted">{label}</dt>
      <dd className="flex min-w-0 items-center gap-1">
        <span className="truncate">{children}</span>
        {copy && <CopyButton text={copy} className="-my-1 shrink-0" />}
      </dd>
    </div>
  );
}

export function SiteDetailPage() {
  const { id = "" } = useParams();
  const me = useMe();
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const php = usePHPVersions();
  const site = useQuery({
    queryKey: ["site", id],
    queryFn: () => api.get<Site>(`/sites/${id}`),
    refetchInterval: (q) => (q.state.data && ["pending", "deleting"].includes(q.state.data.state) ? 1500 : 15000),
    retry: (n, err) => !(err instanceof ApiError && err.status === 404) && n < 2,
  });
  const serverId = site.data?.server_id;
  const server = useQuery({
    queryKey: ["server", serverId],
    queryFn: () => api.get<Server>(`/servers/${serverId}`),
    enabled: !!serverId,
    refetchInterval: 10000,
  });
  const [settings, setSettings] = useState<SiteSettings | null>(null);
  const [aliases, setAliases] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const s = site.data;
  // Load the form from the server once, and again after each save.
  useEffect(() => {
    if (!s || settings) return;
    setSettings({ kind: s.kind, php_version: s.php_version ?? php.data?.default ?? "8.4", tls: s.tls });
    setAliases(s.aliases_display.join("\n"));
  }, [s, settings, php.data]);

  // A deleted site disappears: go back to the list.
  const gone = site.error instanceof ApiError && site.error.status === 404;
  useEffect(() => {
    if (gone) {
      qc.invalidateQueries({ queryKey: ["sites"] });
      navigate("/sites", { replace: true });
    }
  }, [gone, navigate, qc]);

  if (site.isPending || gone) return <Spinner />;
  if (site.error || !s) return <ErrorAlert>{errorMessage(t, site.error)}</ErrorAlert>;

  const save = async (e?: FormEvent) => {
    e?.preventDefault();
    if (!settings) return;
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      const updated = await api.patch<Site>(`/sites/${s.id}`, {
        aliases: parseLines(aliases),
        kind: settings.kind,
        php_version: settings.kind === "php" ? settings.php_version : null,
        tls: settings.tls,
      });
      qc.setQueryData(["site", id], updated);
      qc.invalidateQueries({ queryKey: ["sites"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setSettings(null); // reload the form from the saved site
      setSaved(true);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  const deleting = s.state === "deleting";
  const hosts = [s.domain_display, ...s.aliases_display];

  return (
    <>
      <Link to="/sites" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("nav.sites")}
      </Link>
      <PageHeader
        title={s.domain_display}
        description={s.kind === "php" ? `PHP ${s.php_version} · ${s.server_name}` : `${t("sites.kind.static")} · ${s.server_name}`}
        actions={
          <>
            <SiteStateBadge site={s} />
            {!deleting && (
              <Link to={`/sites/${s.id}/files`} className="inline-flex h-8 items-center gap-2 rounded-lg border border-line bg-surface px-3 text-sm font-medium hover:bg-sunk">
                <FolderOpen className="size-4" aria-hidden />
                {t("files.title")}
              </Link>
            )}
            {s.state === "active" && (
              <a href={`https://${s.domain}`} target="_blank" rel="noreferrer" className="inline-flex h-8 items-center gap-2 rounded-lg border border-line bg-surface px-3 text-sm font-medium hover:bg-sunk">
                {t("sites.visit")}
                <ExternalLink className="size-4 rtl:-scale-x-100" aria-hidden />
              </a>
            )}
          </>
        }
      />

      <div className="grid gap-6">
        {s.state === "pending" && (
          <div className="flex items-center gap-3 rounded-lg bg-warn-soft px-4 py-3 text-warn" role="status">
            <Loader2 className="size-5 shrink-0 animate-spin" aria-hidden />
            {server.data && !server.data.online ? t("sites.waiting_offline", { server: s.server_name }) : t("sites.applying", { server: s.server_name })}
          </div>
        )}
        {deleting && (
          <div className="flex items-center gap-3 rounded-lg bg-sunk px-4 py-3 text-muted" role="status">
            <Loader2 className="size-5 shrink-0 animate-spin" aria-hidden />
            {s.message ? t("sites.delete_failed", { reason: s.message }) : t("sites.deleting")}
          </div>
        )}
        {s.state === "error" && (
          <div className="grid gap-3 rounded-lg bg-crit-soft p-4 text-crit" role="alert">
            <p className="font-semibold">{t("sites.error_title")}</p>
            <p className="text-sm break-words" dir="auto">
              {s.message}
            </p>
            <div>
              <Button size="sm" onClick={() => save()} loading={busy}>
                <RotateCw className="size-4" aria-hidden />
                {t("sites.retry")}
              </Button>
            </div>
          </div>
        )}

        <div className="grid gap-6 xl:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
          <div className="grid content-start gap-6">
          {!deleting && <WordPressCard site={s} />}
          {!deleting && <CronCard siteId={s.id} />}
          {!deleting && <TrafficCard base={`/sites/${s.id}`} />}
          {!deleting && <LogsCard base={`/sites/${s.id}`} php />}
          {!deleting && <RedirectsCard site={s} />}
          {!deleting && <HotlinkCard site={s} />}
          {!deleting && s.kind === "php" && <PHPSettingsCard site={s} />}
          <Card className="p-6">
            <h2 className="mb-5 text-lg font-semibold">{t("sites.settings")}</h2>
            {settings && (
              <form onSubmit={save} className="grid gap-5">
                <ErrorAlert>{error}</ErrorAlert>
                <label className="grid gap-1.5">
                  <span className="text-sm font-medium">{t("sites.aliases")}</span>
                  <span className="text-sm text-muted">{t("sites.aliases_hint")}</span>
                  <textarea
                    value={aliases}
                    onChange={(e) => setAliases(e.target.value)}
                    rows={3}
                    dir="ltr"
                    disabled={deleting}
                    className="rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm text-ink"
                  />
                </label>
                <SiteSettingsFields value={settings} onChange={setSettings} />
                <div className="flex items-center gap-3">
                  <Button type="submit" variant="primary" loading={busy} disabled={deleting}>
                    {t("sites.save")}
                  </Button>
                  {saved && <span className="text-sm text-ok">{t("sites.saved")}</span>}
                </div>
              </form>
            )}
          </Card>
          </div>

          <div className="grid content-start gap-6">
            {!deleting && <SiteSFTP site={s} server={server.data} />}
            {!deleting && <SiteFTP site={s} server={server.data} />}
            {!deleting && <CertificateCard site={s} />}
            <Card className="p-6">
              <h2 className="mb-4 text-lg font-semibold">{t("sites.details")}</h2>
              <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2.5 text-sm">
                <Fact label={t("sites.addresses")}>
                  <span className="grid">
                    {hosts.map((h) => (
                      <bdi key={h}>{h}</bdi>
                    ))}
                  </span>
                </Fact>
                <Fact label={t("sites.col.server")}>
                  {me.data?.role === "admin" ? (
                    <Link to={`/servers/${s.server_id}`} className="text-accent hover:underline">
                      {s.server_name}
                    </Link>
                  ) : (
                    s.server_name
                  )}
                </Fact>
                {s.owner_name && <Fact label={t("customers.owner")}>{s.owner_name}</Fact>}
                <Fact label={t("sites.linux_user")} copy={s.linux_user}>
                  <span className="font-mono" dir="ltr">{s.linux_user}</span>
                </Fact>
                <Fact label={t("sites.docroot")} copy={`/home/${s.linux_user}/public_html`}>
                  <span className="font-mono" dir="ltr">/home/{s.linux_user}/public_html</span>
                </Fact>
                <Fact label={t("sites.created")}>{formatDateTime(s.created_at, i18n.language)}</Fact>
              </dl>
            </Card>
            <Card className="grid gap-3 p-6">
              <h2 className="text-lg font-semibold">{t("sites.delete.title_short")}</h2>
              <p className="text-sm text-muted">{t("sites.delete.body")}</p>
              <div>
                <Button variant="danger" onClick={() => setConfirmDelete(true)} disabled={deleting}>
                  <Trash2 className="size-4" aria-hidden />
                  {t("sites.delete.button")}
                </Button>
              </div>
            </Card>
          </div>
        </div>
      </div>

      <DeleteSiteDialog open={confirmDelete} site={s} onClose={() => setConfirmDelete(false)} />
    </>
  );
}

/** Deleting removes the site's files, so it asks for the domain to be typed. */
function DeleteSiteDialog({ open, site, onClose }: { open: boolean; site: Site; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const matches = typed.trim().toLowerCase() === site.domain_display.toLowerCase() || typed.trim().toLowerCase() === site.domain;
  return (
    <Dialog open={open} onClose={onClose} title={t("sites.delete.title", { domain: site.domain_display })}>
      <form
        className="grid gap-5"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          try {
            qc.setQueryData(["site", site.id], await api.del(`/sites/${site.id}`).then(() => ({ ...site, state: "deleting" })));
            qc.invalidateQueries({ queryKey: ["tasks"] });
            onClose();
          } catch (err) {
            setError(errorMessage(t, err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <p className="text-muted">{t("sites.delete.warning", { user: site.linux_user })}</p>
        <ErrorAlert>{error}</ErrorAlert>
        <Field label={t("sites.delete.confirm", { domain: site.domain_display })} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="danger" loading={busy} disabled={!matches}>
            {t("sites.delete.button")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

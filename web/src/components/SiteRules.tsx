import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { Plus, ShieldCheck, Trash2 } from "lucide-react";
import { api, errorMessage, type Site, type SiteWeb } from "../lib/api";
import { formatDateTime } from "../lib/format";
import { usePHPVersions } from "../pages/SitesPage";
import { Badge, Button, Card, ErrorAlert, Field, cx } from "./ui";

const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";
const textarea = "rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm text-ink";
const defaultExtensions = ["jpg", "jpeg", "png", "gif", "webp", "avif", "svg", "mp4", "webm", "mp3"];

const lines = (s: string) =>
  s
    .split(/[\n,]/)
    .map((l) => l.trim())
    .filter(Boolean);

/** Saves one part of a site's web settings, keeping the rest as the site has it. */
function useSaveWeb(site: Site) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const save = async (part: Partial<SiteWeb>) => {
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      const updated = await api.put<Site>(`/sites/${site.id}/web`, { ...site.web, ...part });
      qc.setQueryData(["site", site.id], updated);
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setSaved(true);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return { save, busy, error, saved };
}

function SaveRow({ busy, saved, disabled }: { busy: boolean; saved: boolean; disabled?: boolean }) {
  const { t } = useTranslation();
  return (
    <div className="flex items-center gap-3">
      <Button type="submit" variant="primary" loading={busy} disabled={disabled}>
        {t("sites.save")}
      </Button>
      {saved && <span className="text-sm text-ok">{t("sites.saved")}</span>}
    </div>
  );
}

function RuleCard({ title, intro, children }: { title: string; intro: string; children: ReactNode }) {
  return (
    <Card className="grid gap-4 p-6">
      <div>
        <h2 className="text-lg font-semibold">{title}</h2>
        <p className="text-sm text-muted">{intro}</p>
      </div>
      {children}
    </Card>
  );
}

type Rule = { from: string; to: string; code: number; keep_path: boolean };

export function RedirectsCard({ site }: { site: Site }) {
  const { t } = useTranslation();
  const { save, busy, error, saved } = useSaveWeb(site);
  const [rules, setRules] = useState<Rule[]>([]);
  const [aliasRedirect, setAliasRedirect] = useState(false);
  useEffect(() => {
    setRules((site.web.redirects ?? []).map((r) => ({ ...r, keep_path: !!r.keep_path })));
    setAliasRedirect(!!site.web.alias_redirect);
  }, [site.web]);
  const set = (i: number, r: Partial<Rule>) => setRules(rules.map((x, j) => (j === i ? { ...x, ...r } : x)));

  return (
    <RuleCard title={t("redirects.title")} intro={t("redirects.intro")}>
      <form
        className="grid gap-4"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save({ alias_redirect: aliasRedirect, redirects: rules.filter((r) => r.from || r.to) });
        }}
      >
        <ErrorAlert>{error}</ErrorAlert>
        {site.aliases.length > 0 && (
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" checked={aliasRedirect} onChange={(e) => setAliasRedirect(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
            <span>
              <span className="font-medium">{t("redirects.aliases", { domain: site.domain_display })}</span>
              <span className="block text-muted">{t("redirects.aliases_hint")}</span>
            </span>
          </label>
        )}
        {rules.length > 0 && (
          <ul className="grid gap-3">
            {rules.map((r, i) => (
              <li key={i} className="grid gap-3 rounded-lg border border-line p-3 sm:grid-cols-[1fr_1.4fr_auto_auto] sm:items-end">
                <Field
                  label={t("redirects.from")}
                  value={r.from}
                  onChange={(e) => set(i, { from: e.target.value.trim() })}
                  dir="ltr"
                  placeholder="/old-page"
                  className="[&_input]:font-mono"
                />
                <Field
                  label={t("redirects.to")}
                  value={r.to}
                  onChange={(e) => set(i, { to: e.target.value.trim() })}
                  dir="ltr"
                  placeholder="https://example.com/new-page"
                  className="[&_input]:font-mono"
                />
                <label className="grid gap-1.5">
                  <span className="text-sm font-medium">{t("redirects.code")}</span>
                  <select value={r.code} onChange={(e) => set(i, { code: Number(e.target.value) })} className={select}>
                    {[301, 302, 307, 308].map((c) => (
                      <option key={c} value={c}>
                        {t(`redirects.codes.${c}`)}
                      </option>
                    ))}
                  </select>
                </label>
                <Button type="button" variant="ghost" onClick={() => setRules(rules.filter((_, j) => j !== i))} aria-label={t("redirects.remove")}>
                  <Trash2 className="size-4" aria-hidden />
                </Button>
                {r.from.endsWith("/*") && (
                  <label className="flex items-center gap-2 text-sm sm:col-span-4">
                    <input type="checkbox" checked={r.keep_path} onChange={(e) => set(i, { keep_path: e.target.checked })} className="accent-[var(--accent)]" />
                    {t("redirects.keep_path")}
                  </label>
                )}
              </li>
            ))}
          </ul>
        )}
        <p className="text-sm text-muted">{t("redirects.hint")}</p>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <SaveRow busy={busy} saved={saved} disabled={site.state === "deleting"} />
          <Button type="button" onClick={() => setRules([...rules, { from: "", to: "", code: 301, keep_path: false }])}>
            <Plus className="size-4" aria-hidden />
            {t("redirects.add")}
          </Button>
        </div>
      </form>
    </RuleCard>
  );
}

export function HotlinkCard({ site }: { site: Site }) {
  const { t } = useTranslation();
  const { save, busy, error, saved } = useSaveWeb(site);
  const [on, setOn] = useState(false);
  const [extensions, setExtensions] = useState("");
  const [allowed, setAllowed] = useState("");
  useEffect(() => {
    setOn(!!site.web.hotlink);
    setExtensions((site.web.hotlink?.extensions ?? defaultExtensions).join(", "));
    setAllowed((site.web.hotlink?.allowed ?? []).join("\n"));
  }, [site.web]);
  return (
    <RuleCard title={t("hotlink.title")} intro={t("hotlink.intro")}>
      <form
        className="grid gap-4"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save({ hotlink: on ? { extensions: lines(extensions), allowed: lines(allowed) } : null });
        }}
      >
        <ErrorAlert>{error}</ErrorAlert>
        <label className="flex items-center gap-2 text-sm font-medium">
          <input type="checkbox" checked={on} onChange={(e) => setOn(e.target.checked)} className="accent-[var(--accent)]" />
          {t("hotlink.enable")}
        </label>
        {on && (
          <>
            <Field
              label={t("hotlink.extensions")}
              hint={t("hotlink.extensions_hint")}
              value={extensions}
              onChange={(e) => setExtensions(e.target.value)}
              dir="ltr"
              className="[&_input]:font-mono"
            />
            <label className="grid gap-1.5">
              <span className="text-sm font-medium">{t("hotlink.allowed")}</span>
              <textarea
                value={allowed}
                onChange={(e) => setAllowed(e.target.value)}
                rows={3}
                dir="ltr"
                className={textarea}
                placeholder="partner.com\n*.cdn.example.com"
              />
              <span className="text-sm text-muted">{t("hotlink.allowed_hint")}</span>
            </label>
          </>
        )}
        <SaveRow busy={busy} saved={saved} disabled={site.state === "deleting"} />
      </form>
    </RuleCard>
  );
}

export function PHPSettingsCard({ site }: { site: Site }) {
  const { t } = useTranslation();
  const php = usePHPVersions();
  const { save, busy, error, saved } = useSaveWeb(site);
  const [values, setValues] = useState<Record<string, string>>({});
  useEffect(() => setValues(site.web.php ?? {}), [site.web]);
  const settings = php.data?.settings ?? [];
  return (
    <RuleCard title={t("php_settings.title")} intro={t("php_settings.intro", { version: site.php_version })}>
      <form
        className="grid gap-4"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save({ php: values });
        }}
      >
        <ErrorAlert>{error}</ErrorAlert>
        <div className="grid gap-4 sm:grid-cols-2">
          {settings.map((s) => {
            const value = values[s.name] ?? "";
            const changed = value !== "" && value !== s.default;
            return s.flag ? (
              <label key={s.name} className="grid content-start gap-1.5">
                <span className={cx("font-mono text-sm", changed && "font-semibold")} dir="ltr">
                  {s.name}
                </span>
                <select value={value || s.default} onChange={(e) => setValues({ ...values, [s.name]: e.target.value })} className={select}>
                  <option value="On">On</option>
                  <option value="Off">Off</option>
                </select>
                <span className="text-sm text-muted">{t(`php_settings.help.${s.name.replace(".", "_")}`)}</span>
              </label>
            ) : (
              <Field
                key={s.name}
                label={s.name}
                hint={t(`php_settings.help.${s.name.replace(".", "_")}`)}
                value={value}
                placeholder={s.default}
                onChange={(e) => setValues({ ...values, [s.name]: e.target.value.trim() })}
                dir="ltr"
                className={cx("[&_input]:font-mono [&_label]:font-mono", changed && "[&_label]:font-semibold")}
              />
            );
          })}
        </div>
        <p className="text-sm text-muted">{t("php_settings.hint")}</p>
        <SaveRow busy={busy} saved={saved} disabled={site.state === "deleting"} />
      </form>
    </RuleCard>
  );
}

/** An uploaded certificate instead of a free one. */
export function CertificateCard({ site }: { site: Site }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [cert, setCert] = useState("");
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [uncovered, setUncovered] = useState<string[]>([]);
  const custom = site.tls === "custom";
  const readFile = (f: File | undefined, to: (s: string) => void) => f?.text().then(to);

  const upload = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const r = await api.put<{ site: Site; uncovered: string[] }>(`/sites/${site.id}/certificate`, { cert_pem: cert, key_pem: key });
      qc.setQueryData(["site", site.id], r.site);
      setUncovered(r.uncovered);
      setOpen(false);
      setCert("");
      setKey("");
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    setBusy(true);
    setError("");
    try {
      qc.setQueryData(["site", site.id], await api.del<Site>(`/sites/${site.id}/certificate`));
      setUncovered([]);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const soon = site.tls_expires_at && Date.parse(site.tls_expires_at) - Date.now() < 14 * 86400 * 1000;

  return (
    <RuleCard title={t("certificate.title")} intro={custom ? t("certificate.intro_custom") : t("certificate.intro")}>
      <ErrorAlert>{error}</ErrorAlert>
      {custom && site.tls_expires_at && (
        <div className="grid gap-2 rounded-lg bg-sunk px-4 py-3 text-sm">
          <span className="flex flex-wrap items-center gap-2">
            <ShieldCheck className="size-4 text-ok" aria-hidden />
            {t("certificate.expires", { date: formatDateTime(site.tls_expires_at, i18n.language) })}
            {soon && <Badge tone="warn">{t("certificate.soon")}</Badge>}
          </span>
          <span className="font-mono text-xs break-all text-muted" dir="ltr">
            {(site.tls_names ?? []).join(", ")}
          </span>
        </div>
      )}
      {uncovered.length > 0 && (
        <p className="rounded-lg bg-warn-soft px-3 py-2 text-sm text-warn">{t("certificate.uncovered", { names: uncovered.join(", ") })}</p>
      )}
      {open ? (
        <form onSubmit={upload} className="grid gap-4">
          {(
            [
              ["cert", t("certificate.cert"), cert, setCert, "-----BEGIN CERTIFICATE-----"],
              ["key", t("certificate.key"), key, setKey, "-----BEGIN PRIVATE KEY-----"],
            ] as const
          ).map(([name, label, value, setValue, placeholder]) => (
            <label key={name} className="grid gap-1.5">
              <span className="text-sm font-medium">{label}</span>
              <textarea
                value={value}
                onChange={(e) => setValue(e.target.value)}
                rows={5}
                dir="ltr"
                spellCheck={false}
                className={textarea}
                placeholder={placeholder}
                required
              />
              <input
                type="file"
                accept=".pem,.crt,.cer,.key,.txt"
                onChange={(e) => readFile(e.target.files?.[0], setValue)}
                className="text-sm text-muted file:me-3 file:rounded-md file:border file:border-line file:bg-surface file:px-3 file:py-1 file:text-ink"
              />
            </label>
          ))}
          <p className="text-sm text-muted">{t("certificate.hint")}</p>
          <div className="flex gap-2">
            <Button type="submit" variant="primary" loading={busy}>
              {t("certificate.use")}
            </Button>
            <Button type="button" variant="ghost" onClick={() => setOpen(false)}>
              {t("common.cancel")}
            </Button>
          </div>
        </form>
      ) : (
        <div className="flex flex-wrap gap-2">
          <Button onClick={() => setOpen(true)} disabled={site.state === "deleting"}>
            {custom ? t("certificate.replace") : t("certificate.upload")}
          </Button>
          {custom && (
            <Button variant="ghost" onClick={remove} loading={busy}>
              {t("certificate.remove")}
            </Button>
          )}
        </div>
      )}
    </RuleCard>
  );
}

import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, Loader2, ShieldAlert } from "lucide-react";
import { api, errorMessage, type Site, type Task } from "../lib/api";
import { useMe } from "../lib/session";
import { Button, Card, CopyButton, ErrorAlert, Field } from "./ui";
import { useTasks } from "./Tasks";

/** WordPress languages offered at install; each is shown in its own language. */
export const wordpressLocales: [string, string][] = [
  ["en_US", "English"], ["bn_BD", "বাংলা"], ["hi_IN", "हिन्दी"], ["ur", "اردو"], ["ar", "العربية"],
  ["es_ES", "Español"], ["fr_FR", "Français"], ["de_DE", "Deutsch"], ["pt_BR", "Português (Brasil)"],
  ["id_ID", "Bahasa Indonesia"], ["tr_TR", "Türkçe"], ["ru_RU", "Русский"], ["zh_CN", "简体中文"], ["ja", "日本語"],
];

export function defaultWordPressLocale(uiLanguage: string) {
  return uiLanguage.startsWith("bn") ? "bn_BD" : "en_US";
}

/** A sensible admin name from an email address: "rahim.k@x.com" -> "rahim.k". */
export function adminNameFrom(email: string) {
  const name = email.split("@")[0].replace(/[^A-Za-z0-9._-]/g, "");
  return name.length >= 3 ? name.slice(0, 60) : "admin";
}

export interface WordPressCredentials {
  admin_user: string;
  admin_password: string;
  admin_url: string;
}

/** Shown once after starting an install. */
export function WordPressCredentialsBox({ creds }: { creds: WordPressCredentials }) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-2 rounded-lg bg-warn-soft p-3 text-warn">
      <p className="flex items-start gap-2 text-sm">
        <ShieldAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
        {t("wordpress.password_once")}
      </p>
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] items-center gap-x-4 gap-y-1 text-sm text-ink">
        <dt className="text-muted">{t("wordpress.admin_user")}</dt>
        <dd className="flex items-center gap-1 font-mono" dir="ltr">
          {creds.admin_user} <CopyButton text={creds.admin_user} className="-my-1" />
        </dd>
        <dt className="text-muted">{t("databases.password")}</dt>
        <dd className="flex items-center gap-1 font-mono" dir="ltr">
          {creds.admin_password} <CopyButton text={creds.admin_password} className="-my-1" />
        </dd>
      </dl>
    </div>
  );
}

export function WordPressCard({ site }: { site: Site }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const me = useMe();
  // Credentials from this browser session only (e.g. set by the add-website dialog).
  const creds = useQuery<WordPressCredentials | null>({ queryKey: ["wp-credentials", site.id], queryFn: () => null, staleTime: Infinity, gcTime: Infinity });
  const tasks = useTasks(`server_id=${encodeURIComponent(site.server_id)}&limit=20`);
  const installTask = tasks.data?.find((x: Task) => x.kind === "site.install_wordpress" && x.params.site_id === site.id);
  const running = installTask && (installTask.status === "queued" || installTask.status === "running");

  const [title, setTitle] = useState(site.domain_display);
  const [user, setUser] = useState(adminNameFrom(me.data?.email ?? ""));
  const [email, setEmail] = useState(me.data?.email ?? "");
  const [locale, setLocale] = useState(defaultWordPressLocale(i18n.language));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  if (site.kind !== "php" && !site.app) return null;

  if (site.app === "wordpress") {
    return (
      <Card className="grid gap-3 p-6">
        <h2 className="text-lg font-semibold">WordPress {site.app_version}</h2>
        {creds.data && <WordPressCredentialsBox creds={creds.data} />}
        <div className="flex flex-wrap gap-2">
          <a href={`https://${site.domain}/wp-admin/`} target="_blank" rel="noreferrer" className="inline-flex h-10 items-center gap-2 rounded-lg bg-accent px-4 font-medium text-accent-ink hover:brightness-110">
            {t("wordpress.open_admin")}
            <ExternalLink className="size-4 rtl:-scale-x-100" aria-hidden />
          </a>
        </div>
        <p className="text-sm text-muted">{t("wordpress.installed_hint")}</p>
      </Card>
    );
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const r = await api.post<WordPressCredentials & { task: Task }>(`/sites/${site.id}/wordpress`, { title, admin_user: user, admin_email: email, locale });
      qc.setQueryData(["wp-credentials", site.id], { admin_user: r.admin_user, admin_password: r.admin_password, admin_url: r.admin_url });
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="grid gap-4 p-6">
      <div>
        <h2 className="text-lg font-semibold">{t("wordpress.title")}</h2>
        <p className="mt-1 text-sm text-muted">{t("wordpress.intro")}</p>
      </div>
      {creds.data && <WordPressCredentialsBox creds={creds.data} />}
      {running ? (
        <div className="flex items-center gap-3 rounded-lg bg-accent-soft px-4 py-3 text-accent" role="status">
          <Loader2 className="size-5 shrink-0 animate-spin" aria-hidden />
          {t("wordpress.installing")}
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-4">
          {installTask?.status === "failed" && <ErrorAlert>{t("wordpress.failed", { reason: installTask.error })}</ErrorAlert>}
          <ErrorAlert>{error}</ErrorAlert>
          <Field label={t("wordpress.site_title")} value={title} onChange={(e) => setTitle(e.target.value)} maxLength={100} required />
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={t("wordpress.admin_user")} value={user} onChange={(e) => setUser(e.target.value)} className="[&_input]:font-mono" autoComplete="off" required />
            <Field label={t("wordpress.admin_email")} type="email" value={email} onChange={(e) => setEmail(e.target.value)} required />
          </div>
          <label className="grid max-w-64 gap-1.5">
            <span className="text-sm font-medium">{t("wordpress.language")}</span>
            <select value={locale} onChange={(e) => setLocale(e.target.value)} className="h-10 rounded-lg border border-line bg-surface px-3 text-ink">
              {wordpressLocales.map(([code, name]) => (
                <option key={code} value={code}>
                  {name}
                </option>
              ))}
            </select>
          </label>
          <p className="text-sm text-muted">{t("wordpress.empty_note")}</p>
          <div>
            <Button type="submit" variant="primary" loading={busy} disabled={site.state === "deleting"}>
              {t("wordpress.install")}
            </Button>
          </div>
        </form>
      )}
    </Card>
  );
}

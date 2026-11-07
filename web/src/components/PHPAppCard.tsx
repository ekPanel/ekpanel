import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink, Loader2, ShieldAlert } from "lucide-react";
import { api, errorMessage, type Site, type Task } from "../lib/api";
import { useMe } from "../lib/session";
import { Button, Card, CopyButton, ErrorAlert, Field, cx } from "./ui";
import { useTasks } from "./Tasks";
import { adminNameFrom } from "./WordPressCard";

export interface PHPApp {
  id: string;
  name: string;
  version: string;
  min_php: string;
  doc_root?: string;
  admin: boolean;
  admin_path?: string;
  website: string;
}

interface Started {
  admin_user?: string;
  admin_password?: string;
  admin_url?: string;
}

const atLeast = (v: string, min: string) => {
  const [a, b] = v.split(".").map(Number);
  const [c, d] = min.split(".").map(Number);
  return a > c || (a === c && b >= d);
};

/** Joomla, Drupal, Moodle and Laravel: the installer on an empty PHP site, or what is installed. */
export function PHPAppCard({ site }: { site: Site }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const me = useMe();
  const apps = useQuery({ queryKey: ["php-apps"], queryFn: () => api.get<PHPApp[]>("/php-apps"), staleTime: Infinity });
  const creds = useQuery<Started | null>({ queryKey: ["app-credentials", site.id], queryFn: () => null, staleTime: Infinity, gcTime: Infinity });
  const tasks = useTasks(`server_id=${encodeURIComponent(site.server_id)}&limit=20`);
  const task = tasks.data?.find((x: Task) => x.kind === "site.install_app" && x.params.site_id === site.id);
  const running = task && (task.status === "queued" || task.status === "running");
  const [choice, setChoice] = useState("");
  const [title, setTitle] = useState(site.domain_display);
  const [user, setUser] = useState(adminNameFrom(me.data?.email ?? ""));
  const [email, setEmail] = useState(me.data?.email ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  if (site.kind !== "php" || site.app === "wordpress" || !apps.data) return null;
  const installed = site.app ? apps.data.find((a) => a.id === site.app) : undefined;

  if (installed) {
    return (
      <Card className="grid gap-3 p-6">
        <h2 className="text-lg font-semibold">
          {installed.name} {site.app_version}
        </h2>
        {creds.data?.admin_password && <Credentials started={creds.data} />}
        <div className="flex flex-wrap gap-2">
          <a
            href={`https://${site.domain}${installed.admin_path ?? "/"}`}
            target="_blank"
            rel="noreferrer"
            className="inline-flex h-10 items-center gap-2 rounded-lg bg-accent px-4 font-medium text-accent-ink hover:brightness-110"
          >
            {installed.admin ? t("phpapps.open_admin", { name: installed.name }) : t("sites.visit")}
            <ExternalLink className="size-4 rtl:-scale-x-100" aria-hidden />
          </a>
        </div>
        {installed.doc_root && <p className="text-sm text-muted">{t("phpapps.doc_root", { folder: `public_html/${installed.doc_root}` })}</p>}
        {installed.id === "moodle" && <p className="text-sm text-muted">{t("phpapps.moodle_note")}</p>}
        {installed.id === "laravel" && <p className="text-sm text-muted">{t("phpapps.laravel_note")}</p>}
      </Card>
    );
  }

  const app = apps.data.find((a) => a.id === choice);
  const php = site.php_version ?? "";
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!app) return;
    setBusy(true);
    setError("");
    try {
      const r = await api.post<Started & { task: Task }>(`/sites/${site.id}/app`, { app: app.id, title, admin_user: user, admin_email: email });
      qc.setQueryData(["app-credentials", site.id], { admin_user: r.admin_user, admin_password: r.admin_password, admin_url: r.admin_url });
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
        <h2 className="text-lg font-semibold">{t("phpapps.title")}</h2>
        <p className="mt-1 text-sm text-muted">{t("phpapps.intro")}</p>
      </div>
      {creds.data?.admin_password && <Credentials started={creds.data} />}
      {running ? (
        <div className="flex items-center gap-3 rounded-lg bg-accent-soft px-4 py-3 text-accent" role="status">
          <Loader2 className="size-5 shrink-0 animate-spin" aria-hidden />
          {t("phpapps.installing", { name: apps.data.find((a) => a.id === task?.params.app)?.name ?? "" })}
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-4">
          {task?.status === "failed" && <ErrorAlert>{t("phpapps.failed", { reason: task.error })}</ErrorAlert>}
          <ErrorAlert>{error}</ErrorAlert>
          <div className="grid gap-2 sm:grid-cols-2">
            {apps.data.map((a) => {
              const ok = atLeast(php, a.min_php);
              return (
                <label
                  key={a.id}
                  className={cx(
                    "flex gap-3 rounded-lg border p-3",
                    !ok ? "cursor-not-allowed opacity-60" : "cursor-pointer",
                    choice === a.id ? "border-accent bg-accent-soft" : "border-line hover:bg-sunk",
                  )}
                >
                  <input
                    type="radio"
                    name="app"
                    disabled={!ok}
                    checked={choice === a.id}
                    onChange={() => setChoice(a.id)}
                    className="mt-1 accent-[var(--accent)]"
                  />
                  <span className="grid gap-0.5">
                    <span className="font-medium">
                      {a.name} <span className="text-sm font-normal text-muted">{a.version}</span>
                    </span>
                    <span className="text-sm text-muted">{ok ? t(`phpapps.about.${a.id}`) : t("phpapps.needs_php", { version: a.min_php })}</span>
                  </span>
                </label>
              );
            })}
          </div>
          {app && (
            <>
              <Field label={t("wordpress.site_title")} value={title} onChange={(e) => setTitle(e.target.value)} maxLength={100} required />
              {app.admin && (
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field
                    label={t("wordpress.admin_user")}
                    value={user}
                    onChange={(e) => setUser(e.target.value)}
                    className="[&_input]:font-mono"
                    autoComplete="off"
                    required
                  />
                  <Field label={t("wordpress.admin_email")} type="email" value={email} onChange={(e) => setEmail(e.target.value)} required />
                </div>
              )}
              <p className="text-sm text-muted">{t("phpapps.note", { name: app.name })}</p>
            </>
          )}
          <div>
            <Button type="submit" variant="primary" loading={busy} disabled={!app || site.state === "deleting"}>
              {app ? t("phpapps.install", { name: app.name }) : t("phpapps.choose")}
            </Button>
          </div>
        </form>
      )}
    </Card>
  );
}

function Credentials({ started }: { started: Started }) {
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
          {started.admin_user} <CopyButton text={started.admin_user ?? ""} className="-my-1" />
        </dd>
        <dt className="text-muted">{t("databases.password")}</dt>
        <dd className="flex items-center gap-1 font-mono" dir="ltr">
          {started.admin_password} <CopyButton text={started.admin_password ?? ""} className="-my-1" />
        </dd>
      </dl>
    </div>
  );
}

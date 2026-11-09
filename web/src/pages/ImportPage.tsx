import { useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, FileArchive, Info, Loader2, XCircle } from "lucide-react";
import { api, errorMessage, type Server, type Task } from "../lib/api";
import { formatBytes } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, ErrorAlert, Field, cx } from "../components/ui";
import { OwnerField } from "./CustomersPage";

const importDir = "/srv/ekpanel/imports";
const select = "h-9 rounded-lg border border-line bg-surface px-2 text-sm text-ink";

interface CPanelDomain {
  domain: string;
  kind: "main" | "addon" | "sub";
  docroot: string;
  aliases: string[] | null;
  php_version: string;
  has_cert: boolean;
}

interface CPanelDatabase {
  name: string;
  user: string;
  bytes: number;
}

interface Inspection {
  user: string;
  main_domain: string;
  domains: CPanelDomain[] | null;
  databases: CPanelDatabase[] | null;
  cron: string[] | null;
  home_bytes: number;
  cron_settings?: string[];
  mail: CPanelMailDomain[] | null;
  zones: { name: string; records: unknown[] | null }[] | null;
  taken_domains: string[];
  taken_databases: string[];
  php_versions: string[];
  new_password_databases: string[];
  new_password_mailboxes: string[];
  mail_enabled: boolean;
  taken_mail: string[];
  name_servers: boolean;
  taken_zones: string[];
}

interface CPanelMailDomain {
  domain: string;
  mailboxes: { name: string; quota_mb?: number; bytes: number; messages: number; mdbox?: boolean }[] | null;
  forwarders: { name: string; to: string[] }[] | null;
  catch_all?: string;
  other?: string[];
}

interface ImportReport {
  sites: string[];
  databases: string[] | null;
  cron: number;
  mail_domains?: string[];
  mailboxes?: number;
  forwarders?: number;
  mail_messages?: number;
  mail_failed?: number;
  new_mail_password?: string[];
  zones?: string[];
  files: number;
  bytes: number;
  new_password?: string[];
  paths_changed?: string[];
  paths_changed_count?: number;
  not_imported: ImportNote[] | null;
}

interface ImportFile {
  name: string;
  kind: "archive" | "zip" | "dump";
  bytes: number;
}

interface SiteInspection {
  root: string;
  files: number;
  bytes: number;
  kind: "wordpress" | "php" | "static";
  domain?: string;
  wordpress?: { db_name: string; db_user: string; db_host: string; table_prefix?: string };
  dumps: { name: string; bytes: number }[] | null;
  keeps_password: boolean;
  domain_taken: boolean;
  database_taken: boolean;
  php_versions: string[];
}

/** One thing not imported; code picks the sentence (imports.note.*), the rest fills it in. */
interface ImportNote {
  code: string;
  name?: string;
  detail?: string;
  count?: number;
  domains?: number;
}

/** Import → cPanel: read a cpmove backup on a server, choose what to bring over, and import it. */
export function ImportPage() {
  const { t, i18n } = useTranslation();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers") });
  const ready = (servers.data ?? []).filter((s) => s.enrolled_at).sort((a, b) => Number(b.online) - Number(a.online));
  const [server, setServer] = useState("");
  const [file, setFile] = useState("");
  // The file that was read: what the choices below belong to, whatever the field says now.
  const [readFile, setReadFile] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [plan, setPlan] = useState<Inspection | null>(null);
  const [sitePlan, setSitePlan] = useState<SiteInspection | null>(null);
  const [mode, setMode] = useState<"cpanel" | "archive">("cpanel");
  const [taskId, setTaskId] = useState("");
  const [dbPassword, setDBPassword] = useState("");
  const files = useQuery({
    queryKey: ["import-files", server],
    queryFn: () => api.get<ImportFile[]>(`/imports/files?server_id=${server}`),
    enabled: !!server,
    retry: false,
  });
  const choices = (files.data ?? []).filter((f) => (mode === "cpanel" ? f.kind === "archive" : f.kind !== "dump"));

  useEffect(() => {
    if (!server && ready.length > 0) setServer(ready[0].id);
  }, [ready, server]);

  const inspect = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    setPlan(null);
    setSitePlan(null);
    setReadFile(file.trim());
    try {
      if (mode === "cpanel") setPlan(await api.post<Inspection>("/imports/inspect", { server_id: server, file: file.trim() }));
      else setSitePlan(await api.post<SiteInspection>("/imports/site/inspect", { server_id: server, archive: file.trim() }));
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  const chosen = ready.find((s) => s.id === server);
  const host = chosen?.public_ipv4 || chosen?.hostname || "SERVER";
  const example = mode === "cpanel" ? "cpmove-USER.tar.gz" : "example.com.zip";
  const scp = `scp ${example} root@${host}:${importDir}/`;
  const started = (id: string) => {
    setTaskId(id);
    window.scrollTo({ top: 0 });
  };
  return (
    // Room at the bottom: the running-tasks box must not cover the import button.
    <div className="grid gap-6 pb-28">
      <PageHeader title={t("imports.title")} description={t("imports.description")} />
      {taskId ? (
        <ImportProgress
          taskId={taskId}
          dbPassword={dbPassword}
          onAgain={() => {
            setTaskId("");
            setPlan(null);
            setSitePlan(null);
            setDBPassword("");
            files.refetch();
          }}
        />
      ) : (
        <>
          <div role="tablist" aria-label={t("imports.kinds")} className="flex flex-wrap gap-2">
            {(["cpanel", "archive"] as const).map((m) => (
              <button
                key={m}
                role="tab"
                aria-selected={mode === m}
                onClick={() => {
                  setMode(m);
                  setPlan(null);
                  setSitePlan(null);
                  setFile("");
                  setError("");
                }}
                className={cx("rounded-lg border px-4 py-2 text-start", mode === m ? "border-accent bg-accent-soft" : "border-line hover:bg-sunk")}
              >
                <span className="block font-medium">{t(`imports.mode.${m}`)}</span>
                <span className="block text-xs text-muted">{t(`imports.mode.${m}_hint`)}</span>
              </button>
            ))}
          </div>
          <Card className="grid gap-4 p-5">
            <h2 className="text-lg font-semibold">{t("imports.step_upload")}</h2>
            <p className="max-w-2xl text-sm text-muted">{t(mode === "cpanel" ? "imports.upload_body" : "imports.archive_body", { dir: importDir })}</p>
            <div className="flex min-w-0 items-center gap-2 rounded-lg border border-line bg-sunk px-3 py-2 font-mono text-sm">
              <span className="min-w-0 flex-1 truncate">{scp}</span>
              <CopyButton text={scp} />
            </div>
            <form onSubmit={inspect} className="grid gap-4 sm:grid-cols-[minmax(0,14rem)_minmax(0,1fr)_auto] sm:items-end">
              <label className="grid min-w-0 gap-1.5">
                <span className="text-sm font-medium">{t("sites.server")}</span>
                <select
                  value={server}
                  onChange={(e) => {
                    setServer(e.target.value);
                    setPlan(null);
                    setSitePlan(null);
                  }}
                  className="h-10 rounded-lg border border-line bg-surface px-3 text-ink"
                >
                  {ready.map((s) => (
                    <option key={s.id} value={s.id}>
                      {s.name}
                      {s.online ? "" : ` (${t("imports.offline")})`}
                    </option>
                  ))}
                </select>
              </label>
              <Field
                label={t("imports.file")}
                placeholder={example}
                list="import-files"
                value={file}
                onChange={(e) => setFile(e.target.value)}
                className="[&_input]:font-mono"
                autoComplete="off"
                spellCheck={false}
                required
              />
              <Button type="submit" variant="primary" loading={busy} disabled={!server || !file.trim()}>
                <FileArchive className="size-4" aria-hidden /> {t("imports.read")}
              </Button>
            </form>
            <datalist id="import-files">
              {choices.map((f) => (
                <option key={f.name} value={f.name}>
                  {formatBytes(f.bytes, i18n.language)}
                </option>
              ))}
            </datalist>
            {choices.length > 0 && <p className="text-sm text-muted">{t("imports.files_here", { names: choices.slice(0, 5).map((f) => f.name).join(", ") })}</p>}
            {busy && <p className="text-sm text-muted">{t("imports.reading")}</p>}
            <ErrorAlert>{error}</ErrorAlert>
          </Card>
          {plan && (
            <ImportChoices
              key={server + readFile}
              plan={plan}
              server={server}
              file={readFile}
              onStarted={started}
            />
          )}
          {sitePlan && (
            <ArchiveChoices
              key={server + readFile}
              plan={sitePlan}
              server={server}
              archive={readFile}
              dumps={(files.data ?? []).filter((f) => f.kind === "dump")}
              onStarted={(id, password) => {
                setDBPassword(password ?? "");
                started(id);
              }}
            />
          )}
        </>
      )}
    </div>
  );
}

function ImportChoices({ plan, server, file, onStarted }: { plan: Inspection; server: string; file: string; onStarted: (taskId: string) => void }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const domains = plan.domains ?? [];
  const dbs = plan.databases ?? [];
  const cron = plan.cron ?? [];
  const takenDomain = (d: CPanelDomain) => [d.domain, ...(d.aliases ?? [])].some((n) => plan.taken_domains.includes(n));
  const [sites, setSites] = useState<string[]>(() => domains.filter((d) => !takenDomain(d)).map((d) => d.domain));
  const [databases, setDatabases] = useState<string[]>(() => dbs.filter((d) => !plan.taken_databases.includes(d.name)).map((d) => d.name));
  const [php, setPHP] = useState<Record<string, string>>(() =>
    Object.fromEntries(domains.map((d) => [d.domain, plan.php_versions.includes(d.php_version) ? d.php_version : plan.php_versions.includes("8.4") ? "8.4" : plan.php_versions[0]])),
  );
  const [withCron, setWithCron] = useState(cron.length > 0);
  const mailDomains = plan.mail ?? [];
  const zones = plan.zones ?? [];
  const [mail, setMail] = useState<string[]>(() => (plan.mail_enabled ? mailDomains.filter((d) => !plan.taken_mail.includes(d.domain)).map((d) => d.domain) : []));
  const [chosenZones, setZones] = useState<string[]>(() => (plan.name_servers ? zones.filter((z) => !plan.taken_zones.includes(z.name)).map((z) => z.name) : []));
  const [owner, setOwner] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const lng = i18n.language;
  const toggle = (list: string[], set: (v: string[]) => void, name: string) => set(list.includes(name) ? list.filter((n) => n !== name) : [...list, name]);
  const mainChosen = domains.some((d) => d.kind === "main" && sites.includes(d.domain));

  const start = async () => {
    setBusy(true);
    setError("");
    try {
      const task = await api.post<Task>("/imports", {
        server_id: server,
        file,
        owner_id: owner,
        sites,
        databases,
        php: Object.fromEntries(sites.map((s) => [s, php[s]])),
        cron: withCron && mainChosen,
        mail,
        zones: chosenZones,
      });
      qc.invalidateQueries({ queryKey: ["mail-domains"] });
      qc.invalidateQueries({ queryKey: ["hosted-zones"] });
      qc.invalidateQueries({ queryKey: ["sites"] });
      qc.invalidateQueries({ queryKey: ["databases"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
      onStarted(task.id);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  const notes: string[] = [];
  if (domains.some((d) => d.has_cert)) notes.push(t("imports.not_certs"));
  for (const c of plan.cron_settings ?? []) notes.push(t("imports.note.cron_setting", { name: c }));

  return (
    <Card className="grid gap-6 p-5">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-lg font-semibold">{t("imports.step_choose", { user: plan.user })}</h2>
        <p className="text-sm text-muted">{t("imports.home_size", { size: formatBytes(plan.home_bytes, lng) })}</p>
      </div>

      <section className="grid gap-2">
        <h3 className="font-semibold">{t("imports.websites")}</h3>
        {domains.length === 0 ? (
          <p className="text-sm text-muted">{t("imports.no_websites")}</p>
        ) : (
          <ul className="divide-y divide-line rounded-lg border border-line">
            {domains.map((d) => {
              const taken = takenDomain(d);
              const on = sites.includes(d.domain);
              return (
                <li key={d.domain} className={cx("flex flex-wrap items-center gap-3 px-3 py-2.5", taken && "opacity-70")}>
                  <label className="flex min-w-0 flex-1 cursor-pointer items-start gap-3">
                    <input type="checkbox" checked={on} disabled={taken} onChange={() => toggle(sites, setSites, d.domain)} className="mt-1 accent-[var(--accent)]" />
                    <span className="grid min-w-0 gap-0.5">
                      <span className="flex flex-wrap items-center gap-2">
                        <span className="truncate font-medium">{d.domain}</span>
                        <Badge tone={d.kind === "main" ? "accent" : "neutral"}>{t(`imports.kind.${d.kind}`)}</Badge>
                        {taken && <Badge tone="warn">{t("imports.taken")}</Badge>}
                      </span>
                      <span className="truncate font-mono text-xs text-muted">~/{d.docroot}</span>
                      {!!d.aliases?.length && <span className="truncate text-xs text-muted">{t("imports.aliases", { names: d.aliases.join(", ") })}</span>}
                    </span>
                  </label>
                  <label className="flex items-center gap-2 text-sm">
                    <span className="text-muted">PHP</span>
                    <select value={php[d.domain]} disabled={!on} onChange={(e) => setPHP({ ...php, [d.domain]: e.target.value })} className={select}>
                      {plan.php_versions.map((v) => (
                        <option key={v} value={v}>
                          {v}
                        </option>
                      ))}
                    </select>
                    {d.php_version && d.php_version !== php[d.domain] && <span className="text-xs text-muted">{t("imports.was_php", { version: d.php_version })}</span>}
                  </label>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <section className="grid gap-2">
        <h3 className="font-semibold">{t("imports.databases")}</h3>
        {dbs.length === 0 ? (
          <p className="text-sm text-muted">{t("imports.no_databases")}</p>
        ) : (
          <ul className="divide-y divide-line rounded-lg border border-line">
            {dbs.map((d) => {
              const taken = plan.taken_databases.includes(d.name);
              return (
                <li key={d.name} className={cx("px-3 py-2.5", taken && "opacity-70")}>
                  <label className="flex cursor-pointer items-start gap-3">
                    <input type="checkbox" checked={databases.includes(d.name)} disabled={taken} onChange={() => toggle(databases, setDatabases, d.name)} className="mt-1 accent-[var(--accent)]" />
                    <span className="grid min-w-0 flex-1 gap-0.5">
                      <span className="flex flex-wrap items-center gap-2">
                        <span className="truncate font-mono font-medium">{d.name}</span>
                        <span className="text-sm text-muted tabular-nums">{formatBytes(d.bytes, lng)}</span>
                        {taken && <Badge tone="warn">{t("imports.taken")}</Badge>}
                      </span>
                      <span className="truncate text-xs text-muted">
                        {d.user ? t("imports.db_user", { user: d.user }) : t("imports.db_no_user")}
                        {d.user && plan.new_password_databases.includes(d.name) && ` · ${t("imports.db_new_password")}`}
                      </span>
                    </span>
                  </label>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      {cron.length > 0 && (
        <section className="grid gap-2">
          <label className="flex cursor-pointer items-start gap-3">
            <input type="checkbox" checked={withCron && mainChosen} disabled={!mainChosen} onChange={(e) => setWithCron(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
            <span className="grid gap-0.5">
              <span className="font-semibold">{t("imports.cron", { count: cron.length })}</span>
              <span className="text-sm text-muted">{mainChosen ? t("imports.cron_hint", { user: plan.user }) : t("imports.cron_needs_main")}</span>
            </span>
          </label>
          <ul className="grid gap-1 ps-7">
            {cron.map((c, i) => (
              <li key={i} className="truncate font-mono text-xs text-muted">
                {c}
              </li>
            ))}
          </ul>
        </section>
      )}

      {mailDomains.length > 0 && (
        <section className="grid gap-2">
          <h3 className="font-semibold">{t("imports.email")}</h3>
          {!plan.mail_enabled && <p className="text-sm text-warn">{t("imports.mail_off")}</p>}
          <ul className="divide-y divide-line rounded-lg border border-line">
            {mailDomains.map((d) => {
              const taken = plan.taken_mail.includes(d.domain);
              const boxes = d.mailboxes ?? [];
              const bytes = boxes.reduce((n, b) => n + b.bytes, 0);
              const messages = boxes.reduce((n, b) => n + b.messages, 0);
              const newPw = boxes.filter((b) => plan.new_password_mailboxes.includes(b.name + "@" + d.domain)).length;
              return (
                <li key={d.domain} className={cx("px-3 py-2.5", (taken || !plan.mail_enabled) && "opacity-70")}>
                  <label className="flex cursor-pointer items-start gap-3">
                    <input type="checkbox" checked={mail.includes(d.domain)} disabled={taken || !plan.mail_enabled} onChange={() => toggle(mail, setMail, d.domain)} className="mt-1 accent-[var(--accent)]" />
                    <span className="grid min-w-0 flex-1 gap-0.5">
                      <span className="flex flex-wrap items-center gap-2">
                        <span className="truncate font-medium">{d.domain}</span>
                        {taken && <Badge tone="warn">{t("imports.taken")}</Badge>}
                      </span>
                      <span className="text-xs text-muted">
                        {t("imports.mail_line", { mailboxes: boxes.length, forwarders: (d.forwarders ?? []).length, messages, size: formatBytes(bytes, lng) })}
                        {newPw > 0 && ` · ${t("imports.mail_new_passwords", { count: newPw })}`}
                      </span>
                      {!!boxes.length && <span className="truncate font-mono text-xs text-muted">{boxes.map((b) => b.name).join(", ")}</span>}
                    </span>
                  </label>
                </li>
              );
            })}
          </ul>
        </section>
      )}

      {zones.length > 0 && (
        <section className="grid gap-2">
          <h3 className="font-semibold">{t("imports.zones")}</h3>
          <p className="text-sm text-muted">{plan.name_servers ? t("imports.zones_hint") : t("imports.zones_off")}</p>
          <ul className="divide-y divide-line rounded-lg border border-line">
            {zones.map((z) => {
              const taken = plan.taken_zones.includes(z.name);
              return (
                <li key={z.name} className={cx("px-3 py-2.5", (taken || !plan.name_servers) && "opacity-70")}>
                  <label className="flex cursor-pointer items-center gap-3">
                    <input type="checkbox" checked={chosenZones.includes(z.name)} disabled={taken || !plan.name_servers} onChange={() => toggle(chosenZones, setZones, z.name)} className="accent-[var(--accent)]" />
                    <span className="min-w-0 truncate font-medium">{z.name}</span>
                    <span className="text-xs text-muted">{t("imports.zone_records", { count: (z.records ?? []).length })}</span>
                    {taken && <Badge tone="warn">{t("imports.taken")}</Badge>}
                  </label>
                </li>
              );
            })}
          </ul>
        </section>
      )}

      <OwnerField value={owner} onChange={setOwner} />

      {notes.length > 0 && (
        <div className="flex gap-3 rounded-lg bg-sunk px-4 py-3 text-sm">
          <Info className="mt-0.5 size-4 shrink-0 text-muted" aria-hidden />
          <div className="grid gap-1">
            <p className="font-medium">{t("imports.not_yet")}</p>
            <ul className="list-disc ps-5 text-muted">
              {notes.map((n) => (
                <li key={n}>{n}</li>
              ))}
            </ul>
          </div>
        </div>
      )}

      <ErrorAlert>{error}</ErrorAlert>
      <div className="flex flex-wrap items-center justify-end gap-3">
        <p className="text-sm text-muted">{t("imports.summary", { sites: sites.length, databases: databases.length, mail: mail.length, zones: chosenZones.length })}</p>
        <Button variant="primary" loading={busy} disabled={sites.length + databases.length + mail.length + chosenZones.length === 0} onClick={start}>
          {t("imports.start")}
        </Button>
      </div>
    </Card>
  );
}

function ImportProgress({ taskId, dbPassword, onAgain }: { taskId: string; dbPassword: string; onAgain: () => void }) {
  const { t, i18n } = useTranslation();
  const task = useQuery({
    queryKey: ["tasks", taskId],
    queryFn: () => api.get<Task>(`/tasks/${taskId}`),
    refetchInterval: (q) => (q.state.data?.status === "succeeded" || q.state.data?.status === "failed" ? false : 2000),
  });
  const d = task.data;
  const report = (d?.result ?? null) as ImportReport | null;
  const done = d?.status === "succeeded";
  const failed = d?.status === "failed";
  return (
    <Card className="grid gap-4 p-5" aria-live="polite">
      <div className="flex items-center gap-3">
        {done ? (
          <CheckCircle2 className="size-6 text-ok" aria-hidden />
        ) : failed ? (
          <XCircle className="size-6 text-crit" aria-hidden />
        ) : (
          <Loader2 className="size-6 animate-spin text-accent" aria-hidden />
        )}
        <h2 className="text-lg font-semibold">{done ? t("imports.done") : failed ? t("imports.failed") : t("imports.running")}</h2>
      </div>
      {!done && !failed && <p className="text-sm text-muted">{t("imports.running_body")}</p>}
      {dbPassword && (
        <div className="grid gap-2 rounded-lg bg-warn-soft px-4 py-3 text-sm text-warn">
          <p>{t("imports.db_password_once")}</p>
          <div className="flex min-w-0 items-center gap-2 font-mono text-ink">
            <span className="min-w-0 truncate">{dbPassword}</span>
            <CopyButton text={dbPassword} />
          </div>
        </div>
      )}
      {failed && d?.error && <ErrorAlert>{d.error}</ErrorAlert>}
      {failed && <p className="text-sm text-muted">{t("imports.failed_body")}</p>}
      {done && report && (
        <div className="grid gap-4">
          <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
            <dt className="text-muted">{t("imports.websites")}</dt>
            <dd>{report.sites.length ? report.sites.join(", ") : "—"}</dd>
            <dt className="text-muted">{t("imports.databases")}</dt>
            <dd className="font-mono">{report.databases?.length ? report.databases.join(", ") : "—"}</dd>
            <dt className="text-muted">{t("imports.files")}</dt>
            <dd>{t("imports.files_value", { count: report.files, size: formatBytes(report.bytes, i18n.language) })}</dd>
            <dt className="text-muted">{t("imports.cron_jobs")}</dt>
            <dd>{report.cron}</dd>
            {!!report.mail_domains?.length && (
              <>
                <dt className="text-muted">{t("imports.email")}</dt>
                <dd>
                  {report.mail_domains.join(", ")} ·{" "}
                  {t("imports.mail_done", { mailboxes: report.mailboxes ?? 0, forwarders: report.forwarders ?? 0, messages: report.mail_messages ?? 0 })}
                  {!!report.mail_failed && <span className="text-warn"> · {t("imports.mail_failed", { count: report.mail_failed })}</span>}
                </dd>
              </>
            )}
            {!!report.zones?.length && (
              <>
                <dt className="text-muted">{t("imports.zones")}</dt>
                <dd>{report.zones.join(", ")}</dd>
              </>
            )}
          </dl>
          {!!report.paths_changed_count && (
            <details className="text-sm">
              <summary className="cursor-pointer">{t("imports.paths_changed", { count: report.paths_changed_count })}</summary>
              <ul className="mt-1 grid gap-0.5 ps-5 font-mono text-xs text-muted">
                {(report.paths_changed ?? []).map((p) => (
                  <li key={p} className="truncate">
                    /home/{p}
                  </li>
                ))}
              </ul>
            </details>
          )}
          {!!report.new_mail_password?.length && (
            <p className="rounded-lg bg-warn-soft px-4 py-3 text-sm text-warn">{t("imports.new_mail_passwords", { names: report.new_mail_password.join(", ") })}</p>
          )}
          {!!report.new_password?.length && (
            <p className="rounded-lg bg-warn-soft px-4 py-3 text-sm text-warn">{t("imports.new_passwords", { names: report.new_password.join(", ") })}</p>
          )}
          {!!report.not_imported?.length && (
            <div className="grid gap-1 text-sm">
              <p className="font-medium">{t("imports.not_imported")}</p>
              <ul className="list-disc ps-5 text-muted">
                {report.not_imported.map((n, i) => (
                  <li key={i} className="break-words">
                    {t(`imports.note.${n.code}`, { ...n, defaultValue: [n.code, n.name, n.detail].filter(Boolean).join(": ") })}
                  </li>
                ))}
              </ul>
            </div>
          )}
          <p className="text-sm text-muted">{t("imports.after")}</p>
        </div>
      )}
      {(done || failed) && (
        <div className="flex flex-wrap justify-end gap-2">
          <Link to="/sites" className="inline-flex h-10 items-center rounded-lg px-4 font-medium text-accent hover:bg-sunk">
            {t("imports.open_sites")}
          </Link>
          <Button onClick={onAgain}>{t("imports.another")}</Button>
        </div>
      )}
    </Card>
  );
}

/** Website archive → choices: the domain, PHP, and the database with its dump. */
function ArchiveChoices({ plan, server, archive, dumps, onStarted }: {
  plan: SiteInspection;
  server: string;
  archive: string;
  dumps: ImportFile[];
  onStarted: (taskId: string, dbPassword?: string) => void;
}) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const lng = i18n.language;
  const inArchive = plan.dumps ?? [];
  const wp = plan.kind === "wordpress" ? plan.wordpress : undefined;
  const [domain, setDomain] = useState(plan.domain ?? "");
  const [kind, setKind] = useState<"php" | "static">(plan.kind === "static" ? "static" : "php");
  const [php, setPHP] = useState(plan.php_versions.includes("8.4") ? "8.4" : plan.php_versions[0]);
  // The dump: "a:<name>" inside the archive, "f:<name>" next to it, "" none.
  const [dump, setDump] = useState(inArchive.length ? "a:" + inArchive[0].name : dumps.length ? "f:" + dumps[0].name : "");
  const [withDB, setWithDB] = useState(!!wp || inArchive.length + dumps.length > 0);
  const [dbName, setDBName] = useState(wp?.db_name ?? "");
  const [dbUser, setDBUser] = useState(wp?.db_user ?? "");
  const [owner, setOwner] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const clean = (v: string) => v.toLowerCase().replace(/[^a-z0-9_]/g, "_").slice(0, 32);

  const start = async () => {
    setBusy(true);
    setError("");
    try {
      const res = await api.post<{ task: Task; database_password?: string }>("/imports/site", {
        server_id: server,
        archive,
        domain,
        kind,
        php_version: kind === "php" ? php : undefined,
        owner_id: owner,
        dump: withDB && dump.startsWith("f:") ? dump.slice(2) : "",
        dump_in_archive: withDB && dump.startsWith("a:") ? dump.slice(2) : "",
        database: withDB ? { name: dbName, user: dbUser } : undefined,
      });
      qc.invalidateQueries({ queryKey: ["sites"] });
      qc.invalidateQueries({ queryKey: ["databases"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
      onStarted(res.task.id, res.database_password);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="grid gap-6 p-5">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="flex items-center gap-2 text-lg font-semibold">
          {t("imports.archive_choose")} <Badge tone="accent">{t(`imports.site_kind.${plan.kind}`)}</Badge>
        </h2>
        <p className="text-sm text-muted">{t("imports.files_value", { count: plan.files, size: formatBytes(plan.bytes, lng) })}</p>
      </div>
      <p className="text-sm text-muted">
        {t("imports.archive_root")} <span className="font-mono">{plan.root ? plan.root + "/" : t("imports.archive_top")}</span>
      </p>

      <div className="grid gap-4 sm:grid-cols-2">
        <Field
          label={t("imports.domain")}
          value={domain}
          onChange={(e) => setDomain(e.target.value.trim().toLowerCase())}
          placeholder="example.com"
          error={plan.domain_taken && domain === plan.domain ? t("imports.domain_taken") : undefined}
          className="[&_input]:font-mono"
          autoComplete="off"
          spellCheck={false}
          required
        />
        <div className="grid gap-1.5">
          <span className="text-sm font-medium">{t("imports.site_type")}</span>
          <div className="flex flex-wrap items-center gap-3">
            {(["php", "static"] as const).map((k) => (
              <label key={k} className="flex items-center gap-2 text-sm">
                <input type="radio" name="site-kind" checked={kind === k} onChange={() => setKind(k)} className="accent-[var(--accent)]" />
                {t(`imports.site_kind.${k}`)}
              </label>
            ))}
            {kind === "php" && (
              <select value={php} onChange={(e) => setPHP(e.target.value)} className={select} aria-label="PHP">
                {plan.php_versions.map((v) => (
                  <option key={v} value={v}>
                    PHP {v}
                  </option>
                ))}
              </select>
            )}
          </div>
        </div>
      </div>

      <section className="grid gap-3">
        <label className="flex cursor-pointer items-start gap-3">
          <input type="checkbox" checked={withDB} onChange={(e) => setWithDB(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
          <span className="grid gap-0.5">
            <span className="font-semibold">{t("imports.with_database")}</span>
            <span className="text-sm text-muted">{wp ? (plan.keeps_password ? t("imports.wp_keeps") : t("imports.wp_new_password")) : t("imports.db_hint")}</span>
          </span>
        </label>
        {withDB && (
          <div className="grid gap-4 ps-7 sm:grid-cols-3">
            <label className="grid min-w-0 gap-1.5 sm:col-span-3">
              <span className="text-sm font-medium">{t("imports.dump")}</span>
              <select value={dump} onChange={(e) => setDump(e.target.value)} className="h-10 rounded-lg border border-line bg-surface px-3 text-ink">
                <option value="">{t("imports.dump_none")}</option>
                {inArchive.map((d) => (
                  <option key={d.name} value={"a:" + d.name}>
                    {t("imports.dump_inside", { name: d.name, size: formatBytes(d.bytes, lng) })}
                  </option>
                ))}
                {dumps.map((d) => (
                  <option key={d.name} value={"f:" + d.name}>
                    {t("imports.dump_beside", { name: d.name, size: formatBytes(d.bytes, lng) })}
                  </option>
                ))}
              </select>
            </label>
            <Field label={t("databases.name")} value={dbName} onChange={(e) => setDBName(clean(e.target.value))} placeholder={t("imports.automatic")} className="[&_input]:font-mono" autoComplete="off" />
            <Field label={t("databases.username")} value={dbUser} onChange={(e) => setDBUser(clean(e.target.value))} placeholder={t("imports.automatic")} className="[&_input]:font-mono" autoComplete="off" />
            {plan.database_taken && wp && dbName === wp.db_name && <p className="text-sm text-warn sm:col-span-3">{t("imports.db_taken_rename")}</p>}
          </div>
        )}
      </section>

      <OwnerField value={owner} onChange={setOwner} />
      {wp && <p className="text-sm text-muted">{t("imports.wp_address")}</p>}
      <ErrorAlert>{error}</ErrorAlert>
      <div className="flex justify-end">
        <Button variant="primary" loading={busy} disabled={!domain} onClick={start}>
          {t("imports.start")}
        </Button>
      </div>
    </Card>
  );
}

import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, CircleAlert, Loader2, ShieldBan, ShieldCheck } from "lucide-react";
import { api, ApiError, errorMessage, type Server } from "../lib/api";
import { formatDateTime, formatRelative } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, ErrorAlert, Field, Spinner } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { MalwareCard } from "../components/ServerMalware";
import { CountriesCard } from "../components/CountryBlocking";

interface CrowdSecStatus {
  installed: boolean;
  active: boolean;
  bouncer_active: boolean;
  version?: string;
  bans: number;
}

export interface MalwareStatus {
  installed: boolean;
  updating: boolean;
  version?: string;
  ready: boolean;
  signatures?: string;
  signatures_date?: string;
  low_memory?: boolean;
  error?: string;
}

export interface SecurityView {
  crowdsec: boolean;
  allowlist: string[];
  always: string[];
  countries: { mode: "off" | "block" | "allow"; countries: string[] };
  country_codes: string[];
  malware_scan: "off" | "manual" | "daily" | "weekly";
  malware_quarantine: boolean;
  malware_scanned_at: string | null;
  malware_last: { at: string; files: number; found: number; new: number; seconds: number; truncated?: boolean; error?: string } | null;
  status: {
    crowdsec: CrowdSecStatus;
    error?: string;
    countries?: { active: boolean; ranges?: number; list?: string; error?: string };
    malware?: MalwareStatus;
  } | null;
  checked_at: string | null;
}

interface Overview {
  server: {
    ssh: { ports: number[]; root_login: string; password_auth: boolean };
    updates: { pending: number; security: number; auto_updates: boolean; reboot_required: boolean; lists_age: number };
    firewall: boolean;
    crowdsec: CrowdSecStatus;
  };
  admins_without_2fa: string[];
}

interface Ban {
  id: number;
  ip: string;
  reason: string;
  origin: string;
  country?: string;
  until: string;
  manually: boolean;
}

interface Alert {
  id: number;
  scenario: string;
  ip: string;
  country?: string;
  as?: string;
  events: number;
  at: string;
  banned: boolean;
}

const durations = ["1h", "4h", "24h", "168h", "720h", "8760h"];
const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

/** "crowdsecurity/http-bf-wordpress_bf" -> "http-bf-wordpress_bf", looked up in the texts. */
function scenarioName(t: (k: string, o?: Record<string, unknown>) => string, s: string) {
  const short = s.replace(/^crowdsecurity\//, "");
  return t(`serversec.scenarios.${short.replace(/[^a-z0-9_]/g, "_")}`, { defaultValue: short });
}

export function ServerSecurityPage() {
  const { t } = useTranslation();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers") });
  const enrolled = (servers.data ?? []).filter((s) => s.enrolled_at);
  const [id, setId] = useState("");
  useEffect(() => {
    if (!id && enrolled.length) setId((enrolled.find((s) => s.online) ?? enrolled[0]).id);
  }, [id, enrolled]);
  const server = enrolled.find((s) => s.id === id);
  return (
    <>
      <PageHeader
        title={t("nav.security_page")}
        description={t("serversec.intro")}
        actions={
          enrolled.length > 1 ? (
            <select value={id} onChange={(e) => setId(e.target.value)} className={select} aria-label={t("sites.server")}>
              {enrolled.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name}
                </option>
              ))}
            </select>
          ) : undefined
        }
      />
      {servers.isPending && <Spinner />}
      {servers.data && enrolled.length === 0 && <EmptyState icon={<ShieldCheck />} title={t("serversec.no_server")} body={t("sites.need_server")} />}
      {server && <ServerSecurity key={server.id} server={server} />}
    </>
  );
}

function ServerSecurity({ server }: { server: Server }) {
  const { t } = useTranslation();
  const sec = useQuery({
    queryKey: ["security", server.id],
    queryFn: () => api.get<SecurityView>(`/servers/${server.id}/security`),
    refetchInterval: (q) => {
      const d = q.state.data;
      const settling =
        d &&
        (d.crowdsec !== !!d.status?.crowdsec.active ||
          (d.malware_scan !== "off" && !d.status?.malware?.ready && !d.status?.malware?.error) ||
          (d.countries.mode !== "off") !== !!d.status?.countries?.active);
      return settling ? 3000 : 30000;
    },
  });
  if (sec.isPending) return <Spinner />;
  if (sec.error || !sec.data) return <ErrorAlert>{errorMessage(t, sec.error)}</ErrorAlert>;
  const running = !!sec.data.status?.crowdsec.active;
  return (
    <div className="grid gap-6 xl:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
      <div className="grid content-start gap-6">
        <OverviewCard server={server} />
        <MalwareCard server={server} sec={sec.data} />
        {running && <BansCard server={server} />}
        {running && <AlertsCard server={server} />}
      </div>
      <div className="grid content-start gap-6">
        <CrowdSecCard server={server} sec={sec.data} />
        <CountriesCard server={server} sec={sec.data} />
        <AllowlistCard server={server} sec={sec.data} />
      </div>
    </div>
  );
}

function Check({ ok, warn, children }: { ok: boolean; warn?: boolean; children: ReactNode }) {
  return (
    <li className="flex items-start gap-2">
      {ok ? (
        <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-ok" aria-hidden />
      ) : (
        <CircleAlert className={warn ? "mt-0.5 size-4 shrink-0 text-warn" : "mt-0.5 size-4 shrink-0 text-crit"} aria-hidden />
      )}
      <span className="grid min-w-0 gap-1">{children}</span>
    </li>
  );
}

function OverviewCard({ server }: { server: Server }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["security-overview", server.id], queryFn: () => api.get<Overview>(`/servers/${server.id}/security/overview`), retry: false });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const autoUpdates = async () => {
    setBusy(true);
    setError("");
    try {
      qc.setQueryData(["security-overview", server.id], await api.post<Overview>(`/servers/${server.id}/security/auto-updates`, { on: true }));
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const o = q.data?.server;
  const rootOK = o && ["no", "prohibit-password", "without-password"].includes(o.ssh.root_login);
  return (
    <Card className="grid gap-4 p-6">
      <h2 className="text-lg font-semibold">{t("serversec.overview.title")}</h2>
      {q.isPending && <Spinner />}
      {q.error && <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>}
      <ErrorAlert>{error}</ErrorAlert>
      {o && q.data && (
        <ul className="grid gap-3 text-sm">
          <Check ok={o.firewall}>
            <span>{o.firewall ? t("serversec.overview.firewall_on") : t("serversec.overview.firewall_off")}</span>
            {!o.firewall && (
              <Link to={`/servers/${server.id}`} className="font-medium text-accent hover:underline">
                {t("serversec.overview.open_firewall")}
              </Link>
            )}
          </Check>
          <Check ok={o.crowdsec.active} warn>
            {o.crowdsec.active ? t("serversec.overview.crowdsec_on", { count: o.crowdsec.bans }) : t("serversec.overview.crowdsec_off")}
          </Check>
          <Check ok={!!rootOK} warn>
            {rootOK ? t("serversec.overview.root_ok") : t("serversec.overview.root_password", { value: o.ssh.root_login })}
          </Check>
          <Check ok={!o.ssh.password_auth} warn>
            {o.ssh.password_auth ? t("serversec.overview.password_on", { ports: o.ssh.ports.join(", ") }) : t("serversec.overview.password_off")}
          </Check>
          <Check ok={o.updates.security === 0} warn={o.updates.security === 0}>
            {o.updates.pending === 0
              ? t("serversec.overview.updates_none")
              : t("serversec.overview.updates", { count: o.updates.pending, security: o.updates.security })}
            {o.updates.lists_age > 48 && <span className="text-muted">{t("serversec.overview.lists_old", { hours: o.updates.lists_age })}</span>}
          </Check>
          <Check ok={o.updates.auto_updates} warn>
            <span>{o.updates.auto_updates ? t("serversec.overview.auto_on") : t("serversec.overview.auto_off")}</span>
            {!o.updates.auto_updates && (
              <span>
                <Button size="sm" onClick={autoUpdates} loading={busy}>
                  {t("serversec.overview.auto_enable")}
                </Button>
              </span>
            )}
          </Check>
          {o.updates.reboot_required && (
            <Check ok={false} warn>
              {t("serversec.overview.reboot")}
            </Check>
          )}
          <Check ok={q.data.admins_without_2fa.length === 0}>
            {q.data.admins_without_2fa.length === 0 ? (
              t("serversec.overview.twofa_ok")
            ) : (
              <>
                <span>{t("serversec.overview.twofa_missing", { count: q.data.admins_without_2fa.length })}</span>
                <span className="text-muted" dir="ltr">
                  {q.data.admins_without_2fa.join(", ")}
                </span>
                <Link to="/settings/security" className="font-medium text-accent hover:underline">
                  {t("serversec.overview.twofa_setup")}
                </Link>
              </>
            )}
          </Check>
        </ul>
      )}
    </Card>
  );
}

function CrowdSecCard({ server, sec }: { server: Server; sec: SecurityView }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const st = sec.status?.crowdsec;
  const set = async (on: boolean) => {
    setBusy(true);
    setError("");
    try {
      qc.setQueryData(["security", server.id], await api.put<SecurityView>(`/servers/${server.id}/security`, { crowdsec: on, allowlist: sec.allowlist }));
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const applying = sec.crowdsec !== !!st?.active && !sec.status?.error;
  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <h2 className="text-lg font-semibold">CrowdSec</h2>
        {st?.active ? (
          <Badge tone="ok">{t("serversec.crowdsec.running", { version: st.version })}</Badge>
        ) : applying ? (
          <Badge tone="warn">
            <Loader2 className="size-3 animate-spin" aria-hidden />
            {sec.crowdsec ? t("serversec.crowdsec.starting") : t("serversec.crowdsec.stopping")}
          </Badge>
        ) : (
          <Badge>{t("serversec.crowdsec.off")}</Badge>
        )}
      </div>
      <p className="text-sm text-muted">{t("serversec.crowdsec.intro")}</p>
      <ErrorAlert>{error || sec.status?.error}</ErrorAlert>
      {st?.active && !st.bouncer_active && <ErrorAlert>{t("serversec.crowdsec.bouncer_down")}</ErrorAlert>}
      <div className="flex flex-wrap items-center gap-3">
        <Button variant={sec.crowdsec ? "secondary" : "primary"} onClick={() => set(!sec.crowdsec)} loading={busy}>
          {sec.crowdsec ? t("serversec.crowdsec.disable") : t("serversec.crowdsec.enable")}
        </Button>
        {sec.checked_at && (
          <span className="text-sm text-muted">{t("serversec.crowdsec.checked", { when: formatRelative(sec.checked_at, i18n.language) })}</span>
        )}
      </div>
      <p className="text-sm text-muted">{t("serversec.crowdsec.cloudflare_note")}</p>
    </Card>
  );
}

function BansCard({ server }: { server: Server }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["bans", server.id], queryFn: () => api.get<{ bans: Ban[] }>(`/servers/${server.id}/security/bans`), refetchInterval: 30000 });
  const [ip, setIP] = useState("");
  const [duration, setDuration] = useState("24h");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const run = async (key: string, fn: () => Promise<{ bans: Ban[] }>) => {
    setBusy(key);
    setError("");
    try {
      qc.setQueryData(["bans", server.id], await fn());
      qc.invalidateQueries({ queryKey: ["security", server.id] });
      return true;
    } catch (err) {
      setError(err instanceof ApiError && err.code === "agent_allowlisted" ? t("serversec.bans.allowlisted", { ip }) : errorMessage(t, err));
      return false;
    } finally {
      setBusy("");
    }
  };
  const ban = async (e: FormEvent) => {
    e.preventDefault();
    if (
      await run("ban", () => api.post(`/servers/${server.id}/security/bans`, { ip: ip.trim(), duration, reason: reason.trim() || t("serversec.bans.by_hand") }))
    ) {
      setIP("");
      setReason("");
    }
  };
  const bans = q.data?.bans ?? [];
  return (
    <Card className="grid gap-4 p-6">
      <div>
        <h2 className="text-lg font-semibold">{t("serversec.bans.title")}</h2>
        <p className="text-sm text-muted">{t("serversec.bans.intro")}</p>
      </div>
      <form onSubmit={ban} className="grid items-end gap-3 sm:grid-cols-[minmax(0,1.2fr)_auto_minmax(0,1.5fr)_auto]">
        <Field
          label={t("serversec.bans.ip")}
          value={ip}
          onChange={(e) => setIP(e.target.value)}
          placeholder="203.0.113.7"
          dir="ltr"
          className="[&_input]:font-mono"
          required
        />
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("serversec.bans.duration")}</span>
          <select value={duration} onChange={(e) => setDuration(e.target.value)} className={select}>
            {durations.map((d) => (
              <option key={d} value={d}>
                {t(`serversec.bans.d_${d}`)}
              </option>
            ))}
          </select>
        </label>
        <Field label={t("serversec.bans.reason")} value={reason} onChange={(e) => setReason(e.target.value)} maxLength={100} />
        <Button type="submit" variant="danger" loading={busy === "ban"}>
          <ShieldBan className="size-4" aria-hidden />
          {t("serversec.bans.ban")}
        </Button>
      </form>
      <ErrorAlert>{error}</ErrorAlert>
      {q.isPending && <Spinner />}
      {q.error && <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>}
      {q.data && bans.length === 0 && <p className="text-sm text-muted">{t("serversec.bans.none")}</p>}
      {bans.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[560px] text-sm">
            <thead>
              <tr className="border-b border-line text-xs tracking-wider text-muted uppercase">
                <th className="py-2 pe-3 text-start font-semibold">{t("serversec.bans.ip")}</th>
                <th className="py-2 pe-3 text-start font-semibold">{t("serversec.bans.reason")}</th>
                <th className="py-2 pe-3 text-start font-semibold">{t("serversec.bans.until")}</th>
                <th className="py-2" />
              </tr>
            </thead>
            <tbody>
              {bans.map((b) => (
                <tr key={b.id} className="border-b border-line last:border-0">
                  <td className="py-2 pe-3 font-mono" dir="ltr">
                    {b.ip}
                    {b.country && <span className="ms-2 text-xs text-muted">{b.country}</span>}
                  </td>
                  <td className="py-2 pe-3">
                    {b.manually ? b.reason : scenarioName(t, b.reason)}
                    <div className="text-xs text-muted">{b.manually ? t("serversec.bans.by_hand") : t("serversec.bans.detected")}</div>
                  </td>
                  <td className="py-2 pe-3 text-muted">{formatDateTime(b.until, i18n.language)}</td>
                  <td className="py-2 text-end">
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => run("unban" + b.ip, () => api.del(`/servers/${server.id}/security/bans/${encodeURIComponent(b.ip)}`))}
                      loading={busy === "unban" + b.ip}
                    >
                      {t("serversec.bans.unban")}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function AlertsCard({ server }: { server: Server }) {
  const { t, i18n } = useTranslation();
  const q = useQuery({
    queryKey: ["security-alerts", server.id],
    queryFn: () => api.get<{ alerts: Alert[] }>(`/servers/${server.id}/security/alerts`),
    refetchInterval: 30000,
  });
  const alerts = q.data?.alerts ?? [];
  return (
    <Card className="grid gap-4 p-6">
      <div>
        <h2 className="text-lg font-semibold">{t("serversec.alerts.title")}</h2>
        <p className="text-sm text-muted">{t("serversec.alerts.intro")}</p>
      </div>
      {q.isPending && <Spinner />}
      {q.error && <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>}
      {q.data && alerts.length === 0 && <p className="text-sm text-muted">{t("serversec.alerts.none")}</p>}
      {alerts.length > 0 && (
        <ul className="divide-y divide-line text-sm">
          {alerts.slice(0, 30).map((a) => (
            <li key={a.id} className="flex flex-wrap items-center justify-between gap-2 py-2">
              <div className="grid min-w-0">
                <span className="font-medium">{scenarioName(t, a.scenario)}</span>
                <span className="text-xs text-muted" dir="ltr">
                  {a.ip}
                  {a.country && ` · ${a.country}`}
                  {a.as && ` · ${a.as}`}
                </span>
              </div>
              <div className="flex items-center gap-2 text-muted">
                {a.banned && <Badge tone="crit">{t("serversec.alerts.banned")}</Badge>}
                <span>{formatRelative(a.at, i18n.language)}</span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

function AllowlistCard({ server, sec }: { server: Server; sec: SecurityView }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [text, setText] = useState(sec.allowlist.join("\n"));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const save = async () => {
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      const list = text
        .split(/[\n,]/)
        .map((x) => x.trim())
        .filter(Boolean);
      const v = await api.put<SecurityView>(`/servers/${server.id}/security`, { crowdsec: sec.crowdsec, allowlist: list });
      qc.setQueryData(["security", server.id], v);
      setText(v.allowlist.join("\n"));
      setSaved(true);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card className="grid gap-3 p-6">
      <h2 className="text-lg font-semibold">{t("serversec.allowlist.title")}</h2>
      <p className="text-sm text-muted">{t("serversec.allowlist.intro")}</p>
      <ErrorAlert>{error}</ErrorAlert>
      <textarea
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          setSaved(false);
        }}
        rows={5}
        className="rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm"
        dir="ltr"
        spellCheck={false}
        placeholder={"203.0.113.10\n198.51.100.0/24"}
        aria-label={t("serversec.allowlist.title")}
      />
      <div className="flex items-center gap-3">
        <Button onClick={save} loading={busy}>
          {t("common.save")}
        </Button>
        {saved && <span className="text-sm text-ok">{t("serversec.allowlist.saved")}</span>}
      </div>
      <details className="text-sm text-muted">
        <summary className="cursor-pointer">{t("serversec.allowlist.always", { count: sec.always.length })}</summary>
        <p className="mt-2 font-mono text-xs break-all" dir="ltr">
          {sec.always.join(", ")}
        </p>
      </details>
    </Card>
  );
}

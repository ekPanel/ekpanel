import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  CheckCircle2,
  CircleAlert,
  CloudCog,
  ExternalLink,
  Forward,
  KeyRound,
  Loader2,
  Mail,
  MailPlus,
  Pencil,
  Plus,
  Reply,
  ShieldAlert,
  Stethoscope,
  Trash2,
} from "lucide-react";
import {
  api,
  ApiError,
  errorMessage,
  type MailCheck,
  type MailDNSRecord,
  type MailDomain,
  type MailDomainDetail,
  type MailForwarder,
  type Mailbox,
  type MailboxWithPassword,
  type MailServer,
  type Server,
} from "../lib/api";
import { formatBytes, formatNumber } from "../lib/format";
import { useMe } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, Dialog, ErrorAlert, Field, Meter, Spinner, cx, type Tone } from "../components/ui";
import { OwnerField } from "./CustomersPage";
import { EmptyState } from "./ServerParts";

const stateTone: Record<MailDomain["state"], Tone> = {
  pending: "warn",
  active: "ok",
  error: "crit",
  deleting: "neutral",
};
const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

export function useMailServers() {
  return useQuery({
    queryKey: ["mail-servers"],
    queryFn: () => api.get<MailServer[]>("/mail/servers"),
    refetchInterval: (q) => (q.state.data?.some((m) => m.state === "pending") ? 3000 : 30000),
  });
}

function useMailDomains() {
  return useQuery({
    queryKey: ["mail-domains"],
    queryFn: () => api.get<MailDomain[]>("/mail/domains"),
    refetchInterval: (q) => (q.state.data?.some((d) => d.state === "pending" || d.state === "deleting") ? 2000 : 30000),
  });
}

function StateBadge({ state }: { state: MailDomain["state"] }) {
  const { t } = useTranslation();
  return (
    <Badge tone={stateTone[state]}>
      {(state === "pending" || state === "deleting") && <Loader2 className="size-3 animate-spin" aria-hidden />}
      {t(`mail.state.${state}`)}
    </Badge>
  );
}

// ---- Mail servers (administrators) ----

function MailServerDialog({ open, onClose, server, current }: { open: boolean; onClose: () => void; server: Server; current?: MailServer }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [hostname, setHostname] = useState("");
  const [tls, setTLS] = useState<MailServer["tls"]>("auto");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!open) return;
    setHostname(current?.hostname ?? "");
    setTLS(current?.tls ?? "auto");
    setError("");
  }, [open, current]);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.put(`/servers/${server.id}/mail`, { hostname, tls });
      qc.invalidateQueries({ queryKey: ["mail-servers"] });
      qc.invalidateQueries({ queryKey: ["tasks"] });
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open={open} onClose={onClose} title={current ? t("mail.server.edit", { name: server.name }) : t("mail.server.enable_title", { name: server.name })}>
      <form onSubmit={submit} className="grid gap-5">
        <ErrorAlert>{error}</ErrorAlert>
        {!current && <p className="text-sm text-muted">{t("mail.server.enable_body")}</p>}
        <Field
          label={t("mail.server.hostname")}
          hint={t("mail.server.hostname_hint")}
          value={hostname}
          onChange={(e) => setHostname(e.target.value.trim().toLowerCase())}
          placeholder="mail.example.com"
          dir="ltr"
          className="[&_input]:font-mono"
          required
          autoComplete="off"
          spellCheck={false}
        />
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("mail.server.tls")}</span>
          <select value={tls} onChange={(e) => setTLS(e.target.value as MailServer["tls"])} className={select}>
            <option value="auto">{t("mail.server.tls_auto")}</option>
            <option value="internal">{t("mail.server.tls_internal")}</option>
          </select>
        </label>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!hostname}>
            {current ? t("common.save") : t("mail.server.enable")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function CheckLine({ ok, warn, children }: { ok: boolean; warn?: boolean; children: ReactNode }) {
  return (
    <li className="flex items-start gap-2">
      {ok ? (
        <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-ok" aria-hidden />
      ) : (
        <CircleAlert className={cx("mt-0.5 size-4 shrink-0", warn ? "text-warn" : "text-crit")} aria-hidden />
      )}
      <span className="min-w-0 break-words">{children}</span>
    </li>
  );
}

/** What decides whether other mail servers accept this server's mail. */
function DeliverabilityDialog({ open, onClose, mail }: { open: boolean; onClose: () => void; mail: MailServer }) {
  const { t } = useTranslation();
  const q = useQuery({
    queryKey: ["mail-check", mail.server_id],
    queryFn: () => api.post<MailCheck>(`/servers/${mail.server_id}/mail/check`),
    enabled: open,
    staleTime: 0,
    gcTime: 0,
  });
  const c = q.data?.check;
  const pointsRight = c && q.data && q.data.public_addresses.length > 0 && q.data.public_addresses.every((a) => c.hostname_addresses.includes(a));
  return (
    <Dialog open={open} onClose={onClose} title={t("mail.check.title")} wide>
      <div className="grid gap-4">
        {q.isFetching && <Spinner label={t("mail.check.running")} />}
        {q.error && <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>}
        {c && !q.isFetching && (
          <ul className="grid gap-3 text-sm">
            <CheckLine ok={!!pointsRight}>
              {pointsRight
                ? t("mail.check.host_ok", { host: c.hostname })
                : t("mail.check.host_bad", {
                    host: c.hostname,
                    have: c.hostname_addresses.join(", ") || t("mail.check.nothing"),
                    want: q.data?.public_addresses.join(", ") || t("mail.check.no_public"),
                  })}
            </CheckLine>
            <CheckLine ok={c.port25 === "open"}>{t(c.port25 === "open" ? "mail.check.port25_open" : "mail.check.port25_blocked")}</CheckLine>
            {c.addresses.length === 0 && <CheckLine ok={false}>{t("mail.check.no_public")}</CheckLine>}
            {c.addresses.map((a) => (
              <li key={a.address} className="grid gap-2 rounded-lg border border-line p-3">
                <span className="font-mono font-medium" dir="ltr">
                  {a.address}
                </span>
                <ul className="grid gap-2">
                  <CheckLine ok={a.ptr_ok} warn>
                    {a.ptr_ok
                      ? t("mail.check.ptr_ok", { host: c.hostname })
                      : t("mail.check.ptr_bad", {
                          have: a.ptr.join(", ") || t("mail.check.nothing"),
                          host: c.hostname,
                        })}
                  </CheckLine>
                  {a.checked.length > 0 && (
                    <CheckLine ok={a.listed.length === 0} warn={a.listed.length === 0}>
                      {a.listed.length > 0
                        ? t("mail.check.listed", { lists: a.listed.join(", ") })
                        : a.unknown.length > 0
                          ? t("mail.check.unknown", {
                              lists: a.unknown.join(", "),
                            })
                          : t("mail.check.clean", { count: a.checked.length })}
                    </CheckLine>
                  )}
                </ul>
              </li>
            ))}
          </ul>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => q.refetch()} disabled={q.isFetching}>
            {t("mail.check.again")}
          </Button>
          <Button variant="primary" onClick={onClose}>
            {t("common.close")}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

function MailServersCard() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const servers = useQuery({
    queryKey: ["servers"],
    queryFn: () => api.get<Server[]>("/servers"),
  });
  const mail = useMailServers();
  const [editing, setEditing] = useState<Server | null>(null);
  const [checking, setChecking] = useState<MailServer | null>(null);
  const [error, setError] = useState("");
  const enrolled = (servers.data ?? []).filter((s) => s.enrolled_at);
  if (!servers.data || !mail.data) return null;
  const turnOff = async (m: MailServer) => {
    setError("");
    try {
      await api.del(`/servers/${m.server_id}/mail`);
      qc.invalidateQueries({ queryKey: ["mail-servers"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };
  return (
    <Card className="grid gap-4 p-6">
      <div>
        <h2 className="text-lg font-semibold">{t("mail.server.title")}</h2>
        <p className="text-sm text-muted">{t("mail.server.intro")}</p>
      </div>
      <ErrorAlert>{error}</ErrorAlert>
      {enrolled.length === 0 && <p className="text-sm text-muted">{t("sites.need_server")}</p>}
      <ul className="grid gap-2">
        {enrolled.map((s) => {
          const m = mail.data.find((x) => x.server_id === s.id);
          return (
            <li key={s.id} className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-line px-4 py-3">
              <div className="grid min-w-0 gap-0.5">
                <span className="font-medium">{s.name}</span>
                {m ? (
                  <span className="truncate text-sm text-muted" dir="ltr">
                    {m.hostname} ·{" "}
                    {t("mail.server.domains", {
                      count: m.domains,
                      n: formatNumber(m.domains, i18n.language),
                    })}
                  </span>
                ) : (
                  <span className="text-sm text-muted">{t("mail.server.off")}</span>
                )}
                {m?.state === "error" && <span className="text-sm break-words text-crit">{m.message}</span>}
              </div>
              <div className="flex flex-wrap items-center gap-2">
                {m && (
                  <Badge tone={m.state === "active" ? "ok" : m.state === "error" ? "crit" : "warn"}>
                    {m.state === "pending" && <Loader2 className="size-3 animate-spin" aria-hidden />}
                    {t(`mail.server.state.${m.state}`)}
                  </Badge>
                )}
                {m ? (
                  <>
                    <Button size="sm" variant="ghost" onClick={() => setChecking(m)}>
                      <Stethoscope className="size-4" aria-hidden />
                      {t("mail.check.button")}
                    </Button>
                    <Button size="sm" variant="ghost" onClick={() => setEditing(s)} title={t("common.edit")}>
                      <Pencil className="size-4" aria-hidden />
                      <span className="sr-only">{t("common.edit")}</span>
                    </Button>
                    {m.domains === 0 && (
                      <Button size="sm" variant="ghost" onClick={() => turnOff(m)}>
                        {t("mail.server.disable")}
                      </Button>
                    )}
                  </>
                ) : (
                  <Button size="sm" onClick={() => setEditing(s)}>
                    {t("mail.server.enable")}
                  </Button>
                )}
              </div>
            </li>
          );
        })}
      </ul>
      {editing && <MailServerDialog open onClose={() => setEditing(null)} server={editing} current={mail.data.find((m) => m.server_id === editing.id)} />}
      {checking && <DeliverabilityDialog open onClose={() => setChecking(null)} mail={checking} />}
    </Card>
  );
}

// ---- Domains ----

function AddMailDomainDialog({ open, onClose, servers }: { open: boolean; onClose: () => void; servers: MailServer[] }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [server, setServer] = useState("");
  const [domain, setDomain] = useState("");
  const [owner, setOwner] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!open) return;
    setDomain("");
    setError("");
    setServer((s) => s || servers[0]?.server_id || "");
  }, [open, servers]);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const d = await api.post<MailDomain>("/mail/domains", {
        server_id: server,
        domain,
        owner_id: owner,
      });
      qc.invalidateQueries({ queryKey: ["mail-domains"] });
      onClose();
      navigate(`/mail/${d.id}`);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open={open} onClose={onClose} title={t("mail.domain.add")}>
      <form onSubmit={submit} className="grid gap-5">
        <ErrorAlert>{error}</ErrorAlert>
        <Field
          label={t("mail.domain.name")}
          hint={t("mail.domain.name_hint")}
          value={domain}
          onChange={(e) => setDomain(e.target.value.trim().toLowerCase())}
          placeholder="example.com"
          dir="ltr"
          className="[&_input]:font-mono"
          required
          autoComplete="off"
          spellCheck={false}
        />
        {servers.length > 1 && (
          <label className="grid gap-1.5">
            <span className="text-sm font-medium">{t("sites.server")}</span>
            <select value={server} onChange={(e) => setServer(e.target.value)} className={select}>
              {servers.map((s) => (
                <option key={s.server_id} value={s.server_id}>
                  {s.server_name} ({s.hostname})
                </option>
              ))}
            </select>
          </label>
        )}
        <OwnerField value={owner} onChange={setOwner} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!domain || !server}>
            {t("mail.domain.create")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

export function MailPage() {
  const { t, i18n } = useTranslation();
  const me = useMe();
  const isAdmin = me.data?.role === "admin";
  const servers = useMailServers();
  const domains = useMailDomains();
  const [params, setParams] = useSearchParams();
  const adding = params.get("add") === "1";
  const setAdding = (on: boolean) => setParams(on ? { add: "1" } : {}, { replace: true });
  const available = servers.data ?? [];
  const addButton = (
    <Button variant="primary" onClick={() => setAdding(true)} disabled={available.length === 0}>
      <Plus className="size-4" aria-hidden />
      {t("mail.domain.add")}
    </Button>
  );
  const n = (x: number) => formatNumber(x, i18n.language);
  return (
    <>
      <PageHeader title={t("nav.mail")} description={t("mail.intro")} actions={domains.data?.length ? addButton : undefined} />
      <div className="grid gap-6">
        {isAdmin && <MailServersCard />}
        {(domains.isPending || servers.isPending) && <Spinner />}
        {domains.error && <ErrorAlert>{errorMessage(t, domains.error)}</ErrorAlert>}
        {domains.data?.length === 0 && servers.data && (
          <EmptyState
            icon={<Mail />}
            title={t("mail.empty.title")}
            body={available.length > 0 ? t("mail.empty.body") : isAdmin ? t("mail.empty.no_server_admin") : t("mail.empty.no_server")}
            action={available.length > 0 ? addButton : undefined}
          />
        )}
        {!!domains.data?.length && (
          <Card className="overflow-x-auto">
            <table className="w-full min-w-[640px] text-sm">
              <thead>
                <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                  <th className="px-4 py-3 text-start font-semibold">{t("mail.domain.name")}</th>
                  <th className="px-4 py-3 text-end font-semibold">{t("mail.mailboxes.title")}</th>
                  <th className="px-4 py-3 text-end font-semibold">{t("mail.forwarders.title")}</th>
                  <th className="px-4 py-3 text-start font-semibold">{t("sites.col.server")}</th>
                  <th className="px-4 py-3 text-start font-semibold">{t("sites.col.status")}</th>
                </tr>
              </thead>
              <tbody>
                {domains.data.map((d) => (
                  <tr key={d.id} className="border-b border-line last:border-0 hover:bg-sunk/60">
                    <td className="px-4 py-3">
                      <Link to={`/mail/${d.id}`} className="font-mono font-semibold hover:text-accent" dir="ltr">
                        {d.domain}
                      </Link>
                    </td>
                    <td className="px-4 py-3 text-end tabular-nums">{n(d.mailboxes)}</td>
                    <td className="px-4 py-3 text-end tabular-nums">{n(d.forwarders)}</td>
                    <td className="px-4 py-3 text-muted">
                      {d.server_name}
                      {d.owner_name && <div className="text-xs">{d.owner_name}</div>}
                    </td>
                    <td className="px-4 py-3">
                      <StateBadge state={d.state} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Card>
        )}
      </div>
      <AddMailDomainDialog open={adding} onClose={() => setAdding(false)} servers={available} />
    </>
  );
}

// ---- Domain detail ----

function Row({ label, value, copy = true, mono = true }: { label: string; value: string; copy?: boolean; mono?: boolean }) {
  return (
    <div className="contents">
      <dt className="text-muted">{label}</dt>
      <dd className="flex min-w-0 items-center gap-1">
        <span className={cx("truncate", mono && "font-mono")} dir="ltr">
          {value}
        </span>
        {copy && <CopyButton text={value} className="-my-1 shrink-0" />}
      </dd>
    </div>
  );
}

/** Settings for mail apps (Thunderbird, Outlook, phones). */
function ClientSettings({ d }: { d: MailDomain }) {
  const { t } = useTranslation();
  const webmail = `https://${d.hostname}/`;
  return (
    <Card className="grid content-start gap-4 p-6">
      <h2 className="text-lg font-semibold">{t("mail.apps.title")}</h2>
      <p className="text-sm text-muted">{t("mail.apps.intro", { domain: d.domain })}</p>
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
        <Row label={t("mail.apps.server")} value={d.hostname} />
        <Row label={t("mail.apps.imap")} value={t("mail.apps.imap_value")} copy={false} />
        <Row label={t("mail.apps.pop3")} value={t("mail.apps.pop3_value")} copy={false} />
        <Row label={t("mail.apps.smtp")} value={t("mail.apps.smtp_value")} copy={false} />
        <Row label={t("mail.apps.username")} value={t("mail.apps.username_value", { domain: d.domain })} copy={false} mono={false} />
      </dl>
      <div>
        <a
          href={webmail}
          target="_blank"
          rel="noreferrer"
          className="inline-flex h-10 items-center gap-2 rounded-lg border border-line px-4 font-medium hover:bg-sunk"
        >
          <Mail className="size-4" aria-hidden />
          {t("mail.apps.webmail")}
          <ExternalLink className="size-3.5 text-muted" aria-hidden />
        </a>
      </div>
    </Card>
  );
}

function recordValue(r: MailDNSRecord) {
  return r.type === "MX" ? `${r.priority ?? 10} ${r.content}` : r.content;
}

function DNSCard({ detail }: { detail: MailDomainDetail }) {
  const { t } = useTranslation();
  const d = detail.domain;
  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="text-lg font-semibold">{t("mail.dns.title")}</h2>
          <p className="text-sm text-muted">{detail.zone ? t("mail.dns.published", { zone: detail.zone.name }) : t("mail.dns.manual")}</p>
        </div>
        {detail.zone ? (
          <Link to={`/dns/${detail.zone.id}`} className="inline-flex items-center gap-1.5 text-sm font-medium text-accent hover:underline">
            <CloudCog className="size-4" aria-hidden />
            {t("mail.dns.open_zone")}
          </Link>
        ) : (
          <Link to="/dns" className="inline-flex items-center gap-1.5 text-sm font-medium text-accent hover:underline">
            <CloudCog className="size-4" aria-hidden />
            {t("mail.dns.use_cloudflare")}
          </Link>
        )}
      </div>
      {d.dns_records.length === 0 ? (
        <p className="text-sm text-muted">{t("mail.dns.waiting")}</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[560px] text-sm">
            <thead>
              <tr className="border-b border-line text-xs tracking-wider text-muted uppercase">
                <th className="py-2 pe-3 text-start font-semibold">{t("mail.dns.type")}</th>
                <th className="py-2 pe-3 text-start font-semibold">{t("mail.dns.name")}</th>
                <th className="py-2 text-start font-semibold">{t("mail.dns.value")}</th>
              </tr>
            </thead>
            <tbody>
              {d.dns_records.map((r) => (
                <tr key={r.type + r.name + r.content} className="border-b border-line align-top last:border-0">
                  <td className="py-2 pe-3 font-mono">{r.type}</td>
                  <td className="py-2 pe-3 font-mono break-all" dir="ltr">
                    {r.name}
                  </td>
                  <td className="py-2">
                    <div className="flex items-start gap-1">
                      <span className="max-w-[28rem] font-mono break-all text-muted" dir="ltr">
                        {recordValue(r)}
                      </span>
                      {!detail.zone && <CopyButton text={r.content} className="-my-1 shrink-0" />}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="text-sm text-muted">{t("mail.dns.hint")}</p>
    </Card>
  );
}

function PasswordShown({ address, password }: { address: string; password: string }) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-4">
      <div className="flex items-start gap-3 rounded-lg bg-warn-soft px-4 py-3 text-warn">
        <ShieldAlert className="mt-0.5 size-5 shrink-0" aria-hidden />
        <p className="text-sm">{t("mail.mailboxes.password_once")}</p>
      </div>
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
        <Row label={t("mail.mailboxes.address")} value={address} />
        <Row label={t("mail.mailboxes.password")} value={password} />
      </dl>
    </div>
  );
}

function lines(s: string) {
  return s
    .split(/[\n,]/)
    .map((x) => x.trim())
    .filter(Boolean);
}

/** Adding a mailbox, or changing one: password, size, forwarding and auto-reply. */
function MailboxDialog({ open, onClose, detail, mailbox }: { open: boolean; onClose: () => void; detail: MailDomainDetail; mailbox?: Mailbox }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const d = detail.domain;
  const [local, setLocal] = useState("");
  const [password, setPassword] = useState("");
  const [quota, setQuota] = useState("");
  const [forward, setForward] = useState("");
  const [keep, setKeep] = useState(true);
  const [reply, setReply] = useState(false);
  const [subject, setSubject] = useState("");
  const [body, setBody] = useState("");
  const [start, setStart] = useState("");
  const [end, setEnd] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [shown, setShown] = useState<MailboxWithPassword | null>(null);
  useEffect(() => {
    if (!open) return;
    setLocal(mailbox?.local_part ?? "");
    setPassword("");
    setQuota(mailbox ? (mailbox.quota_mb ? String(mailbox.quota_mb) : "") : "");
    setForward((mailbox?.forward_to ?? []).join("\n"));
    setKeep(mailbox?.keep_copy ?? true);
    setReply(!!mailbox?.auto_reply);
    setSubject(mailbox?.auto_reply?.subject ?? "");
    setBody(mailbox?.auto_reply?.body ?? "");
    setStart(mailbox?.auto_reply?.start ?? "");
    setEnd(mailbox?.auto_reply?.end ?? "");
    setError("");
    setShown(null);
  }, [open, mailbox]);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const req: Record<string, unknown> = {
      forward_to: lines(forward),
      keep_copy: keep,
      ...(quota.trim() ? { quota_mb: Number(quota) } : mailbox ? { quota_mb: 0 } : {}),
      ...(password ? { password } : {}),
      ...(reply
        ? {
            auto_reply: {
              subject,
              body,
              ...(start ? { start } : {}),
              ...(end ? { end } : {}),
            },
          }
        : { clear_auto_reply: true }),
    };
    try {
      const r = mailbox
        ? await api.patch<MailboxWithPassword>(`/mail/mailboxes/${mailbox.id}`, req)
        : await api.post<MailboxWithPassword>(`/mail/domains/${d.id}/mailboxes`, { ...req, local_part: local });
      qc.invalidateQueries({ queryKey: ["mail-domain", d.id] });
      qc.invalidateQueries({ queryKey: ["mail-domains"] });
      if (r.password) setShown(r);
      else onClose();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const title = mailbox ? t("mail.mailboxes.edit", { address: `${mailbox.local_part}@${d.domain}` }) : t("mail.mailboxes.add");
  return (
    <Dialog open={open} onClose={onClose} title={title} wide>
      {shown ? (
        <>
          <PasswordShown address={`${shown.mailbox.local_part}@${d.domain}`} password={shown.password ?? ""} />
          <div className="flex justify-end">
            <Button variant="primary" onClick={onClose}>
              {t("databases.saved_it")}
            </Button>
          </div>
        </>
      ) : (
        <form onSubmit={submit} className="grid gap-5">
          <ErrorAlert>{error}</ErrorAlert>
          {!mailbox && (
            <label className="grid gap-1.5">
              <span className="text-sm font-medium">{t("mail.mailboxes.address")}</span>
              <span className="flex items-center gap-2" dir="ltr">
                <input
                  value={local}
                  onChange={(e) => setLocal(e.target.value.trim().toLowerCase())}
                  className="h-10 w-full min-w-0 rounded-lg border border-line bg-surface px-3 font-mono"
                  required
                  autoComplete="off"
                  spellCheck={false}
                  aria-label={t("mail.mailboxes.address")}
                />
                <span className="shrink-0 font-mono text-muted">@{d.domain}</span>
              </span>
            </label>
          )}
          <div className="grid gap-3 sm:grid-cols-2">
            <Field
              label={mailbox ? t("mail.mailboxes.new_password") : t("mail.mailboxes.password")}
              hint={t("mail.mailboxes.password_hint")}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
              minLength={10}
            />
            <Field
              label={t("mail.mailboxes.quota")}
              hint={t("mail.mailboxes.quota_hint")}
              value={quota}
              onChange={(e) => setQuota(e.target.value.replace(/[^0-9]/g, ""))}
              inputMode="numeric"
              dir="ltr"
            />
          </div>
          <fieldset className="grid gap-3 rounded-lg border border-line p-4">
            <legend className="px-1 text-sm font-medium">{t("mail.mailboxes.forwarding")}</legend>
            <label className="grid gap-1.5">
              <span className="text-sm text-muted">{t("mail.mailboxes.forward_hint")}</span>
              <textarea
                value={forward}
                onChange={(e) => setForward(e.target.value)}
                rows={2}
                className="rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm"
                dir="ltr"
                spellCheck={false}
                placeholder="someone@example.org"
              />
            </label>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={keep} onChange={(e) => setKeep(e.target.checked)} className="accent-[var(--accent)]" />
              {t("mail.mailboxes.keep_copy")}
            </label>
          </fieldset>
          <fieldset className="grid gap-3 rounded-lg border border-line p-4">
            <legend className="px-1 text-sm font-medium">{t("mail.mailboxes.auto_reply")}</legend>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={reply} onChange={(e) => setReply(e.target.checked)} className="accent-[var(--accent)]" />
              {t("mail.mailboxes.auto_reply_on")}
            </label>
            {reply && (
              <>
                <Field label={t("mail.mailboxes.subject")} value={subject} onChange={(e) => setSubject(e.target.value)} maxLength={200} required />
                <label className="grid gap-1.5">
                  <span className="text-sm font-medium">{t("mail.mailboxes.message")}</span>
                  <textarea
                    value={body}
                    onChange={(e) => setBody(e.target.value)}
                    rows={4}
                    maxLength={4000}
                    required
                    className="rounded-lg border border-line bg-surface px-3 py-2 text-sm"
                  />
                </label>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label={t("mail.mailboxes.from")} type="date" value={start} onChange={(e) => setStart(e.target.value)} dir="ltr" />
                  <Field label={t("mail.mailboxes.until")} type="date" value={end} onChange={(e) => setEnd(e.target.value)} dir="ltr" />
                </div>
                <p className="text-sm text-muted">{t("mail.mailboxes.auto_reply_hint")}</p>
              </>
            )}
          </fieldset>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!mailbox && !local}>
              {mailbox ? t("common.save") : t("mail.mailboxes.create")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

function ForwarderDialog({ open, onClose, detail, forwarder }: { open: boolean; onClose: () => void; detail: MailDomainDetail; forwarder?: MailForwarder }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const d = detail.domain;
  const [local, setLocal] = useState("");
  const [to, setTo] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!open) return;
    setLocal(forwarder?.local_part ?? "");
    setTo((forwarder?.destinations ?? []).join("\n"));
    setError("");
  }, [open, forwarder]);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (forwarder)
        await api.put(`/mail/forwarders/${forwarder.id}`, {
          destinations: lines(to),
        });
      else
        await api.post(`/mail/domains/${d.id}/forwarders`, {
          local_part: local,
          destinations: lines(to),
        });
      qc.invalidateQueries({ queryKey: ["mail-domain", d.id] });
      qc.invalidateQueries({ queryKey: ["mail-domains"] });
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={
        forwarder
          ? t("mail.forwarders.edit", {
              address: `${forwarder.local_part}@${d.domain}`,
            })
          : t("mail.forwarders.add")
      }
    >
      <form onSubmit={submit} className="grid gap-5">
        <ErrorAlert>{error}</ErrorAlert>
        <p className="text-sm text-muted">{t("mail.forwarders.intro")}</p>
        {!forwarder && (
          <label className="grid gap-1.5">
            <span className="text-sm font-medium">{t("mail.mailboxes.address")}</span>
            <span className="flex items-center gap-2" dir="ltr">
              <input
                value={local}
                onChange={(e) => setLocal(e.target.value.trim().toLowerCase())}
                className="h-10 w-full min-w-0 rounded-lg border border-line bg-surface px-3 font-mono"
                required
                autoComplete="off"
                spellCheck={false}
                aria-label={t("mail.mailboxes.address")}
              />
              <span className="shrink-0 font-mono text-muted">@{d.domain}</span>
            </span>
          </label>
        )}
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("mail.forwarders.to")}</span>
          <textarea
            value={to}
            onChange={(e) => setTo(e.target.value)}
            rows={3}
            required
            className="rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm"
            dir="ltr"
            spellCheck={false}
          />
          <span className="text-sm text-muted">{t("mail.forwarders.to_hint")}</span>
        </label>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!to.trim() || (!forwarder && !local)}>
            {forwarder ? t("common.save") : t("mail.forwarders.create")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function MailboxesCard({ detail }: { detail: MailDomainDetail }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const d = detail.domain;
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Mailbox | null>(null);
  const [shown, setShown] = useState<MailboxWithPassword | null>(null);
  const [deleting, setDeleting] = useState<Mailbox | null>(null);
  const [error, setError] = useState("");
  const busy = d.state === "deleting";
  const reset = async (m: Mailbox) => {
    setError("");
    try {
      setShown(await api.post<MailboxWithPassword>(`/mail/mailboxes/${m.id}/password`));
      qc.invalidateQueries({ queryKey: ["mail-domain", d.id] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };
  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-lg font-semibold">{t("mail.mailboxes.title")}</h2>
        <Button size="sm" onClick={() => setAdding(true)} disabled={busy}>
          <MailPlus className="size-4" aria-hidden />
          {t("mail.mailboxes.add")}
        </Button>
      </div>
      <ErrorAlert>{error}</ErrorAlert>
      {detail.mailboxes.length === 0 ? (
        <p className="text-sm text-muted">{t("mail.mailboxes.empty")}</p>
      ) : (
        <ul className="grid gap-2">
          {detail.mailboxes.map((m) => {
            const used = m.used_bytes ?? 0;
            const limit = m.quota_mb * 1024 * 1024;
            return (
              <li key={m.id} className="grid gap-2 rounded-lg border border-line px-4 py-3">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <span className="min-w-0 truncate font-mono font-medium" dir="ltr">
                    {m.local_part}@{d.domain}
                  </span>
                  <div className="flex flex-wrap items-center gap-1">
                    {d.catch_all === m.local_part && <Badge tone="accent">{t("mail.catch_all.badge")}</Badge>}
                    {m.forward_to.length > 0 && (
                      <Badge>
                        <Forward className="size-3" aria-hidden />
                        {m.keep_copy ? t("mail.mailboxes.forwards_copy") : t("mail.mailboxes.forwards_only")}
                      </Badge>
                    )}
                    {m.auto_reply && (
                      <Badge>
                        <Reply className="size-3" aria-hidden />
                        {t("mail.mailboxes.auto_reply")}
                      </Badge>
                    )}
                    <Button size="sm" variant="ghost" onClick={() => setEditing(m)} disabled={busy} title={t("common.edit")}>
                      <Pencil className="size-4" aria-hidden />
                      <span className="sr-only">{t("common.edit")}</span>
                    </Button>
                    <Button size="sm" variant="ghost" onClick={() => reset(m)} disabled={busy} title={t("mail.mailboxes.reset")}>
                      <KeyRound className="size-4" aria-hidden />
                      <span className="sr-only">{t("mail.mailboxes.reset")}</span>
                    </Button>
                    <Button size="sm" variant="ghost" onClick={() => setDeleting(m)} disabled={busy} title={t("common.delete")}>
                      <Trash2 className="size-4" aria-hidden />
                      <span className="sr-only">{t("common.delete")}</span>
                    </Button>
                  </div>
                </div>
                <div className="grid gap-1 text-sm text-muted">
                  <span>
                    {m.quota_mb
                      ? t("mail.mailboxes.usage_of", {
                          used: formatBytes(used, i18n.language),
                          limit: formatBytes(limit, i18n.language),
                        })
                      : t("mail.mailboxes.usage", {
                          used: formatBytes(used, i18n.language),
                        })}
                  </span>
                  {m.quota_mb > 0 && <Meter percent={(used / limit) * 100} label={t("mail.mailboxes.quota")} />}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      <MailboxDialog open={adding} onClose={() => setAdding(false)} detail={detail} />
      <MailboxDialog open={!!editing} onClose={() => setEditing(null)} detail={detail} mailbox={editing ?? undefined} />
      <Dialog open={!!shown} onClose={() => setShown(null)} title={t("mail.mailboxes.reset")}>
        {shown && <PasswordShown address={`${shown.mailbox.local_part}@${d.domain}`} password={shown.password ?? ""} />}
        <div className="flex justify-end">
          <Button variant="primary" onClick={() => setShown(null)}>
            {t("databases.saved_it")}
          </Button>
        </div>
      </Dialog>
      <ConfirmDelete
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={t("mail.mailboxes.delete_title", {
          address: deleting ? `${deleting.local_part}@${d.domain}` : "",
        })}
        body={t("mail.mailboxes.delete_body")}
        onConfirm={async () => {
          await api.del(`/mail/mailboxes/${deleting!.id}`);
          qc.invalidateQueries({ queryKey: ["mail-domain", d.id] });
          qc.invalidateQueries({ queryKey: ["mail-domains"] });
        }}
      />
    </Card>
  );
}

function ForwardersCard({ detail }: { detail: MailDomainDetail }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const d = detail.domain;
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<MailForwarder | null>(null);
  const [deleting, setDeleting] = useState<MailForwarder | null>(null);
  const busy = d.state === "deleting";
  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">{t("mail.forwarders.title")}</h2>
          <p className="text-sm text-muted">{t("mail.forwarders.intro")}</p>
        </div>
        <Button size="sm" onClick={() => setAdding(true)} disabled={busy}>
          <Forward className="size-4" aria-hidden />
          {t("mail.forwarders.add")}
        </Button>
      </div>
      {detail.forwarders.length > 0 && (
        <ul className="grid gap-2">
          {detail.forwarders.map((f) => (
            <li key={f.id} className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-line px-4 py-3">
              <div className="grid min-w-0 gap-0.5" dir="ltr">
                <span className="truncate font-mono font-medium">
                  {f.local_part}@{d.domain}
                </span>
                <span className="text-sm break-all text-muted">→ {f.destinations.join(", ")}</span>
              </div>
              <div className="flex gap-1">
                <Button size="sm" variant="ghost" onClick={() => setEditing(f)} disabled={busy} title={t("common.edit")}>
                  <Pencil className="size-4" aria-hidden />
                  <span className="sr-only">{t("common.edit")}</span>
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setDeleting(f)} disabled={busy} title={t("common.delete")}>
                  <Trash2 className="size-4" aria-hidden />
                  <span className="sr-only">{t("common.delete")}</span>
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}
      <ForwarderDialog open={adding} onClose={() => setAdding(false)} detail={detail} />
      <ForwarderDialog open={!!editing} onClose={() => setEditing(null)} detail={detail} forwarder={editing ?? undefined} />
      <ConfirmDelete
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={t("mail.forwarders.delete_title", {
          address: deleting ? `${deleting.local_part}@${d.domain}` : "",
        })}
        body={t("mail.forwarders.delete_body")}
        onConfirm={async () => {
          await api.del(`/mail/forwarders/${deleting!.id}`);
          qc.invalidateQueries({ queryKey: ["mail-domain", d.id] });
          qc.invalidateQueries({ queryKey: ["mail-domains"] });
        }}
      />
    </Card>
  );
}

function CatchAllCard({ detail }: { detail: MailDomainDetail }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const d = detail.domain;
  const [error, setError] = useState("");
  const change = async (v: string) => {
    setError("");
    try {
      await api.patch(`/mail/domains/${d.id}`, { catch_all: v });
      qc.invalidateQueries({ queryKey: ["mail-domain", d.id] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };
  return (
    <Card className="grid gap-3 p-6">
      <h2 className="text-lg font-semibold">{t("mail.catch_all.title")}</h2>
      <p className="text-sm text-muted">{t("mail.catch_all.intro", { domain: d.domain })}</p>
      <ErrorAlert>{error}</ErrorAlert>
      <select
        value={d.catch_all ?? ""}
        onChange={(e) => change(e.target.value)}
        disabled={d.state === "deleting"}
        className={select}
        aria-label={t("mail.catch_all.title")}
      >
        <option value="">{t("mail.catch_all.off")}</option>
        {detail.mailboxes.map((m) => (
          <option key={m.id} value={m.local_part}>
            {m.local_part}@{d.domain}
          </option>
        ))}
      </select>
    </Card>
  );
}

function ConfirmDelete({
  open,
  onClose,
  title,
  body,
  onConfirm,
  typed,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  body: string;
  onConfirm: () => Promise<void>;
  typed?: string;
}) {
  const { t } = useTranslation();
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (open) {
      setValue("");
      setError("");
    }
  }, [open]);
  return (
    <Dialog open={open} onClose={onClose} title={title}>
      <form
        className="grid gap-5"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            await onConfirm();
            onClose();
          } catch (err) {
            setError(errorMessage(t, err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <p className="text-muted">{body}</p>
        <ErrorAlert>{error}</ErrorAlert>
        {typed && (
          <Field
            label={t("databases.delete.confirm", { name: typed })}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            autoComplete="off"
            className="[&_input]:font-mono"
          />
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="danger" loading={busy} disabled={!!typed && value.trim() !== typed}>
            {t("common.delete")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

export function MailDomainPage() {
  const { id = "" } = useParams();
  const { t } = useTranslation();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [deleting, setDeleting] = useState(false);
  const q = useQuery({
    queryKey: ["mail-domain", id],
    queryFn: () => api.get<MailDomainDetail>(`/mail/domains/${id}`),
    refetchInterval: (x) => (x.state.data && ["pending", "deleting"].includes(x.state.data.domain.state) ? 1500 : 15000),
    retry: (n, err) => !(err instanceof ApiError && err.status === 404) && n < 2,
  });
  const gone = q.error instanceof ApiError && q.error.status === 404;
  useEffect(() => {
    if (gone) {
      qc.invalidateQueries({ queryKey: ["mail-domains"] });
      navigate("/mail", { replace: true });
    }
  }, [gone, navigate, qc]);
  if (q.isPending || gone) return <Spinner />;
  if (q.error || !q.data) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;
  const detail = q.data;
  const d = detail.domain;
  return (
    <>
      <Link to="/mail" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("nav.mail")}
      </Link>
      <PageHeader title={d.domain} description={`${d.server_name} · ${d.hostname}`} actions={<StateBadge state={d.state} />} />
      <div className="grid gap-6">
        {d.state === "error" && (
          <div className="grid gap-1 rounded-lg bg-crit-soft p-4 text-crit" role="alert">
            <p className="font-semibold">{t("mail.error_title")}</p>
            <p className="text-sm break-words">{d.message}</p>
          </div>
        )}
        {d.state === "pending" && (
          <div className="flex items-center gap-3 rounded-lg bg-warn-soft px-4 py-3 text-warn" role="status">
            <Loader2 className="size-5 shrink-0 animate-spin" aria-hidden />
            {t("mail.applying")}
          </div>
        )}
        <div className="grid gap-6 xl:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
          <div className="grid content-start gap-6">
            <MailboxesCard detail={detail} />
            <ForwardersCard detail={detail} />
            <DNSCard detail={detail} />
          </div>
          <div className="grid content-start gap-6">
            <ClientSettings d={d} />
            <CatchAllCard detail={detail} />
            <Card className="grid gap-3 p-6">
              <h2 className="text-lg font-semibold">{t("mail.domain.delete_title_short")}</h2>
              <p className="text-sm text-muted">{t("mail.domain.delete_body")}</p>
              <div>
                <Button variant="danger" onClick={() => setDeleting(true)} disabled={d.state === "deleting"}>
                  <Trash2 className="size-4" aria-hidden />
                  {t("mail.domain.delete")}
                </Button>
              </div>
            </Card>
          </div>
        </div>
      </div>
      <ConfirmDelete
        open={deleting}
        onClose={() => setDeleting(false)}
        title={t("mail.domain.delete_title", { domain: d.domain })}
        body={t("mail.domain.delete_warning")}
        typed={d.domain}
        onConfirm={async () => {
          await api.del(`/mail/domains/${d.id}`);
          qc.setQueryData(["mail-domain", d.id], {
            ...detail,
            domain: { ...d, state: "deleting" },
          });
          qc.invalidateQueries({ queryKey: ["mail-domains"] });
        }}
      />
    </>
  );
}

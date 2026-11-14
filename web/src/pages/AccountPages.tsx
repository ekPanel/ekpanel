import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { Fingerprint, LogOut, ShieldCheck, ShieldOff, Trash2 } from "lucide-react";
import { api, errorMessage, type AuditEntry, type User } from "../lib/api";
import { formatDateTime, formatRelative } from "../lib/format";
import { cancelled, createPasskey, passkeysSupported } from "../lib/passkeys";
import { meKey, useMe } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, ErrorAlert, Field, Spinner } from "../components/ui";

// ---- Security settings ----

function TwoFactorCard({ user }: { user: User }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [setup, setSetup] = useState<{ secret: string; qr_png: string } | null>(null);
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");
  const [disabling, setDisabling] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // Recovery codes are shown once, right after they are made.
  const [codes, setCodes] = useState<string[] | null>(null);
  const [renewing, setRenewing] = useState(false);
  const left = useQuery({
    queryKey: ["recovery-codes"],
    queryFn: () => api.get<{ left: number }>("/auth/2fa/recovery-codes"),
    enabled: user.totp_enabled,
  });

  const run = async (fn: () => Promise<void>) => {
    setBusy(true);
    setError("");
    try {
      await fn();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  const start = () => run(async () => setSetup(await api.post("/auth/2fa/setup")));
  const enable = (e: FormEvent) => {
    e.preventDefault();
    run(async () => {
      const r = await api.post<User & { recovery_codes: string[] }>("/auth/2fa/enable", { code });
      qc.setQueryData(meKey, r);
      setCodes(r.recovery_codes);
      qc.invalidateQueries({ queryKey: ["recovery-codes"] });
      setSetup(null);
      setCode("");
    });
  };
  const renew = (e: FormEvent) => {
    e.preventDefault();
    run(async () => {
      const r = await api.post<{ recovery_codes: string[] }>("/auth/2fa/recovery-codes", { password });
      setCodes(r.recovery_codes);
      qc.invalidateQueries({ queryKey: ["recovery-codes"] });
      setRenewing(false);
      setPassword("");
    });
  };
  const disable = (e: FormEvent) => {
    e.preventDefault();
    run(async () => {
      qc.setQueryData(meKey, await api.post<User>("/auth/2fa/disable", { password }));
      setDisabling(false);
      setPassword("");
    });
  };

  return (
    <Card className="grid gap-5 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex gap-3">
          <div className={`grid size-10 shrink-0 place-items-center rounded-full ${user.totp_enabled ? "bg-ok-soft text-ok" : "bg-warn-soft text-warn"}`}>
            {user.totp_enabled ? <ShieldCheck className="size-5" aria-hidden /> : <ShieldOff className="size-5" aria-hidden />}
          </div>
          <div>
            <h2 className="font-semibold">{t("security.twofa.title")}</h2>
            <p className="text-muted">{t("security.twofa.body")}</p>
          </div>
        </div>
        <Badge tone={user.totp_enabled ? "ok" : "warn"}>{user.totp_enabled ? t("security.twofa.on") : t("security.twofa.off")}</Badge>
      </div>
      <ErrorAlert>{error}</ErrorAlert>

      {!user.totp_enabled && !setup && (
        <div>
          <Button variant="primary" onClick={start} loading={busy}>
            {t("security.twofa.enable")}
          </Button>
        </div>
      )}

      {setup && (
        <form onSubmit={enable} className="grid gap-5 sm:grid-cols-[auto_minmax(0,1fr)]">
          <img src={setup.qr_png} alt={t("security.twofa.qr_alt")} className="size-44 rounded-lg border border-line bg-white p-2" />
          <div className="grid content-start gap-4">
            <ol className="grid list-decimal gap-1 ps-5 text-sm">
              <li>{t("security.twofa.step_scan")}</li>
              <li>{t("security.twofa.step_code")}</li>
            </ol>
            <div className="grid gap-1">
              <p className="text-sm text-muted">{t("security.twofa.manual")}</p>
              <div className="flex items-center gap-2">
                <code dir="ltr" className="min-w-0 truncate rounded bg-sunk px-2 py-1 font-mono text-sm">{setup.secret}</code>
                <CopyButton text={setup.secret} />
              </div>
            </div>
            <Field
              label={t("security.code")}
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, "").slice(0, 6))}
              inputMode="numeric"
              autoComplete="one-time-code"
              className="max-w-48 [&_input]:font-mono [&_input]:tracking-[0.3em]"
              autoFocus
            />
            <div className="flex gap-2">
              <Button type="submit" variant="primary" loading={busy} disabled={code.length !== 6}>
                {t("security.twofa.confirm")}
              </Button>
              <Button type="button" variant="ghost" onClick={() => setSetup(null)}>
                {t("common.cancel")}
              </Button>
            </div>
          </div>
        </form>
      )}

      {codes && <RecoveryCodes codes={codes} onDone={() => setCodes(null)} />}

      {user.totp_enabled && !codes && left.data && (
        <div className="grid gap-2 rounded-lg border border-line p-4">
          <p className="font-medium">{t("security.recovery.title")}</p>
          <p className={`text-sm ${left.data.left <= 3 ? "text-warn" : "text-muted"}`}>{t("security.recovery.left", { count: left.data.left })}</p>
          {renewing ? (
            <form onSubmit={renew} className="grid max-w-sm gap-3">
              <Field label={t("security.recovery.password")} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" autoFocus />
              <div className="flex gap-2">
                <Button type="submit" loading={busy} disabled={!password}>
                  {t("security.recovery.renew")}
                </Button>
                <Button type="button" variant="ghost" onClick={() => setRenewing(false)}>
                  {t("common.cancel")}
                </Button>
              </div>
            </form>
          ) : (
            <div>
              <Button size="sm" onClick={() => (setRenewing(true), setDisabling(false))}>
                {t("security.recovery.renew")}
              </Button>
            </div>
          )}
        </div>
      )}

      {user.totp_enabled && !disabling && (
        <div>
          <Button variant="danger" onClick={() => (setDisabling(true), setRenewing(false))}>
            {t("security.twofa.disable")}
          </Button>
        </div>
      )}
      {user.totp_enabled && disabling && (
        <form onSubmit={disable} className="grid max-w-sm gap-4">
          <Field label={t("security.twofa.disable_password")} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" autoFocus />
          <div className="flex gap-2">
            <Button type="submit" variant="danger" loading={busy} disabled={!password}>
              {t("security.twofa.disable")}
            </Button>
            <Button type="button" variant="ghost" onClick={() => setDisabling(false)}>
              {t("common.cancel")}
            </Button>
          </div>
        </form>
      )}
    </Card>
  );
}

/** Recovery codes, shown once: copy or download them before going on. */
function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const { t } = useTranslation();
  const text = codes.join("\n") + "\n";
  return (
    <div className="grid gap-3 rounded-lg border border-warn bg-warn-soft p-4">
      <p className="font-medium text-warn">{t("security.recovery.save")}</p>
      <ul className="grid grid-cols-2 gap-x-6 gap-y-1 font-mono text-sm sm:grid-cols-5" dir="ltr">
        {codes.map((c) => (
          <li key={c}>{c}</li>
        ))}
      </ul>
      <div className="flex flex-wrap items-center gap-2">
        <CopyButton text={text} />
        <a
          href={"data:text/plain;charset=utf-8," + encodeURIComponent(text)}
          download="ekpanel-recovery-codes.txt"
          className="inline-flex h-8 items-center rounded-lg border border-line bg-surface px-3 text-sm font-medium text-ink hover:bg-sunk"
        >
          {t("security.recovery.download")}
        </a>
        <Button size="sm" variant="primary" onClick={onDone}>
          {t("security.recovery.saved")}
        </Button>
      </div>
    </div>
  );
}

interface Passkey {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string | null;
}

function PasskeysCard() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const keys = useQuery({ queryKey: ["passkeys"], queryFn: () => api.get<Passkey[]>("/auth/passkeys") });
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const supported = passkeysSupported();

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const start = await api.post<{ ceremony: string; options: unknown }>("/auth/passkeys/options", { password });
      const credential = await createPasskey(start.options);
      await api.post("/auth/passkeys", { ceremony: start.ceremony, name, credential });
      qc.invalidateQueries({ queryKey: ["passkeys"] });
      setAdding(false);
      setName("");
      setPassword("");
    } catch (err) {
      if (!cancelled(err)) setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const remove = async (k: Passkey) => {
    if (!window.confirm(t("security.passkeys.remove_confirm", { name: k.name }))) return;
    setError("");
    try {
      await api.del(`/auth/passkeys/${k.id}`);
      qc.invalidateQueries({ queryKey: ["passkeys"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };

  return (
    <Card className="grid gap-4 p-6">
      <div className="flex gap-3">
        <div className="grid size-10 shrink-0 place-items-center rounded-full bg-accent-soft text-accent">
          <Fingerprint className="size-5" aria-hidden />
        </div>
        <div>
          <h2 className="font-semibold">{t("security.passkeys.title")}</h2>
          <p className="text-muted">{t("security.passkeys.body")}</p>
        </div>
      </div>
      <ErrorAlert>{error}</ErrorAlert>
      {!!keys.data?.length && (
        <ul className="divide-y divide-line rounded-lg border border-line">
          {keys.data.map((k) => (
            <li key={k.id} className="flex flex-wrap items-center gap-3 px-3 py-2.5 text-sm">
              <span className="min-w-0 flex-1">
                <span className="block truncate font-medium">{k.name}</span>
                <span className="block text-xs text-muted">
                  {t("security.passkeys.added", { when: formatDateTime(k.created_at, i18n.language) })}
                  {" · "}
                  {k.last_used_at ? t("security.passkeys.used", { when: formatRelative(k.last_used_at, i18n.language) }) : t("security.passkeys.never")}
                </span>
              </span>
              <Button size="sm" variant="ghost" onClick={() => remove(k)} aria-label={t("security.passkeys.remove", { name: k.name })}>
                <Trash2 className="size-4" aria-hidden />
              </Button>
            </li>
          ))}
        </ul>
      )}
      {!supported ? (
        <p className="text-sm text-muted">{t("security.passkeys.unsupported")}</p>
      ) : adding ? (
        <form onSubmit={add} className="grid gap-3 sm:grid-cols-2">
          <Field label={t("security.passkeys.name")} value={name} onChange={(e) => setName(e.target.value)} placeholder={t("security.passkeys.name_hint")} maxLength={60} autoFocus />
          <Field label={t("security.passkeys.password")} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
          <div className="flex gap-2 sm:col-span-2">
            <Button type="submit" variant="primary" loading={busy} disabled={!password}>
              {t("security.passkeys.create")}
            </Button>
            <Button type="button" variant="ghost" onClick={() => setAdding(false)}>
              {t("common.cancel")}
            </Button>
          </div>
        </form>
      ) : (
        <div>
          <Button onClick={() => setAdding(true)}>
            <Fingerprint className="size-4" aria-hidden />
            {t("security.passkeys.add")}
          </Button>
        </div>
      )}
    </Card>
  );
}

interface SessionInfo {
  id: string;
  ip: string;
  user_agent: string;
  method: "password" | "passkey";
  created_at: string;
  last_seen_at: string;
  impersonated: boolean;
  current: boolean;
}

/** "Firefox on Linux": enough to tell browsers apart. */
function browserName(ua: string): string {
  const b = /Edg\//.test(ua) ? "Edge" : /OPR\//.test(ua) ? "Opera" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : /curl|ekctl/i.test(ua) ? ua.split(" ")[0] : "";
  const os = /Android/.test(ua) ? "Android" : /iPhone|iPad/.test(ua) ? "iOS" : /Windows/.test(ua) ? "Windows" : /Mac OS X/.test(ua) ? "macOS" : /Linux/.test(ua) ? "Linux" : "";
  return [b, os].filter(Boolean).join(" · ") || ua.slice(0, 40) || "—";
}

function SessionsCard() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const sessions = useQuery({ queryKey: ["sessions"], queryFn: () => api.get<SessionInfo[]>("/auth/sessions") });
  const [state, setState] = useState<"idle" | "busy" | "done">("idle");
  const [error, setError] = useState("");
  const end = async (s: SessionInfo) => {
    setError("");
    try {
      await api.del(`/auth/sessions/${s.id}`);
      qc.invalidateQueries({ queryKey: ["sessions"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };
  return (
    <Card className="grid gap-4 p-6">
      <div>
        <h2 className="font-semibold">{t("security.sessions.title")}</h2>
        <p className="text-muted">{t("security.sessions.body")}</p>
      </div>
      <ErrorAlert>{error}</ErrorAlert>
      {!!sessions.data?.length && (
        <ul className="divide-y divide-line rounded-lg border border-line">
          {sessions.data.map((s) => (
            <li key={s.id} className="flex flex-wrap items-center gap-3 px-3 py-2.5 text-sm">
              <span className="min-w-0 flex-1">
                <span className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{browserName(s.user_agent)}</span>
                  {s.current && <Badge tone="ok">{t("security.sessions.this")}</Badge>}
                  {s.method === "passkey" && <Badge tone="accent">{t("security.sessions.passkey")}</Badge>}
                  {s.impersonated && <Badge tone="warn">{t("security.sessions.admin")}</Badge>}
                </span>
                <span className="block text-xs text-muted" dir="auto">
                  {s.ip} · {t("security.sessions.since", { when: formatDateTime(s.created_at, i18n.language) })} ·{" "}
                  {t("security.sessions.seen", { when: formatRelative(s.last_seen_at, i18n.language) })}
                </span>
              </span>
              {!s.current && (
                <Button size="sm" onClick={() => end(s)}>
                  {t("security.sessions.end")}
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      <div className="flex flex-wrap items-center gap-3">
        <Button
          loading={state === "busy"}
          onClick={async () => {
            setState("busy");
            setError("");
            try {
              await api.post("/auth/sessions/revoke-others");
              qc.invalidateQueries({ queryKey: ["sessions"] });
              setState("done");
            } catch (err) {
              setError(errorMessage(t, err));
              setState("idle");
            }
          }}
        >
          <LogOut className="size-4 rtl:-scale-x-100" aria-hidden />
          {t("security.sessions.revoke")}
        </Button>
        {state === "done" && <span className="text-sm text-ok">{t("security.sessions.revoked")}</span>}
      </div>
    </Card>
  );
}

function PasswordCard() {
  const { t } = useTranslation();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [error, setError] = useState("");
  return (
    <Card className="p-6">
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          setDone(false);
          try {
            await api.post("/auth/password", { current, new: next });
            setCurrent("");
            setNext("");
            setDone(true);
          } catch (err) {
            setError(errorMessage(t, err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <div>
          <h2 className="font-semibold">{t("security.password.title")}</h2>
          <p className="text-muted">{t("security.password.body")}</p>
        </div>
        <ErrorAlert>{error}</ErrorAlert>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t("security.password.current")} type="password" value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" required />
          <Field label={t("security.password.new")} type="password" value={next} onChange={(e) => setNext(e.target.value)} autoComplete="new-password" minLength={10} required />
        </div>
        <div className="flex flex-wrap items-center gap-3">
          <Button type="submit" loading={busy}>
            {t("security.password.save")}
          </Button>
          {done && <span className="text-sm text-ok">{t("security.password.saved")}</span>}
        </div>
      </form>
    </Card>
  );
}

export function SecurityPage() {
  const { t } = useTranslation();
  const me = useMe();
  return (
    <>
      <PageHeader title={t("nav.security")} description={t("security.intro")} />
      <div className="grid max-w-3xl gap-6">
        {me.data!.impersonator_id ? (
          <ErrorAlert>{t("security.not_while_acting")}</ErrorAlert>
        ) : (
          <>
            <PasswordCard />
            <TwoFactorCard user={me.data!} />
            <PasskeysCard />
            <SessionsCard />
          </>
        )}
      </div>
    </>
  );
}

// ---- Audit log ----

export function AuditPage() {
  const { t, i18n } = useTranslation();
  const pageSize = 50;
  const q = useInfiniteQuery({
    queryKey: ["audit"],
    queryFn: ({ pageParam }) => api.get<AuditEntry[]>(`/audit?limit=${pageSize}${pageParam ? `&before=${pageParam}` : ""}`),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.length === pageSize ? last[last.length - 1].id : undefined),
  });
  const rows = q.data?.pages.flat() ?? [];

  return (
    <>
      <PageHeader title={t("nav.audit")} description={t("audit.intro")} />
      {q.isPending && <Spinner />}
      {q.error && <ErrorAlert>{t("errors.internal")}</ErrorAlert>}
      {q.isSuccess && (
        <Card className="overflow-x-auto">
          <table className="w-full min-w-[720px] text-sm">
            <thead>
              <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                <th className="px-4 py-3 text-start font-semibold">{t("audit.col.when")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("audit.col.who")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("audit.col.what")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("audit.col.target")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("audit.col.ip")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((e) => (
                <tr key={e.id} className="border-b border-line last:border-0">
                  <td className="px-4 py-2.5 whitespace-nowrap text-muted tabular-nums">{formatDateTime(e.at, i18n.language)}</td>
                  <td className="px-4 py-2.5">
                    {e.actor_name ?? <span className="text-muted">{t("audit.system")}</span>}
                    {typeof e.details.via_token === "string" && (
                      <span className="block text-xs text-muted">{t("audit.via_token", { name: e.details.via_token })}</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <Badge tone={e.action.endsWith("failed") ? "crit" : e.action.includes("deleted") ? "warn" : "neutral"}>
                      {t(`audit.actions.${e.action}`, { defaultValue: e.action })}
                    </Badge>
                  </td>
                  <td className="px-4 py-2.5" dir="auto">{e.target}</td>
                  <td className="px-4 py-2.5 font-mono text-xs text-muted" dir="ltr">{e.actor_ip}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {q.hasNextPage && (
            <div className="border-t border-line p-3 text-center">
              <Button size="sm" onClick={() => q.fetchNextPage()} loading={q.isFetchingNextPage}>
                {t("audit.more")}
              </Button>
            </div>
          )}
        </Card>
      )}
    </>
  );
}

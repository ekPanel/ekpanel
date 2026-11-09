import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { LogOut, ShieldCheck, ShieldOff } from "lucide-react";
import { api, errorMessage, type AuditEntry, type User } from "../lib/api";
import { formatDateTime } from "../lib/format";
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
      qc.setQueryData(meKey, await api.post<User>("/auth/2fa/enable", { code }));
      setSetup(null);
      setCode("");
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

      {user.totp_enabled && !disabling && (
        <div>
          <Button variant="danger" onClick={() => setDisabling(true)}>
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

function SessionsCard() {
  const { t } = useTranslation();
  const [state, setState] = useState<"idle" | "busy" | "done">("idle");
  const [error, setError] = useState("");
  return (
    <Card className="grid gap-4 p-6">
      <div>
        <h2 className="font-semibold">{t("security.sessions.title")}</h2>
        <p className="text-muted">{t("security.sessions.body")}</p>
      </div>
      <ErrorAlert>{error}</ErrorAlert>
      <div className="flex flex-wrap items-center gap-3">
        <Button
          loading={state === "busy"}
          onClick={async () => {
            setState("busy");
            setError("");
            try {
              await api.post("/auth/sessions/revoke-others");
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

import { Suspense, lazy, useState, type FormEvent } from "react";
import { Link, useParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, RotateCw, ShieldAlert, SquareTerminal } from "lucide-react";
import { api, errorMessage, type App, type Server } from "../lib/api";
import { meKey, useMe } from "../lib/session";
import { Badge, Button, Card, ErrorAlert, Field, Spinner } from "../components/ui";
import type { TerminalState } from "../components/TerminalView";

const TerminalView = lazy(() => import("../components/TerminalView"));

/** Asks for the password (and 2FA code) again before a terminal opens. */
function ConfirmCard({ onDone }: { onDone: () => void }) {
  const { t } = useTranslation();
  const me = useMe();
  const qc = useQueryClient();
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.post("/auth/confirm", { password, totp_code: code });
      await qc.invalidateQueries({ queryKey: meKey });
      onDone();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card className="mx-auto mt-8 w-full max-w-md p-6">
      <form onSubmit={submit} className="grid gap-4">
        <div className="flex items-start gap-3">
          <ShieldAlert className="mt-0.5 size-5 shrink-0 text-warn" aria-hidden />
          <div>
            <h2 className="font-semibold">{t("terminal.confirm_title")}</h2>
            <p className="text-sm text-muted">{t("terminal.confirm_body")}</p>
          </div>
        </div>
        <ErrorAlert>{error}</ErrorAlert>
        <Field
          label={t("account.password")}
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="current-password"
          autoFocus
          required
        />
        {me.data?.totp_enabled && (
          <Field
            label={t("security.code")}
            value={code}
            onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
            inputMode="numeric"
            autoComplete="one-time-code"
            maxLength={6}
            required
          />
        )}
        <Button type="submit" variant="primary" loading={busy}>
          {t("terminal.confirm")}
        </Button>
      </form>
    </Card>
  );
}

function TerminalScreen({
  title,
  back,
  backLabel,
  path,
  children,
}: {
  title: string;
  back: string;
  backLabel: string;
  path: string;
  children?: React.ReactNode;
}) {
  const { t } = useTranslation();
  const me = useMe();
  const [confirmed, setConfirmed] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const [state, setState] = useState<TerminalState>({ kind: "connecting" });
  const until = me.data?.confirmed_until ? new Date(me.data.confirmed_until).getTime() : 0;
  const ready = confirmed || until > Date.now();
  const reasonText = (r: string) =>
    r === "idle" || r === "time_limit" || r === "exit" || r === "agent_offline" ? t(`terminal.reasons.${r}`) : r || t("terminal.reasons.lost");

  return (
    <div className="flex h-[calc(100dvh-8rem)] min-h-[24rem] flex-col gap-3">
      <div className="flex flex-wrap items-center gap-3">
        <Link to={back} className="inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
          <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
          {backLabel}
        </Link>
        <h1 className="flex items-center gap-2 text-lg font-semibold">
          <SquareTerminal className="size-5 text-muted" aria-hidden />
          {title}
        </h1>
        {children}
        {ready && (
          <span className="ms-auto flex items-center gap-2">
            {state.kind === "open" && <Badge tone="ok">{t("terminal.connected")}</Badge>}
            {state.kind === "connecting" && <Badge tone="warn">{t("terminal.connecting")}</Badge>}
            {state.kind === "closed" && (
              <>
                <Badge>{reasonText(state.reason)}</Badge>
                <Button size="sm" onClick={() => setAttempt((a) => a + 1)}>
                  <RotateCw className="size-4" aria-hidden />
                  {t("terminal.reconnect")}
                </Button>
              </>
            )}
          </span>
        )}
      </div>
      {!ready ? (
        <ConfirmCard onDone={() => setConfirmed(true)} />
      ) : (
        <div className="min-h-0 flex-1 overflow-hidden rounded-xl border border-line bg-[#0f1513] p-2">
          <Suspense fallback={<Spinner />}>
            <TerminalView path={path} attempt={attempt} onState={setState} />
          </Suspense>
        </div>
      )}
      <p className="text-xs text-muted">{t("terminal.note")}</p>
    </div>
  );
}

export function ServerTerminalPage() {
  const { id = "" } = useParams();
  const { t } = useTranslation();
  const srv = useQuery({ queryKey: ["servers", id], queryFn: () => api.get<Server>(`/servers/${id}`) });
  if (srv.isPending) return <Spinner />;
  if (srv.error) return <ErrorAlert>{errorMessage(t, srv.error)}</ErrorAlert>;
  return (
    <TerminalScreen
      title={t("terminal.server_title", { name: srv.data.name })}
      back={`/servers/${id}`}
      backLabel={srv.data.name}
      path={`/api/v1/servers/${id}/terminal`}
    />
  );
}

export function AppTerminalPage() {
  const { id = "" } = useParams();
  const { t } = useTranslation();
  const app = useQuery({ queryKey: ["apps", id], queryFn: () => api.get<App>(`/apps/${id}`) });
  const [service, setService] = useState("");
  if (app.isPending) return <Spinner />;
  if (app.error) return <ErrorAlert>{errorMessage(t, app.error)}</ErrorAlert>;
  const svc = service || app.data.service || "app";
  return (
    <TerminalScreen
      title={t("terminal.app_title", { name: app.data.name })}
      back={`/apps/${id}`}
      backLabel={app.data.name}
      path={`/api/v1/apps/${id}/terminal?service=${encodeURIComponent(svc)}`}
    >
      {app.data.kind === "compose" && (
        <input
          defaultValue={svc}
          onBlur={(e) => setService(e.target.value.trim())}
          onKeyDown={(e) => e.key === "Enter" && setService((e.target as HTMLInputElement).value.trim())}
          className="h-8 w-32 rounded-lg border border-line bg-surface px-2 font-mono text-sm"
          aria-label={t("terminal.service")}
          dir="ltr"
        />
      )}
    </TerminalScreen>
  );
}

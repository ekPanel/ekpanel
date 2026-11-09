import { useState, type FormEvent, type ReactNode } from "react";
import { Navigate, useNavigate, useSearchParams } from "react-router";
import { Trans, useTranslation } from "react-i18next";
import { Fingerprint, KeyRound } from "lucide-react";
import { api, ApiError, errorMessage, type User } from "../lib/api";
import { useMe, useSetupStatus, useSignedIn } from "../lib/session";
import { cancelled, getPasskey, passkeysSupported } from "../lib/passkeys";
import { LanguageMenu, Logo, ThemeMenu } from "../components/AppShell";
import { Button, ErrorAlert, Field, Spinner } from "../components/ui";

function AuthLayout({ title, subtitle, children }: { title: string; subtitle: string; children: ReactNode }) {
  return (
    <div className="flex min-h-full flex-col">
      <header className="flex items-center justify-between gap-3 px-4 py-4 sm:px-8">
        <Logo />
        <div className="flex items-center gap-2">
          <LanguageMenu />
          <ThemeMenu />
        </div>
      </header>
      <main className="flex flex-1 items-start justify-center px-4 pt-6 pb-16 sm:pt-16">
        <div className="w-full max-w-md">
          <h1 className="text-2xl font-bold tracking-tight text-balance">{title}</h1>
          <p className="mt-1.5 text-muted">{subtitle}</p>
          <div className="mt-8 rounded-2xl border border-line bg-surface p-6 sm:p-8">{children}</div>
        </div>
      </main>
    </div>
  );
}

export function SetupPage() {
  const { t, i18n } = useTranslation();
  const status = useSetupStatus();
  const signedIn = useSignedIn();
  const navigate = useNavigate();
  const [form, setForm] = useState({ token: "", name: "", email: "", password: "" });
  const [error, setError] = useState<{ field?: string; message: string } | null>(null);
  const [busy, setBusy] = useState(false);

  if (status.isPending) return <Spinner />;
  if (status.data && !status.data.needs_setup) return <Navigate to="/" replace />;

  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const user = await api.post<User>("/setup", { ...form, locale: i18n.resolvedLanguage });
      signedIn(user);
      navigate("/", { replace: true });
    } catch (err) {
      const field =
        err instanceof ApiError
          ? { invalid_setup_token: "token", invalid_email: "email", invalid_name: "name", weak_password: "password" }[err.code]
          : undefined;
      setError({ field, message: errorMessage(t, err) });
    } finally {
      setBusy(false);
    }
  };

  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);

  return (
    <AuthLayout title={t("setup.title")} subtitle={t("setup.subtitle")}>
      <form onSubmit={submit} className="grid gap-5" noValidate>
        {error && !error.field && <ErrorAlert>{error.message}</ErrorAlert>}
        <Field
          label={t("setup.token")}
          value={form.token}
          onChange={set("token")}
          autoComplete="off"
          spellCheck={false}
          className="[&_input]:font-mono"
          hint={<Trans i18nKey="setup.token_hint" components={{ code: <code className="rounded bg-sunk px-1 font-mono text-xs" /> }} />}
          error={fieldError("token")}
          required
        />
        <Field label={t("account.name")} value={form.name} onChange={set("name")} autoComplete="name" error={fieldError("name")} required />
        <Field label={t("account.email")} type="email" value={form.email} onChange={set("email")} autoComplete="email" error={fieldError("email")} required />
        <Field
          label={t("account.password")}
          type="password"
          value={form.password}
          onChange={set("password")}
          autoComplete="new-password"
          hint={t("account.password_hint")}
          error={fieldError("password")}
          required
        />
        <Button type="submit" variant="primary" loading={busy}>
          {t("setup.submit")}
        </Button>
      </form>
    </AuthLayout>
  );
}

export function LoginPage() {
  const { t } = useTranslation();
  const me = useMe();
  const status = useSetupStatus();
  const signedIn = useSignedIn();
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const [form, setForm] = useState({ email: "", password: "", totp_code: "" });
  const [needsCode, setNeedsCode] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  if (me.isPending || status.isPending) return <Spinner />;
  if (status.data?.needs_setup) return <Navigate to="/setup" replace />;
  if (me.data) return <Navigate to="/" replace />;

  // Only same-site paths are allowed as a return address.
  const next = params.get("next");
  const target = next && next.startsWith("/") && !next.startsWith("//") ? next : "/";

  const withPasskey = async () => {
    setBusy(true);
    setError("");
    try {
      const start = await api.post<{ ceremony: string; options: unknown }>("/auth/passkey/options");
      const credential = await getPasskey(start.options);
      const user = await api.post<User>("/auth/passkey/login", { ceremony: start.ceremony, credential });
      signedIn(user);
      navigate(target, { replace: true });
    } catch (err) {
      if (!cancelled(err)) setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const user = await api.post<User>("/auth/login", needsCode ? form : { email: form.email, password: form.password });
      signedIn(user);
      navigate(target, { replace: true });
    } catch (err) {
      if (err instanceof ApiError && err.code === "totp_required") {
        setNeedsCode(true);
      } else {
        setError(errorMessage(t, err));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthLayout title={t("login.title")} subtitle={needsCode ? t("login.code_subtitle") : t("login.subtitle")}>
      <form onSubmit={submit} className="grid gap-5">
        <ErrorAlert>{error}</ErrorAlert>
        {!needsCode ? (
          <>
            <Field label={t("account.email")} type="email" value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} autoComplete="username" required autoFocus />
            <Field label={t("account.password")} type="password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} autoComplete="current-password" required />
          </>
        ) : (
          <div className="grid gap-4">
            <div className="flex items-center gap-3 rounded-lg bg-accent-soft px-3 py-2.5 text-sm text-accent">
              <KeyRound className="size-4 shrink-0" aria-hidden />
              {t("login.code_help")}
            </div>
            <Field
              label={t("login.code_or_recovery")}
              hint={t("login.recovery_hint")}
              value={form.totp_code}
              onChange={(e) => setForm({ ...form, totp_code: e.target.value.replace(/[^0-9A-Za-z -]/g, "").slice(0, 12) })}
              autoComplete="one-time-code"
              className="[&_input]:font-mono [&_input]:tracking-[0.3em]"
              spellCheck={false}
              required
              autoFocus
            />
          </div>
        )}
        <Button type="submit" variant="primary" loading={busy}>
          {needsCode ? t("login.verify") : t("login.submit")}
        </Button>
        {needsCode && (
          <Button type="button" variant="ghost" onClick={() => { setNeedsCode(false); setForm({ ...form, totp_code: "" }); }}>
            {t("common.back")}
          </Button>
        )}
        {!needsCode && passkeysSupported() && (
          <>
            <div className="flex items-center gap-3 text-xs text-muted" aria-hidden>
              <span className="h-px flex-1 bg-line" />
              {t("login.or")}
              <span className="h-px flex-1 bg-line" />
            </div>
            <Button type="button" onClick={withPasskey} disabled={busy}>
              <Fingerprint className="size-4" aria-hidden />
              {t("login.passkey")}
            </Button>
          </>
        )}
      </form>
    </AuthLayout>
  );
}

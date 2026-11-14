import { useEffect, useRef, useState, type CSSProperties, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ImageUp, Trash2 } from "lucide-react";
import { api, ApiError, errorMessage, type BrandSettings, type Branding } from "../lib/api";
import { brandingKey } from "../lib/brand";
import { useMe } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { BrandMark } from "../components/Brand";
import { Badge, Button, Card, CopyButton, ErrorAlert, Field, Spinner, type Tone } from "../components/ui";

const settingsKey = ["brand"] as const;
const hexRE = /^#[0-9a-f]{6}$/;

const stateTone: Record<BrandSettings["domain_state"], Tone> = { "": "neutral", active: "ok", pending: "warn", failed: "crit", no_server: "crit" };

/** White-label: the panel's own look (administrators) or a reseller's, for their customers. */
export function BrandingPage() {
  const { t } = useTranslation();
  const me = useMe();
  const qc = useQueryClient();
  const reseller = me.data?.role === "reseller";
  const q = useQuery({
    queryKey: settingsKey,
    queryFn: () => api.get<BrandSettings>("/brand"),
    // While a new domain is being set up, look again now and then.
    refetchInterval: (query) => (query.state.data?.domain_state === "pending" ? 5000 : false),
  });
  const [form, setForm] = useState({ name: "", color: "", domain: "" });
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState<"" | "save" | "logo" | "remove">("");
  const file = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (q.data && !loaded) {
      setForm({ name: q.data.name, color: q.data.color, domain: q.data.domain ?? "" });
      setLoaded(true);
    }
  }, [q.data, loaded]);

  if (q.isPending) return <Spinner />;
  if (q.error) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;
  const s = q.data;

  const done = (next: BrandSettings) => {
    qc.setQueryData(settingsKey, next);
    qc.invalidateQueries({ queryKey: brandingKey });
  };

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setSaved(false);
    if (form.color && !hexRE.test(form.color.toLowerCase())) {
      setError(t("errors.invalid_color"));
      return;
    }
    setBusy("save");
    try {
      const next = await api.put<BrandSettings>("/brand", form);
      done(next);
      setForm({ name: next.name, color: next.color, domain: next.domain ?? "" });
      setSaved(true);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };

  const upload = async (f: File) => {
    setError("");
    setBusy("logo");
    try {
      const res = await fetch("/api/v1/brand/logo", {
        method: "PUT",
        credentials: "same-origin",
        headers: { "Content-Type": "application/octet-stream" },
        body: f,
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new ApiError(res.status, data.code ?? "internal", data.message ?? res.statusText, data);
      done(data as BrandSettings);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
      if (file.current) file.current.value = "";
    }
  };

  const removeLogo = async () => {
    setError("");
    setBusy("remove");
    try {
      done(await api.del<BrandSettings>("/brand/logo"));
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };

  // What the form would look like, for the preview.
  const fallback = s.panel;
  const preview: Branding = {
    name: form.name.trim() || (reseller ? "" : "ekPanel"),
    color: hexRE.test(form.color) ? form.color : (fallback?.color ?? ""),
    logo_url: s.logo_url,
    custom: reseller || Boolean(form.name.trim() || s.logo_url || form.color),
    url: null,
    source_url: "",
  };
  // The colors as the panel will use them (adjusted for each theme); ekPanel's otherwise.
  const palette = window.ekpanelBrandPalette?.(preview.color) ?? {
    light: { accent: "#13804a", ink: "#ffffff", soft: "#ddf3e6" },
    dark: { accent: "#36d27e", ink: "#0f1513", soft: "#12301f" },
  };

  return (
    <div className="grid max-w-3xl gap-6">
      <PageHeader title={t("branding.title")} description={t(reseller ? "branding.intro_reseller" : "branding.intro")} />
      {!s.allowed && (
        <Card className="p-5">
          <p className="font-semibold">{t("branding.not_allowed_title")}</p>
          <p className="mt-1 text-sm text-muted">{t("branding.not_allowed")}</p>
        </Card>
      )}
      <fieldset disabled={!s.allowed} className="grid gap-6 disabled:opacity-60">
        <Card className="grid gap-5 p-5 sm:p-6">
          <form onSubmit={save} className="grid gap-5">
            <Field
              label={t("branding.name")}
              hint={t(reseller ? "branding.name_hint_reseller" : "branding.name_hint")}
              placeholder={reseller ? "" : "ekPanel"}
              value={form.name}
              maxLength={60}
              required={reseller}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
            />
            <div className="grid gap-1.5">
              <label htmlFor="brand-color" className="text-sm font-medium">
                {t("branding.color")}
              </label>
              <div className="flex flex-wrap items-center gap-2">
                <input
                  type="color"
                  aria-label={t("branding.color")}
                  value={hexRE.test(form.color) ? form.color : "#13804a"}
                  onChange={(e) => setForm({ ...form, color: e.target.value })}
                  className="h-10 w-14 cursor-pointer rounded-lg border border-line bg-surface p-1"
                />
                <input
                  id="brand-color"
                  value={form.color}
                  placeholder={t("branding.color_default")}
                  onChange={(e) => setForm({ ...form, color: e.target.value.trim().toLowerCase() })}
                  className="h-10 w-32 rounded-lg border border-line bg-surface px-3 font-mono text-sm"
                  spellCheck={false}
                />
                {form.color && (
                  <Button type="button" variant="ghost" size="sm" onClick={() => setForm({ ...form, color: "" })}>
                    {t("branding.color_reset")}
                  </Button>
                )}
              </div>
              <p className="text-sm text-muted">{t(reseller ? "branding.color_hint_reseller" : "branding.color_hint")}</p>
            </div>
            <Field
              label={t("branding.domain")}
              hint={t(reseller ? "branding.domain_hint_reseller" : "branding.domain_hint")}
              placeholder="panel.example.com"
              value={form.domain}
              onChange={(e) => setForm({ ...form, domain: e.target.value })}
              spellCheck={false}
              autoCapitalize="off"
            />
            {s.domain && (
              <div className="grid gap-2 rounded-lg bg-sunk p-4 text-sm">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{s.domain}</span>
                  <Badge tone={stateTone[s.domain_state]}>{t(`branding.state.${s.domain_state || "none"}`)}</Badge>
                </div>
                {s.domain_error && <p className="text-crit">{s.domain_error}</p>}
                {s.addresses.length > 0 ? (
                  <div className="grid gap-1.5 text-muted">
                    <p>{t("branding.dns", { domain: s.domain })}</p>
                    <ul className="grid gap-1">
                      {s.addresses.map((a) => (
                        <li key={a} className="flex items-center gap-2 font-mono text-ink">
                          {a.includes(":") ? "AAAA" : "A"} {a}
                          <CopyButton text={a} />
                        </li>
                      ))}
                    </ul>
                  </div>
                ) : (
                  <p className="text-muted">{t("branding.no_server")}</p>
                )}
                <p className="text-muted">{t("branding.passkeys_note")}</p>
              </div>
            )}
            {error && <ErrorAlert>{error}</ErrorAlert>}
            <div className="flex flex-wrap items-center gap-3">
              <Button type="submit" variant="primary" loading={busy === "save"}>
                {t("common.save")}
              </Button>
              {saved && <span className="text-sm text-ok">{t("branding.saved")}</span>}
            </div>
          </form>
        </Card>

        <Card className="grid gap-4 p-5 sm:p-6">
          <div>
            <h2 className="font-semibold">{t("branding.logo")}</h2>
            <p className="mt-1 text-sm text-muted">{t("branding.logo_hint")}</p>
          </div>
          <div className="flex flex-wrap items-center gap-3">
            <input
              ref={file}
              type="file"
              accept="image/png,image/jpeg,image/gif,image/webp"
              className="hidden"
              onChange={(e) => e.target.files?.[0] && upload(e.target.files[0])}
            />
            <Button type="button" onClick={() => file.current?.click()} loading={busy === "logo"} disabled={reseller && !s.name}>
              <ImageUp className="size-4" aria-hidden />
              {s.logo_url ? t("branding.logo_change") : t("branding.logo_upload")}
            </Button>
            {s.logo_url && (
              <Button type="button" variant="ghost" onClick={removeLogo} loading={busy === "remove"}>
                <Trash2 className="size-4" aria-hidden />
                {t("branding.logo_remove")}
              </Button>
            )}
            {reseller && !s.name && <span className="text-sm text-muted">{t("errors.brand_name_first")}</span>}
          </div>
        </Card>

        <Card className="grid gap-4 p-5 sm:p-6">
          <h2 className="font-semibold">{t("branding.preview")}</h2>
          <div className="grid gap-3 sm:grid-cols-2">
            {(["light", "dark"] as const).map((mode) => (
              <div
                key={mode}
                className="rounded-xl bg-bg p-4"
                style={{
                  // Each card carries its theme's tokens, whatever the page's theme is.
                  ...(mode === "dark"
                    ? { "--bg": "#0c1312", "--ink": "#e1ebe8", "--line": "#243330" }
                    : { "--bg": "#f3f6f5", "--ink": "#15211f", "--line": "#d6dedb" }),
                  "--accent": palette[mode].accent,
                  "--accent-ink": palette[mode].ink,
                  "--accent-soft": palette[mode].soft,
                } as CSSProperties}
              >
                <div className="flex h-10 items-center">{preview.name || preview.logo_url ? <BrandMark brand={preview} /> : null}</div>
                <div className="mt-4 flex flex-wrap items-center gap-3">
                  <span className="rounded-lg bg-accent px-3 py-1.5 text-sm font-semibold text-accent-ink">{t("branding.preview_button")}</span>
                  <span className="text-sm font-medium text-accent">{t("branding.preview_link")}</span>
                  <span className="rounded-full bg-accent-soft px-2.5 py-0.5 text-xs font-semibold text-accent">{t("branding.preview_badge")}</span>
                </div>
              </div>
            ))}
          </div>
          <p className="text-sm text-muted">{t("branding.preview_note")}</p>
        </Card>
      </fieldset>
    </div>
  );
}

import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Ban, CircleCheck, KeyRound, LogIn, Pencil, Plus, ShieldOff, Store, Trash2, Users } from "lucide-react";
import { api, errorMessage, type Reseller, type ResellerLimits, type Server, type User } from "../lib/api";
import { formatBytes, formatNumber } from "../lib/format";
import { meKey } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, Dialog, ErrorAlert, Field, Spinner } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { usePHPVersions } from "./SitesPage";
import { Checks, OneTimePassword, ResellerUsage, SuspendDialog, useResellers } from "./CustomersPage";

// ---- The limits form ----

interface LimitsForm {
  max_customers: string;
  max_sites: string;
  max_databases: string;
  max_apps: string;
  max_cron_jobs: string;
  max_mail_domains: string;
  max_mailboxes: string;
  disk_gb: string;
  bandwidth_gb: string;
  bandwidth_stop: boolean;
  memory_mb: string;
  cpu_cores: string;
  mailbox_gb: string;
  php_versions: string[];
  app_images: string;
  server_ids: string[];
  sftp: boolean;
  ftp: boolean;
  git_apps: boolean;
  white_label: boolean;
}

const newLimits: LimitsForm = {
  max_customers: "10",
  max_sites: "50",
  max_databases: "50",
  max_apps: "0",
  max_cron_jobs: "50",
  max_mail_domains: "10",
  max_mailboxes: "100",
  disk_gb: "100",
  bandwidth_gb: "",
  bandwidth_stop: false,
  memory_mb: "",
  cpu_cores: "",
  mailbox_gb: "",
  php_versions: [],
  app_images: "",
  server_ids: [],
  sftp: true,
  ftp: false,
  git_apps: false,
  white_label: false,
};

const gb = (mb: number | null) => (mb === null ? "" : String(Math.round((mb / 1024) * 100) / 100));

function formFromLimits(l: ResellerLimits): LimitsForm {
  return {
    max_customers: String(l.max_customers),
    max_sites: String(l.max_sites),
    max_databases: String(l.max_databases),
    max_apps: String(l.max_apps),
    max_cron_jobs: String(l.max_cron_jobs),
    max_mail_domains: String(l.max_mail_domains),
    max_mailboxes: String(l.max_mailboxes),
    disk_gb: gb(l.disk_mb),
    bandwidth_gb: l.bandwidth_gb === null ? "" : String(l.bandwidth_gb),
    bandwidth_stop: l.bandwidth_stop,
    memory_mb: l.memory_mb === null ? "" : String(l.memory_mb),
    cpu_cores: l.cpu_percent === null ? "" : String(l.cpu_percent / 100),
    mailbox_gb: gb(l.mailbox_quota_mb),
    php_versions: l.php_versions,
    app_images: l.app_images.join("\n"),
    server_ids: l.server_ids,
    sftp: l.sftp,
    ftp: l.ftp,
    git_apps: l.git_apps,
    white_label: l.white_label,
  };
}

function limitsFromForm(f: LimitsForm): ResellerLimits {
  const opt = (s: string, scale = 1) => (s.trim() ? Math.round(Number(s) * scale) : null);
  return {
    max_customers: Number(f.max_customers || 0),
    max_sites: Number(f.max_sites || 0),
    max_databases: Number(f.max_databases || 0),
    max_apps: Number(f.max_apps || 0),
    max_cron_jobs: Number(f.max_cron_jobs || 0),
    max_mail_domains: Number(f.max_mail_domains || 0),
    max_mailboxes: Number(f.max_mailboxes || 0),
    disk_mb: opt(f.disk_gb, 1024),
    bandwidth_gb: opt(f.bandwidth_gb),
    bandwidth_stop: f.bandwidth_stop,
    memory_mb: opt(f.memory_mb),
    cpu_percent: opt(f.cpu_cores, 100),
    mailbox_quota_mb: opt(f.mailbox_gb, 1024),
    php_versions: f.php_versions,
    app_images: f.app_images
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean),
    server_ids: f.server_ids,
    sftp: f.sftp,
    ftp: f.ftp,
    git_apps: f.git_apps,
    white_label: f.white_label,
  };
}

function ResellerDialog({ open, reseller, onClose }: { open: boolean; reseller: Reseller | null; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const php = usePHPVersions();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers"), enabled: open });
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [f, setF] = useState<LimitsForm>(newLimits);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<{ email: string; password: string } | null>(null);
  useEffect(() => {
    if (!open) return;
    setName(reseller?.name ?? "");
    setEmail(reseller?.email ?? "");
    setF(reseller ? formFromLimits(reseller.limits) : newLimits);
    setError("");
    setCreated(null);
  }, [open, reseller]);
  const set = <K extends keyof LimitsForm>(k: K, v: LimitsForm[K]) => setF((x) => ({ ...x, [k]: v }));
  const digits =
    (k: keyof LimitsForm, decimal = false) =>
    (e: React.ChangeEvent<HTMLInputElement>) =>
      set(k, e.target.value.replace(decimal ? /[^\d.]/g : /\D/g, "") as never);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body = { name, email, locale: i18n.language, limits: limitsFromForm(f) };
      if (reseller) {
        await api.put(`/resellers/${reseller.id}`, body);
        onClose();
      } else {
        const r = await api.post<{ reseller: Reseller; password: string }>("/resellers", body);
        setCreated({ email: r.reseller.email, password: r.password });
      }
      qc.invalidateQueries({ queryKey: ["resellers"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  const count = (k: keyof LimitsForm, label: string) => (
    <Field label={label} value={f[k] as string} onChange={digits(k)} inputMode="numeric" dir="ltr" required />
  );
  return (
    <Dialog open={open} onClose={onClose} title={reseller ? t("resellers.edit") : t("resellers.add")} wide>
      {created ? (
        <div className="grid gap-4">
          <OneTimePassword {...created} />
          <p className="text-sm text-muted">{t("resellers.share_hint")}</p>
          <div className="flex justify-end">
            <Button variant="primary" onClick={onClose}>
              {t("databases.saved_it")}
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-5">
          <ErrorAlert>{error}</ErrorAlert>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label={t("packages.name")} value={name} onChange={(e) => setName(e.target.value)} maxLength={100} required />
            <Field label={t("account.email")} type="email" value={email} onChange={(e) => setEmail(e.target.value)} dir="ltr" required />
          </div>
          {!reseller && <p className="-mt-3 text-sm text-muted">{t("customers.password_generated")}</p>}
          <section className="grid gap-3">
            <h3 className="font-semibold">{t("resellers.totals")}</h3>
            <p className="-mt-2 text-sm text-muted">{t("resellers.totals_hint")}</p>
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
              {count("max_customers", t("nav.customers"))}
              {count("max_sites", t("nav.sites"))}
              {count("max_databases", t("nav.databases"))}
              {count("max_apps", t("nav.apps"))}
              {count("max_cron_jobs", t("cron.title"))}
              {count("max_mail_domains", t("packages.mail_domains"))}
              {count("max_mailboxes", t("packages.mailboxes"))}
              <Field
                label={t("packages.disk")}
                value={f.disk_gb}
                onChange={digits("disk_gb", true)}
                inputMode="decimal"
                dir="ltr"
                placeholder={t("apps.no_limit")}
              />
            </div>
            <div className="grid gap-3 sm:grid-cols-3">
              <Field
                label={t("packages.bandwidth")}
                value={f.bandwidth_gb}
                onChange={digits("bandwidth_gb")}
                inputMode="numeric"
                dir="ltr"
                placeholder={t("apps.no_limit")}
              />
              <label className="flex items-start gap-2 text-sm sm:col-span-2 sm:mt-7">
                <input
                  type="checkbox"
                  checked={f.bandwidth_stop}
                  disabled={!f.bandwidth_gb.trim()}
                  onChange={(e) => set("bandwidth_stop", e.target.checked)}
                  className="mt-1 accent-[var(--accent)]"
                />
                <span>{t("resellers.bandwidth_stop")}</span>
              </label>
            </div>
          </section>
          <section className="grid gap-3">
            <h3 className="font-semibold">{t("resellers.per_package")}</h3>
            <p className="-mt-2 text-sm text-muted">{t("resellers.per_package_hint")}</p>
            <div className="grid gap-3 sm:grid-cols-3">
              <Field label={t("packages.memory")} value={f.memory_mb} onChange={digits("memory_mb")} inputMode="numeric" dir="ltr" placeholder={t("apps.no_limit")} />
              <Field label={t("packages.cpu")} value={f.cpu_cores} onChange={digits("cpu_cores", true)} inputMode="decimal" dir="ltr" placeholder={t("apps.no_limit")} />
              <Field
                label={t("packages.mailbox_size")}
                value={f.mailbox_gb}
                onChange={digits("mailbox_gb", true)}
                inputMode="decimal"
                dir="ltr"
                placeholder={t("apps.no_limit")}
              />
            </div>
          </section>
          <section className="grid gap-4">
            <h3 className="font-semibold">{t("resellers.offer")}</h3>
            <Checks
              label={t("packages.php")}
              hint={t("resellers.php_hint")}
              options={(php.data?.versions ?? []).map((v) => ({ value: v, label: `PHP ${v}` }))}
              value={f.php_versions}
              onChange={(v) => set("php_versions", v)}
            />
            <Checks
              label={t("packages.servers")}
              hint={t("resellers.servers_hint")}
              options={(servers.data ?? []).filter((s) => s.enrolled_at).map((s) => ({ value: s.id, label: s.name }))}
              value={f.server_ids}
              onChange={(v) => set("server_ids", v)}
            />
            {Number(f.max_apps) > 0 && (
              <label className="grid gap-1.5">
                <span className="text-sm font-medium">{t("packages.images")}</span>
                <textarea
                  value={f.app_images}
                  onChange={(e) => set("app_images", e.target.value)}
                  rows={3}
                  dir="ltr"
                  className="rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm text-ink"
                  placeholder={"nginx\nghost\nghcr.io/acme/"}
                />
                <span className="text-sm text-muted">{t("resellers.images_hint")}</span>
              </label>
            )}
            <div className="flex flex-wrap gap-x-5 gap-y-2">
              <label className="flex items-center gap-2 text-sm">
                <input type="checkbox" checked={f.sftp} onChange={(e) => set("sftp", e.target.checked)} className="accent-[var(--accent)]" />
                {t("packages.sftp")}
              </label>
              <label className="flex items-center gap-2 text-sm">
                <input type="checkbox" checked={f.ftp} onChange={(e) => set("ftp", e.target.checked)} className="accent-[var(--accent)]" />
                {t("packages.ftp")}
              </label>
              <label className="flex items-center gap-2 text-sm">
                <input type="checkbox" checked={f.git_apps} onChange={(e) => set("git_apps", e.target.checked)} className="accent-[var(--accent)]" />
                {t("packages.git_apps")}
              </label>
            </div>
            <label className="flex items-start gap-2 text-sm">
              <input type="checkbox" checked={f.white_label} onChange={(e) => set("white_label", e.target.checked)} className="mt-0.5 accent-[var(--accent)]" />
              <span>
                {t("resellers.white_label")}
                <span className="block text-muted">{t("resellers.white_label_hint")}</span>
              </span>
            </label>
          </section>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy}>
              {reseller ? t("sites.save") : t("resellers.create")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

// ---- The list ----

export function ResellersPage() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const resellers = useResellers();
  const [params, setParams] = useSearchParams();
  const adding = params.get("add") === "1";
  const [editing, setEditing] = useState<Reseller | null>(null);
  const [deleting, setDeleting] = useState<Reseller | null>(null);
  const [suspending, setSuspending] = useState<Reseller | null>(null);
  const [reset, setReset] = useState<{ email: string; password: string } | null>(null);
  const [error, setError] = useState("");
  const lng = i18n.language;

  const act = async (run: () => Promise<unknown>) => {
    setError("");
    try {
      await run();
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };
  const addButton = (
    <Button variant="primary" onClick={() => setParams({ add: "1" }, { replace: true })}>
      <Plus className="size-4" aria-hidden />
      {t("resellers.add")}
    </Button>
  );

  return (
    <>
      <PageHeader title={t("nav.resellers")} description={t("resellers.intro")} actions={resellers.data?.length ? addButton : undefined} />
      <div className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        {resellers.isPending && <Spinner />}
        {resellers.error && <ErrorAlert>{errorMessage(t, resellers.error)}</ErrorAlert>}
        {resellers.data?.length === 0 && <EmptyState icon={<Store />} title={t("resellers.empty.title")} body={t("resellers.empty.body")} action={addButton} />}
        <div className="grid gap-4 lg:grid-cols-2">
          {resellers.data?.map((r) => {
            const extra = [
              r.limits.memory_mb && t("customers.limit_memory", { size: formatBytes(r.limits.memory_mb * 1024 * 1024, lng) }),
              r.limits.cpu_percent && t("customers.limit_cpu", { count: r.limits.cpu_percent / 100, cores: formatNumber(r.limits.cpu_percent / 100, lng, 2) }),
            ].filter(Boolean);
            return (
              <Card key={r.id} className="grid gap-4 p-5">
                <div className="flex items-start gap-3">
                  <div className="min-w-0 flex-1">
                    <h2 className="truncate font-semibold">{r.name}</h2>
                    <p className="truncate text-sm text-muted" dir="ltr">
                      {r.email}
                    </p>
                  </div>
                  <span className="rounded bg-sunk px-2 py-0.5 text-xs text-muted">{t("resellers.packages_count", { count: r.packages, formatted: formatNumber(r.packages, lng) })}</span>
                </div>
                {(r.suspended_at || r.bandwidth_blocked || r.limits.white_label) && (
                  <div className="grid gap-1.5">
                    <div className="flex flex-wrap gap-2">
                      {r.suspended_at && <Badge tone="crit">{t("customers.suspended_badge", { date: new Date(r.suspended_at).toLocaleDateString(lng) })}</Badge>}
                      {r.bandwidth_blocked && <Badge tone="warn">{t("customers.bandwidth_blocked")}</Badge>}
                      {r.limits.white_label && <Badge tone="accent">{t("resellers.white_label")}</Badge>}
                    </div>
                    {r.suspended_at && r.suspend_reason && <p className="text-sm text-muted">{t("customers.suspend_reason_is", { reason: r.suspend_reason })}</p>}
                  </div>
                )}
                <ResellerUsage reseller={r} />
                {extra.length > 0 && <p className="-mt-2 text-sm text-muted">{t("resellers.per_package_short", { limits: extra.join(" · ") })}</p>}
                <div className="flex flex-wrap gap-2">
                  <Button
                    size="sm"
                    variant="primary"
                    onClick={() =>
                      act(async () => {
                        const u = await api.post<User>(`/resellers/${r.id}/impersonate`);
                        qc.clear();
                        qc.setQueryData(meKey, { ...u, impersonator_id: "admin" });
                        await qc.invalidateQueries({ queryKey: meKey });
                        navigate("/");
                      })
                    }
                  >
                    <LogIn className="size-4" aria-hidden />
                    {t("customers.sign_in_as")}
                  </Button>
                  <Link
                    to={`/customers?reseller=${r.id}`}
                    className="inline-flex h-8 items-center gap-2 rounded-lg border border-line bg-surface px-3 text-sm font-medium hover:bg-sunk"
                  >
                    <Users className="size-4" aria-hidden />
                    {t("resellers.customers")}
                  </Link>
                  <Button size="sm" onClick={() => setEditing(r)}>
                    <Pencil className="size-4" aria-hidden />
                    {t("resellers.edit")}
                  </Button>
                  <Button
                    size="sm"
                    onClick={() =>
                      act(async () => {
                        const res = await api.post<{ reseller: Reseller; password: string }>(`/resellers/${r.id}/password`);
                        setReset({ email: res.reseller.email, password: res.password });
                      })
                    }
                  >
                    <KeyRound className="size-4" aria-hidden />
                    {t("customers.reset_password")}
                  </Button>
                  {r.totp_enabled && (
                    <Button
                      size="sm"
                      onClick={() =>
                        window.confirm(t("customers.reset_2fa_confirm", { name: r.name })) &&
                        act(async () => {
                          await api.post(`/resellers/${r.id}/2fa/reset`);
                          qc.invalidateQueries({ queryKey: ["resellers"] });
                        })
                      }
                    >
                      <ShieldOff className="size-4" aria-hidden />
                      {t("customers.reset_2fa")}
                    </Button>
                  )}
                  {r.suspended_at ? (
                    <Button
                      size="sm"
                      onClick={() =>
                        act(async () => {
                          await api.post(`/resellers/${r.id}/unsuspend`);
                          qc.invalidateQueries({ queryKey: ["resellers"] });
                        })
                      }
                    >
                      <CircleCheck className="size-4" aria-hidden />
                      {t("customers.unsuspend")}
                    </Button>
                  ) : (
                    <Button size="sm" onClick={() => setSuspending(r)}>
                      <Ban className="size-4" aria-hidden />
                      {t("customers.suspend")}
                    </Button>
                  )}
                  <Button size="sm" variant="ghost" onClick={() => setDeleting(r)} aria-label={t("resellers.delete")}>
                    <Trash2 className="size-4" aria-hidden />
                  </Button>
                </div>
              </Card>
            );
          })}
        </div>
      </div>
      <ResellerDialog open={adding || editing !== null} reseller={editing} onClose={() => (setEditing(null), setParams({}, { replace: true }))} />
      {suspending && <SuspendDialog customer={suspending} kind="resellers" onClose={() => setSuspending(null)} />}
      <Dialog open={reset !== null} onClose={() => setReset(null)} title={t("customers.reset_password")}>
        {reset && <OneTimePassword {...reset} />}
        <div className="flex justify-end">
          <Button variant="primary" onClick={() => setReset(null)}>
            {t("databases.saved_it")}
          </Button>
        </div>
      </Dialog>
      <Dialog open={deleting !== null} onClose={() => setDeleting(null)} title={t("resellers.delete_title", { name: deleting?.name ?? "" })}>
        <p className="text-muted">{t("resellers.delete_body")}</p>
        {deleting && deleting.customers > 0 && (
          <p className="rounded-lg bg-warn-soft p-3 text-sm text-warn">
            {t("resellers.delete_has", { count: deleting.customers, formatted: formatNumber(deleting.customers, lng) })}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => setDeleting(null)}>
            {t("common.cancel")}
          </Button>
          <Button
            variant="danger"
            disabled={!!deleting && deleting.customers > 0}
            onClick={() =>
              act(async () => {
                const id = deleting!.id;
                setDeleting(null);
                await api.del(`/resellers/${id}`);
                qc.invalidateQueries({ queryKey: ["resellers"] });
                qc.invalidateQueries({ queryKey: ["packages"] });
              })
            }
          >
            {t("resellers.delete")}
          </Button>
        </div>
      </Dialog>
    </>
  );
}

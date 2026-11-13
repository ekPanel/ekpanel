import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowRightLeft, Ban, CircleCheck, KeyRound, LogIn, Package as PackageIcon, Pencil, Plus, ShieldOff, Trash2, UserRoundCog, Users } from "lucide-react";
import { api, ApiError, errorMessage, type App, type Customer, type Database, type MailDomain, type Package, type Reseller, type Server, type Site, type Usage, type User } from "../lib/api";
import type { HostedZone } from "../components/HostedDNS";
import { formatBytes, formatNumber } from "../lib/format";
import { meKey, useMe } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, Dialog, ErrorAlert, Field, Meter, Spinner } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { usePHPVersions } from "./SitesPage";

const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

export function useCustomers(enabled = true) {
  return useQuery({ queryKey: ["customers"], queryFn: () => api.get<Customer[]>("/customers"), enabled });
}

export function usePackages(enabled = true) {
  return useQuery({ queryKey: ["packages"], queryFn: () => api.get<Package[]>("/packages"), enabled });
}

export function useResellers(enabled = true) {
  return useQuery({ queryKey: ["resellers"], queryFn: () => api.get<Reseller[]>("/resellers"), enabled });
}

/** Who a new website, database or app is for. Administrators may keep it for themselves;
 * a reseller picks one of their customers; customers own what they create. */
export function OwnerField({ value, onChange }: { value: string; onChange: (id: string) => void }) {
  const { t } = useTranslation();
  const me = useMe();
  const isAdmin = me.data?.role === "admin";
  const isReseller = me.data?.role === "reseller";
  const customers = useCustomers(isAdmin || isReseller);
  const first = customers.data?.[0]?.id;
  useEffect(() => {
    if (isReseller && !value && first) onChange(first);
  }, [isReseller, value, first, onChange]);
  if (isReseller && customers.data?.length === 0) return <ErrorAlert>{t("customers.reseller_needs_customer")}</ErrorAlert>;
  if (!(isAdmin || isReseller) || !customers.data?.length) return null;
  return (
    <label className="grid gap-1.5">
      <span className="text-sm font-medium">{t("customers.owner")}</span>
      <select value={value} onChange={(e) => onChange(e.target.value)} className={select} required={isReseller}>
        {isAdmin && <option value="">{t("customers.owner_admins")}</option>}
        {customers.data.map((c) => (
          <option key={c.id} value={c.id}>
            {c.name} · {c.package_name}
          </option>
        ))}
      </select>
    </label>
  );
}

// ---- Usage ----

function UsageLine({ label, have, limit, text }: { label: string; have: number; limit: number | null; text: ReactNode }) {
  const percent = limit ? (have / limit) * 100 : 0;
  return (
    <div className="grid gap-1.5">
      <div className="flex justify-between gap-3 text-sm">
        <span className="font-medium">{label}</span>
        <span className="text-muted tabular-nums">{text}</span>
      </div>
      {limit !== null && <Meter percent={limit === 0 ? 100 : percent} label={label} />}
    </div>
  );
}

/** The limits a usage list compares with: a package's, or a reseller's totals. */
type UsageLimits = Pick<Package, "max_sites" | "max_databases" | "max_apps" | "max_cron_jobs" | "max_mail_domains" | "max_mailboxes" | "disk_mb" | "bandwidth_gb">;

export function UsageList({ usage, pkg, bandwidth }: { usage: Usage; pkg: UsageLimits; bandwidth?: number }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const n = (x: number) => formatNumber(x, lng);
  const count = (have: number, limit: number) => t("customers.of", { have: n(have), limit: n(limit) });
  const diskLimit = pkg.disk_mb === null ? null : pkg.disk_mb * 1024 * 1024;
  const bandwidthLimit = pkg.bandwidth_gb === null ? null : pkg.bandwidth_gb * 1024 ** 3;
  return (
    <div className="grid gap-4">
      <UsageLine label={t("nav.sites")} have={usage.sites} limit={pkg.max_sites} text={count(usage.sites, pkg.max_sites)} />
      <UsageLine label={t("nav.databases")} have={usage.databases} limit={pkg.max_databases} text={count(usage.databases, pkg.max_databases)} />
      {pkg.max_apps > 0 && <UsageLine label={t("nav.apps")} have={usage.apps} limit={pkg.max_apps} text={count(usage.apps, pkg.max_apps)} />}
      {pkg.max_mail_domains > 0 && (
        <UsageLine label={t("packages.mail_domains")} have={usage.mail_domains} limit={pkg.max_mail_domains} text={count(usage.mail_domains, pkg.max_mail_domains)} />
      )}
      {pkg.max_mail_domains > 0 && (
        <UsageLine label={t("packages.mailboxes")} have={usage.mailboxes} limit={pkg.max_mailboxes} text={count(usage.mailboxes, pkg.max_mailboxes)} />
      )}
      {pkg.max_cron_jobs > 0 && (
        <UsageLine label={t("cron.title")} have={usage.cron_jobs} limit={pkg.max_cron_jobs} text={count(usage.cron_jobs, pkg.max_cron_jobs)} />
      )}
      <UsageLine
        label={t("customers.disk")}
        have={usage.disk_bytes}
        limit={diskLimit}
        text={
          diskLimit === null
            ? t("customers.used_no_limit", { used: formatBytes(usage.disk_bytes, lng) })
            : t("customers.of", { have: formatBytes(usage.disk_bytes, lng), limit: formatBytes(diskLimit, lng) })
        }
      />
      {bandwidth !== undefined && (
        <UsageLine
          label={t("customers.bandwidth")}
          have={bandwidth}
          limit={bandwidthLimit}
          text={
            bandwidthLimit === null
              ? t("customers.used_no_limit", { used: formatBytes(bandwidth, lng) })
              : t("customers.of", { have: formatBytes(bandwidth, lng), limit: formatBytes(bandwidthLimit, lng) })
          }
        />
      )}
    </div>
  );
}

/** A reseller's limits and what all their customers have, on cards and their dashboard. */
export function ResellerUsage({ reseller }: { reseller: Reseller }) {
  const { t, i18n } = useTranslation();
  const n = (x: number) => formatNumber(x, i18n.language);
  const l = reseller.limits;
  return (
    <div className="grid gap-4">
      <UsageLine
        label={t("nav.customers")}
        have={reseller.customers}
        limit={l.max_customers}
        text={t("customers.of", { have: n(reseller.customers), limit: n(l.max_customers) })}
      />
      <UsageList usage={reseller.usage} pkg={l} bandwidth={reseller.bandwidth_month} />
    </div>
  );
}

/** A customer's own package and what they use of it, or a reseller's limits, on their dashboard. */
export function MyUsageCard() {
  const { t, i18n } = useTranslation();
  const q = useQuery({
    queryKey: ["me", "usage"],
    queryFn: () => api.get<{ package: Package | null; reseller?: Reseller; usage: Usage; bandwidth_month: number }>("/me/usage"),
    refetchInterval: 60000,
  });
  if (q.isPending) return <Spinner />;
  if (q.error) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;
  if (q.data.reseller) {
    const l = q.data.reseller.limits;
    return (
      <Card className="grid gap-5 p-6">
        <div className="grid gap-1">
          <h2 className="text-lg font-semibold">{t("resellers.your_account")}</h2>
          <p className="text-sm text-muted">{t("resellers.your_account_hint")}</p>
        </div>
        <ResellerUsage reseller={q.data.reseller} />
        {l.bandwidth_gb !== null && <p className="-mt-2 text-sm text-muted">{t(l.bandwidth_stop ? "resellers.bandwidth_stop_hint" : "customers.bandwidth_hint")}</p>}
      </Card>
    );
  }
  const pkg = q.data.package;
  if (!pkg) return null;
  const limits = [
    pkg.memory_mb && t("customers.limit_memory", { size: formatBytes(pkg.memory_mb * 1024 * 1024, i18n.language) }),
    pkg.cpu_percent && t("customers.limit_cpu", { count: pkg.cpu_percent / 100, cores: formatNumber(pkg.cpu_percent / 100, i18n.language, 2) }),
  ].filter(Boolean);
  return (
    <Card className="grid gap-5 p-6">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-lg font-semibold">{t("customers.your_package", { name: pkg.name })}</h2>
        {limits.length > 0 && <p className="text-sm text-muted">{limits.join(" · ")}</p>}
      </div>
      <UsageList usage={q.data.usage} pkg={pkg} bandwidth={q.data.bandwidth_month} />
      {pkg.bandwidth_gb !== null && <p className="-mt-2 text-sm text-muted">{t(pkg.bandwidth_stop ? "customers.bandwidth_stop_hint" : "customers.bandwidth_hint")}</p>}
    </Card>
  );
}

// ---- Signed in as a customer ----

export function ImpersonationBanner({ user }: { user: User }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  if (!user.impersonator_id) return null;
  return (
    <div className="flex flex-wrap items-center gap-3 bg-warn-soft px-4 py-2 text-sm text-warn sm:px-6" role="status">
      <LogIn className="size-4 shrink-0" aria-hidden />
      <span className="min-w-0 flex-1">{t("customers.acting_as", { name: user.name, email: user.email })}</span>
      <Button
        size="sm"
        loading={busy}
        onClick={async () => {
          setBusy(true);
          try {
            const back = await api.post<User>("/auth/impersonation/end");
            qc.clear();
            qc.setQueryData(meKey, back);
            navigate(user.role === "reseller" ? "/resellers" : "/customers");
          } catch {
            qc.setQueryData(meKey, null);
            navigate("/login");
          } finally {
            setBusy(false);
          }
        }}
      >
        {t("customers.back_to_admin")}
      </Button>
    </div>
  );
}

// ---- Customers (administrators) ----

export function OneTimePassword({ email, password }: { email: string; password: string }) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-2 rounded-lg bg-warn-soft p-4 text-warn">
      <p className="text-sm font-medium">{t("customers.password_once")}</p>
      <dl className="grid grid-cols-[auto_1fr] items-center gap-x-3 gap-y-1 text-sm text-ink">
        <dt className="text-muted">{t("account.email")}</dt>
        <dd dir="ltr">{email}</dd>
        <dt className="text-muted">{t("account.password")}</dt>
        <dd className="flex items-center gap-2">
          <code className="rounded bg-surface px-2 py-1 font-mono break-all" dir="ltr">
            {password}
          </code>
          <CopyButton text={password} />
        </dd>
      </dl>
    </div>
  );
}

function CustomerDialog({ open, customer, reseller: initialReseller, onClose }: { open: boolean; customer: Customer | null; reseller: string; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const me = useMe();
  const isAdmin = me.data?.role === "admin";
  const allPackages = usePackages(open);
  const resellers = useResellers(open && isAdmin);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [packageId, setPackageId] = useState("");
  const [reseller, setReseller] = useState("");
  // A customer's package is one of their reseller's (or the administrators', for theirs).
  const packages = { ...allPackages, data: allPackages.data?.filter((p) => !isAdmin || (p.reseller_id ?? "") === reseller) };
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<{ email: string; password: string } | null>(null);
  useEffect(() => {
    if (!open) return;
    setName(customer?.name ?? "");
    setEmail(customer?.email ?? "");
    setPackageId(customer?.package_id ?? "");
    setReseller(customer ? (customer.reseller_id ?? "") : initialReseller);
    setError("");
    setCreated(null);
  }, [open, customer, initialReseller]);
  const choices = packages.data;
  useEffect(() => {
    if (choices && !choices.some((p) => p.id === packageId)) setPackageId(choices[0]?.id ?? "");
  }, [choices, packageId]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body = { name, email, package_id: packageId, locale: i18n.language, ...(isAdmin ? { reseller_id: reseller } : {}) };
      if (customer) {
        await api.put(`/customers/${customer.id}`, body);
        onClose();
      } else {
        const r = await api.post<{ customer: Customer; password: string }>("/customers", body);
        setCreated({ email: r.customer.email, password: r.password });
      }
      qc.invalidateQueries({ queryKey: ["customers"] });
      qc.invalidateQueries({ queryKey: ["packages"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} title={customer ? t("customers.edit") : t("customers.add")}>
      {created ? (
        <div className="grid gap-4">
          <OneTimePassword {...created} />
          <p className="text-sm text-muted">{t("customers.share_hint")}</p>
          <div className="flex justify-end">
            <Button variant="primary" onClick={onClose}>
              {t("databases.saved_it")}
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-4">
          <ErrorAlert>{error}</ErrorAlert>
          {packages.data?.length === 0 && <ErrorAlert>{t("customers.needs_package")}</ErrorAlert>}
          <Field label={t("packages.name")} value={name} onChange={(e) => setName(e.target.value)} maxLength={100} required />
          <Field label={t("account.email")} type="email" value={email} onChange={(e) => setEmail(e.target.value)} dir="ltr" required />
          {isAdmin && !!resellers.data?.length && (
            <label className="grid gap-1.5">
              <span className="text-sm font-medium">{t("customers.reseller")}</span>
              <select value={reseller} onChange={(e) => setReseller(e.target.value)} className={select}>
                <option value="">{t("customers.reseller_none")}</option>
                {resellers.data.map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.name}
                  </option>
                ))}
              </select>
              <span className="text-sm text-muted">{t("customers.reseller_hint")}</span>
            </label>
          )}
          <label className="grid gap-1.5">
            <span className="text-sm font-medium">{t("customers.package")}</span>
            <select value={packageId} onChange={(e) => setPackageId(e.target.value)} className={select} required>
              {packages.data?.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </label>
          {!customer && <p className="text-sm text-muted">{t("customers.password_generated")}</p>}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!packageId}>
              {customer ? t("sites.save") : t("customers.create")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

export function CustomersPage() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const me = useMe();
  const isAdmin = me.data?.role === "admin";
  const allCustomers = useCustomers();
  const packages = usePackages();
  const resellers = useResellers(isAdmin);
  const [params, setParams] = useSearchParams();
  // Administrators can look at one reseller's customers (or only their own: "admins").
  const resellerFilter = isAdmin ? (params.get("reseller") ?? "") : "";
  const customers = {
    ...allCustomers,
    data: allCustomers.data?.filter((c) => !resellerFilter || (resellerFilter === "admins" ? !c.reseller_id : c.reseller_id === resellerFilter)),
  };
  const setFilter = (v: string) => setParams(v ? { reseller: v } : {}, { replace: true });
  const [editing, setEditing] = useState<Customer | null>(null);
  const adding = params.get("add") === "1";
  const [deleting, setDeleting] = useState<Customer | null>(null);
  const [reset, setReset] = useState<{ email: string; password: string } | null>(null);
  const [moving, setMoving] = useState<Customer | null>(null);
  const [suspending, setSuspending] = useState<Customer | null>(null);
  const [giving, setGiving] = useState<{ from: string } | null>(null);
  const [error, setError] = useState("");

  const act = async (run: () => Promise<unknown>) => {
    setError("");
    try {
      await run();
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };
  const addButton = (
    <Button variant="primary" onClick={() => setParams({ ...(resellerFilter ? { reseller: resellerFilter } : {}), add: "1" }, { replace: true })}>
      <Plus className="size-4" aria-hidden />
      {t("customers.add")}
    </Button>
  );
  const pkgOf = (c: Customer) => packages.data?.find((p) => p.id === c.package_id);
  const closeDialog = () => (setEditing(null), setParams(resellerFilter ? { reseller: resellerFilter } : {}, { replace: true }));

  return (
    <>
      <PageHeader
        title={t("nav.customers")}
        description={t("customers.intro")}
        actions={
          allCustomers.data?.length ? (
            <>
              <Button onClick={() => setGiving({ from: isAdmin ? "" : (customers.data?.[0]?.id ?? "") })}>
                <UserRoundCog className="size-4" aria-hidden />
                {t("owner.button")}
              </Button>
              {addButton}
            </>
          ) : undefined
        }
      />
      <div className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        {isAdmin && !!resellers.data?.length && (
          <label className="flex flex-wrap items-center gap-2 text-sm">
            <span className="font-medium">{t("customers.show")}</span>
            <select value={resellerFilter} onChange={(e) => setFilter(e.target.value)} className={select}>
              <option value="">{t("customers.show_all")}</option>
              <option value="admins">{t("customers.show_admins")}</option>
              {resellers.data.map((r) => (
                <option key={r.id} value={r.id}>
                  {t("customers.show_reseller", { name: r.name })}
                </option>
              ))}
            </select>
          </label>
        )}
        {customers.isPending && <Spinner />}
        {customers.error && <ErrorAlert>{errorMessage(t, customers.error)}</ErrorAlert>}
        {customers.data?.length === 0 && <EmptyState icon={<Users />} title={t("customers.empty.title")} body={t("customers.empty.body")} action={addButton} />}
        <div className="grid gap-4 lg:grid-cols-2">
          {customers.data?.map((c) => {
            const pkg = pkgOf(c);
            return (
              <Card key={c.id} className="grid gap-4 p-5">
                <div className="flex items-start gap-3">
                  <div className="min-w-0 flex-1">
                    <h2 className="truncate font-semibold">{c.name}</h2>
                    <p className="truncate text-sm text-muted" dir="ltr">
                      {c.email}
                    </p>
                  </div>
                  <span className="grid justify-items-end gap-1">
                    <span className="rounded bg-sunk px-2 py-0.5 text-xs text-muted">{c.package_name}</span>
                    {isAdmin && c.reseller_name && <span className="text-xs text-muted">{t("customers.of_reseller", { name: c.reseller_name })}</span>}
                  </span>
                </div>
                {(c.suspended_at || c.bandwidth_blocked || c.reseller_bandwidth_blocked) && (
                  <div className="grid gap-1.5">
                    <div className="flex flex-wrap gap-2">
                      {c.suspended_at && <Badge tone="crit">{t("customers.suspended_badge", { date: new Date(c.suspended_at).toLocaleDateString(i18n.language) })}</Badge>}
                      {c.bandwidth_blocked && <Badge tone="warn">{t("customers.bandwidth_blocked")}</Badge>}
                      {c.reseller_bandwidth_blocked && <Badge tone="warn">{t("customers.reseller_bandwidth_blocked")}</Badge>}
                    </div>
                    {c.suspended_at && c.suspend_reason && <p className="text-sm text-muted">{t("customers.suspend_reason_is", { reason: c.suspend_reason })}</p>}
                  </div>
                )}
                {pkg && <UsageList usage={c.usage} pkg={pkg} bandwidth={c.bandwidth_month} />}
                <div className="flex flex-wrap gap-2">
                  <Button
                    size="sm"
                    variant="primary"
                    onClick={() =>
                      act(async () => {
                        const u = await api.post<User>(`/customers/${c.id}/impersonate`);
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
                  <Button size="sm" onClick={() => setEditing(c)}>
                    <Pencil className="size-4" aria-hidden />
                    {t("customers.edit")}
                  </Button>
                  <Button
                    size="sm"
                    onClick={() =>
                      act(async () => {
                        const r = await api.post<{ customer: Customer; password: string }>(`/customers/${c.id}/password`);
                        setReset({ email: r.customer.email, password: r.password });
                      })
                    }
                  >
                    <KeyRound className="size-4" aria-hidden />
                    {t("customers.reset_password")}
                  </Button>
                  {c.totp_enabled && (
                    <Button
                      size="sm"
                      onClick={() =>
                        window.confirm(t("customers.reset_2fa_confirm", { name: c.name })) &&
                        act(async () => {
                          await api.post(`/customers/${c.id}/2fa/reset`);
                          qc.invalidateQueries({ queryKey: ["customers"] });
                        })
                      }
                    >
                      <ShieldOff className="size-4" aria-hidden />
                      {t("customers.reset_2fa")}
                    </Button>
                  )}
                  {isAdmin && (
                    <Button size="sm" onClick={() => setMoving(c)}>
                      <ArrowRightLeft className="size-4" aria-hidden />
                      {t("customers.move.button")}
                    </Button>
                  )}
                  <Button size="sm" onClick={() => setGiving({ from: c.id })}>
                    <UserRoundCog className="size-4" aria-hidden />
                    {t("owner.button")}
                  </Button>
                  {c.suspended_at ? (
                    <Button
                      size="sm"
                      onClick={() =>
                        act(async () => {
                          await api.post(`/customers/${c.id}/unsuspend`);
                          qc.invalidateQueries({ queryKey: ["customers"] });
                        })
                      }
                    >
                      <CircleCheck className="size-4" aria-hidden />
                      {t("customers.unsuspend")}
                    </Button>
                  ) : (
                    <Button size="sm" onClick={() => setSuspending(c)}>
                      <Ban className="size-4" aria-hidden />
                      {t("customers.suspend")}
                    </Button>
                  )}
                  <Button size="sm" variant="ghost" onClick={() => setDeleting(c)} aria-label={t("customers.delete")}>
                    <Trash2 className="size-4" aria-hidden />
                  </Button>
                </div>
              </Card>
            );
          })}
        </div>
      </div>
      <CustomerDialog
        open={adding || editing !== null}
        customer={editing}
        reseller={resellerFilter === "admins" ? "" : resellerFilter}
        onClose={closeDialog}
      />
      {moving && <MoveCustomerDialog customer={moving} onClose={() => setMoving(null)} />}
      {suspending && <SuspendDialog customer={suspending} onClose={() => setSuspending(null)} />}
      {giving && <OwnerChangeDialog from={giving.from} customers={allCustomers.data ?? []} onClose={() => setGiving(null)} />}
      <Dialog open={reset !== null} onClose={() => setReset(null)} title={t("customers.reset_password")}>
        {reset && <OneTimePassword {...reset} />}
        <div className="flex justify-end">
          <Button variant="primary" onClick={() => setReset(null)}>
            {t("databases.saved_it")}
          </Button>
        </div>
      </Dialog>
      <Dialog open={deleting !== null} onClose={() => setDeleting(null)} title={t("customers.delete_title", { name: deleting?.name ?? "" })}>
        <p className="text-muted">{t("customers.delete_body")}</p>
        {deleting && deleting.usage.sites + deleting.usage.databases + deleting.usage.apps > 0 && (
          <p className="rounded-lg bg-warn-soft p-3 text-sm text-warn">
            {t("customers.delete_has", {
              sites: formatNumber(deleting.usage.sites, i18n.language),
              databases: formatNumber(deleting.usage.databases, i18n.language),
              apps: formatNumber(deleting.usage.apps, i18n.language),
            })}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => setDeleting(null)}>
            {t("common.cancel")}
          </Button>
          <Button
            variant="danger"
            onClick={() =>
              act(async () => {
                const id = deleting!.id;
                setDeleting(null);
                await api.del(`/customers/${id}`);
                qc.invalidateQueries({ queryKey: ["customers"] });
                qc.invalidateQueries({ queryKey: ["packages"] });
              })
            }
          >
            {t("customers.delete")}
          </Button>
        </div>
      </Dialog>
    </>
  );
}

// ---- Packages (administrators) ----

interface PackageForm {
  name: string;
  max_sites: string;
  max_databases: string;
  max_apps: string;
  max_cron_jobs: string;
  disk_gb: string;
  memory_mb: string;
  cpu_cores: string;
  php_versions: string[];
  app_images: string;
  server_ids: string[];
  sftp: boolean;
  gitApps: boolean;
  ftp: boolean;
  max_mail_domains: string;
  max_mailboxes: string;
  mailbox_gb: string;
  bandwidth_gb: string;
  bandwidth_stop: boolean;
}

const newPackage: PackageForm = {
  name: "",
  max_sites: "5",
  max_databases: "5",
  max_apps: "0",
  max_cron_jobs: "5",
  disk_gb: "10",
  memory_mb: "1024",
  cpu_cores: "1",
  php_versions: [],
  app_images: "",
  server_ids: [],
  sftp: true,
  gitApps: false,
  ftp: false,
  max_mail_domains: "1",
  max_mailboxes: "10",
  mailbox_gb: "1",
  bandwidth_gb: "",
  bandwidth_stop: false,
};

function formFromPackage(p: Package): PackageForm {
  return {
    name: p.name,
    max_sites: String(p.max_sites),
    max_databases: String(p.max_databases),
    max_apps: String(p.max_apps),
    max_cron_jobs: String(p.max_cron_jobs),
    disk_gb: p.disk_mb === null ? "" : String(Math.round((p.disk_mb / 1024) * 100) / 100),
    memory_mb: p.memory_mb === null ? "" : String(p.memory_mb),
    cpu_cores: p.cpu_percent === null ? "" : String(p.cpu_percent / 100),
    php_versions: p.php_versions,
    app_images: p.app_images.join("\n"),
    server_ids: p.server_ids,
    sftp: p.sftp,
    gitApps: p.git_apps,
    ftp: p.ftp,
    max_mail_domains: String(p.max_mail_domains),
    max_mailboxes: String(p.max_mailboxes),
    mailbox_gb: p.mailbox_quota_mb === null ? "" : String(Math.round((p.mailbox_quota_mb / 1024) * 100) / 100),
    bandwidth_gb: p.bandwidth_gb === null ? "" : String(p.bandwidth_gb),
    bandwidth_stop: p.bandwidth_stop,
  };
}

export function Checks({
  label,
  hint,
  options,
  value,
  onChange,
}: {
  label: string;
  hint: string;
  options: { value: string; label: string }[];
  value: string[];
  onChange: (v: string[]) => void;
}) {
  return (
    <fieldset className="grid gap-1.5">
      <legend className="mb-1 text-sm font-medium">{label}</legend>
      <div className="flex flex-wrap gap-x-4 gap-y-2">
        {options.map((o) => (
          <label key={o.value} className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={value.includes(o.value)}
              onChange={(e) => onChange(e.target.checked ? [...value, o.value] : value.filter((v) => v !== o.value))}
              className="accent-[var(--accent)]"
            />
            {o.label}
          </label>
        ))}
      </div>
      <span className="text-sm text-muted">{hint}</span>
    </fieldset>
  );
}

function PackageDialog({ open, pkg, onClose }: { open: boolean; pkg: Package | null; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const php = usePHPVersions();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers"), enabled: open });
  const [f, setF] = useState<PackageForm>(newPackage);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (open) {
      setF(pkg ? formFromPackage(pkg) : newPackage);
      setError("");
    }
  }, [open, pkg]);
  const set = <K extends keyof PackageForm>(k: K, v: PackageForm[K]) => setF((x) => ({ ...x, [k]: v }));
  const digits =
    (k: keyof PackageForm, decimal = false) =>
    (e: React.ChangeEvent<HTMLInputElement>) =>
      set(k, e.target.value.replace(decimal ? /[^\d.]/g : /\D/g, "") as never);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const opt = (s: string, scale = 1) => (s.trim() ? Math.round(Number(s) * scale) : null);
    const body = {
      name: f.name,
      max_sites: Number(f.max_sites || 0),
      max_databases: Number(f.max_databases || 0),
      max_apps: Number(f.max_apps || 0),
      max_cron_jobs: Number(f.max_cron_jobs || 0),
      disk_mb: opt(f.disk_gb, 1024),
      memory_mb: opt(f.memory_mb),
      cpu_percent: opt(f.cpu_cores, 100),
      php_versions: f.php_versions,
      app_images: f.app_images
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean),
      server_ids: f.server_ids,
      sftp: f.sftp,
      git_apps: f.gitApps,
      ftp: f.ftp,
      max_mail_domains: Number(f.max_mail_domains || 0),
      max_mailboxes: Number(f.max_mailboxes || 0),
      mailbox_quota_mb: opt(f.mailbox_gb, 1024),
      bandwidth_gb: opt(f.bandwidth_gb),
      bandwidth_stop: f.bandwidth_stop,
    };
    try {
      if (pkg) await api.put(`/packages/${pkg.id}`, body);
      else await api.post("/packages", body);
      qc.invalidateQueries({ queryKey: ["packages"] });
      qc.invalidateQueries({ queryKey: ["customers"] });
      onClose();
    } catch (err) {
      if (err instanceof ApiError && err.code === "package_over_reseller") {
        // Name the part of the form that gives more than the reseller has.
        const labels: Record<string, string> = {
          max_sites: t("nav.sites"),
          max_databases: t("nav.databases"),
          max_apps: t("nav.apps"),
          max_cron_jobs: t("cron.title"),
          max_mail_domains: t("packages.mail_domains"),
          max_mailboxes: t("packages.mailboxes"),
          disk_mb: t("packages.disk"),
          bandwidth_gb: t("packages.bandwidth"),
          memory_mb: t("packages.memory"),
          cpu_percent: t("packages.cpu"),
          mailbox_quota_mb: t("packages.mailbox_size"),
          php_versions: t("packages.php"),
          server_ids: t("packages.servers"),
          app_images: t("packages.images"),
          sftp: t("packages.sftp"),
          ftp: t("packages.ftp"),
          git_apps: t("packages.git_apps"),
        };
        setError(t("errors.package_over_reseller", { field: labels[String(err.data.field)] ?? String(err.data.field) }));
        return;
      }
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onClose={onClose} title={pkg ? t("packages.edit") : t("packages.add")} wide>
      <form onSubmit={submit} className="grid gap-5">
        <ErrorAlert>{error}</ErrorAlert>
        <Field label={t("packages.name")} value={f.name} onChange={(e) => set("name", e.target.value)} maxLength={60} required />
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <Field label={t("nav.sites")} value={f.max_sites} onChange={digits("max_sites")} inputMode="numeric" dir="ltr" required />
          <Field label={t("nav.databases")} value={f.max_databases} onChange={digits("max_databases")} inputMode="numeric" dir="ltr" required />
          <Field label={t("nav.apps")} value={f.max_apps} onChange={digits("max_apps")} inputMode="numeric" dir="ltr" required />
          <Field label={t("cron.title")} value={f.max_cron_jobs} onChange={digits("max_cron_jobs")} inputMode="numeric" dir="ltr" required />
        </div>
        <p className="-mt-3 text-sm text-muted">{t("packages.counts_hint")}</p>
        <div className="grid gap-3 sm:grid-cols-3">
          <Field label={t("packages.mail_domains")} value={f.max_mail_domains} onChange={digits("max_mail_domains")} inputMode="numeric" dir="ltr" required />
          <Field label={t("packages.mailboxes")} value={f.max_mailboxes} onChange={digits("max_mailboxes")} inputMode="numeric" dir="ltr" required />
          <Field label={t("packages.mailbox_size")} value={f.mailbox_gb} onChange={digits("mailbox_gb", true)} inputMode="decimal" dir="ltr" />
        </div>
        <p className="-mt-3 text-sm text-muted">{t("packages.mailbox_size_hint")}</p>
        <div className="grid gap-3 sm:grid-cols-3">
          <Field
            label={t("packages.disk")}
            value={f.disk_gb}
            onChange={digits("disk_gb", true)}
            inputMode="decimal"
            dir="ltr"
            placeholder={t("apps.no_limit")}
          />
          <Field
            label={t("packages.memory")}
            value={f.memory_mb}
            onChange={digits("memory_mb")}
            inputMode="numeric"
            dir="ltr"
            placeholder={t("apps.no_limit")}
          />
          <Field
            label={t("packages.cpu")}
            value={f.cpu_cores}
            onChange={digits("cpu_cores", true)}
            inputMode="decimal"
            dir="ltr"
            placeholder={t("apps.no_limit")}
          />
        </div>
        <p className="-mt-3 text-sm text-muted">{t("packages.limits_hint")}</p>
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
            <span>{t("packages.bandwidth_stop")}</span>
          </label>
        </div>
        <p className="-mt-3 text-sm text-muted">{t("packages.bandwidth_hint")}</p>
        <Checks
          label={t("packages.php")}
          hint={t("packages.php_hint")}
          options={(php.data?.versions ?? []).map((v) => ({ value: v, label: `PHP ${v}` }))}
          value={f.php_versions}
          onChange={(v) => set("php_versions", v)}
        />
        <Checks
          label={t("packages.servers")}
          hint={t("packages.servers_hint")}
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
            <span className="text-sm text-muted">{t("packages.images_hint")}</span>
          </label>
        )}
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={f.sftp} onChange={(e) => set("sftp", e.target.checked)} className="accent-[var(--accent)]" />
          {t("packages.sftp")}
        </label>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={f.ftp} onChange={(e) => set("ftp", e.target.checked)} className="accent-[var(--accent)]" />
          {t("packages.ftp")}
        </label>
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" checked={f.gitApps} onChange={(e) => set("gitApps", e.target.checked)} className="mt-1 accent-[var(--accent)]" />
          <span>
            <span className="font-medium">{t("packages.git_apps")}</span>
            <span className="block text-muted">{t("packages.git_apps_hint")}</span>
          </span>
        </label>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy}>
            {pkg ? t("sites.save") : t("packages.create")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

export function PackagesPage() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const me = useMe();
  const isAdmin = me.data?.role === "admin";
  const packages = usePackages();
  const resellers = useResellers(isAdmin);
  const ownerOf = (p: Package) => (p.reseller_id ? (resellers.data?.find((r) => r.id === p.reseller_id)?.name ?? "") : t("packages.whose_yours"));
  const showOwner = isAdmin && !!packages.data?.some((p) => p.reseller_id);
  const [params, setParams] = useSearchParams();
  const [editing, setEditing] = useState<Package | null>(null);
  const [error, setError] = useState("");
  const adding = params.get("add") === "1";
  const lng = i18n.language;
  const n = (x: number) => formatNumber(x, lng);
  const addButton = (
    <Button variant="primary" onClick={() => setParams({ add: "1" }, { replace: true })}>
      <Plus className="size-4" aria-hidden />
      {t("packages.add")}
    </Button>
  );
  return (
    <>
      <PageHeader title={t("nav.packages")} description={t("packages.intro")} actions={packages.data?.length ? addButton : undefined} />
      <div className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        {packages.isPending && <Spinner />}
        {packages.data?.length === 0 && (
          <EmptyState icon={<PackageIcon />} title={t("packages.empty.title")} body={t("packages.empty.body")} action={addButton} />
        )}
        {!!packages.data?.length && (
          <Card className="overflow-x-auto">
            <table className="w-full min-w-[720px] text-sm">
              <thead>
                <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                  <th className="px-4 py-3 text-start font-semibold">{t("packages.name")}</th>
                  {showOwner && <th className="px-4 py-3 text-start font-semibold">{t("packages.whose")}</th>}
                  <th className="px-4 py-3 text-start font-semibold">{t("packages.includes")}</th>
                  <th className="px-4 py-3 text-start font-semibold">{t("packages.resources")}</th>
                  <th className="px-4 py-3 text-start font-semibold">{t("nav.customers")}</th>
                  <th className="px-4 py-3" />
                </tr>
              </thead>
              <tbody>
                {packages.data.map((p) => (
                  <tr key={p.id} className="border-b border-line last:border-0 hover:bg-sunk/60">
                    <td className="px-4 py-3 font-semibold">{p.name}</td>
                    {showOwner && <td className="px-4 py-3 text-muted">{ownerOf(p)}</td>}
                    <td className="px-4 py-3 text-muted">
                      {t("packages.includes_text", { sites: n(p.max_sites), databases: n(p.max_databases), apps: n(p.max_apps), cron: n(p.max_cron_jobs) })}
                    </td>
                    <td className="px-4 py-3 text-muted">
                      {[
                        p.disk_mb === null ? t("packages.disk_unlimited") : formatBytes(p.disk_mb * 1024 * 1024, lng),
                        p.memory_mb && t("customers.limit_memory", { size: formatBytes(p.memory_mb * 1024 * 1024, lng) }),
                        p.cpu_percent && t("customers.limit_cpu", { count: p.cpu_percent / 100, cores: formatNumber(p.cpu_percent / 100, lng, 2) }),
                        p.bandwidth_gb !== null && t("packages.bandwidth_month", { size: formatBytes(p.bandwidth_gb * 1024 ** 3, lng) }),
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </td>
                    <td className="px-4 py-3 tabular-nums">{n(p.customers)}</td>
                    <td className="px-4 py-3 text-end whitespace-nowrap">
                      <Button size="sm" variant="ghost" onClick={() => setEditing(p)} aria-label={t("packages.edit")}>
                        <Pencil className="size-4" aria-hidden />
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={t("packages.delete")}
                        onClick={async () => {
                          setError("");
                          try {
                            await api.del(`/packages/${p.id}`);
                            qc.invalidateQueries({ queryKey: ["packages"] });
                          } catch (err) {
                            setError(errorMessage(t, err));
                          }
                        }}
                      >
                        <Trash2 className="size-4" aria-hidden />
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Card>
        )}
      </div>
      <PackageDialog open={adding || editing !== null} pkg={editing} onClose={() => (setEditing(null), setParams({}, { replace: true }))} />
    </>
  );
}

/** Moves everything a customer has on one server to another. */
function MoveCustomerDialog({ customer, onClose }: { customer: Customer; onClose: () => void }) {
  const { t } = useTranslation();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers") });
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [started, setStarted] = useState(false);
  const list = (servers.data ?? []).filter((s) => s.enrolled_at);
  const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.post(`/customers/${customer.id}/move`, { from_server_id: from, server_id: to });
      setStarted(true);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open onClose={onClose} title={t("customers.move.title", { name: customer.name })}>
      {started ? (
        <div className="grid gap-4">
          <p className="text-sm">{t("customers.move.started")}</p>
          <Button variant="primary" onClick={onClose} className="justify-self-end">
            {t("common.close", { defaultValue: "OK" })}
          </Button>
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-4">
          {error && <ErrorAlert>{error}</ErrorAlert>}
          <p className="text-sm text-muted">{t("customers.move.intro")}</p>
          <label className="grid gap-1.5 text-sm">
            <span className="font-medium">{t("customers.move.from")}</span>
            <select className={select} value={from} onChange={(e) => setFrom(e.target.value)} required>
              <option value="">{t("move.choose_server")}</option>
              {list.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name}
                </option>
              ))}
            </select>
          </label>
          <label className="grid gap-1.5 text-sm">
            <span className="font-medium">{t("move.target")}</span>
            <select className={select} value={to} onChange={(e) => setTo(e.target.value)} required>
              <option value="">{t("move.choose_server")}</option>
              {list
                .filter((s) => s.id !== from)
                .map((s) => (
                  <option key={s.id} value={s.id} disabled={!s.online}>
                    {s.name}
                    {!s.online ? ` (${t("servers.state.offline")})` : ""}
                  </option>
                ))}
            </select>
          </label>
          <p className="rounded-lg bg-warn-soft px-3 py-2 text-sm text-warn">{t("customers.move.warning")}</p>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!from || !to}>
              <ArrowRightLeft className="size-4" aria-hidden /> {t("move.confirm")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

/** Suspends a customer (or, for administrators, a reseller with all their customers), with
 * a reason the account does not see. */
export function SuspendDialog({ customer, kind = "customers", onClose }: { customer: { id: string; name: string }; kind?: "customers" | "resellers"; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.post(`/${kind}/${customer.id}/suspend`, { reason });
      qc.invalidateQueries({ queryKey: [kind] });
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} title={t("customers.suspend_title", { name: customer.name })}>
      <form onSubmit={submit} className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        <p className="text-sm text-muted">{t(kind === "resellers" ? "resellers.suspend_body" : "customers.suspend_body")}</p>
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("customers.suspend_reason")}</span>
          <textarea
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            maxLength={500}
            rows={3}
            className="rounded-lg border border-line bg-surface px-3 py-2 text-sm text-ink"
          />
          <span className="text-sm text-muted">{t("customers.suspend_reason_hint")}</span>
        </label>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="danger" loading={busy}>
            <Ban className="size-4" aria-hidden />
            {t("customers.suspend")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

type OwnerKind = "sites" | "databases" | "apps" | "mail_domains" | "zones";

/** Gives websites, databases, apps, email domains and DNS zones to another owner. */
function OwnerChangeDialog({ from: initialFrom, customers, onClose }: { from: string; customers: Customer[]; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const me = useMe();
  // A reseller moves things only between their own customers.
  const isAdmin = me.data?.role === "admin";
  const [from, setFrom] = useState(initialFrom);
  const [to, setTo] = useState(isAdmin ? "" : (customers.find((c) => c.id !== initialFrom)?.id ?? ""));
  const [chosen, setChosen] = useState<Record<OwnerKind, string[]>>({ sites: [], databases: [], apps: [], mail_domains: [], zones: [] });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [done, setDone] = useState(0);
  const sites = useQuery({ queryKey: ["sites"], queryFn: () => api.get<Site[]>("/sites") });
  const dbs = useQuery({ queryKey: ["databases"], queryFn: () => api.get<Database[]>("/databases") });
  const apps = useQuery({ queryKey: ["apps"], queryFn: () => api.get<App[]>("/apps") });
  const mail = useQuery({ queryKey: ["mail", "domains"], queryFn: () => api.get<MailDomain[]>("/mail/domains") });
  const zones = useQuery({ queryKey: ["dns", "hosted"], queryFn: () => api.get<HostedZone[]>("/dns/hosted") });
  const owned = (o: { owner_id: string | null }) => (o.owner_id ?? "") === from;
  const groups: { kind: OwnerKind; label: string; items: { id: string; name: string }[] }[] = [
    { kind: "sites", label: t("nav.sites"), items: (sites.data ?? []).filter((x) => owned(x) && x.state !== "deleting").map((x) => ({ id: x.id, name: x.domain })) },
    { kind: "databases", label: t("nav.databases"), items: (dbs.data ?? []).filter((x) => owned(x) && x.state !== "deleting").map((x) => ({ id: x.id, name: x.name })) },
    { kind: "apps", label: t("nav.apps"), items: (apps.data ?? []).filter((x) => owned(x) && x.state !== "deleting").map((x) => ({ id: x.id, name: x.name })) },
    { kind: "mail_domains", label: t("owner.mail_domains"), items: (mail.data ?? []).filter((x) => owned(x) && x.state !== "deleting").map((x) => ({ id: x.id, name: x.domain })) },
    { kind: "zones", label: t("owner.zones"), items: (zones.data ?? []).filter(owned).map((x) => ({ id: x.id, name: x.name })) },
  ];
  const loading = [sites, dbs, apps, mail, zones].some((q) => q.isPending);
  const count = Object.values(chosen).reduce((n, l) => n + l.length, 0);
  const toggle = (kind: OwnerKind, id: string, on: boolean) =>
    setChosen((c) => ({ ...c, [kind]: on ? [...c[kind], id] : c[kind].filter((x) => x !== id) }));
  const ownerName = (id: string) => (id ? (customers.find((c) => c.id === id)?.name ?? "") : t("customers.owner_admins"));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const r = await api.post<{ moved: number }>("/owner-changes", { to, ...chosen });
      setDone(r.moved);
      for (const key of [["customers"], ["sites"], ["databases"], ["apps"], ["mail"], ["dns"], ["cron"]]) qc.invalidateQueries({ queryKey: key });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open onClose={onClose} title={t("owner.title")} wide>
      {done > 0 ? (
        <div className="grid gap-4">
          <p className="text-sm">{t("owner.done", { count: done, name: ownerName(to) })}</p>
          <Button variant="primary" onClick={onClose} className="justify-self-end">
            {t("common.close", { defaultValue: "OK" })}
          </Button>
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-4">
          <ErrorAlert>{error}</ErrorAlert>
          <p className="text-sm text-muted">{t("owner.intro")}</p>
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="grid gap-1.5">
              <span className="text-sm font-medium">{t("owner.from")}</span>
              <select
                value={from}
                onChange={(e) => (setFrom(e.target.value), setChosen({ sites: [], databases: [], apps: [], mail_domains: [], zones: [] }))}
                className={select}
              >
                {isAdmin && <option value="">{t("customers.owner_admins")}</option>}
                {customers.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name}
                  </option>
                ))}
              </select>
            </label>
            <label className="grid gap-1.5">
              <span className="text-sm font-medium">{t("owner.to")}</span>
              <select value={to} onChange={(e) => setTo(e.target.value)} className={select}>
                {isAdmin && <option value="">{t("customers.owner_admins")}</option>}
                {customers.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name} · {c.package_name}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {loading ? (
            <Spinner />
          ) : groups.every((g) => g.items.length === 0) ? (
            <p className="rounded-lg bg-sunk p-3 text-sm text-muted">{t("owner.nothing", { name: ownerName(from) })}</p>
          ) : (
            <div className="grid max-h-80 gap-4 overflow-y-auto rounded-lg border border-line p-4">
              {groups
                .filter((g) => g.items.length > 0)
                .map((g) => (
                  <fieldset key={g.kind} className="grid gap-1.5">
                    <legend className="mb-1 text-sm font-medium">{g.label}</legend>
                    {g.items.map((it) => (
                      <label key={it.id} className="flex items-center gap-2 text-sm">
                        <input
                          type="checkbox"
                          checked={chosen[g.kind].includes(it.id)}
                          onChange={(e) => toggle(g.kind, it.id, e.target.checked)}
                          className="accent-[var(--accent)]"
                        />
                        <span className="truncate" dir="ltr">
                          {it.name}
                        </span>
                      </label>
                    ))}
                  </fieldset>
                ))}
            </div>
          )}
          <p className="text-sm text-muted">{t("owner.hint")}</p>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={count === 0 || from === to}>
              <UserRoundCog className="size-4" aria-hidden />
              {t("owner.confirm", { count })}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, LogIn, Package as PackageIcon, Pencil, Plus, Trash2, Users } from "lucide-react";
import { api, errorMessage, type Customer, type Package, type Server, type Usage, type User } from "../lib/api";
import { formatBytes, formatNumber } from "../lib/format";
import { meKey, useMe } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { Button, Card, CopyButton, Dialog, ErrorAlert, Field, Meter, Spinner } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { usePHPVersions } from "./SitesPage";

const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

export function useCustomers(enabled = true) {
  return useQuery({ queryKey: ["customers"], queryFn: () => api.get<Customer[]>("/customers"), enabled });
}

export function usePackages(enabled = true) {
  return useQuery({ queryKey: ["packages"], queryFn: () => api.get<Package[]>("/packages"), enabled });
}

/** For administrators: who a new website, database or app is for. Customers own what they create. */
export function OwnerField({ value, onChange }: { value: string; onChange: (id: string) => void }) {
  const { t } = useTranslation();
  const me = useMe();
  const isAdmin = me.data?.role === "admin";
  const customers = useCustomers(isAdmin);
  if (!isAdmin || !customers.data?.length) return null;
  return (
    <label className="grid gap-1.5">
      <span className="text-sm font-medium">{t("customers.owner")}</span>
      <select value={value} onChange={(e) => onChange(e.target.value)} className={select}>
        <option value="">{t("customers.owner_admins")}</option>
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

export function UsageList({ usage, pkg }: { usage: Usage; pkg: Package }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const n = (x: number) => formatNumber(x, lng);
  const count = (have: number, limit: number) => t("customers.of", { have: n(have), limit: n(limit) });
  const diskLimit = pkg.disk_mb === null ? null : pkg.disk_mb * 1024 * 1024;
  return (
    <div className="grid gap-4">
      <UsageLine label={t("nav.sites")} have={usage.sites} limit={pkg.max_sites} text={count(usage.sites, pkg.max_sites)} />
      <UsageLine label={t("nav.databases")} have={usage.databases} limit={pkg.max_databases} text={count(usage.databases, pkg.max_databases)} />
      {pkg.max_apps > 0 && <UsageLine label={t("nav.apps")} have={usage.apps} limit={pkg.max_apps} text={count(usage.apps, pkg.max_apps)} />}
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
    </div>
  );
}

/** A customer's own package and what they use of it, on their dashboard. */
export function MyUsageCard() {
  const { t, i18n } = useTranslation();
  const q = useQuery({ queryKey: ["me", "usage"], queryFn: () => api.get<{ package: Package | null; usage: Usage }>("/me/usage"), refetchInterval: 60000 });
  if (q.isPending) return <Spinner />;
  if (q.error) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;
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
      <UsageList usage={q.data.usage} pkg={pkg} />
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
            const admin = await api.post<User>("/auth/impersonation/end");
            qc.clear();
            qc.setQueryData(meKey, admin);
            navigate("/customers");
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

function OneTimePassword({ email, password }: { email: string; password: string }) {
  const { t } = useTranslation();
  return (
    <div className="grid gap-2 rounded-lg bg-warn-soft p-4 text-warn">
      <p className="text-sm font-medium">{t("customers.password_once")}</p>
      <dl className="grid grid-cols-[auto_1fr] items-center gap-x-3 gap-y-1 text-sm text-ink">
        <dt className="text-muted">{t("login.email")}</dt>
        <dd dir="ltr">{email}</dd>
        <dt className="text-muted">{t("login.password")}</dt>
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

function CustomerDialog({ open, customer, onClose }: { open: boolean; customer: Customer | null; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const packages = usePackages(open);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [packageId, setPackageId] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<{ email: string; password: string } | null>(null);
  useEffect(() => {
    if (!open) return;
    setName(customer?.name ?? "");
    setEmail(customer?.email ?? "");
    setPackageId(customer?.package_id ?? "");
    setError("");
    setCreated(null);
  }, [open, customer]);
  useEffect(() => {
    if (!packageId && packages.data?.length) setPackageId(packages.data[0].id);
  }, [packages.data, packageId]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body = { name, email, package_id: packageId, locale: i18n.language };
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
          <Field label={t("setup.name")} value={name} onChange={(e) => setName(e.target.value)} maxLength={100} required />
          <Field label={t("login.email")} type="email" value={email} onChange={(e) => setEmail(e.target.value)} dir="ltr" required />
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
  const customers = useCustomers();
  const packages = usePackages();
  const [params, setParams] = useSearchParams();
  const [editing, setEditing] = useState<Customer | null>(null);
  const adding = params.get("add") === "1";
  const [deleting, setDeleting] = useState<Customer | null>(null);
  const [reset, setReset] = useState<{ email: string; password: string } | null>(null);
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
    <Button variant="primary" onClick={() => setParams({ add: "1" }, { replace: true })}>
      <Plus className="size-4" aria-hidden />
      {t("customers.add")}
    </Button>
  );
  const pkgOf = (c: Customer) => packages.data?.find((p) => p.id === c.package_id);

  return (
    <>
      <PageHeader title={t("nav.customers")} description={t("customers.intro")} actions={customers.data?.length ? addButton : undefined} />
      <div className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
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
                  <span className="rounded bg-sunk px-2 py-0.5 text-xs text-muted">{c.package_name}</span>
                </div>
                {pkg && <UsageList usage={c.usage} pkg={pkg} />}
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
                  <Button size="sm" variant="ghost" onClick={() => setDeleting(c)} aria-label={t("customers.delete")}>
                    <Trash2 className="size-4" aria-hidden />
                  </Button>
                </div>
              </Card>
            );
          })}
        </div>
      </div>
      <CustomerDialog open={adding || editing !== null} customer={editing} onClose={() => (setEditing(null), setParams({}, { replace: true }))} />
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
  disk_gb: string;
  memory_mb: string;
  cpu_cores: string;
  php_versions: string[];
  app_images: string;
  server_ids: string[];
  sftp: boolean;
}

const newPackage: PackageForm = {
  name: "",
  max_sites: "5",
  max_databases: "5",
  max_apps: "0",
  disk_gb: "10",
  memory_mb: "1024",
  cpu_cores: "1",
  php_versions: [],
  app_images: "",
  server_ids: [],
  sftp: true,
};

function formFromPackage(p: Package): PackageForm {
  return {
    name: p.name,
    max_sites: String(p.max_sites),
    max_databases: String(p.max_databases),
    max_apps: String(p.max_apps),
    disk_gb: p.disk_mb === null ? "" : String(Math.round((p.disk_mb / 1024) * 100) / 100),
    memory_mb: p.memory_mb === null ? "" : String(p.memory_mb),
    cpu_cores: p.cpu_percent === null ? "" : String(p.cpu_percent / 100),
    php_versions: p.php_versions,
    app_images: p.app_images.join("\n"),
    server_ids: p.server_ids,
    sftp: p.sftp,
  };
}

function Checks({
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
    };
    try {
      if (pkg) await api.put(`/packages/${pkg.id}`, body);
      else await api.post("/packages", body);
      qc.invalidateQueries({ queryKey: ["packages"] });
      qc.invalidateQueries({ queryKey: ["customers"] });
      onClose();
    } catch (err) {
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
        <div className="grid grid-cols-3 gap-3">
          <Field label={t("nav.sites")} value={f.max_sites} onChange={digits("max_sites")} inputMode="numeric" dir="ltr" required />
          <Field label={t("nav.databases")} value={f.max_databases} onChange={digits("max_databases")} inputMode="numeric" dir="ltr" required />
          <Field label={t("nav.apps")} value={f.max_apps} onChange={digits("max_apps")} inputMode="numeric" dir="ltr" required />
        </div>
        <p className="-mt-3 text-sm text-muted">{t("packages.counts_hint")}</p>
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
  const packages = usePackages();
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
                    <td className="px-4 py-3 text-muted">
                      {t("packages.includes_text", { sites: n(p.max_sites), databases: n(p.max_databases), apps: n(p.max_apps) })}
                    </td>
                    <td className="px-4 py-3 text-muted">
                      {[
                        p.disk_mb === null ? t("packages.disk_unlimited") : formatBytes(p.disk_mb * 1024 * 1024, lng),
                        p.memory_mb && t("customers.limit_memory", { size: formatBytes(p.memory_mb * 1024 * 1024, lng) }),
                        p.cpu_percent && t("customers.limit_cpu", { count: p.cpu_percent / 100, cores: formatNumber(p.cpu_percent / 100, lng, 2) }),
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

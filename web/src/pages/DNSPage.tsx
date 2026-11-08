import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CloudCog, ExternalLink, Plus, Trash2 } from "lucide-react";
import { api, errorMessage, type CloudflareAccount, type DNSZone } from "../lib/api";
import { formatRelative } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, Dialog, ErrorAlert, Field, Spinner } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { HostedDNS } from "../components/HostedDNS";

const tokenURL = "https://dash.cloudflare.com/profile/api-tokens";

export function useCFAccounts() {
  return useQuery({ queryKey: ["cf-accounts"], queryFn: () => api.get<CloudflareAccount[]>("/dns/cloudflare") });
}

export function useZones() {
  return useQuery({
    queryKey: ["dns-zones"],
    queryFn: () => api.get<DNSZone[]>("/dns/zones"),
    refetchInterval: 30000,
  });
}

/** How to make a Cloudflare token, and the form to connect one. */
function ConnectCloudflare({ onDone }: { onDone?: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.post("/dns/cloudflare", { name, token });
      setToken("");
      setName("");
      qc.invalidateQueries({ queryKey: ["cf-accounts"] });
      onDone?.();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit} className="grid gap-4">
      <ol className="grid list-decimal gap-1.5 ps-5 text-sm">
        <li>
          {t("dns.connect.step1")}{" "}
          <a href={tokenURL} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 font-medium text-accent hover:underline">
            {t("dns.connect.open")}
            <ExternalLink className="size-3.5" aria-hidden />
          </a>
        </li>
        <li>{t("dns.connect.step2")}</li>
        <li>
          {t("dns.connect.step3")}
          <ul className="mt-1 grid gap-0.5 font-mono text-xs text-muted" dir="ltr">
            <li>Zone · DNS · Edit</li>
            <li>Zone · Zone Settings · Edit</li>
            <li>Zone · Cache Purge · Purge</li>
            <li>Zone · Zone · Edit</li>
            <li>Account · Account Settings · Read</li>
          </ul>
        </li>
        <li>{t("dns.connect.step4")}</li>
      </ol>
      <ErrorAlert>{error}</ErrorAlert>
      <div className="grid gap-4 sm:grid-cols-[1fr_2fr]">
        <Field label={t("dns.connect.name")} hint={t("dns.connect.name_hint")} value={name} onChange={(e) => setName(e.target.value)} />
        <Field
          label={t("dns.connect.token")}
          type="password"
          autoComplete="off"
          value={token}
          onChange={(e) => setToken(e.target.value.trim())}
          dir="ltr"
          className="[&_input]:font-mono"
          required
        />
      </div>
      <p className="text-sm text-muted">{t("dns.connect.kept")}</p>
      <div>
        <Button type="submit" variant="primary" loading={busy} disabled={!token}>
          {t("dns.connect.button")}
        </Button>
      </div>
    </form>
  );
}

export function zoneTone(z: DNSZone) {
  if (z.sync_error) return "crit" as const;
  if (z.status !== "active" || z.issues.length > 0) return "warn" as const;
  return "ok" as const;
}

export function DNSPage() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const accounts = useCFAccounts();
  const zones = useZones();
  const [adding, setAdding] = useState(false);
  const [connecting, setConnecting] = useState(false);
  const [error, setError] = useState("");

  const disconnect = async (a: CloudflareAccount) => {
    if (!window.confirm(t("dns.disconnect_confirm", { name: a.name }))) return;
    setError("");
    try {
      await api.del(`/dns/cloudflare/${a.id}`);
      qc.invalidateQueries({ queryKey: ["cf-accounts"] });
      qc.invalidateQueries({ queryKey: ["dns-zones"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };

  if (accounts.isPending || zones.isPending) return <Spinner />;
  const hasAccounts = (accounts.data?.length ?? 0) > 0;

  return (
    <>
      <PageHeader
        title={t("dns.title")}
        description={t("dns.intro")}
      />
      <div className="grid gap-6">
        <HostedDNS />
        <div className="flex flex-wrap items-end justify-between gap-3 border-t border-line pt-6">
          <div>
            <h2 className="text-lg font-semibold">Cloudflare</h2>
            <p className="text-sm text-muted">{t("dns.cloudflare_intro")}</p>
          </div>
          {hasAccounts && (
            <Button onClick={() => setAdding(true)}>
              <Plus className="size-4" aria-hidden />
              {t("dns.add_domain")}
            </Button>
          )}
        </div>
        <ErrorAlert>{error || (accounts.error ? errorMessage(t, accounts.error) : "")}</ErrorAlert>
        {!hasAccounts && (
          <Card className="grid gap-4 p-6">
            <div className="flex items-start gap-3">
              <CloudCog className="size-8 shrink-0 text-accent" aria-hidden />
              <div>
                <h2 className="text-lg font-semibold">{t("dns.connect.title")}</h2>
                <p className="text-sm text-muted">{t("dns.connect.why")}</p>
              </div>
            </div>
            <ConnectCloudflare />
          </Card>
        )}

        {hasAccounts && (
          <>
            {zones.data?.length === 0 ? (
              <EmptyState
                icon={<CloudCog />}
                title={t("dns.empty.title")}
                body={t("dns.empty.body")}
                action={
                  <Button variant="primary" onClick={() => setAdding(true)}>
                    {t("dns.add_domain")}
                  </Button>
                }
              />
            ) : (
              <Card className="overflow-x-auto">
                <table className="w-full min-w-[640px] text-sm">
                  <thead>
                    <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                      <th className="px-4 py-3 text-start font-semibold">{t("dns.col.domain")}</th>
                      <th className="px-4 py-3 text-start font-semibold">{t("dns.col.status")}</th>
                      <th className="px-4 py-3 text-start font-semibold">{t("dns.col.records")}</th>
                      <th className="px-4 py-3 text-start font-semibold">{t("dns.col.synced")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {zones.data?.map((z) => (
                      <tr key={z.id} className="border-b border-line last:border-0 hover:bg-sunk/60">
                        <td className="px-4 py-3">
                          <Link to={`/dns/${z.id}`} className="font-semibold hover:text-accent" dir="ltr">
                            {z.name}
                          </Link>
                          <div className="text-xs text-muted">{z.account_name}</div>
                        </td>
                        <td className="px-4 py-3">
                          <Badge tone={zoneTone(z)}>
                            {z.sync_error
                              ? t("dns.state.error")
                              : z.status !== "active"
                                ? t("dns.state.pending")
                                : z.issues.length > 0
                                  ? t("dns.state.issues", { count: z.issues.length })
                                  : t("dns.state.ok")}
                          </Badge>
                        </td>
                        <td className="px-4 py-3 text-muted tabular-nums">{t("dns.managed_count", { count: z.records })}</td>
                        <td className="px-4 py-3 text-muted">{z.synced_at ? formatRelative(z.synced_at, i18n.language) : "—"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </Card>
            )}

            <Card className="grid gap-3 p-5">
              <h2 className="font-semibold">{t("dns.accounts")}</h2>
              <ul className="grid gap-2">
                {accounts.data?.map((a) => (
                  <li key={a.id} className="flex flex-wrap items-center gap-3 rounded-lg border border-line px-3 py-2 text-sm">
                    <CloudCog className="size-4 text-accent" aria-hidden />
                    <span className="font-medium">{a.name}</span>
                    <span className="text-muted">{t("dns.zones_count", { count: a.zones })}</span>
                    {!a.account_id && <span className="text-xs text-muted">{t("dns.no_account_read")}</span>}
                    <Button size="sm" variant="ghost" className="ms-auto" onClick={() => disconnect(a)} aria-label={t("dns.disconnect", { name: a.name })}>
                      <Trash2 className="size-4" aria-hidden />
                    </Button>
                  </li>
                ))}
              </ul>
              {connecting ? (
                <ConnectCloudflare onDone={() => setConnecting(false)} />
              ) : (
                <div>
                  <Button size="sm" onClick={() => setConnecting(true)}>
                    <Plus className="size-4" aria-hidden />
                    {t("dns.connect.another")}
                  </Button>
                </div>
              )}
            </Card>
          </>
        )}
      </div>
      {adding && <AddDomainDialog accounts={accounts.data ?? []} onClose={() => setAdding(false)} />}
    </>
  );
}

function AddDomainDialog({ accounts, onClose }: { accounts: CloudflareAccount[]; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [account, setAccount] = useState(accounts[0]?.id ?? "");
  const [name, setName] = useState("");
  const [proxied, setProxied] = useState(true);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const zones = useQuery({
    queryKey: ["cf-zones", account],
    queryFn: () => api.get<{ id: string; name: string; status: string; managed_id: string | null }[]>(`/dns/cloudflare/${account}/zones`),
    enabled: !!account,
  });
  const add = async (body: Record<string, unknown>, key: string) => {
    setBusy(key);
    setError("");
    try {
      const z = await api.post<DNSZone>("/dns/zones", { account_id: account, proxied, ...body });
      qc.invalidateQueries({ queryKey: ["dns-zones"] });
      navigate(`/dns/${z.id}`);
    } catch (err) {
      setError(errorMessage(t, err));
      setBusy("");
    }
  };
  const acct = accounts.find((a) => a.id === account);
  return (
    <Dialog open onClose={onClose} title={t("dns.add_domain")} wide>
      <div className="grid gap-5">
        <ErrorAlert>{error}</ErrorAlert>
        {accounts.length > 1 && (
          <label className="grid gap-1.5">
            <span className="text-sm font-medium">{t("dns.account")}</span>
            <select value={account} onChange={(e) => setAccount(e.target.value)} className="h-10 rounded-lg border border-line bg-surface px-3 text-ink">
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </select>
          </label>
        )}
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" checked={proxied} onChange={(e) => setProxied(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
          <span>
            <span className="font-medium">{t("dns.proxied_default")}</span>
            <span className="block text-muted">{t("dns.proxied_hint")}</span>
          </span>
        </label>
        <div className="grid gap-2">
          <h3 className="text-sm font-semibold">{t("dns.in_cloudflare")}</h3>
          {zones.isPending && <Spinner />}
          {zones.error && <ErrorAlert>{errorMessage(t, zones.error)}</ErrorAlert>}
          {zones.data?.length === 0 && <p className="text-sm text-muted">{t("dns.no_zones")}</p>}
          <ul className="grid max-h-72 gap-2 overflow-y-auto">
            {zones.data?.map((z) => (
              <li key={z.id} className="flex items-center gap-3 rounded-lg border border-line px-3 py-2 text-sm">
                <span className="font-medium" dir="ltr">
                  {z.name}
                </span>
                {z.status !== "active" && <Badge tone="warn">{t("dns.state.pending")}</Badge>}
                <span className="ms-auto">
                  {z.managed_id ? (
                    <Link to={`/dns/${z.managed_id}`} className="text-accent hover:underline">
                      {t("dns.managed")}
                    </Link>
                  ) : (
                    <Button size="sm" variant="primary" loading={busy === z.id} disabled={!!busy} onClick={() => add({ cf_zone_id: z.id }, z.id)}>
                      {t("dns.manage")}
                    </Button>
                  )}
                </span>
              </li>
            ))}
          </ul>
        </div>
        <form
          className="grid gap-2 border-t border-line pt-4"
          onSubmit={(e) => {
            e.preventDefault();
            add({ name }, "new");
          }}
        >
          <h3 className="text-sm font-semibold">{t("dns.new_domain")}</h3>
          <p className="text-sm text-muted">{acct?.account_id ? t("dns.new_domain_hint") : t("dns.no_account_read")}</p>
          <div className="flex flex-wrap gap-2">
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="example.com"
              dir="ltr"
              aria-label={t("dns.new_domain")}
              className="h-10 min-w-0 flex-1 rounded-lg border border-line bg-surface px-3 text-ink"
              disabled={!acct?.account_id}
            />
            <Button type="submit" loading={busy === "new"} disabled={!name || !!busy || !acct?.account_id}>
              {t("dns.add_to_cloudflare")}
            </Button>
          </div>
        </form>
      </div>
    </Dialog>
  );
}

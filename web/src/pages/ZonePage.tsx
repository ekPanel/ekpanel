import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ArrowLeft, Cloud, CloudOff, Pencil, Plus, RefreshCw, ShieldCheck, Trash2 } from "lucide-react";
import { api, ApiError, errorMessage, type DNSIssue, type DNSRecord, type DNSZone } from "../lib/api";
import { formatRelative } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, Dialog, ErrorAlert, Field, Spinner, cx } from "../components/ui";
import { zoneTone } from "./DNSPage";

type ZoneView = { zone: DNSZone; records: DNSRecord[]; ssl_mode: string };

const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";
const types = ["A", "AAAA", "CNAME", "MX", "TXT", "CAA", "NS"];
const proxiable = (type: string) => type === "A" || type === "AAAA" || type === "CNAME";

export function ZonePage() {
  const { id = "" } = useParams();
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const q = useQuery({
    queryKey: ["dns-zone", id],
    queryFn: () => api.get<ZoneView>(`/dns/zones/${id}`),
    retry: (n, err) => !(err instanceof ApiError && err.status === 404) && n < 2,
  });
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [editing, setEditing] = useState<Partial<DNSRecord> | null>(null);
  const [removing, setRemoving] = useState(false);

  if (q.isPending) return <Spinner />;
  if (q.error || !q.data) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;
  const { zone: z, records, ssl_mode } = q.data;

  const act = async (what: string, run: () => Promise<unknown>, done?: string) => {
    setBusy(what);
    setError("");
    setNote("");
    try {
      await run();
      await qc.invalidateQueries({ queryKey: ["dns-zone", id] });
      qc.invalidateQueries({ queryKey: ["dns-zones"] });
      if (done) setNote(done);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };
  const sync = () =>
    act("sync", async () => {
      await api.post(`/dns/zones/${id}/sync`);
      // The sync runs as a task; give it a moment before reading the zone again.
      await new Promise((r) => setTimeout(r, 2500));
    });

  return (
    <>
      <Link to="/dns" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("dns.title")}
      </Link>
      <PageHeader
        title={z.name}
        description={t("dns.zone_intro", { account: z.account_name })}
        actions={
          <>
            <Button size="sm" onClick={sync} loading={busy === "sync"}>
              <RefreshCw className="size-4" aria-hidden />
              {t("dns.sync")}
            </Button>
            <Button size="sm" onClick={() => act("purge", () => api.post(`/dns/zones/${id}/purge`), t("dns.purged"))} loading={busy === "purge"}>
              {t("dns.purge")}
            </Button>
          </>
        }
      />
      <div className="grid gap-6">
        <ErrorAlert>{error}</ErrorAlert>
        {note && <p className="rounded-lg bg-ok-soft px-4 py-2 text-sm text-ok">{note}</p>}

        <Card className="grid gap-3 p-5">
          <div className="flex flex-wrap items-center gap-3">
            <Badge tone={zoneTone(z)}>{z.status === "active" ? t("dns.state.active") : t("dns.state.pending")}</Badge>
            <span className="text-sm text-muted">
              {z.synced_at ? t("dns.synced", { when: formatRelative(z.synced_at, i18n.language) }) : t("dns.not_synced")}
              {" · "}
              {t("dns.managed_count", { count: z.records })}
            </span>
          </div>
          {z.sync_error && <p className="text-sm break-words text-crit">{z.sync_error}</p>}
          {z.status !== "active" && (
            <div className="grid gap-2 rounded-lg bg-warn-soft p-4 text-sm">
              <p className="font-medium">{t("dns.pending_title")}</p>
              <p>{t("dns.pending_body")}</p>
              <ul className="grid gap-1.5">
                {z.name_servers.map((ns) => (
                  <li key={ns} className="flex items-center gap-2 rounded-md bg-surface px-3 py-1.5">
                    <span className="flex-1 font-mono" dir="ltr">
                      {ns}
                    </span>
                    <CopyButton text={ns} className="-my-1" />
                  </li>
                ))}
              </ul>
            </div>
          )}
        </Card>

        {z.issues.length > 0 && (
          <Card className="overflow-hidden">
            <h2 className="flex items-center gap-2 border-b border-line px-4 py-3 font-semibold">
              <AlertTriangle className="size-4 text-warn" aria-hidden />
              {t("dns.issues_title")}
            </h2>
            <ul className="divide-y divide-line">
              {z.issues.map((i: DNSIssue) => (
                <li key={i.kind + i.type + i.name} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-3 text-sm">
                  <span className="font-mono" dir="ltr">
                    {i.type} {i.name}
                  </span>
                  <span className="text-muted">{t(`dns.issue.${i.kind}`, { want: i.want, have: i.have, server: i.server })}</span>
                  {i.kind !== "no_address" && (
                    <Button
                      size="sm"
                      className="ms-auto"
                      loading={busy === i.type + i.name}
                      onClick={() => act(i.type + i.name, () => api.post(`/dns/zones/${id}/resolve`, { type: i.type, name: i.name }))}
                    >
                      {t("dns.use_ours")}
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          </Card>
        )}

        <div className="grid gap-6 lg:grid-cols-2">
          <Card className="grid content-start gap-3 p-5">
            <h2 className="flex items-center gap-2 font-semibold">
              <ShieldCheck className="size-4" aria-hidden />
              {t("dns.ssl_title")}
            </h2>
            {ssl_mode ? (
              <p className="text-sm">
                {t("dns.ssl_now", { mode: t(`dns.ssl_modes.${ssl_mode}`, { defaultValue: ssl_mode }) })}{" "}
                {ssl_mode === "strict" ? <Badge tone="ok">{t("dns.ssl_good")}</Badge> : <Badge tone="warn">{t("dns.ssl_weak")}</Badge>}
              </p>
            ) : (
              <p className="text-sm text-muted">{t("dns.ssl_unknown")}</p>
            )}
            <p className="text-sm text-muted">{t("dns.ssl_hint")}</p>
            {ssl_mode && ssl_mode !== "strict" && (
              <div>
                <Button
                  size="sm"
                  variant="primary"
                  loading={busy === "ssl"}
                  onClick={() => act("ssl", () => api.post(`/dns/zones/${id}/ssl`, { mode: "strict" }))}
                >
                  {t("dns.ssl_use_strict")}
                </Button>
              </div>
            )}
          </Card>
          <Card className="grid content-start gap-3 p-5">
            <h2 className="font-semibold">{t("dns.settings")}</h2>
            <label className="flex items-start gap-2 text-sm">
              <input
                type="checkbox"
                checked={z.proxied}
                onChange={(e) => act("proxied", () => api.patch(`/dns/zones/${id}`, { proxied: e.target.checked }))}
                className="mt-1 accent-[var(--accent)]"
              />
              <span>
                <span className="font-medium">{t("dns.proxied_default")}</span>
                <span className="block text-muted">{t("dns.proxied_hint")}</span>
              </span>
            </label>
            <div>
              <Button size="sm" variant="ghost" onClick={() => setRemoving(true)}>
                <Trash2 className="size-4" aria-hidden />
                {t("dns.remove")}
              </Button>
            </div>
          </Card>
        </div>

        <Card className="overflow-hidden">
          <div className="flex flex-wrap items-center gap-3 border-b border-line px-4 py-3">
            <h2 className="font-semibold">{t("dns.records_title")}</h2>
            <span className="text-sm text-muted">{t("dns.records_hint")}</span>
            <Button size="sm" className="ms-auto" onClick={() => setEditing({ type: "A", name: "", content: "", ttl: 1, proxied: z.proxied })}>
              <Plus className="size-4" aria-hidden />
              {t("dns.add_record")}
            </Button>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[720px] text-sm">
              <thead>
                <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                  <th className="px-4 py-2 text-start font-semibold">{t("dns.col.type")}</th>
                  <th className="px-4 py-2 text-start font-semibold">{t("dns.col.name")}</th>
                  <th className="px-4 py-2 text-start font-semibold">{t("dns.col.content")}</th>
                  <th className="px-4 py-2 text-start font-semibold">{t("dns.col.proxy")}</th>
                  <th className="px-4 py-2" />
                </tr>
              </thead>
              <tbody>
                {records.map((r) => (
                  <tr key={r.id} className="border-b border-line last:border-0">
                    <td className="px-4 py-2 font-mono">{r.type}</td>
                    <td className="max-w-64 truncate px-4 py-2 font-mono" dir="ltr">
                      {r.name}
                      {r.managed && (
                        <span className="ms-2">
                          <Badge tone="ok">ekPanel</Badge>
                        </span>
                      )}
                    </td>
                    <td className="max-w-80 truncate px-4 py-2 font-mono text-muted" dir="ltr" title={r.content}>
                      {r.priority != null && `${r.priority} `}
                      {r.content}
                    </td>
                    <td className="px-4 py-2">
                      {proxiable(r.type) ? (
                        <button
                          type="button"
                          onClick={() => act("p" + r.id, () => api.put(`/dns/zones/${id}/records/${r.id}`, { ...r, proxied: !r.proxied }))}
                          disabled={!!busy}
                          aria-pressed={r.proxied}
                          className={cx(
                            "inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium",
                            r.proxied ? "bg-amber-soft text-amber" : "bg-sunk text-muted",
                          )}
                        >
                          {r.proxied ? <Cloud className="size-3.5" aria-hidden /> : <CloudOff className="size-3.5" aria-hidden />}
                          {r.proxied ? t("dns.proxied") : t("dns.dns_only")}
                        </button>
                      ) : (
                        <span className="text-xs text-muted">—</span>
                      )}
                    </td>
                    <td className="px-4 py-2 text-end whitespace-nowrap">
                      {!r.managed && (
                        <>
                          <Button size="sm" variant="ghost" onClick={() => setEditing(r)} aria-label={t("dns.edit_record")}>
                            <Pencil className="size-4" aria-hidden />
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            onClick={() =>
                              window.confirm(t("dns.delete_record_confirm", { name: r.name })) &&
                              act("d" + r.id, () => api.del(`/dns/zones/${id}/records/${r.id}`))
                            }
                            aria-label={t("dns.delete_record")}
                          >
                            <Trash2 className="size-4" aria-hidden />
                          </Button>
                        </>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      </div>
      {editing && <RecordDialog zone={z} record={editing} onClose={() => setEditing(null)} />}
      {removing && (
        <RemoveZoneDialog
          zone={z}
          onClose={() => setRemoving(false)}
          onRemoved={() => {
            qc.invalidateQueries({ queryKey: ["dns-zones"] });
            navigate("/dns", { replace: true });
          }}
        />
      )}
    </>
  );
}

function RecordDialog({ zone, record, onClose }: { zone: DNSZone; record: Partial<DNSRecord>; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [r, setR] = useState(record);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const set = (p: Partial<DNSRecord>) => setR({ ...r, ...p });
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body = { ...r, priority: r.type === "MX" ? Number(r.priority ?? 10) : null };
      if (r.id) await api.put(`/dns/zones/${zone.id}/records/${r.id}`, body);
      else await api.post(`/dns/zones/${zone.id}/records`, body);
      qc.invalidateQueries({ queryKey: ["dns-zone", zone.id] });
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} title={r.id ? t("dns.edit_record") : t("dns.add_record")}>
      <form onSubmit={submit} className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        <div className="grid gap-4 sm:grid-cols-[auto_1fr]">
          <label className="grid content-start gap-1.5">
            <span className="text-sm font-medium">{t("dns.col.type")}</span>
            <select value={r.type} onChange={(e) => set({ type: e.target.value })} className={select} disabled={!!r.id}>
              {types.map((x) => (
                <option key={x}>{x}</option>
              ))}
            </select>
          </label>
          <Field
            label={t("dns.col.name")}
            hint={t("dns.name_hint", { zone: zone.name })}
            value={r.name ?? ""}
            onChange={(e) => set({ name: e.target.value })}
            dir="ltr"
            placeholder="www"
          />
        </div>
        <Field
          label={t("dns.col.content")}
          hint={t(`dns.content_hint.${r.type}`, { defaultValue: "" })}
          value={r.content ?? ""}
          onChange={(e) => set({ content: e.target.value })}
          dir="ltr"
          className="[&_input]:font-mono"
          required
        />
        {r.type === "MX" && (
          <Field
            label={t("dns.priority")}
            value={String(r.priority ?? 10)}
            onChange={(e) => set({ priority: Number(e.target.value.replace(/\D/g, "")) })}
            inputMode="numeric"
            dir="ltr"
          />
        )}
        {proxiable(r.type ?? "") && (
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={!!r.proxied} onChange={(e) => set({ proxied: e.target.checked })} className="accent-[var(--accent)]" />
            {t("dns.proxied")}
          </label>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy}>
            {t("sites.save")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function RemoveZoneDialog({ zone, onClose, onRemoved }: { zone: DNSZone; onClose: () => void; onRemoved: () => void }) {
  const { t } = useTranslation();
  const [deleteRecords, setDeleteRecords] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <Dialog open onClose={onClose} title={t("dns.remove_title", { name: zone.name })}>
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            await api.del(`/dns/zones/${zone.id}${deleteRecords ? "?records=delete" : ""}`);
            onRemoved();
          } catch (err) {
            setError(errorMessage(t, err));
            setBusy(false);
          }
        }}
      >
        <ErrorAlert>{error}</ErrorAlert>
        <p className="text-sm text-muted">{t("dns.remove_body")}</p>
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" checked={deleteRecords} onChange={(e) => setDeleteRecords(e.target.checked)} className="mt-1 accent-[var(--accent)]" />
          <span>
            <span className="font-medium">{t("dns.remove_records")}</span>
            <span className="block text-muted">{t("dns.remove_records_hint")}</span>
          </span>
        </label>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="danger" loading={busy}>
            {t("dns.remove")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

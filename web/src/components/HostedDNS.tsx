import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Server as ServerIcon, Trash2 } from "lucide-react";
import { api, errorMessage, type Server } from "../lib/api";
import { useMe } from "../lib/session";
import { Badge, Button, Card, Dialog, ErrorAlert, Field, Spinner } from "./ui";
import { OwnerField } from "../pages/CustomersPage";

export interface HostedRecord {
  id?: string;
  name: string;
  type: string;
  content: string;
  priority: number;
  ttl: number;
}

export interface NameServer {
  server_id: string;
  server_name: string;
  hostname: string;
  primary: boolean;
  transfer_address: string | null;
  ipv4: string;
  ipv6: string;
  transfer: string;
  checked_at: string | null;
  status: { active: boolean; version?: string; error?: string; zones: { name: string; serial: number; error?: string }[] } | null;
}

export interface HostedZone {
  id: string;
  owner_id: string | null;
  owner_name: string | null;
  name: string;
  dnssec: boolean;
  serial: number;
  ds: string[];
  error: string | null;
  issues: { kind: string; name: string; type: string; want?: string; have?: string; server?: string }[];
  records: number;
  created_at: string;
}

export interface DNSTemplate {
  id: string;
  name: string;
  records: HostedRecord[];
}

export const recordTypes = ["A", "AAAA", "CNAME", "MX", "TXT", "SRV", "CAA", "NS"];
const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

export function useHostedZones() {
  return useQuery({ queryKey: ["hosted-zones"], queryFn: () => api.get<HostedZone[]>("/dns/hosted"), refetchInterval: 30000 });
}

export function useDNSTemplates() {
  return useQuery({ queryKey: ["dns-templates"], queryFn: () => api.get<DNSTemplate[]>("/dns/templates") });
}

/** Zones on ekPanel's own name servers, the name servers themselves and the templates. */
export function HostedDNS() {
  const { t } = useTranslation();
  const me = useMe();
  const admin = me.data?.role === "admin";
  const zones = useHostedZones();
  const [adding, setAdding] = useState(false);
  return (
    <section className="grid gap-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">{t("hosted.title")}</h2>
          <p className="text-sm text-muted">{t("hosted.intro")}</p>
        </div>
        <Button variant="primary" onClick={() => setAdding(true)}>
          <Plus className="size-4" aria-hidden />
          {t("hosted.add")}
        </Button>
      </div>
      {zones.error && <ErrorAlert>{errorMessage(t, zones.error)}</ErrorAlert>}
      {zones.isPending && <Spinner />}
      {zones.data && zones.data.length > 0 && (
        <Card className="overflow-x-auto">
          <table className="w-full min-w-[560px] text-sm">
            <thead>
              <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                <th className="px-4 py-3 text-start font-semibold">{t("dns.col.domain")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("dns.col.status")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("dns.col.records")}</th>
                <th className="px-4 py-3 text-start font-semibold">DNSSEC</th>
              </tr>
            </thead>
            <tbody>
              {zones.data.map((z) => (
                <tr key={z.id} className="border-b border-line last:border-0 hover:bg-sunk/60">
                  <td className="px-4 py-3">
                    <Link to={`/dns/hosted/${z.id}`} className="font-semibold hover:text-accent" dir="ltr">
                      {z.name}
                    </Link>
                    {(admin || me.data?.role === "reseller") && z.owner_name && <div className="text-xs text-muted">{z.owner_name}</div>}
                  </td>
                  <td className="px-4 py-3">
                    <ZoneStateBadge zone={z} />
                  </td>
                  <td className="px-4 py-3 text-muted tabular-nums">{t("hosted.kept_count", { count: z.records })}</td>
                  <td className="px-4 py-3">{z.dnssec ? <Badge tone="ok">{t("hosted.signed")}</Badge> : <span className="text-muted">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      {zones.data?.length === 0 && <p className="text-sm text-muted">{t("hosted.none")}</p>}
      {admin && <NameServersCard />}
      {admin && <TemplatesCard />}
      {adding && <AddHostedZoneDialog onClose={() => setAdding(false)} />}
    </section>
  );
}

export function ZoneStateBadge({ zone: z }: { zone: HostedZone }) {
  const { t } = useTranslation();
  if (z.error) return <Badge tone="crit">{t("dns.state.error")}</Badge>;
  if (z.serial === 0) return <Badge tone="warn">{t("hosted.state.pending")}</Badge>;
  if (z.issues.length > 0) return <Badge tone="warn">{t("dns.state.issues", { count: z.issues.length })}</Badge>;
  return <Badge tone="ok">{t("hosted.state.serving")}</Badge>;
}

function AddHostedZoneDialog({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const templates = useDNSTemplates();
  const [name, setName] = useState("");
  const [template, setTemplate] = useState("");
  const [owner, setOwner] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const z = await api.post<HostedZone>("/dns/hosted", { name, template_id: template, owner_id: owner });
      qc.invalidateQueries({ queryKey: ["hosted-zones"] });
      navigate(`/dns/hosted/${z.id}`);
    } catch (err) {
      setError(errorMessage(t, err));
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} title={t("hosted.add")}>
      <form onSubmit={submit} className="grid gap-4">
        <p className="text-sm text-muted">{t("hosted.add_hint")}</p>
        <ErrorAlert>{error}</ErrorAlert>
        <Field label={t("dns.col.domain")} value={name} onChange={(e) => setName(e.target.value)} placeholder="example.com" dir="ltr" required />
        <OwnerField value={owner} onChange={setOwner} />
        {(templates.data?.length ?? 0) > 0 && (
          <label className="grid gap-1.5">
            <span className="text-sm font-medium">{t("hosted.template")}</span>
            <select value={template} onChange={(e) => setTemplate(e.target.value)} className={select}>
              <option value="">{t("hosted.no_template")}</option>
              {templates.data?.map((tp) => (
                <option key={tp.id} value={tp.id}>
                  {tp.name}
                </option>
              ))}
            </select>
          </label>
        )}
        <div>
          <Button type="submit" variant="primary" loading={busy} disabled={!name.trim()}>
            {t("hosted.add")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

type NSRow = { server_id: string; hostname: string; primary: boolean; transfer_address: string };

function NameServersCard() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const ns = useQuery({ queryKey: ["name-servers"], queryFn: () => api.get<NameServer[]>("/dns/nameservers"), refetchInterval: 30000 });
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers") });
  const [rows, setRows] = useState<NSRow[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  if (ns.isPending || servers.isPending) return <Spinner />;
  const enrolled = (servers.data ?? []).filter((s) => s.enrolled_at);
  const editing = rows !== null;
  const start = () =>
    setRows(
      ns.data && ns.data.length > 0
        ? ns.data.map((n) => ({ server_id: n.server_id, hostname: n.hostname, primary: n.primary, transfer_address: n.transfer_address ?? "" }))
        : [{ server_id: enrolled[0]?.id ?? "", hostname: "", primary: true, transfer_address: "" }],
    );
  const set = (i: number, part: Partial<NSRow>) =>
    setRows((rs) => rs!.map((r, j) => (j === i ? { ...r, ...part } : part.primary ? { ...r, primary: false } : r)));
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      qc.setQueryData(["name-servers"], await api.put<NameServer[]>("/dns/nameservers", rows));
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setRows(null);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="font-semibold">{t("hosted.ns.title")}</h3>
          <p className="text-sm text-muted">{t("hosted.ns.intro")}</p>
        </div>
        {!editing && (
          <Button size="sm" onClick={start} disabled={enrolled.length === 0}>
            {(ns.data?.length ?? 0) > 0 ? t("hosted.ns.change") : t("hosted.ns.setup")}
          </Button>
        )}
      </div>
      <ErrorAlert>{error}</ErrorAlert>
      {!editing && ns.data && ns.data.length > 0 && (
        <ul className="grid gap-2 text-sm">
          {ns.data.map((n) => (
            <li key={n.server_id} className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border border-line px-3 py-2">
              <ServerIcon className="size-4 text-accent" aria-hidden />
              <span className="font-medium" dir="ltr">
                {n.hostname}
              </span>
              <Badge tone={n.primary ? "accent" : "neutral"}>{n.primary ? t("hosted.ns.primary") : t("hosted.ns.secondary")}</Badge>
              <span className="text-muted">{n.server_name}</span>
              <span className="font-mono text-xs text-muted" dir="ltr">
                {[n.ipv4, n.ipv6].filter(Boolean).join(" · ") || t("hosted.ns.no_address")}
              </span>
              <span className="ms-auto">
                {n.status?.error ? (
                  <Badge tone="crit">{t("dns.state.error")}</Badge>
                ) : n.status?.active ? (
                  <Badge tone="ok">PowerDNS {n.status.version}</Badge>
                ) : (
                  <Badge tone="warn">{t("hosted.state.pending")}</Badge>
                )}
              </span>
              {n.status?.error && <span className="w-full text-xs text-crit">{n.status.error}</span>}
            </li>
          ))}
        </ul>
      )}
      {editing && (
        <div className="grid gap-3">
          {rows!.map((r, i) => (
            <div
              key={i}
              className="grid items-end gap-2 rounded-lg border border-line p-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)_minmax(0,1fr)_auto_auto]"
            >
              <label className="grid gap-1.5">
                <span className="text-sm font-medium">{t("sites.server")}</span>
                <select value={r.server_id} onChange={(e) => set(i, { server_id: e.target.value })} className={select}>
                  {enrolled.map((s) => (
                    <option key={s.id} value={s.id}>
                      {s.name}
                    </option>
                  ))}
                </select>
              </label>
              <Field
                label={t("hosted.ns.hostname")}
                value={r.hostname}
                onChange={(e) => set(i, { hostname: e.target.value })}
                placeholder={`ns${i + 1}.example.com`}
                dir="ltr"
              />
              <Field
                label={t("hosted.ns.transfer")}
                value={r.transfer_address}
                onChange={(e) => set(i, { transfer_address: e.target.value })}
                placeholder={t("hosted.ns.transfer_auto")}
                dir="ltr"
              />
              <label className="flex h-10 items-center gap-2 text-sm">
                <input type="radio" checked={r.primary} onChange={() => set(i, { primary: true })} className="accent-[var(--accent)]" />
                {t("hosted.ns.primary")}
              </label>
              <Button
                size="sm"
                variant="ghost"
                className="h-10"
                onClick={() => setRows(rows!.filter((_, j) => j !== i))}
                aria-label={t("hosted.ns.remove", { name: r.hostname })}
              >
                <Trash2 className="size-4" aria-hidden />
              </Button>
            </div>
          ))}
          <p className="text-xs text-muted">{t("hosted.ns.transfer_hint")}</p>
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              onClick={() =>
                setRows([
                  ...rows!,
                  {
                    server_id: enrolled.find((s) => !rows!.some((r) => r.server_id === s.id))?.id ?? "",
                    hostname: "",
                    primary: rows!.length === 0,
                    transfer_address: "",
                  },
                ])
              }
              disabled={rows!.length >= enrolled.length}
            >
              <Plus className="size-4" aria-hidden />
              {t("hosted.ns.add")}
            </Button>
            <Button size="sm" variant="primary" onClick={save} loading={busy}>
              {t("common.save")}
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setRows(null)}>
              {t("common.cancel")}
            </Button>
          </div>
        </div>
      )}
    </Card>
  );
}

function TemplatesCard() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const templates = useDNSTemplates();
  const [editing, setEditing] = useState<DNSTemplate | null>(null);
  const [error, setError] = useState("");
  const remove = async (tp: DNSTemplate) => {
    if (!window.confirm(t("hosted.tpl.delete_confirm", { name: tp.name }))) return;
    try {
      await api.del(`/dns/templates/${tp.id}`);
      qc.invalidateQueries({ queryKey: ["dns-templates"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };
  return (
    <Card className="grid gap-3 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="font-semibold">{t("hosted.tpl.title")}</h3>
          <p className="text-sm text-muted">{t("hosted.tpl.intro")}</p>
        </div>
        <Button size="sm" onClick={() => setEditing({ id: "", name: "", records: [] })}>
          <Plus className="size-4" aria-hidden />
          {t("hosted.tpl.new")}
        </Button>
      </div>
      <ErrorAlert>{error}</ErrorAlert>
      {templates.data && templates.data.length > 0 && (
        <ul className="grid gap-2 text-sm">
          {templates.data.map((tp) => (
            <li key={tp.id} className="flex flex-wrap items-center gap-3 rounded-lg border border-line px-3 py-2">
              <button type="button" className="font-medium hover:text-accent" onClick={() => setEditing(tp)}>
                {tp.name}
              </button>
              <span className="text-muted">{t("hosted.kept_count", { count: tp.records.length })}</span>
              <Button size="sm" variant="ghost" className="ms-auto" onClick={() => remove(tp)} aria-label={t("hosted.tpl.delete", { name: tp.name })}>
                <Trash2 className="size-4" aria-hidden />
              </Button>
            </li>
          ))}
        </ul>
      )}
      {editing && <TemplateDialog template={editing} onClose={() => setEditing(null)} />}
    </Card>
  );
}

function TemplateDialog({ template, onClose }: { template: DNSTemplate; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [name, setName] = useState(template.name);
  const [records, setRecords] = useState<HostedRecord[]>(
    template.records.length ? template.records : [{ name: "@", type: "A", content: "", priority: 0, ttl: 3600 }],
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const set = (i: number, part: Partial<HostedRecord>) => setRecords((rs) => rs.map((r, j) => (j === i ? { ...r, ...part } : r)));
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body = { name, records: records.filter((r) => r.content.trim()) };
      if (template.id) await api.put(`/dns/templates/${template.id}`, body);
      else await api.post("/dns/templates", body);
      qc.invalidateQueries({ queryKey: ["dns-templates"] });
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
      setBusy(false);
    }
  };
  return (
    <Dialog open onClose={onClose} title={template.id ? template.name : t("hosted.tpl.new")} wide>
      <form onSubmit={save} className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        <Field label={t("hosted.tpl.name")} value={name} onChange={(e) => setName(e.target.value)} maxLength={60} required />
        <p className="text-sm text-muted">{t("hosted.tpl.hint")}</p>
        <div className="grid gap-2">
          {records.map((r, i) => (
            <div key={i} className="grid gap-2 sm:grid-cols-[minmax(0,0.8fr)_auto_minmax(0,2fr)_5rem_auto]">
              <input
                value={r.name}
                onChange={(e) => set(i, { name: e.target.value })}
                placeholder="@"
                aria-label={t("hosted.rec.name")}
                dir="ltr"
                className="h-10 rounded-lg border border-line bg-surface px-3 font-mono text-sm"
              />
              <select value={r.type} onChange={(e) => set(i, { type: e.target.value })} className={select} aria-label={t("hosted.rec.type")}>
                {recordTypes.map((ty) => (
                  <option key={ty}>{ty}</option>
                ))}
              </select>
              <input
                value={r.content}
                onChange={(e) => set(i, { content: e.target.value })}
                aria-label={t("hosted.rec.content")}
                dir="ltr"
                className="h-10 rounded-lg border border-line bg-surface px-3 font-mono text-sm"
              />
              <input
                type="number"
                value={r.priority}
                onChange={(e) => set(i, { priority: Number(e.target.value) })}
                aria-label={t("hosted.rec.priority")}
                disabled={r.type !== "MX" && r.type !== "SRV"}
                className="h-10 rounded-lg border border-line bg-surface px-3 text-sm disabled:opacity-40"
              />
              <Button size="sm" variant="ghost" className="h-10" onClick={() => setRecords(records.filter((_, j) => j !== i))} aria-label={t("common.delete")}>
                <Trash2 className="size-4" aria-hidden />
              </Button>
            </div>
          ))}
        </div>
        <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={() => setRecords([...records, { name: "@", type: "A", content: "", priority: 0, ttl: 3600 }])}>
            <Plus className="size-4" aria-hidden />
            {t("hosted.rec.add")}
          </Button>
          <Button type="submit" size="sm" variant="primary" loading={busy} disabled={!name.trim()}>
            {t("common.save")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

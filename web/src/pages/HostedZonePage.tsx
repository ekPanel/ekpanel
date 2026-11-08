import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ArrowLeft, KeyRound, Pencil, Plus, Trash2 } from "lucide-react";
import { api, ApiError, errorMessage } from "../lib/api";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, Dialog, ErrorAlert, Field, Spinner } from "../components/ui";
import { ZoneStateBadge, recordTypes, type HostedRecord, type HostedZone, type NameServer } from "../components/HostedDNS";

type ZoneView = HostedZone & { kept: HostedRecord[]; made: HostedRecord[]; name_servers: NameServer[] };

const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

/** The part of name before the zone: "www" for www.example.com, "@" for the zone itself. */
function relative(name: string, zone: string) {
  if (name === zone) return "@";
  return name.endsWith("." + zone) ? name.slice(0, -zone.length - 1) : name + ".";
}

function shown(r: HostedRecord) {
  if (r.type === "MX") return `${r.priority} ${r.content}`;
  if (r.type === "SRV") return `${r.priority} ${r.content}`;
  return r.content;
}

export function HostedZonePage() {
  const { id = "" } = useParams();
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const q = useQuery({
    queryKey: ["hosted-zone", id],
    queryFn: () => api.get<ZoneView>(`/dns/hosted/${id}`),
    retry: (n, err) => !(err instanceof ApiError && err.status === 404) && n < 2,
    refetchInterval: (q) => (q.state.data && (q.state.data.serial === 0 || (q.state.data.dnssec && q.state.data.ds.length === 0)) ? 3000 : 30000),
  });
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [editing, setEditing] = useState<HostedRecord | null>(null);

  if (q.isPending) return <Spinner />;
  if (q.error || !q.data) return <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>;
  const z = q.data;

  const act = async (what: string, run: () => Promise<unknown>) => {
    setBusy(what);
    setError("");
    try {
      await run();
      await qc.invalidateQueries({ queryKey: ["hosted-zone", id] });
      qc.invalidateQueries({ queryKey: ["hosted-zones"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };
  const remove = async () => {
    if (!window.confirm(t("hosted.delete_confirm", { name: z.name }))) return;
    await act("delete", async () => {
      await api.del(`/dns/hosted/${id}`);
      navigate("/dns");
    });
  };
  const glue = z.name_servers.filter((n) => n.hostname === z.name || n.hostname.endsWith("." + z.name));

  return (
    <>
      <Link to="/dns" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("dns.title")}
      </Link>
      <PageHeader
        title={z.name}
        description={t("hosted.zone_intro")}
        actions={
          <Button size="sm" variant="danger" onClick={remove} loading={busy === "delete"}>
            <Trash2 className="size-4" aria-hidden />
            {t("hosted.delete")}
          </Button>
        }
      />
      <div className="grid gap-6 xl:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
        <div className="grid content-start gap-6">
          <ErrorAlert>{error || z.error}</ErrorAlert>
          {z.issues.length > 0 && (
            <Card className="grid gap-2 p-5">
              <h2 className="flex items-center gap-2 font-semibold">
                <AlertTriangle className="size-4 text-warn" aria-hidden />
                {t("hosted.issues")}
              </h2>
              <ul className="grid gap-1.5 text-sm">
                {z.issues.map((i, n) => (
                  <li key={n}>
                    {i.kind === "conflict"
                      ? t("hosted.issue_conflict", { name: i.name, type: i.type, want: i.want, have: i.have })
                      : t("hosted.issue_no_address", { name: i.name, server: i.server })}
                  </li>
                ))}
              </ul>
            </Card>
          )}
          <Card className="grid gap-4 p-6">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h2 className="text-lg font-semibold">{t("hosted.records")}</h2>
                <p className="text-sm text-muted">{t("hosted.records_intro")}</p>
              </div>
              <Button size="sm" variant="primary" onClick={() => setEditing({ name: "", type: "A", content: "", priority: 0, ttl: 3600 })}>
                <Plus className="size-4" aria-hidden />
                {t("hosted.rec.add")}
              </Button>
            </div>
            <div className="overflow-x-auto">
              <table className="w-full min-w-[560px] text-sm">
                <thead>
                  <tr className="border-b border-line text-xs tracking-wider text-muted uppercase">
                    <th className="py-2 pe-3 text-start font-semibold">{t("hosted.rec.name")}</th>
                    <th className="py-2 pe-3 text-start font-semibold">{t("hosted.rec.type")}</th>
                    <th className="py-2 pe-3 text-start font-semibold">{t("hosted.rec.content")}</th>
                    <th className="py-2 pe-3 text-start font-semibold">TTL</th>
                    <th className="py-2" />
                  </tr>
                </thead>
                <tbody>
                  {z.kept.map((r) => (
                    <tr key={r.id} className="border-b border-line last:border-0">
                      <td className="py-2 pe-3 font-mono" dir="ltr">
                        {relative(r.name, z.name)}
                      </td>
                      <td className="py-2 pe-3">{r.type}</td>
                      <td className="max-w-[24rem] py-2 pe-3 font-mono text-xs break-all" dir="ltr">
                        {shown(r)}
                      </td>
                      <td className="py-2 pe-3 text-muted tabular-nums">{r.ttl}</td>
                      <td className="py-2 text-end whitespace-nowrap">
                        <Button size="sm" variant="ghost" onClick={() => setEditing(r)} aria-label={t("hosted.rec.edit")}>
                          <Pencil className="size-4" aria-hidden />
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => act("del" + r.id, () => api.del(`/dns/hosted/${id}/records/${r.id}`))}
                          loading={busy === "del" + r.id}
                          aria-label={t("common.delete")}
                        >
                          <Trash2 className="size-4" aria-hidden />
                        </Button>
                      </td>
                    </tr>
                  ))}
                  {z.made.map((r, i) => (
                    <tr key={"m" + i} className="border-b border-line text-muted last:border-0">
                      <td className="py-2 pe-3 font-mono" dir="ltr">
                        {relative(r.name, z.name)}
                      </td>
                      <td className="py-2 pe-3">{r.type}</td>
                      <td className="max-w-[24rem] py-2 pe-3 font-mono text-xs break-all" dir="ltr">
                        {shown(r)}
                      </td>
                      <td className="py-2 pe-3 tabular-nums">{r.ttl}</td>
                      <td className="py-2 text-end">
                        <Badge>{t("hosted.made")}</Badge>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="text-xs text-muted">{t("hosted.made_hint")}</p>
          </Card>
        </div>
        <div className="grid content-start gap-6">
          <Card className="grid gap-3 p-6">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="font-semibold">{t("hosted.delegate")}</h2>
              <ZoneStateBadge zone={z} />
            </div>
            <p className="text-sm text-muted">{t("hosted.delegate_intro")}</p>
            <ul className="grid gap-1.5">
              {z.name_servers.map((n) => (
                <li key={n.hostname} className="flex items-center gap-2 font-mono text-sm" dir="ltr">
                  {n.hostname}
                  <CopyButton text={n.hostname} />
                </li>
              ))}
            </ul>
            {glue.length > 0 && (
              <div className="grid gap-2 rounded-lg bg-sunk p-3 text-sm">
                <p>{t("hosted.glue_intro")}</p>
                <ul className="grid gap-1 font-mono text-xs" dir="ltr">
                  {glue.map((n) => (
                    <li key={n.hostname}>
                      {n.hostname} → {[n.ipv4, n.ipv6].filter(Boolean).join(", ") || "?"}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            {z.serial > 0 && <p className="text-xs text-muted">{t("hosted.serial", { serial: z.serial })}</p>}
          </Card>
          <Card className="grid gap-3 p-6">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="flex items-center gap-2 font-semibold">
                <KeyRound className="size-4 text-accent" aria-hidden />
                DNSSEC
              </h2>
              {z.dnssec ? <Badge tone="ok">{t("hosted.signed")}</Badge> : <Badge>{t("serversec.crowdsec.off")}</Badge>}
            </div>
            <p className="text-sm text-muted">{t("hosted.dnssec_intro")}</p>
            {z.dnssec && z.ds.length === 0 && <p className="text-sm text-muted">{t("hosted.dnssec_signing")}</p>}
            {z.dnssec && z.ds.length > 0 && (
              <div className="grid gap-2">
                <p className="text-sm">{t("hosted.ds_intro")}</p>
                {z.ds.map((ds) => {
                  const [tag, alg, digestType, digest] = ds.split(" ");
                  return (
                    <dl key={ds} className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 rounded-lg bg-sunk p-3 text-xs">
                      <dt className="text-muted">{t("hosted.ds_tag")}</dt>
                      <dd className="font-mono">{tag}</dd>
                      <dt className="text-muted">{t("hosted.ds_alg")}</dt>
                      <dd className="font-mono">{alg} (ECDSAP256SHA256)</dd>
                      <dt className="text-muted">{t("hosted.ds_digest_type")}</dt>
                      <dd className="font-mono">{digestType} (SHA-256)</dd>
                      <dt className="text-muted">{t("hosted.ds_digest")}</dt>
                      <dd className="flex items-start gap-1 font-mono break-all" dir="ltr">
                        {digest}
                        <CopyButton text={digest} />
                      </dd>
                    </dl>
                  );
                })}
              </div>
            )}
            <div>
              <Button
                size="sm"
                variant={z.dnssec ? "secondary" : "primary"}
                onClick={() => {
                  if (z.dnssec && !window.confirm(t("hosted.dnssec_off_confirm"))) return;
                  act("dnssec", () => api.patch(`/dns/hosted/${id}`, { dnssec: !z.dnssec }));
                }}
                loading={busy === "dnssec"}
              >
                {z.dnssec ? t("hosted.dnssec_off") : t("hosted.dnssec_on")}
              </Button>
            </div>
          </Card>
        </div>
      </div>
      {editing && <RecordDialog zone={z} record={editing} onClose={() => setEditing(null)} />}
    </>
  );
}

function RecordDialog({ zone, record, onClose }: { zone: ZoneView; record: HostedRecord; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [r, setR] = useState<HostedRecord>({ ...record, name: record.id ? relative(record.name, zone.name) : record.name });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const set = (part: Partial<HostedRecord>) => setR({ ...r, ...part });
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const body = { name: r.name, type: r.type, content: r.content, priority: r.type === "MX" || r.type === "SRV" ? r.priority : 0, ttl: r.ttl };
      if (record.id) await api.put(`/dns/hosted/${zone.id}/records/${record.id}`, body);
      else await api.post(`/dns/hosted/${zone.id}/records`, body);
      qc.invalidateQueries({ queryKey: ["hosted-zone", zone.id] });
      onClose();
    } catch (err) {
      setError(errorMessage(t, err));
      setBusy(false);
    }
  };
  const hasPriority = r.type === "MX" || r.type === "SRV";
  return (
    <Dialog open onClose={onClose} title={record.id ? t("hosted.rec.edit") : t("hosted.rec.add")}>
      <form onSubmit={save} className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_auto]">
          <Field
            label={t("hosted.rec.name")}
            hint={t("hosted.rec.name_hint", { zone: zone.name })}
            value={r.name}
            onChange={(e) => set({ name: e.target.value })}
            placeholder="@"
            dir="ltr"
          />
          <label className="grid content-start gap-1.5">
            <span className="text-sm font-medium">{t("hosted.rec.type")}</span>
            <select value={r.type} onChange={(e) => set({ type: e.target.value })} className={select}>
              {recordTypes.map((ty) => (
                <option key={ty}>{ty}</option>
              ))}
            </select>
          </label>
        </div>
        <Field
          label={t("hosted.rec.content")}
          hint={t(`hosted.rec.hint_${r.type.toLowerCase()}`)}
          value={r.content}
          onChange={(e) => set({ content: e.target.value })}
          dir="ltr"
          className="[&_input]:font-mono"
          required
        />
        <div className="grid gap-4 sm:grid-cols-2">
          {hasPriority && (
            <Field
              label={t("hosted.rec.priority")}
              type="number"
              min={0}
              max={65535}
              value={r.priority}
              onChange={(e) => set({ priority: Number(e.target.value) })}
            />
          )}
          <Field
            label="TTL"
            hint={t("hosted.rec.ttl_hint")}
            type="number"
            min={60}
            max={604800}
            value={r.ttl}
            onChange={(e) => set({ ttl: Number(e.target.value) })}
          />
        </div>
        <div>
          <Button type="submit" variant="primary" loading={busy}>
            {t("common.save")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

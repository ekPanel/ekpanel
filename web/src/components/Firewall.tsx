import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Ban, DoorOpen, ShieldCheck, ShieldOff, Trash2 } from "lucide-react";
import { api, errorMessage, type Firewall, type FirewallRule } from "../lib/api";
import { formatRelative } from "../lib/format";
import { Badge, Button, Card, Dialog, ErrorAlert, Field, Spinner } from "./ui";

/** A port in the user's digits, without thousands separators. */
const port = (n: number, lng: string) => new Intl.NumberFormat(lng, { useGrouping: false }).format(n);

function portsText(r: FirewallRule, lng: string) {
  if (r.port_from == null) return null;
  const n = (x: number) => port(x, lng);
  return r.port_to != null && r.port_to !== r.port_from ? `${n(r.port_from)}–${n(r.port_to)}` : n(r.port_from);
}

function RuleText({ rule }: { rule: FirewallRule }) {
  const { t, i18n } = useTranslation();
  const ports = portsText(rule, i18n.language);
  const proto = t(`firewall.protocol.${rule.protocol}`);
  const source = rule.source || t("firewall.anywhere");
  if (rule.action === "allow") return <>{t("firewall.rule_allow", { ports, proto, source })}</>;
  return <>{ports ? t("firewall.rule_deny_ports", { ports, proto, source }) : t("firewall.rule_deny", { source })}</>;
}

function AddRuleForm({ serverId }: { serverId: string }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [action, setAction] = useState<"allow" | "deny">("allow");
  const [ports, setPorts] = useState("");
  const [protocol, setProtocol] = useState<FirewallRule["protocol"]>("tcp");
  const [source, setSource] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const fw = await api.post<Firewall>(`/servers/${serverId}/firewall/rules`, { action, ports, protocol: action === "deny" && !ports ? "both" : protocol, source, note });
      qc.setQueryData(["firewall", serverId], fw);
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setPorts("");
      setSource("");
      setNote("");
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";
  return (
    <form onSubmit={submit} className="grid gap-3 border-t border-line p-4">
      <h3 className="text-sm font-semibold">{t("firewall.add")}</h3>
      <ErrorAlert>{error}</ErrorAlert>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("firewall.action")}</span>
          <select value={action} onChange={(e) => setAction(e.target.value as "allow" | "deny")} className={select}>
            <option value="allow">{t("firewall.actions.allow")}</option>
            <option value="deny">{t("firewall.actions.deny")}</option>
          </select>
        </label>
        <Field
          label={action === "allow" ? t("firewall.ports") : t("firewall.ports_optional")}
          placeholder="8080, 6000-6010"
          value={ports}
          onChange={(e) => setPorts(e.target.value)}
          dir="ltr"
          inputMode="numeric"
          required={action === "allow"}
        />
        <label className="grid gap-1.5">
          <span className="text-sm font-medium">{t("firewall.protocol_label")}</span>
          <select value={protocol} onChange={(e) => setProtocol(e.target.value as FirewallRule["protocol"])} className={select} disabled={action === "deny" && !ports}>
            {(["tcp", "udp", "both"] as const).map((p) => (
              <option key={p} value={p}>
                {t(`firewall.protocol.${p}`)}
              </option>
            ))}
          </select>
        </label>
        <Field
          label={action === "allow" ? t("firewall.source_optional") : t("firewall.source")}
          placeholder={action === "allow" ? t("firewall.anywhere") : "203.0.113.9"}
          value={source}
          onChange={(e) => setSource(e.target.value)}
          dir="ltr"
          required={action === "deny"}
        />
      </div>
      <div className="flex flex-wrap items-end gap-3">
        <Field label={t("firewall.note")} value={note} onChange={(e) => setNote(e.target.value)} maxLength={200} className="min-w-0 flex-1" />
        <Button type="submit" variant="primary" loading={busy}>
          {t("firewall.add_button")}
        </Button>
      </div>
    </form>
  );
}

/** ekPanel's ufw firewall for one server: on/off, the ports that always stay open, and the user's rules. */
export function FirewallCard({ serverId }: { serverId: string }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["firewall", serverId], queryFn: () => api.get<Firewall>(`/servers/${serverId}/firewall`), refetchInterval: 10000 });
  const [error, setError] = useState("");
  const [confirming, setConfirming] = useState<Firewall["mode"] | null>(null);
  const [busy, setBusy] = useState(false);

  const setMode = async (mode: Firewall["mode"]) => {
    setBusy(true);
    setError("");
    try {
      qc.setQueryData(["firewall", serverId], await api.put<Firewall>(`/servers/${serverId}/firewall`, { mode }));
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setConfirming(null);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const remove = async (id: string) => {
    setError("");
    try {
      qc.setQueryData(["firewall", serverId], await api.del<Firewall>(`/servers/${serverId}/firewall/rules/${id}`));
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };

  const fw = q.data;
  const ssh = (fw?.status?.ssh_ports ?? [22]).map((p) => port(p, i18n.language)).join(", ");
  return (
    <Card className="overflow-hidden">
      <div className="flex flex-wrap items-center gap-3 border-b border-line px-4 py-3">
        <h2 className="font-semibold">{t("firewall.title")}</h2>
        {fw && fw.mode !== "unmanaged" && (
          <Badge tone={fw.mode === "on" && fw.status?.active && !fw.status.error ? "ok" : fw.status?.error ? "crit" : "neutral"}>
            {fw.mode === "on" ? (fw.status?.active ? t("firewall.state.active") : t("firewall.state.applying")) : t("firewall.state.off")}
          </Badge>
        )}
        <div className="ms-auto flex gap-2">
          {fw && fw.mode !== "on" && (
            <Button size="sm" variant="primary" onClick={() => setConfirming("on")}>
              <ShieldCheck className="size-4" aria-hidden />
              {t("firewall.turn_on")}
            </Button>
          )}
          {fw?.mode === "on" && (
            <Button size="sm" onClick={() => setConfirming("off")}>
              <ShieldOff className="size-4" aria-hidden />
              {t("firewall.turn_off")}
            </Button>
          )}
          {fw?.mode === "off" && (
            <Button size="sm" variant="ghost" onClick={() => setMode("unmanaged")} loading={busy}>
              {t("firewall.stop_managing")}
            </Button>
          )}
        </div>
      </div>
      {q.isPending && <Spinner />}
      {(q.error || error) && (
        <div className="px-4 pt-3">
          <ErrorAlert>{q.error ? errorMessage(t, q.error) : error}</ErrorAlert>
        </div>
      )}
      {fw && (
        <div className="grid gap-3 p-4 text-sm">
          {fw.mode === "unmanaged" && <p className="text-muted">{t("firewall.unmanaged")}</p>}
          {fw.mode === "off" && <p className="text-muted">{t("firewall.off")}</p>}
          {fw.mode === "on" && (
            <>
              <p className="text-muted">{t("firewall.always_open", { ssh })}</p>
              {fw.status?.error && <ErrorAlert>{fw.status.error}</ErrorAlert>}
              {fw.status && <p className="text-xs text-muted">{t("firewall.checked", { when: formatRelative(fw.status.at, i18n.language) })}</p>}
            </>
          )}
        </div>
      )}
      {fw && fw.mode !== "unmanaged" && (
        <>
          {fw.rules.length > 0 && (
            <ul className="divide-y divide-line border-t border-line">
              {fw.rules.map((r) => (
                <li key={r.id} className="flex items-center gap-3 px-4 py-2.5 text-sm">
                  {r.action === "allow" ? <DoorOpen className="size-4 shrink-0 text-ok" aria-hidden /> : <Ban className="size-4 shrink-0 text-crit" aria-hidden />}
                  <span className="min-w-0">
                    <span className="block">
                      <RuleText rule={r} />
                    </span>
                    {r.note && <span className="block truncate text-muted">{r.note}</span>}
                  </span>
                  <Button size="sm" variant="ghost" className="ms-auto" onClick={() => remove(r.id)} aria-label={t("firewall.remove")}>
                    <Trash2 className="size-4" aria-hidden />
                  </Button>
                </li>
              ))}
            </ul>
          )}
          <AddRuleForm serverId={serverId} />
        </>
      )}
      <Dialog open={confirming !== null} onClose={() => setConfirming(null)} title={confirming === "on" ? t("firewall.confirm_on_title") : t("firewall.confirm_off_title")}>
        <p className="text-muted">{confirming === "on" ? t("firewall.confirm_on", { ssh }) : t("firewall.confirm_off")}</p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => setConfirming(null)}>
            {t("common.cancel")}
          </Button>
          <Button variant={confirming === "on" ? "primary" : "danger"} loading={busy} onClick={() => confirming && setMode(confirming)}>
            {confirming === "on" ? t("firewall.turn_on") : t("firewall.turn_off")}
          </Button>
        </div>
      </Dialog>
    </Card>
  );
}

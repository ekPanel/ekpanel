import { useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, KeyRound, SquareTerminal, Trash2 } from "lucide-react";
import { api, ApiError, errorMessage, type EnrollResult, type MetricPoint, type Server } from "../lib/api";
import { formatBytes, formatDateTime, formatDuration, formatNumber, formatPercent, formatRate } from "../lib/format";
import { useServerLive } from "../lib/live";
import { PageHeader } from "../components/AppShell";
import { AreaChart, Sparkline } from "../components/Charts";
import { Badge, Button, Card, Dialog, ErrorAlert, Field, Meter, Spinner, cx } from "../components/ui";
import { ServicesCard } from "../components/Services";
import { RecentTasks } from "../components/Tasks";
import { FirewallCard } from "../components/Firewall";
import { ServerDatabases, ServerSites } from "./ServerSites";

const ranges = ["live", "1h", "24h", "7d", "30d"] as const;
type Range = (typeof ranges)[number];

function RangePicker({ value, onChange }: { value: Range; onChange: (r: Range) => void }) {
  const { t } = useTranslation();
  return (
    <div role="radiogroup" aria-label={t("metrics.range")} className="flex flex-wrap rounded-lg border border-line bg-surface p-0.5">
      {ranges.map((r) => (
        <button
          key={r}
          role="radio"
          aria-checked={value === r}
          onClick={() => onChange(r)}
          className={cx("rounded-md px-3 py-1 text-sm", value === r ? "bg-accent-soft font-semibold text-accent" : "text-muted hover:text-ink")}
        >
          {t(`metrics.ranges.${r}`)}
        </button>
      ))}
    </div>
  );
}
import { EnrollInstructions, ServerStateBadge } from "./ServerParts";

function Tile({ label, value, sub, children }: { label: string; value: string; sub?: string; children?: ReactNode }) {
  return (
    <Card className="grid gap-1 p-4">
      <span className="text-sm text-muted">{label}</span>
      <span className="font-mono text-xl font-medium tabular-nums">{value}</span>
      {sub && <span className="text-xs text-muted tabular-nums">{sub}</span>}
      {children && <div className="mt-2">{children}</div>}
    </Card>
  );
}

function ChartCard({ title, legend, children }: { title: string; legend?: ReactNode; children: ReactNode }) {
  return (
    <Card className="grid gap-4 p-5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="font-semibold">{title}</h2>
        {legend}
      </div>
      {children}
    </Card>
  );
}

function Legend({ items }: { items: { color: string; label: string }[] }) {
  return (
    <div className="flex gap-4 text-sm text-muted">
      {items.map((i) => (
        <span key={i.label} className="flex items-center gap-1.5">
          <span className="size-2.5 rounded-full" style={{ background: i.color }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}

export function ServerDetailPage() {
  const { id = "" } = useParams();
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const navigate = useNavigate();
  const qc = useQueryClient();
  const server = useQuery({ queryKey: ["server", id], queryFn: () => api.get<Server>(`/servers/${id}`), refetchInterval: 15000 });
  const live = useServerLive(id);
  const [enroll, setEnroll] = useState<EnrollResult | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [error, setError] = useState("");
  const [range, setRange] = useState<Range>("live");
  const history = useQuery({
    queryKey: ["metrics", id, range],
    queryFn: () => api.get<{ step_seconds: number; points: MetricPoint[] }>(`/servers/${id}/metrics?range=${range}`),
    enabled: range !== "live",
    refetchInterval: 60000,
  });

  if (server.isPending) return <Spinner />;
  if (server.error) {
    const notFound = server.error instanceof ApiError && server.error.status === 404;
    return (
      <div className="grid gap-4">
        <ErrorAlert>{notFound ? t("errors.server_not_found") : t("errors.internal")}</ErrorAlert>
        <Link to="/servers" className="text-accent hover:underline">
          {t("servers.back")}
        </Link>
      </div>
    );
  }

  const s = { ...server.data, online: live.connected ? live.online : server.data.online };
  const h = live.history;
  const m = s.online ? (h[h.length - 1] ?? s.latest) : null;
  // Live samples and stored history share the fields the charts use.
  const series: Pick<MetricPoint, "cpu_percent" | "mem_used" | "mem_total" | "net_rx_rate" | "net_tx_rate">[] =
    range === "live" ? h : (history.data?.points ?? []);
  const memTotal = m?.mem_total ?? series[series.length - 1]?.mem_total ?? 0;

  const newToken = async () => {
    setError("");
    try {
      setEnroll(await api.post<EnrollResult>(`/servers/${id}/enroll-token`));
      qc.invalidateQueries({ queryKey: ["servers"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };

  return (
    <>
      <Link to="/servers" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("nav.servers")}
      </Link>
      <PageHeader
        title={s.name}
        description={[s.hostname, s.os].filter(Boolean).join(" · ") || undefined}
        actions={
          <>
            <ServerStateBadge server={s} />
            {s.online && (
              <Link to={`/servers/${s.id}/terminal`} className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-line px-3 text-sm hover:bg-sunk">
                <SquareTerminal className="size-4" aria-hidden />
                {t("terminal.open")}
              </Link>
            )}
            {s.enrolled_at && (
              <Button size="sm" onClick={newToken}>
                <KeyRound className="size-4" aria-hidden />
                {t("servers.reenroll")}
              </Button>
            )}
            <Button size="sm" variant="danger" onClick={() => setConfirmDelete(true)}>
              <Trash2 className="size-4" aria-hidden />
              {t("common.delete")}
            </Button>
          </>
        }
      />
      <div className="grid gap-6">
        <ErrorAlert>{error}</ErrorAlert>

        {!s.enrolled_at && !enroll && (
          <Card className="grid gap-4 p-5">
            <h2 className="font-semibold">{t("servers.pending.title")}</h2>
            <p className="text-muted">{t("servers.pending.body")}</p>
            <div>
              <Button variant="primary" onClick={newToken}>
                <KeyRound className="size-4" aria-hidden />
                {t("servers.pending.show_command")}
              </Button>
            </div>
          </Card>
        )}
        {enroll && (
          <Card className="p-5">
            <EnrollInstructions result={enroll} />
          </Card>
        )}

        {s.enrolled_at && !s.online && (
          <ErrorAlert>{t("servers.offline_help", { when: s.last_seen_at ? formatDateTime(s.last_seen_at, lng) : t("servers.never") })}</ErrorAlert>
        )}

        {m && (
          <>
            <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
              <Tile label={t("metrics.cpu")} value={formatPercent(m.cpu_percent, lng)} sub={t("metrics.cores", { count: s.cpu_count, formatted: formatNumber(s.cpu_count, lng) })}>
                <Sparkline values={h.slice(-60).map((x) => x.cpu_percent)} max={100} className="h-7 w-full" />
              </Tile>
              <Tile label={t("metrics.memory")} value={formatBytes(m.mem_used, lng)} sub={t("metrics.of_total", { total: formatBytes(m.mem_total, lng) })}>
                <Meter percent={m.mem_total ? (m.mem_used / m.mem_total) * 100 : 0} label={t("metrics.memory")} />
              </Tile>
              <Tile label={t("metrics.disk")} value={formatBytes(m.disk_used, lng)} sub={t("metrics.of_total", { total: formatBytes(m.disk_total, lng) })}>
                <Meter percent={m.disk_total ? (m.disk_used / m.disk_total) * 100 : 0} label={t("metrics.disk")} />
              </Tile>
              <Tile label={t("metrics.uptime")} value={formatDuration(m.uptime_seconds, lng)} sub={t("metrics.load", { l1: formatNumber(m.load1, lng, 2), l5: formatNumber(m.load5, lng, 2), l15: formatNumber(m.load15, lng, 2) })} />
            </div>
          </>
        )}

        {s.enrolled_at && (
          <>
            <div className="flex flex-wrap items-center justify-between gap-3">
              <RangePicker value={range} onChange={setRange} />
              <p className="text-sm text-muted">
                {range === "live" ? (
                  <>
                    {live.connected ? <Badge tone="accent">{t("servers.live")}</Badge> : <Badge>{t("servers.reconnecting")}</Badge>} {t("servers.history_note")}
                  </>
                ) : (
                  t("metrics.history_step", { count: Math.round((history.data?.step_seconds ?? 60) / 60) })
                )}
              </p>
            </div>
            {range !== "live" && history.isPending && <Spinner />}
            {range !== "live" && history.data?.points.length === 0 && <p className="text-muted">{t("metrics.no_history")}</p>}
            {(range === "live" || (history.data?.points.length ?? 0) > 0) && (
              <div className="grid gap-4 xl:grid-cols-2">
                <ChartCard title={t("metrics.cpu")}>
                  <AreaChart series={[{ values: series.map((x) => x.cpu_percent), color: "var(--accent)", label: t("metrics.cpu") }]} max={100} formatY={(v) => formatPercent(v, lng)} />
                </ChartCard>
                <ChartCard title={t("metrics.memory")}>
                  <AreaChart
                    series={[{ values: series.map((x) => x.mem_used), color: "var(--amber)", label: t("metrics.memory") }]}
                    max={memTotal || undefined}
                    formatY={(v) => formatBytes(v, lng)}
                  />
                </ChartCard>
                <ChartCard
                  title={t("metrics.network")}
                  legend={<Legend items={[{ color: "var(--accent)", label: t("metrics.download") }, { color: "var(--amber)", label: t("metrics.upload") }]} />}
                >
                  <AreaChart
                    series={[
                      { values: series.map((x) => x.net_rx_rate), color: "var(--accent)", label: t("metrics.download") },
                      { values: series.map((x) => x.net_tx_rate), color: "var(--amber)", label: t("metrics.upload") },
                    ]}
                    formatY={(v) => formatRate(v, lng)}
                  />
                </ChartCard>
                <ChartCard title={t("servers.facts.title")}>
                  <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
                    {(
                      [
                        ["hostname", s.hostname],
                        ["os", s.os],
                        ["kernel", s.kernel],
                        ["arch", s.arch],
                        ["agent", s.agent_version],
                        ["enrolled", s.enrolled_at ? formatDateTime(s.enrolled_at, lng) : ""],
                      ] as const
                    ).map(([k, v]) => (
                      <div key={k} className="contents">
                        <dt className="text-muted">{t(`servers.facts.${k}`)}</dt>
                        <dd className="truncate font-mono" dir="auto">{v || "—"}</dd>
                      </div>
                    ))}
                  </dl>
                </ChartCard>
              </div>
            )}
            <div className="grid gap-4 xl:grid-cols-2">
              <ServerSites serverId={s.id} />
              <ServerDatabases serverId={s.id} />
            </div>
            <div className="grid gap-4 xl:grid-cols-2">
              <ServicesCard serverId={s.id} online={s.online} />
              <RecentTasks serverId={s.id} />
            </div>
            <FirewallCard serverId={s.id} />
          </>
        )}
      </div>

      <DeleteServerDialog
        open={confirmDelete}
        server={s}
        onClose={() => setConfirmDelete(false)}
        onDeleted={() => {
          qc.invalidateQueries({ queryKey: ["servers"] });
          navigate("/servers", { replace: true });
        }}
      />
    </>
  );
}

/** Deleting asks for the server's name so it cannot happen by a stray click. */
function DeleteServerDialog({ open, server, onClose, onDeleted }: { open: boolean; server: Server; onClose: () => void; onDeleted: () => void }) {
  const { t } = useTranslation();
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <Dialog open={open} onClose={onClose} title={t("servers.delete.title", { name: server.name })}>
      <form
        className="grid gap-5"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          try {
            await api.del(`/servers/${server.id}`);
            onDeleted();
          } catch (err) {
            setError(errorMessage(t, err));
            setBusy(false);
          }
        }}
      >
        <p className="text-muted">{t("servers.delete.body")}</p>
        <ErrorAlert>{error}</ErrorAlert>
        <Field label={t("servers.delete.confirm", { name: server.name })} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="danger" loading={busy} disabled={typed !== server.name}>
            {t("servers.delete.submit")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

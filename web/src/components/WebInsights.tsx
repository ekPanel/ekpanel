import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { Pause, Play } from "lucide-react";
import { api, errorMessage, type AccessLogEntry, type Traffic } from "../lib/api";
import { formatBytes, formatNumber, formatPercent } from "../lib/format";
import { AreaChart } from "./Charts";
import { Badge, Button, Card, ErrorAlert, Spinner, cx, type Tone } from "./ui";

type Range = "24h" | "7d" | "30d";

/** Requests, bandwidth, status codes and top pages of a website or app. */
export function TrafficCard({ base }: { base: string }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const [range, setRange] = useState<Range>("24h");
  const q = useQuery({ queryKey: ["traffic", base, range], queryFn: () => api.get<Traffic>(`${base}/traffic?range=${range}`), refetchInterval: 60000 });

  // Fill hours or days without requests, so the chart has an even time axis.
  const values = useMemo(() => {
    if (!q.data) return [];
    const hourly = range === "24h";
    const step = hourly ? 3600e3 : 86400e3;
    const count = hourly ? 24 : range === "7d" ? 7 : 30;
    const now = Date.now();
    const end = Math.floor(now / step) * step;
    const byTime = new Map(q.data.points.map((p) => [new Date(p.at).getTime(), p.requests]));
    return Array.from({ length: count }, (_, i) => byTime.get(end - (count - 1 - i) * step) ?? 0);
  }, [q.data, range]);

  const d = q.data;
  const errors = d ? d.status[2] + d.status[3] : 0;
  return (
    <Card className="grid gap-4 p-5">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="font-semibold">{t("traffic.title")}</h2>
        <div role="radiogroup" aria-label={t("traffic.range")} className="ms-auto flex rounded-lg border border-line p-0.5 text-sm">
          {(["24h", "7d", "30d"] as const).map((r) => (
            <button
              key={r}
              role="radio"
              aria-checked={range === r}
              onClick={() => setRange(r)}
              className={cx("rounded-md px-2.5 py-1", range === r ? "bg-accent-soft text-accent" : "text-muted hover:text-ink")}
            >
              {t(`traffic.ranges.${r}`)}
            </button>
          ))}
        </div>
      </div>
      {q.isPending && <Spinner />}
      {q.error && <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>}
      {d && (
        <>
          <dl className="grid grid-cols-3 gap-3 text-sm">
            <div>
              <dt className="text-muted">{t("traffic.requests")}</dt>
              <dd className="text-lg font-semibold tabular-nums">{formatNumber(d.requests, lng)}</dd>
            </div>
            <div>
              <dt className="text-muted">{t("traffic.bandwidth")}</dt>
              <dd className="text-lg font-semibold tabular-nums">{formatBytes(d.bytes, lng)}</dd>
            </div>
            <div>
              <dt className="text-muted">{t("traffic.errors")}</dt>
              <dd className={cx("text-lg font-semibold tabular-nums", errors > 0 && "text-warn")}>
                {d.requests ? formatPercent((errors / d.requests) * 100, lng) : "—"}
              </dd>
            </div>
          </dl>
          {d.requests === 0 ? (
            <p className="text-sm text-muted">{t("traffic.none")}</p>
          ) : (
            <>
              <AreaChart series={[{ values, color: "var(--accent)", label: t("traffic.requests") }]} formatY={(v) => formatNumber(v, lng)} height={120} />
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="grid content-start gap-1 text-sm">
                  <h3 className="font-medium">{t("traffic.status")}</h3>
                  {(["2xx", "3xx", "4xx", "5xx"] as const).map((k, i) => (
                    <div key={k} className="flex justify-between">
                      <span className="text-muted">{t(`traffic.status_names.${k}`)}</span>
                      <span className="tabular-nums">{formatNumber(d.status[i], lng)}</span>
                    </div>
                  ))}
                </div>
                <div className="grid content-start gap-1 text-sm">
                  <h3 className="font-medium">{t("traffic.top_paths")}</h3>
                  {d.paths.map((p) => (
                    <div key={p.path} className="flex justify-between gap-3">
                      <span className="truncate font-mono text-xs" dir="ltr" title={p.path}>
                        {p.path}
                      </span>
                      <span className="tabular-nums">{formatNumber(p.requests, lng)}</span>
                    </div>
                  ))}
                </div>
              </div>
            </>
          )}
          <p className="text-xs text-muted">{t("traffic.delay")}</p>
        </>
      )}
    </Card>
  );
}

const statusTone = (s: number): Tone => (s >= 500 ? "crit" : s >= 400 ? "warn" : s >= 300 ? "neutral" : "ok");

/** The newest requests, and for websites the PHP error log, with a live mode. */
export function LogsCard({ base, php }: { base: string; php: boolean }) {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const [kind, setKind] = useState<"access" | "php">("access");
  const [filter, setFilter] = useState<"all" | "2" | "3" | "4" | "5">("all");
  const [search, setSearch] = useState("");
  const [live, setLive] = useState(false);
  const [entries, setEntries] = useState<AccessLogEntry[]>([]);
  const [lines, setLines] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const cursor = useRef(0);

  useEffect(() => {
    let stop = false;
    cursor.current = 0;
    setEntries([]);
    setLines([]);
    setLoading(true);
    const load = async () => {
      try {
        if (kind === "access") {
          const r = await api.get<{ entries: AccessLogEntry[]; cursor: number }>(
            `${base}/${php ? "logs?kind=access&" : "access-log?"}cursor=${cursor.current}&lines=300`,
          );
          if (stop) return;
          setEntries((old) => (cursor.current ? [...old, ...r.entries] : r.entries).slice(-1000));
          cursor.current = r.cursor;
        } else {
          const r = await api.get<{ lines: string[]; cursor: number }>(`${base}/logs?kind=php&cursor=${cursor.current}`);
          if (stop) return;
          setLines((old) => (cursor.current ? [...old, ...r.lines] : r.lines).slice(-1000));
          cursor.current = r.cursor;
        }
        setError("");
      } catch (err) {
        if (!stop) setError(errorMessage(t, err));
      } finally {
        if (!stop) setLoading(false);
      }
    };
    load();
    const timer = live ? window.setInterval(load, 3000) : undefined;
    return () => {
      stop = true;
      window.clearInterval(timer);
    };
  }, [base, kind, live, php, t]);

  const needle = search.trim().toLowerCase();
  const shown = entries
    .filter((e) => filter === "all" || String(e.status)[0] === filter)
    .filter((e) => !needle || `${e.ip} ${e.method} ${e.uri} ${e.status} ${e.user_agent ?? ""}`.toLowerCase().includes(needle))
    .slice()
    .reverse();
  const shownLines = lines
    .filter((l) => !needle || l.toLowerCase().includes(needle))
    .slice()
    .reverse();
  const time = new Intl.DateTimeFormat(lng, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });

  return (
    <Card className="overflow-hidden">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2.5">
        <h2 className="me-2 font-semibold">{t("weblogs.title")}</h2>
        {php && (
          <div role="radiogroup" aria-label={t("weblogs.which")} className="flex rounded-lg border border-line p-0.5 text-sm">
            {(["access", "php"] as const).map((k) => (
              <button
                key={k}
                role="radio"
                aria-checked={kind === k}
                onClick={() => setKind(k)}
                className={cx("rounded-md px-2.5 py-1", kind === k ? "bg-accent-soft text-accent" : "text-muted hover:text-ink")}
              >
                {t(`weblogs.kinds.${k}`)}
              </button>
            ))}
          </div>
        )}
        {kind === "access" && (
          <select
            value={filter}
            onChange={(e) => setFilter(e.target.value as typeof filter)}
            className="h-8 rounded-lg border border-line bg-surface px-2 text-sm"
            aria-label={t("weblogs.filter")}
          >
            <option value="all">{t("weblogs.all")}</option>
            {(["2", "3", "4", "5"] as const).map((c) => (
              <option key={c} value={c}>
                {t(`traffic.status_names.${c}xx`)}
              </option>
            ))}
          </select>
        )}
        <input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder={t("weblogs.search")}
          className="h-8 min-w-0 flex-1 rounded-lg border border-line bg-surface px-2 text-sm"
          aria-label={t("weblogs.search")}
        />
        <Button size="sm" variant={live ? "primary" : "secondary"} onClick={() => setLive((v) => !v)}>
          {live ? <Pause className="size-4" aria-hidden /> : <Play className="size-4" aria-hidden />}
          {t("weblogs.live")}
        </Button>
      </div>
      {error && (
        <div className="p-4">
          <ErrorAlert>{error}</ErrorAlert>
        </div>
      )}
      {loading && <Spinner />}
      {!loading && kind === "access" && (
        <div className="max-h-[28rem] overflow-auto">
          {shown.length === 0 ? (
            <p className="px-4 py-6 text-center text-sm text-muted">{t("weblogs.empty")}</p>
          ) : (
            <table className="w-full min-w-[640px] text-xs">
              <tbody>
                {shown.map((e, i) => (
                  <tr key={i} className="border-b border-line last:border-0 hover:bg-sunk/60" title={e.user_agent}>
                    <td className="px-3 py-1.5 whitespace-nowrap text-muted tabular-nums">{time.format(new Date(e.time))}</td>
                    <td className="px-2 py-1.5 font-mono" dir="ltr">
                      {e.ip}
                    </td>
                    <td className="px-2 py-1.5">
                      <Badge tone={statusTone(e.status)}>{e.status}</Badge>
                    </td>
                    <td className="max-w-[24rem] truncate px-2 py-1.5 font-mono" dir="ltr">
                      {e.method} {e.uri}
                    </td>
                    <td className="px-2 py-1.5 text-end whitespace-nowrap text-muted tabular-nums">{formatBytes(e.size, lng)}</td>
                    <td className="px-3 py-1.5 text-end whitespace-nowrap text-muted tabular-nums">{formatNumber(e.duration_ms, lng)} ms</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
      {!loading && kind === "php" && (
        <pre className="max-h-[28rem] overflow-auto bg-sunk p-4 font-mono text-xs whitespace-pre-wrap" dir="ltr">
          {shownLines.length ? shownLines.join("\n") : t("weblogs.php_empty")}
        </pre>
      )}
    </Card>
  );
}

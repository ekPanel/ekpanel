import { useId } from "react";

/** A tiny trend line for stat tiles. Values are plotted against max (or their own peak). */
export function Sparkline({ values, max, className }: { values: number[]; max?: number; className?: string }) {
  const gid = useId();
  if (values.length < 2) return <div className={className} />;
  const top = max ?? Math.max(...values, 1);
  const w = 100;
  const h = 28;
  const pts = values.map((v, i) => [(i / (values.length - 1)) * w, h - (Math.min(v, top) / top) * (h - 2) - 1]);
  const line = pts.map(([x, y]) => `${x.toFixed(2)},${y.toFixed(2)}`).join(" ");
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className={className} aria-hidden>
      <defs>
        <linearGradient id={gid} x1="0" x2="0" y1="0" y2="1">
          <stop offset="0" stopColor="var(--accent)" stopOpacity="0.25" />
          <stop offset="1" stopColor="var(--accent)" stopOpacity="0" />
        </linearGradient>
      </defs>
      <polygon points={`0,${h} ${line} ${w},${h}`} fill={`url(#${gid})`} />
      <polyline points={line} fill="none" stroke="var(--accent)" strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

export interface Series {
  values: number[];
  color: string;
  label: string;
}

/**
 * Area chart for the server detail page. One shared y scale from 0 to max, with three
 * gridlines labelled by formatY. Mirrors cleanly in RTL because it has no text inside the plot.
 */
export function AreaChart({ series, max, formatY, height = 160 }: { series: Series[]; max?: number; formatY: (v: number) => string; height?: number }) {
  const gid = useId();
  const n = Math.max(...series.map((s) => s.values.length), 0);
  const peak = max ?? Math.max(1, ...series.flatMap((s) => s.values));
  const w = 600;
  const h = height;
  const x = (i: number) => (n <= 1 ? w : (i / (n - 1)) * w);
  const y = (v: number) => h - (Math.min(v, peak) / peak) * (h - 4) - 2;
  const grid = [0.25, 0.5, 0.75, 1];

  return (
    <div className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3">
      <div className="relative text-end text-[11px] text-muted tabular-nums" style={{ height: h }} aria-hidden>
        {/* The labels are positioned absolutely; this invisible copy of the widest one gives the column its width. */}
        <span className="invisible block h-0 whitespace-nowrap">{formatY(peak)}</span>
        {grid.map((g) => (
          <span key={g} className="absolute end-0 -translate-y-1/2 whitespace-nowrap" style={{ top: y(peak * g) }}>
            {formatY(peak * g)}
          </span>
        ))}
      </div>
      <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="w-full" style={{ height: h }} role="img" aria-label={series.map((s) => s.label).join(", ")}>
        {grid.map((g) => (
          <line key={g} x1="0" x2={w} y1={y(peak * g)} y2={y(peak * g)} stroke="var(--line)" strokeDasharray="3 4" vectorEffect="non-scaling-stroke" />
        ))}
        <line x1="0" x2={w} y1={h - 1} y2={h - 1} stroke="var(--line)" vectorEffect="non-scaling-stroke" />
        {series.map((s, si) => {
          if (s.values.length < 2) return null;
          const line = s.values.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ");
          return (
            <g key={si}>
              <defs>
                <linearGradient id={`${gid}-${si}`} x1="0" x2="0" y1="0" y2="1">
                  <stop offset="0" stopColor={s.color} stopOpacity="0.22" />
                  <stop offset="1" stopColor={s.color} stopOpacity="0" />
                </linearGradient>
              </defs>
              <polygon points={`0,${h} ${line} ${x(s.values.length - 1)},${h}`} fill={`url(#${gid}-${si})`} />
              <polyline points={line} fill="none" stroke={s.color} strokeWidth="2" vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
            </g>
          );
        })}
      </svg>
    </div>
  );
}

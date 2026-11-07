// Locale-aware formatting. Everything goes through Intl so numbers, dates and units follow
// the user's language (for example Bengali digits in Bangla).

const nf = (lng: string, opts?: Intl.NumberFormatOptions) => new Intl.NumberFormat(lng, opts);

export function formatNumber(n: number, lng: string, maxFraction = 0): string {
  return nf(lng, { maximumFractionDigits: maxFraction }).format(n);
}

export function formatPercent(value: number, lng: string): string {
  return nf(lng, { style: "percent", maximumFractionDigits: value < 10 ? 1 : 0 }).format(value / 100);
}

const byteUnits = ["byte", "kilobyte", "megabyte", "gigabyte", "terabyte", "petabyte"] as const;

/** Binary sizes (1 GB = 1024³ bytes), labelled with the locale's own unit names. */
export function formatBytes(bytes: number, lng: string): string {
  let i = 0;
  let v = bytes;
  while (v >= 1024 && i < byteUnits.length - 1) {
    v /= 1024;
    i++;
  }
  return nf(lng, {
    style: "unit",
    unit: byteUnits[i],
    unitDisplay: "short",
    maximumFractionDigits: v < 10 && i > 0 ? 1 : 0,
  }).format(v);
}

const bitUnits = ["bit-per-second", "kilobit-per-second", "megabit-per-second", "gigabit-per-second"] as const;

/** Network rates are shown in bits per second (1 Mbps = 10⁶ bit/s), as ISPs quote them. */
export function formatRate(bytesPerSecond: number, lng: string): string {
  let i = 0;
  let v = bytesPerSecond * 8;
  while (v >= 1000 && i < bitUnits.length - 1) {
    v /= 1000;
    i++;
  }
  return nf(lng, { style: "unit", unit: bitUnits[i], unitDisplay: "short", maximumFractionDigits: v < 10 ? 1 : 0 }).format(v);
}

export function formatDateTime(iso: string, lng: string): string {
  return new Intl.DateTimeFormat(lng, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso));
}

export function formatTime(iso: string, lng: string): string {
  return new Intl.DateTimeFormat(lng, { timeStyle: "medium" }).format(new Date(iso));
}

/** "3 minutes ago", "yesterday", in the user's language. */
export function formatRelative(iso: string, lng: string, now = Date.now()): string {
  const seconds = Math.round((new Date(iso).getTime() - now) / 1000);
  const rtf = new Intl.RelativeTimeFormat(lng, { numeric: "auto" });
  const steps: [Intl.RelativeTimeFormatUnit, number][] = [
    ["second", 60],
    ["minute", 60],
    ["hour", 24],
    ["day", 30],
    ["month", 12],
    ["year", Infinity],
  ];
  let v = seconds;
  for (const [unit, size] of steps) {
    if (Math.abs(v) < size) return rtf.format(Math.round(v), unit);
    v /= size;
  }
  return rtf.format(Math.round(v), "year");
}

/** Uptime as the two largest units, e.g. "12 days 4 hr". */
export function formatDuration(totalSeconds: number, lng: string): string {
  const parts: [number, string][] = [
    [Math.floor(totalSeconds / 86400), "day"],
    [Math.floor((totalSeconds % 86400) / 3600), "hour"],
    [Math.floor((totalSeconds % 3600) / 60), "minute"],
  ];
  const shown = parts.filter(([v]) => v > 0).slice(0, 2);
  if (shown.length === 0) shown.push([0, "minute"]);
  return shown.map(([v, unit]) => nf(lng, { style: "unit", unit, unitDisplay: "short" }).format(v)).join(" ");
}

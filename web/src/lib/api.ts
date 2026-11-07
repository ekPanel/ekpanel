import type { TFunction } from "i18next";

export interface User {
  id: string;
  email: string;
  name: string;
  role: "admin" | "reseller" | "customer";
  locale: string;
  totp_enabled: boolean;
  created_at: string;
  package_id: string | null;
  /** Set while an administrator is signed in as this user. */
  impersonator_id?: string;
  /** Until when this session may open terminals without the password again. */
  confirmed_until?: string;
}

export interface Package {
  id: string;
  name: string;
  max_sites: number;
  max_databases: number;
  max_apps: number;
  max_cron_jobs: number;
  disk_mb: number | null;
  memory_mb: number | null;
  cpu_percent: number | null;
  php_versions: string[];
  app_images: string[];
  server_ids: string[];
  sftp: boolean;
  customers: number;
  created_at: string;
}

export interface Usage {
  sites: number;
  databases: number;
  apps: number;
  cron_jobs: number;
  disk_bytes: number;
}

export interface Customer extends User {
  package_name: string;
  usage: Usage;
}

/** Who owns a website, database or app; null means the administrators. */
export interface Owned {
  owner_id: string | null;
  owner_name: string | null;
}

export interface Metrics {
  at: string;
  cpu_percent: number;
  load1: number;
  load5: number;
  load15: number;
  mem_total: number;
  mem_used: number;
  swap_total: number;
  swap_used: number;
  disk_total: number;
  disk_used: number;
  net_rx_rate: number;
  net_tx_rate: number;
  uptime_seconds: number;
}

export interface Server {
  id: string;
  name: string;
  enrolled_at: string | null;
  enroll_expires_at: string | null;
  hostname: string;
  os: string;
  kernel: string;
  arch: string;
  cpu_count: number;
  agent_version: string;
  last_seen_at: string | null;
  created_at: string;
  online: boolean;
  latest: Metrics | null;
}

export interface EnrollResult {
  server: Server;
  enroll_token: string;
  /** Fingerprint of the panel's agent CA; the agent refuses any other panel. */
  ca_pin: string;
  agent_port: number;
}

export interface MetricPoint {
  at: string;
  cpu_percent: number;
  load1: number;
  mem_used: number;
  mem_total: number;
  disk_used: number;
  disk_total: number;
  net_rx_rate: number;
  net_tx_rate: number;
}

export interface ServiceInfo {
  name: string;
  installed: boolean;
  active_state: string;
  sub_state: string;
  since: string | null;
  restartable: boolean;
}

export type TaskStatus = "queued" | "running" | "succeeded" | "failed";

export interface Task {
  id: string;
  kind: string;
  server_id: string | null;
  server_name: string | null;
  params: Record<string, unknown>;
  status: TaskStatus;
  attempts: number;
  max_attempts: number;
  result: unknown;
  error: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export type SiteState = "pending" | "active" | "error" | "deleting";

export interface Site extends Owned {
  id: string;
  server_id: string;
  server_name: string;
  domain: string;
  aliases: string[];
  /** Unicode forms of the host names, for display. */
  domain_display: string;
  aliases_display: string[];
  kind: "static" | "php";
  php_version: string | null;
  tls: "auto" | "internal";
  linux_user: string;
  sftp_enabled: boolean;
  app: "wordpress" | null;
  app_version: string | null;
  ssh_keys: string[];
  state: SiteState;
  message: string | null;
  generation: number;
  applied_generation: number;
  created_at: string;
  updated_at: string;
}

export interface Database extends Owned {
  id: string;
  server_id: string;
  server_name: string;
  engine: "mariadb" | "postgresql";
  name: string;
  username: string;
  state: SiteState;
  message: string | null;
  size_bytes: number | null;
  generation: number;
  applied_generation: number;
  created_at: string;
  updated_at: string;
}

/** Returned when a password is created or reset. The password is shown only this once. */
export interface DatabaseWithPassword {
  database: Database;
  password: string;
}

export interface FileEntry {
  name: string;
  type: "file" | "dir" | "link";
  size: number;
  mode: string;
  modified: string;
  target?: string;
}

/** Result of a file action. `data` is base64 (Go []byte). */
export interface FilesResult {
  entries?: FileEntry[];
  data?: string;
  size?: number;
  eof?: boolean;
  path?: string;
  count?: number;
}

export type BackupScheduleKind = "off" | "hourly" | "daily" | "weekly";

export interface BackupRepo {
  id: string;
  server_id: string;
  server_name: string;
  name: string;
  kind: "local" | "s3";
  location: string;
  schedule: BackupScheduleKind;
  hour_utc: number;
  keep_daily: number;
  keep_weekly: number;
  keep_monthly: number;
  next_run_at: string | null;
  last_run_at: string | null;
  last_status: "succeeded" | "partial" | "failed" | null;
  last_message: string | null;
  created_at: string;
}

export interface BackupSnapshot {
  id: string;
  time: string;
  kind: "site" | "database";
  item_id: string;
  name: string;
}

export interface App extends Owned {
  id: string;
  server_id: string;
  server_name: string;
  name: string;
  kind: "image" | "compose";
  image: string | null;
  compose: string | null;
  service: string | null;
  container_port: number | null;
  host_port: number | null;
  domain: string | null;
  domain_display: string;
  tls: "auto" | "internal";
  env: { key: string; value: string }[];
  volumes: string[];
  memory_mb: number | null;
  cpus: number | null;
  public_ports: boolean;
  stopped: boolean;
  state: "pending" | "active" | "error" | "deleting";
  message: string | null;
  generation: number;
  applied_generation: number;
  created_at: string;
  updated_at: string;
}

export interface AppContainer {
  service: string;
  name: string;
  image: string;
  state: string;
  status: string;
  health?: string;
}

export interface CronJob {
  id: string;
  server_id: string;
  site_id: string | null;
  app_id: string | null;
  service: string | null;
  name: string;
  schedule: string;
  timezone: string;
  command: string;
  timeout_seconds: number;
  enabled: boolean;
  target_name: string;
  owner_id: string | null;
  state: "pending" | "active" | "error" | "deleting";
  message: string | null;
  generation: number;
  applied_generation: number;
  next_run: string | null;
}

export interface CronStatus {
  running: boolean;
  last_run?: string;
  last_end?: string;
  exit_code?: number;
  result?: string;
  next_run?: string;
  output: string;
}

export interface AccessLogEntry {
  time: string;
  ip: string;
  method: string;
  host: string;
  uri: string;
  proto: string;
  status: number;
  size: number;
  duration_ms: number;
  user_agent?: string;
  referer?: string;
}

export interface Traffic {
  points: { at: string; requests: number; bytes: number }[];
  requests: number;
  bytes: number;
  status: [number, number, number, number];
  paths: { path: string; requests: number }[];
}

export interface NotificationItem {
  id: number;
  event: string;
  severity: "info" | "warning" | "critical";
  title: string;
  body: string;
  link: string;
  created_at: string;
  read_at: string | null;
}

export type ChannelKind = "email" | "telegram" | "discord" | "slack" | "webhook";

export interface Channel {
  id: string;
  kind: ChannelKind;
  name: string;
  config: Record<string, string>;
  events: string[];
  enabled: boolean;
  last_sent_at: string | null;
  last_error: string | null;
  created_at: string;
  /** The webhook signing key, only right after it was made. */
  key?: string;
}

export interface FirewallRule {
  id: string;
  action: "allow" | "deny";
  protocol: "tcp" | "udp" | "both";
  port_from: number | null;
  port_to: number | null;
  source: string;
  note: string;
  created_at: string;
}

export interface Firewall {
  mode: "unmanaged" | "on" | "off";
  status: { active: boolean; ssh_ports: number[]; error?: string; at: string } | null;
  rules: FirewallRule[];
}

export interface AuditEntry {
  id: number;
  at: string;
  actor_id: string | null;
  actor_name: string | null;
  actor_ip: string;
  action: string;
  target: string;
  details: Record<string, unknown>;
}

/** An error response from the panel. `code` is stable; translate it with errorMessage. */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  let res: Response;
  try {
    res = await fetch("/api/v1" + path, {
      method,
      credentials: "same-origin",
      headers: body === undefined ? undefined : { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, "network", "could not reach the panel");
  }
  if (res.status === 204) return undefined as T;
  let data: unknown = null;
  try {
    data = await res.json();
  } catch {
    // non-JSON body (e.g. a proxy error page)
  }
  if (!res.ok) {
    const e = (data ?? {}) as { code?: string; message?: string };
    throw new ApiError(res.status, e.code ?? "internal", e.message ?? res.statusText);
  }
  return data as T;
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body ?? {}),
  patch: <T>(path: string, body: unknown) => request<T>("PATCH", path, body),
  put: <T>(path: string, body: unknown) => request<T>("PUT", path, body),
  del: <T = void>(path: string) => request<T>("DELETE", path),
};

/** Turns any thrown error into a sentence in the user's language. */
export function errorMessage(t: TFunction, err: unknown): string {
  if (err instanceof ApiError) {
    return t(`errors.${err.code}`, { defaultValue: err.message });
  }
  return t("errors.internal");
}

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
  /** Set while an administrator has suspended this customer. */
  suspended_at?: string;
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
  git_apps: boolean;
  max_mail_domains: number;
  max_mailboxes: number;
  /** Largest size of one mailbox; null means no limit. */
  mailbox_quota_mb: number | null;
  ftp: boolean;
  /** Bandwidth per calendar month (UTC); null means no limit. */
  bandwidth_gb: number | null;
  /** Stop websites and apps when the bandwidth is used up. */
  bandwidth_stop: boolean;
  customers: number;
  created_at: string;
}

export interface Usage {
  sites: number;
  databases: number;
  apps: number;
  cron_jobs: number;
  mail_domains: number;
  mailboxes: number;
  disk_bytes: number;
}

export interface Customer extends User {
  package_name: string;
  usage: Usage;
  /** Why the customer is suspended; only administrators see it. */
  suspend_reason: string | null;
  /** Websites and apps are stopped for using up this month's bandwidth. */
  bandwidth_blocked: boolean;
  /** Bytes served this month (UTC). */
  bandwidth_month: number;
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
  /** The server that runs the panel itself. */
  panel_host: boolean;
  last_seen_at: string | null;
  created_at: string;
  online: boolean;
  latest: Metrics | null;
  addresses: string[];
  public_ipv4: string | null;
  public_ipv6: string | null;
}

export interface CloudflareAccount {
  id: string;
  name: string;
  account_id: string | null;
  account_name: string | null;
  zones: number;
  created_at: string;
}

export interface DNSIssue {
  kind: "changed" | "conflict" | "no_address";
  name: string;
  type: string;
  want?: string;
  have?: string;
  server?: string;
}

export interface DNSZone {
  id: string;
  account_id: string;
  account_name: string;
  owner_id: string | null;
  cf_zone_id: string;
  name: string;
  status: string;
  name_servers: string[];
  proxied: boolean;
  issues: DNSIssue[];
  sync_error: string | null;
  synced_at: string | null;
  records: number;
  created_at: string;
}

export interface DNSRecord {
  id: string;
  type: string;
  name: string;
  content: string;
  ttl: number;
  proxied: boolean;
  proxiable?: boolean;
  priority?: number | null;
  comment?: string;
  managed: boolean;
}

export interface EnrollResult {
  server: Server;
  enroll_token: string;
  /** Fingerprint of the panel's agent CA; the agent refuses any other panel. */
  ca_pin: string;
  agent_port: number;
  /** The installer of the panel's release: one command sets up the server. Absent for builds from source. */
  install_url?: string;
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
  tls: "auto" | "internal" | "custom";
  web: SiteWeb;
  tls_expires_at?: string | null;
  tls_names?: string[] | null;
  linux_user: string;
  sftp_enabled: boolean;
  ftp_enabled: boolean;
  app: "wordpress" | "joomla" | "drupal" | "moodle" | "laravel" | null;
  app_version: string | null;
  ssh_keys: string[];
  state: SiteState;
  message: string | null;
  generation: number;
  applied_generation: number;
  /** What the last WordPress scan found, and when. */
  wordpress: WordPressInfo | null;
  wp_scanned_at: string | null;
  wp_auto: WordPressAuto;
  /** For a staging copy: the live site it copies. */
  staging_of: string | null;
  created_at: string;
  updated_at: string;
}

/** A site's redirects, hotlink protection and PHP settings (agentproto.SiteWeb). */
export interface SiteWeb {
  alias_redirect?: boolean;
  redirects?: { from: string; to: string; code: number; keep_path?: boolean }[];
  hotlink?: { extensions: string[]; allowed?: string[] } | null;
  php?: Record<string, string>;
  doc_root?: string;
  block_xmlrpc?: boolean;
  no_uploads_php?: boolean;
}

export interface WordPressItem {
  name: string;
  title: string;
  status: string;
  version: string;
  update?: string;
}

export interface WordPressInfo {
  installed: boolean;
  version?: string;
  /** Newest version available, and newest of the installed branch. */
  core_update?: string;
  core_minor?: string;
  plugins: WordPressItem[];
  themes: WordPressItem[];
  maintenance: boolean;
  file_edit_disabled: boolean;
  debug: boolean;
  loose_permissions: number;
  site_url?: string;
  error?: string;
}

export interface WordPressAuto {
  core: "off" | "minor" | "all";
  plugins: boolean;
  themes: boolean;
}

export interface PHPVersionInfo {
  version: string;
  extensions: { name: string; installed: boolean; required?: boolean }[];
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

// ---- Email ----

/** A server with email: Stalwart and webmail at its mail host name. */
export interface MailServer {
  server_id: string;
  server_name: string;
  hostname: string;
  tls: "auto" | "internal";
  state: "pending" | "active" | "error";
  message: string | null;
  domains: number;
  created_at: string;
  updated_at: string;
}

export interface MailDNSRecord {
  type: string;
  name: string;
  content: string;
  priority?: number;
}

export interface MailDomain extends Owned {
  id: string;
  server_id: string;
  server_name: string;
  /** The server's mail host name: MX target, webmail and mail app settings. */
  hostname: string;
  domain: string;
  catch_all: string | null;
  state: SiteState;
  message: string | null;
  dns_records: MailDNSRecord[];
  generation: number;
  applied_generation: number;
  mailboxes: number;
  forwarders: number;
  created_at: string;
  updated_at: string;
}

export interface AutoReply {
  subject: string;
  body: string;
  start?: string;
  end?: string;
}

export interface Mailbox {
  id: string;
  domain_id: string;
  local_part: string;
  quota_mb: number;
  forward_to: string[];
  keep_copy: boolean;
  auto_reply: AutoReply | null;
  used_bytes: number | null;
  created_at: string;
}

/** Returned when the panel chose a mailbox password; it is shown only this once. */
export interface MailboxWithPassword {
  mailbox: Mailbox;
  password?: string;
}

export interface MailForwarder {
  id: string;
  domain_id: string;
  local_part: string;
  destinations: string[];
  created_at: string;
}

export interface MailDomainDetail {
  domain: MailDomain;
  mailboxes: Mailbox[];
  forwarders: MailForwarder[];
  /** The Cloudflare zone the records are published to; null: add them by hand. */
  zone: DNSZone | null;
}

export interface MailCheck {
  check: {
    hostname: string;
    hostname_addresses: string[];
    port25: "open" | "blocked";
    addresses: { address: string; ptr: string[]; ptr_ok: boolean; listed: string[]; unknown: string[]; checked: string[] }[];
  };
  public_addresses: string[];
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
  kind: "image" | "compose" | "git";
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
  template: string | null;
  template_version: number | null;
  git_url: string | null;
  git_branch: string | null;
  dockerfile: string | null;
  build_context: string | null;
  auto_deploy: boolean;
  deploy_key_public: string | null;
  builder: "auto" | "dockerfile" | "railpack";
  previews: boolean;
  preview_domain: string | null;
  preview_of: string | null;
  preview_number: number | null;
  created_at: string;
  updated_at: string;
}

/** One build and release of a Git app, or a rollback to an earlier build. */
export interface Deployment {
  id: string;
  app_id: string;
  number: number;
  status: "queued" | "building" | "releasing" | "live" | "failed" | "superseded" | "cancelled";
  trigger: "manual" | "webhook" | "rollback";
  branch: string;
  ref: string | null;
  commit_sha: string | null;
  commit_message: string | null;
  image: string | null;
  rollback_of: string | null;
  error: string | null;
  created_by_name: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  rollback_possible: boolean;
}

/** A one-click service of the catalogue. Its description is translated in catalog.apps.<id>. */
export interface CatalogTemplate {
  id: string;
  name: string;
  category: string;
  website: string;
  version: number;
  service: string;
  port?: number;
  needs_domain: boolean;
  images: string[];
  memory_mb: number;
  vars: { name: string; generate?: string; value?: string; login?: boolean }[];
  compose: string;
  allowed: boolean;
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
// ---- Moving websites ----

export interface SiteMove {
  id: string;
  site_id: string | null;
  from_server: string;
  to_server: string;
  from_server_name: string;
  to_server_name: string;
  database_ids: string[];
  databases: { id: string; engine: string; name: string; username: string }[];
  phase: "copying" | "switching" | "moved" | "cleaning" | "done" | "failed";
  error: string | null;
  bytes: number;
  check_status: number | null;
  target_cleanup: boolean;
  created_at: string;
  moved_at: string | null;
  finished_at: string | null;
}

// ---- API tokens ----

export interface APIToken {
  id: string;
  name: string;
  /** The first characters of the token, to tell tokens apart. */
  prefix: string;
  scopes: string[];
  expires_at: string | null;
  last_used_at: string | null;
  last_used_ip: string;
  created_at: string;
}

/** Returned once, when a token is made. */
export interface CreatedAPIToken extends APIToken {
  token: string;
}

// ---- Updates ----

export type UpdatePhase = "panel" | "agents" | "done" | "failed";

export interface ServerUpdate {
  server_id: string;
  name: string;
  from: string;
  status: "updated" | "current" | "offline" | "failed";
  error?: string;
}

export interface UpdateState {
  target: string;
  from?: string;
  phase: UpdatePhase;
  error?: string;
  backup?: string;
  started_at: string;
  finished_at?: string;
  servers?: ServerUpdate[];
}

export interface AgentVersion {
  id: string;
  name: string;
  agent_version: string;
  online: boolean;
  panel_host: boolean;
  outdated: boolean;
}

export interface UpdatesInfo {
  current: string;
  /** False for a panel built from source ("dev"). */
  release: boolean;
  latest: { version?: string; published_at?: string; notes_url?: string; checked_at?: string; error?: string };
  update_available: boolean;
  check_enabled: boolean;
  /** The agent on the panel's own server is connected. */
  can_update: boolean;
  state: UpdateState | null;
  servers: AgentVersion[];
  outdated: number;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    /** The whole error body, for errors that carry more than a message. */
    public data: Record<string, unknown> = {},
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
    throw new ApiError(res.status, e.code ?? "internal", e.message ?? res.statusText, (data ?? {}) as Record<string, unknown>);
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
    return t(`errors.${err.code}`, { ...err.data, defaultValue: err.message });
  }
  return t("errors.internal");
}

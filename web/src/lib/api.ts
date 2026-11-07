import type { TFunction } from "i18next";

export interface User {
  id: string;
  email: string;
  name: string;
  role: "admin" | "reseller" | "customer";
  locale: string;
  totp_enabled: boolean;
  created_at: string;
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
  del: (path: string) => request<void>("DELETE", path),
};

/** Turns any thrown error into a sentence in the user's language. */
export function errorMessage(t: TFunction, err: unknown): string {
  if (err instanceof ApiError) {
    return t(`errors.${err.code}`, { defaultValue: err.message });
  }
  return t("errors.internal");
}

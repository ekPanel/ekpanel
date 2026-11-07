import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { KeyRound, ShieldAlert } from "lucide-react";
import { api, errorMessage, type Server, type Site } from "../lib/api";
import { Badge, Button, Card, CopyButton, ErrorAlert } from "./ui";

/** SFTP sign-in for a site: switch, one-time password, and SSH keys. */
export function SiteSFTP({ site, server }: { site: Site; server?: Server }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [password, setPassword] = useState("");
  const [keys, setKeys] = useState(site.ssh_keys.join("\n"));
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [keysSaved, setKeysSaved] = useState(false);
  const host = server?.hostname || t("sftp.server_ip");

  const call = async (what: string, fn: () => Promise<{ site: Site; password?: string } | Site>) => {
    setBusy(what);
    setError("");
    try {
      const r = await fn();
      const updated = "site" in r ? r.site : r;
      qc.setQueryData(["site", site.id], updated);
      qc.invalidateQueries({ queryKey: ["tasks"] });
      if ("password" in r && r.password) setPassword(r.password);
      return updated;
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };

  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-lg font-semibold">{t("sftp.title")}</h2>
        <Badge tone={site.sftp_enabled ? "ok" : "neutral"}>{site.sftp_enabled ? t("sftp.on") : t("sftp.off")}</Badge>
      </div>
      <p className="text-sm text-muted">{t("sftp.intro")}</p>
      <ErrorAlert>{error}</ErrorAlert>

      {site.sftp_enabled && (
        <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
          <dt className="text-muted">{t("sftp.host")}</dt>
          <dd className="flex items-center gap-1 font-mono" dir="ltr">
            {host} {server?.hostname && <CopyButton text={server.hostname} className="-my-1" />}
          </dd>
          <dt className="text-muted">{t("databases.port")}</dt>
          <dd className="font-mono">22</dd>
          <dt className="text-muted">{t("databases.username")}</dt>
          <dd className="flex items-center gap-1 font-mono" dir="ltr">
            {site.linux_user} <CopyButton text={site.linux_user} className="-my-1" />
          </dd>
        </dl>
      )}

      {password && (
        <div className="grid gap-2 rounded-lg bg-warn-soft p-3 text-warn">
          <p className="flex items-start gap-2 text-sm">
            <ShieldAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
            {t("sftp.password_once")}
          </p>
          <div className="flex items-center gap-2">
            <code className="rounded bg-surface px-2 py-1 font-mono text-sm text-ink" dir="ltr">
              {password}
            </code>
            <CopyButton text={password} />
          </div>
        </div>
      )}

      <div className="flex flex-wrap gap-2">
        {site.sftp_enabled ? (
          <>
            <Button size="sm" onClick={() => call("reset", () => api.post(`/sites/${site.id}/sftp/password`))} loading={busy === "reset"}>
              <KeyRound className="size-4" aria-hidden />
              {t("sftp.new_password")}
            </Button>
            <Button size="sm" variant="danger" onClick={() => (setPassword(""), call("off", () => api.post(`/sites/${site.id}/sftp`, { enabled: false })))} loading={busy === "off"}>
              {t("sftp.turn_off")}
            </Button>
          </>
        ) : (
          <Button size="sm" variant="primary" onClick={() => call("on", () => api.post(`/sites/${site.id}/sftp`, { enabled: true }))} loading={busy === "on"}>
            {t("sftp.turn_on")}
          </Button>
        )}
      </div>

      <details className="group" open={site.ssh_keys.length > 0}>
        <summary className="cursor-pointer text-sm font-medium text-accent">{t("sftp.keys", { count: site.ssh_keys.length })}</summary>
        <div className="mt-3 grid gap-2">
          <p className="text-sm text-muted">{t("sftp.keys_hint")}</p>
          <textarea
            value={keys}
            onChange={(e) => (setKeys(e.target.value), setKeysSaved(false))}
            rows={3}
            dir="ltr"
            placeholder="ssh-ed25519 AAAA… you@laptop"
            className="rounded-lg border border-line bg-surface px-3 py-2 font-mono text-xs text-ink"
          />
          <div className="flex items-center gap-3">
            <Button
              size="sm"
              onClick={async () => {
                const r = await call("keys", () => api.put<Site>(`/sites/${site.id}/ssh-keys`, { keys: keys.split("\n").map((k) => k.trim()).filter(Boolean) }));
                if (r) {
                  setKeys(r.ssh_keys.join("\n"));
                  setKeysSaved(true);
                }
              }}
              loading={busy === "keys"}
            >
              {t("sftp.save_keys")}
            </Button>
            {keysSaved && <span className="text-sm text-ok">{t("sites.saved")}</span>}
          </div>
        </div>
      </details>
    </Card>
  );
}

/** FTP over TLS for a site: its own login and one-time password. */
export function SiteFTP({ site, server }: { site: Site; server?: Server }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const host = server?.hostname || t("sftp.server_ip");
  const call = async (what: string, path: string, body?: unknown) => {
    setBusy(what);
    setError("");
    try {
      const r = await api.post<{ site: Site; password?: string }>(path, body);
      qc.setQueryData(["site", site.id], r.site);
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setPassword(r.password ?? "");
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };
  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-lg font-semibold">{t("ftp.title")}</h2>
        <Badge tone={site.ftp_enabled ? "ok" : "neutral"}>{site.ftp_enabled ? t("sftp.on") : t("sftp.off")}</Badge>
      </div>
      <p className="text-sm text-muted">{t("ftp.intro")}</p>
      <ErrorAlert>{error}</ErrorAlert>
      {site.ftp_enabled && (
        <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-6 gap-y-2 text-sm">
          <dt className="text-muted">{t("sftp.host")}</dt>
          <dd className="flex items-center gap-1 font-mono" dir="ltr">
            {host} {server?.hostname && <CopyButton text={server.hostname} className="-my-1" />}
          </dd>
          <dt className="text-muted">{t("databases.port")}</dt>
          <dd className="font-mono">21</dd>
          <dt className="text-muted">{t("ftp.encryption")}</dt>
          <dd>{t("ftp.explicit_tls")}</dd>
          <dt className="text-muted">{t("databases.username")}</dt>
          <dd className="flex items-center gap-1 font-mono" dir="ltr">
            {site.linux_user} <CopyButton text={site.linux_user} className="-my-1" />
          </dd>
        </dl>
      )}
      {password && (
        <div className="grid gap-2 rounded-lg bg-warn-soft p-3 text-warn">
          <p className="flex items-start gap-2 text-sm">
            <ShieldAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
            {t("ftp.password_once")}
          </p>
          <div className="flex items-center gap-2">
            <code className="rounded bg-surface px-2 py-1 font-mono text-sm text-ink" dir="ltr">
              {password}
            </code>
            <CopyButton text={password} />
          </div>
        </div>
      )}
      {site.ftp_enabled && <p className="text-sm text-muted">{t("ftp.certificate_note")}</p>}
      <div className="flex flex-wrap gap-2">
        {site.ftp_enabled ? (
          <>
            <Button size="sm" onClick={() => call("reset", `/sites/${site.id}/ftp/password`)} loading={busy === "reset"}>
              <KeyRound className="size-4" aria-hidden />
              {t("sftp.new_password")}
            </Button>
            <Button size="sm" variant="danger" onClick={() => call("off", `/sites/${site.id}/ftp`, { enabled: false })} loading={busy === "off"}>
              {t("ftp.turn_off")}
            </Button>
          </>
        ) : (
          <Button size="sm" variant="primary" onClick={() => call("on", `/sites/${site.id}/ftp`, { enabled: true })} loading={busy === "on"}>
            {t("ftp.turn_on")}
          </Button>
        )}
      </div>
    </Card>
  );
}

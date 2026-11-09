import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, Lock } from "lucide-react";
import { api, errorMessage, type PHPVersionInfo, type Task } from "../lib/api";
import { Card, ErrorAlert, Spinner, cx } from "./ui";
import { useTasks } from "./Tasks";

/** The PHP versions installed on a server, with extensions to add or remove. */
export function PHPExtensionsCard({ serverId, online }: { serverId: string; online: boolean }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const info = useQuery({
    queryKey: ["php-info", serverId],
    queryFn: () => api.get<{ versions: PHPVersionInfo[] }>(`/servers/${serverId}/php`),
    enabled: online,
    retry: false,
  });
  const tasks = useTasks(`server_id=${encodeURIComponent(serverId)}&limit=20`, online);
  const running = (tasks.data ?? []).filter((x: Task) => x.kind === "php.extension" && (x.status === "queued" || x.status === "running"));
  const [version, setVersion] = useState("");
  const [error, setError] = useState("");
  const versions = info.data?.versions ?? [];
  useEffect(() => {
    if (!version && versions.length) setVersion(versions[0].version);
  }, [versions, version]);
  // When a change finishes, read the server again.
  const pending = running.length;
  useEffect(() => {
    if (pending === 0) qc.invalidateQueries({ queryKey: ["php-info", serverId] });
  }, [pending, qc, serverId]);

  const current = versions.find((v) => v.version === version);
  const busy = (name: string) => running.some((x) => x.params.version === version && x.params.name === name);
  const toggle = async (name: string, installed: boolean) => {
    setError("");
    try {
      await api.post(`/servers/${serverId}/php/extensions`, { version, name, installed });
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };

  return (
    <Card className="overflow-hidden">
      <div className="flex flex-wrap items-center gap-3 border-b border-line px-4 py-3">
        <h2 className="font-semibold">{t("php_ext.title")}</h2>
        {versions.length > 1 && (
          <div className="ms-auto flex flex-wrap gap-1.5" role="group" aria-label={t("php_ext.version")}>
            {versions.map((v) => (
              <button
                key={v.version}
                type="button"
                aria-pressed={v.version === version}
                onClick={() => setVersion(v.version)}
                className={cx(
                  "rounded-full border px-3 py-1 text-sm",
                  v.version === version ? "border-accent bg-accent-soft text-accent" : "border-line text-muted hover:text-ink",
                )}
              >
                PHP {v.version}
              </button>
            ))}
          </div>
        )}
      </div>
      <div className="grid gap-3 p-4">
        {!online && <p className="text-sm text-muted">{t("php_ext.offline")}</p>}
        {info.isFetching && !info.data && online && <Spinner />}
        {(info.error || error) && <ErrorAlert>{error || errorMessage(t, info.error)}</ErrorAlert>}
        {info.data && versions.length === 0 && <p className="text-sm text-muted">{t("php_ext.none")}</p>}
        {current && (
          <>
            <p className="text-sm text-muted">{t("php_ext.intro", { version: current.version })}</p>
            <ul className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
              {current.extensions.map((e) => (
                <li key={e.name}>
                  <label
                    className={cx(
                      "flex items-center gap-2 rounded-lg border border-line px-3 py-2 text-sm",
                      e.required ? "text-muted" : "cursor-pointer hover:bg-sunk",
                    )}
                  >
                    <input
                      type="checkbox"
                      checked={e.installed}
                      disabled={e.required || busy(e.name)}
                      onChange={(ev) => toggle(e.name, ev.target.checked)}
                      className="accent-[var(--accent)]"
                    />
                    <span className="font-mono" dir="ltr">
                      {e.name}
                    </span>
                    {e.required && <Lock className="ms-auto size-3.5" aria-label={t("php_ext.required")} />}
                    {busy(e.name) && <Loader2 className="ms-auto size-4 animate-spin" aria-label={t("php_ext.working")} />}
                  </label>
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </Card>
  );
}

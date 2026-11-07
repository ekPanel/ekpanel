import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { RotateCw } from "lucide-react";
import { api, errorMessage, type ServiceInfo, type Task } from "../lib/api";
import { formatRelative } from "../lib/format";
import { Badge, Button, Card, ErrorAlert, Spinner, type Tone } from "./ui";

function serviceTone(s: ServiceInfo): Tone {
  if (!s.installed) return "neutral";
  if (s.active_state === "active") return "ok";
  if (s.active_state === "failed") return "crit";
  if (s.active_state === "activating" || s.active_state === "reloading") return "warn";
  return "neutral";
}

/** Live state of the services the panel watches on a server, with restart buttons. */
export function ServicesCard({ serverId, online }: { serverId: string; online: boolean }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const [requested, setRequested] = useState<string | null>(null);
  const q = useQuery({
    queryKey: ["services", serverId],
    queryFn: () => api.get<{ services: ServiceInfo[] }>(`/servers/${serverId}/services`),
    enabled: online,
    refetchInterval: 15000,
    retry: false,
  });

  const restart = async (name: string) => {
    setError("");
    try {
      await api.post<Task>(`/servers/${serverId}/services/${encodeURIComponent(name)}/restart`);
      setRequested(name);
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setTimeout(() => qc.invalidateQueries({ queryKey: ["services", serverId] }), 4000);
    } catch (err) {
      setError(errorMessage(t, err));
    }
  };

  return (
    <Card className="overflow-hidden">
      <h2 className="border-b border-line px-4 py-3 font-semibold">{t("services.title")}</h2>
      {!online && <p className="px-4 py-6 text-center text-sm text-muted">{t("services.offline")}</p>}
      {online && q.isPending && <Spinner />}
      {online && q.error && (
        <div className="p-4">
          <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>
        </div>
      )}
      {error && (
        <div className="px-4 pt-3">
          <ErrorAlert>{error}</ErrorAlert>
        </div>
      )}
      {online && q.data && (
        <ul className="divide-y divide-line">
          {q.data.services.map((s) => (
            <li key={s.name} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5">
              <span className="font-mono text-sm" dir="ltr">
                {s.name}
              </span>
              <Badge tone={serviceTone(s)}>{s.installed ? t(`services.state.${s.active_state}`, { defaultValue: s.active_state }) : t("services.not_installed")}</Badge>
              {s.installed && s.since && s.active_state === "active" && (
                <span className="text-xs text-muted">{t("services.since", { when: formatRelative(s.since, i18n.language) })}</span>
              )}
              {s.restartable && (
                <Button size="sm" variant="ghost" className="ms-auto" onClick={() => restart(s.name)}>
                  <RotateCw className="size-4" aria-hidden />
                  {requested === s.name ? t("services.restart_requested") : t("services.restart")}
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

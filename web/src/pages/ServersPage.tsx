import { Link, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { Plus, Server as ServerIcon } from "lucide-react";
import { formatBytes, formatPercent, formatRelative } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Button, Card, ErrorAlert, Spinner } from "../components/ui";
import { useServers } from "./DashboardPage";
import { AddServerDialog, EmptyState, ServerStatus } from "./ServerParts";

export function ServersPage() {
  const { t, i18n } = useTranslation();
  const lng = i18n.language;
  const servers = useServers();
  const [params, setParams] = useSearchParams();
  const adding = params.get("add") === "1";
  const setAdding = (on: boolean) => setParams(on ? { add: "1" } : {}, { replace: true });

  const addButton = (
    <Button variant="primary" onClick={() => setAdding(true)}>
      <Plus className="size-4" aria-hidden />
      {t("servers.add")}
    </Button>
  );

  return (
    <>
      <PageHeader title={t("nav.servers")} description={t("servers.intro")} actions={servers.data?.length ? addButton : undefined} />
      {servers.isPending && <Spinner />}
      {servers.error && <ErrorAlert>{t("errors.internal")}</ErrorAlert>}
      {servers.data?.length === 0 && <EmptyState icon={<ServerIcon />} title={t("servers.empty.title")} body={t("servers.empty.body")} action={addButton} />}
      {!!servers.data?.length && (
        <Card className="overflow-x-auto">
          <table className="w-full min-w-[720px] text-sm">
            <thead>
              <tr className="border-b border-line bg-sunk text-start text-xs tracking-wider text-muted uppercase">
                <th className="px-4 py-3 text-start font-semibold">{t("servers.col.server")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("servers.col.status")}</th>
                <th className="px-4 py-3 text-end font-semibold">{t("metrics.cpu")}</th>
                <th className="px-4 py-3 text-end font-semibold">{t("metrics.memory")}</th>
                <th className="px-4 py-3 text-end font-semibold">{t("metrics.disk")}</th>
                <th className="px-4 py-3 text-start font-semibold">{t("servers.col.last_seen")}</th>
              </tr>
            </thead>
            <tbody>
              {servers.data.map((s) => {
                const m = s.online ? s.latest : null;
                return (
                  <tr key={s.id} className="border-b border-line last:border-0 hover:bg-sunk/60">
                    <td className="px-4 py-3">
                      <Link to={`/servers/${s.id}`} className="font-semibold hover:text-accent">
                        {s.name}
                      </Link>
                      <div className="text-muted">{[s.hostname, s.os].filter(Boolean).join(" · ") || t("servers.not_connected_yet")}</div>
                    </td>
                    <td className="px-4 py-3">
                      <ServerStatus server={s} />
                    </td>
                    <td className="px-4 py-3 text-end tabular-nums">{m ? formatPercent(m.cpu_percent, lng) : "—"}</td>
                    <td className="px-4 py-3 text-end tabular-nums">{m ? `${formatBytes(m.mem_used, lng)} / ${formatBytes(m.mem_total, lng)}` : "—"}</td>
                    <td className="px-4 py-3 text-end tabular-nums">{m ? `${formatBytes(m.disk_used, lng)} / ${formatBytes(m.disk_total, lng)}` : "—"}</td>
                    <td className="px-4 py-3 text-muted">{s.last_seen_at ? formatRelative(s.last_seen_at, lng) : t("servers.never")}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </Card>
      )}
      <AddServerDialog open={adding} onClose={() => setAdding(false)} />
    </>
  );
}

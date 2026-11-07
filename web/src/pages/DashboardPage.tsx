import { useState } from "react";
import { Link } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { ArrowRight, Check, Circle, Plus, Server as ServerIcon } from "lucide-react";
import { api, type Server, type User } from "../lib/api";
import { formatNumber, formatRate } from "../lib/format";
import { useMe } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { Button, Card, ErrorAlert, Spinner } from "../components/ui";
import { AddServerDialog, EmptyState, ServerStatus, UsageBars, serverState } from "./ServerParts";
import { MyUsageCard } from "./CustomersPage";

export function useServers(enabled = true) {
  return useQuery({
    queryKey: ["servers"],
    queryFn: () => api.get<Server[]>("/servers"),
    refetchInterval: 5000,
    enabled,
  });
}

function GettingStarted({ user, servers }: { user: User; servers: Server[] }) {
  const { t } = useTranslation();
  const steps = [
    { done: true, label: t("dashboard.start.admin"), to: undefined },
    { done: servers.some((s) => s.enrolled_at), label: t("dashboard.start.server"), to: "/servers?add=1" },
    { done: user.totp_enabled, label: t("dashboard.start.twofa"), to: "/settings/security" },
  ];
  if (steps.every((s) => s.done)) return null;
  return (
    <Card className="p-5">
      <h2 className="font-semibold">{t("dashboard.start.title")}</h2>
      <ol className="mt-3 grid gap-2">
        {steps.map((s) => (
          <li key={s.label} className="flex items-center gap-3">
            {s.done ? (
              <span className="grid size-5 place-items-center rounded-full bg-ok text-white">
                <Check className="size-3.5" strokeWidth={3} aria-hidden />
              </span>
            ) : (
              <Circle className="size-5 text-line" aria-hidden />
            )}
            {s.done || !s.to ? (
              <span className={s.done ? "text-muted line-through" : ""}>{s.label}</span>
            ) : (
              <Link to={s.to} className="font-medium text-accent hover:underline">
                {s.label}
              </Link>
            )}
          </li>
        ))}
      </ol>
    </Card>
  );
}

export function DashboardPage() {
  const { t, i18n } = useTranslation();
  const me = useMe();
  const user = me.data!;
  const isAdmin = user.role === "admin";
  const servers = useServers(isAdmin);
  const [adding, setAdding] = useState(false);

  const list = servers.data ?? [];
  const online = list.filter((s) => s.online).length;
  const attention = list.filter((s) => serverState(s) !== "online").length;

  return (
    <>
      <PageHeader
        title={t("dashboard.greeting", { name: user.name.split(" ")[0] })}
        description={
          !isAdmin
            ? t("dashboard.customer_intro")
            : list.length === 0
              ? t("dashboard.no_servers_intro")
              : attention === 0
                ? t("dashboard.all_good", { count: online, formatted: formatNumber(online, i18n.language) })
                : t("dashboard.attention", { count: attention, formatted: formatNumber(attention, i18n.language) })
        }
        actions={
          isAdmin && list.length > 0 ? (
            <Button variant="primary" onClick={() => setAdding(true)}>
              <Plus className="size-4" aria-hidden />
              {t("servers.add")}
            </Button>
          ) : undefined
        }
      />

      <div className="grid gap-6">
        {isAdmin ? <GettingStarted user={user} servers={list} /> : <MyUsageCard />}

        {isAdmin && servers.isPending && <Spinner />}
        {isAdmin && servers.error && <ErrorAlert>{t("errors.internal")}</ErrorAlert>}
        {isAdmin && servers.isSuccess && list.length === 0 && (
          <EmptyState
            icon={<ServerIcon />}
            title={t("servers.empty.title")}
            body={t("servers.empty.body")}
            action={
              <Button variant="primary" onClick={() => setAdding(true)}>
                <Plus className="size-4" aria-hidden />
                {t("servers.add")}
              </Button>
            }
          />
        )}

        {list.length > 0 && (
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
            {list.map((s) => (
              <Link key={s.id} to={`/servers/${s.id}`} className="group">
                <Card className="grid h-full gap-4 p-5 transition-colors group-hover:border-accent">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <h2 className="truncate font-semibold">{s.name}</h2>
                      <p className="truncate text-sm text-muted">{s.os || s.hostname || t("servers.not_connected_yet")}</p>
                    </div>
                    <ServerStatus server={s} />
                  </div>
                  {s.latest && s.online ? (
                    <>
                      <UsageBars m={s.latest} />
                      <p className="flex justify-between text-sm text-muted tabular-nums">
                        <span>↓ {formatRate(s.latest.net_rx_rate, i18n.language)}</span>
                        <span>↑ {formatRate(s.latest.net_tx_rate, i18n.language)}</span>
                      </p>
                    </>
                  ) : (
                    <p className="flex items-center gap-1 text-sm font-medium text-accent">
                      {serverState(s) === "pending" ? t("servers.finish_setup") : t("servers.view_details")}
                      <ArrowRight className="size-4 rtl:-scale-x-100" aria-hidden />
                    </p>
                  )}
                </Card>
              </Link>
            ))}
          </div>
        )}
      </div>
      <AddServerDialog open={adding} onClose={() => setAdding(false)} />
    </>
  );
}

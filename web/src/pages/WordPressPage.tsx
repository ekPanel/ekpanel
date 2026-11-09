import { useState } from "react";
import { Link } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Newspaper, Search } from "lucide-react";
import { api, errorMessage, type Site } from "../lib/api";
import { formatRelative } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, ErrorAlert, Spinner } from "../components/ui";
import { EmptyState } from "./ServerParts";

/** Every WordPress site in one list, with what needs attention. */
export function WordPressPage() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const sites = useQuery({ queryKey: ["wordpress"], queryFn: () => api.get<Site[]>("/wordpress"), refetchInterval: 30000 });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [scanned, setScanned] = useState(false);
  const scan = async () => {
    setBusy(true);
    setError("");
    try {
      await api.post("/wordpress/scan");
      setScanned(true);
      qc.invalidateQueries({ queryKey: ["tasks"] });
      setTimeout(() => qc.invalidateQueries({ queryKey: ["wordpress"] }), 8000);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const scanButton = (
    <Button onClick={scan} loading={busy}>
      <Search className="size-4" aria-hidden />
      {t("wptools.scan")}
    </Button>
  );
  const byID = new Map((sites.data ?? []).map((s) => [s.id, s]));
  return (
    <>
      <PageHeader title="WordPress" description={t("wptools.intro")} actions={scanButton} />
      <div className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        {scanned && <p className="text-sm text-muted">{t("wptools.scan_started")}</p>}
        {sites.isPending && <Spinner />}
        {sites.error && <ErrorAlert>{errorMessage(t, sites.error)}</ErrorAlert>}
        {sites.data?.length === 0 && (
          <EmptyState
            icon={<Newspaper />}
            title={t("wptools.empty.title")}
            body={t("wptools.empty.body")}
            action={
              <Link to="/sites?add=1" className="inline-flex h-10 items-center rounded-lg bg-accent px-4 font-medium text-accent-ink hover:brightness-110">
                {t("sites.add")}
              </Link>
            }
          />
        )}
        {!!sites.data?.length && (
          <Card className="overflow-x-auto">
            <table className="w-full min-w-[720px] text-sm">
              <thead>
                <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                  <th className="px-4 py-3 text-start font-semibold">{t("sites.col.site")}</th>
                  <th className="px-4 py-3 text-start font-semibold">{t("wptools.version")}</th>
                  <th className="px-4 py-3 text-start font-semibold">{t("wptools.attention")}</th>
                  <th className="px-4 py-3 text-start font-semibold">{t("wptools.auto.title")}</th>
                  <th className="px-4 py-3 text-start font-semibold">{t("wptools.checked")}</th>
                </tr>
              </thead>
              <tbody>
                {sites.data.map((s) => {
                  const wp = s.wordpress;
                  const plugins = wp?.plugins.filter((p) => p.update).length ?? 0;
                  const themes = wp?.themes.filter((p) => p.update).length ?? 0;
                  const live = s.staging_of ? byID.get(s.staging_of) : undefined;
                  return (
                    <tr key={s.id} className="border-b border-line align-top last:border-0 hover:bg-sunk/60">
                      <td className="px-4 py-3">
                        <Link to={`/sites/${s.id}`} className="font-semibold hover:text-accent" dir="ltr">
                          {s.domain_display}
                        </Link>
                        {s.staging_of && (
                          <div className="text-xs text-muted">{t("wptools.staging_of", { domain: live?.domain_display ?? t("wptools.staging.title") })}</div>
                        )}
                      </td>
                      <td className="px-4 py-3 tabular-nums">{wp?.version ?? s.app_version ?? "—"}</td>
                      <td className="px-4 py-3">
                        <div className="flex flex-wrap gap-1">
                          {wp?.error && <Badge tone="crit">{t("wptools.unreadable")}</Badge>}
                          {wp?.core_update && <Badge tone="warn">{t("wptools.core_available", { version: wp.core_update })}</Badge>}
                          {plugins > 0 && <Badge tone="warn">{t("wptools.plugin_updates", { count: plugins })}</Badge>}
                          {themes > 0 && <Badge tone="warn">{t("wptools.theme_updates", { count: themes })}</Badge>}
                          {wp?.maintenance && <Badge tone="accent">{t("wptools.maintenance")}</Badge>}
                          {!!wp?.loose_permissions && <Badge tone="warn">{t("wptools.hardening.loose", { count: wp.loose_permissions })}</Badge>}
                          {wp && !wp.error && !wp.core_update && !plugins && !themes && !wp.loose_permissions && (
                            <Badge tone="ok">{t("wptools.up_to_date")}</Badge>
                          )}
                        </div>
                      </td>
                      <td className="px-4 py-3 text-muted">
                        {t(`wptools.auto.core_${s.wp_auto.core}`)}
                        {(s.wp_auto.plugins || s.wp_auto.themes) && ` · ${t("wptools.auto.and_more")}`}
                      </td>
                      <td className="px-4 py-3 text-muted">{s.wp_scanned_at ? formatRelative(s.wp_scanned_at, i18n.language) : "—"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </Card>
        )}
      </div>
    </>
  );
}

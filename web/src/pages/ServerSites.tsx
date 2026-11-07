import { useState } from "react";
import { Link } from "react-router";
import { useTranslation } from "react-i18next";
import { Plus } from "lucide-react";
import { Button, Card } from "../components/ui";
import { AddSiteDialog, SiteStateBadge, useSites } from "./SitesPage";

/** The websites hosted on one server, on that server's page. */
export function ServerSites({ serverId }: { serverId: string }) {
  const { t } = useTranslation();
  const sites = useSites(serverId);
  const [adding, setAdding] = useState(false);
  return (
    <Card className="overflow-hidden">
      <div className="flex items-center justify-between gap-3 border-b border-line px-4 py-2.5">
        <h2 className="font-semibold">{t("nav.sites")}</h2>
        <Button size="sm" variant="ghost" onClick={() => setAdding(true)}>
          <Plus className="size-4" aria-hidden />
          {t("sites.add")}
        </Button>
      </div>
      {sites.data?.length === 0 && <p className="px-4 py-6 text-center text-sm text-muted">{t("sites.none_on_server")}</p>}
      <ul className="divide-y divide-line">
        {sites.data?.map((s) => (
          <li key={s.id} className="flex flex-wrap items-center gap-3 px-4 py-2.5">
            <Link to={`/sites/${s.id}`} className="font-medium hover:text-accent">
              <bdi>{s.domain_display}</bdi>
            </Link>
            <span className="text-sm text-muted">{s.kind === "php" ? `PHP ${s.php_version}` : t("sites.kind.static")}</span>
            <span className="ms-auto">
              <SiteStateBadge site={s} />
            </span>
          </li>
        ))}
      </ul>
      <AddSiteDialog open={adding} onClose={() => setAdding(false)} serverId={serverId} />
    </Card>
  );
}

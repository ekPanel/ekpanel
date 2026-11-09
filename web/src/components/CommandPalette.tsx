import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { ArchiveRestore, Bell, Boxes, CircleArrowUp, CloudCog, CornerDownLeft, Database as DatabaseIcon, Gauge, Globe, KeyRound, LayoutGrid, Package as PackageIcon, Plus, ScrollText, Search, Server as ServerIcon, ShieldCheck, Users } from "lucide-react";
import { api, type Database, type Server, type Site, type User } from "../lib/api";
import { cx } from "./ui";

interface Item {
  id: string;
  group: string;
  label: string;
  hint?: string;
  icon: ReactNode;
  run: () => void;
}

/** Ctrl+K: jump to any page, server or action by typing. */
export function CommandPalette({ open, onClose, user }: { open: boolean; onClose: () => void; user: User }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const ref = useRef<HTMLDialogElement>(null);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const isAdmin = user.role === "admin";

  const servers = useQuery({
    queryKey: ["servers"],
    queryFn: () => api.get<Server[]>("/servers"),
    enabled: open && isAdmin,
  });
  const sites = useQuery({
    queryKey: ["sites", "all"],
    queryFn: () => api.get<Site[]>("/sites"),
    enabled: open,
  });
  const databases = useQuery({
    queryKey: ["databases", "all"],
    queryFn: () => api.get<Database[]>("/databases"),
    enabled: open,
  });

  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) {
      setQuery("");
      setActive(0);
      d.showModal();
    }
    if (!open && d.open) d.close();
  }, [open]);

  const items = useMemo<Item[]>(() => {
    const go = (path: string) => () => {
      onClose();
      navigate(path);
    };
    const pages = t("palette.pages");
    const list: Item[] = [{ id: "dash", group: pages, label: t("nav.dashboard"), icon: <Gauge />, run: go("/") }];
    list.push(
      { id: "sites", group: pages, label: t("nav.sites"), icon: <Globe />, run: go("/sites") },
      { id: "databases", group: pages, label: t("nav.databases"), icon: <DatabaseIcon />, run: go("/databases") },
      { id: "apps", group: pages, label: t("nav.apps"), icon: <Boxes />, run: go("/apps") },
      { id: "catalog", group: pages, label: t("catalog.title"), icon: <LayoutGrid />, run: go("/apps/catalog") },
      { id: "dns", group: pages, label: t("nav.dns"), icon: <CloudCog />, run: go("/dns") },
      { id: "add-site", group: t("palette.actions"), label: t("sites.add"), icon: <Plus />, run: go("/sites?add=1") },
      { id: "add-db", group: t("palette.actions"), label: t("databases.add"), icon: <Plus />, run: go("/databases?add=1") },
      { id: "add-app", group: t("palette.actions"), label: t("apps.add"), icon: <Plus />, run: go("/apps?add=1") },
    );
    for (const d of databases.data ?? []) {
      list.push({ id: "d-" + d.id, group: t("nav.databases"), label: d.name, hint: d.server_name, icon: <DatabaseIcon />, run: go(`/databases/${d.id}`) });
    }
    for (const s of sites.data ?? []) {
      list.push({ id: "w-" + s.id, group: t("nav.sites"), label: s.domain_display, hint: s.server_name, icon: <Globe />, run: go(`/sites/${s.id}`) });
    }
    if (isAdmin) {
      list.push(
        { id: "customers", group: pages, label: t("nav.customers"), icon: <Users />, run: go("/customers") },
        { id: "packages", group: pages, label: t("nav.packages"), icon: <PackageIcon />, run: go("/packages") },
        { id: "add-customer", group: t("palette.actions"), label: t("customers.add"), icon: <Plus />, run: go("/customers?add=1") },
        { id: "backups", group: pages, label: t("nav.backups"), icon: <ArchiveRestore />, run: go("/backups") },
        { id: "servers", group: pages, label: t("nav.servers"), icon: <ServerIcon />, run: go("/servers") },
        { id: "audit", group: pages, label: t("nav.audit"), icon: <ScrollText />, run: go("/audit") },
        { id: "updates", group: pages, label: t("nav.updates"), icon: <CircleArrowUp />, run: go("/settings/updates") },
        { id: "add-backup", group: t("palette.actions"), label: t("backups.add"), icon: <Plus />, run: go("/backups?add=1") },
        { id: "add-server", group: t("palette.actions"), label: t("servers.add"), icon: <Plus />, run: go("/servers?add=1") },
      );
      for (const s of servers.data ?? []) {
        list.push({ id: "s-" + s.id, group: t("nav.servers"), label: s.name, hint: s.hostname, icon: <ServerIcon />, run: go(`/servers/${s.id}`) });
      }
    }
    list.push({ id: "security", group: pages, label: t("nav.security"), icon: <ShieldCheck />, run: go("/settings/security") });
    list.push({ id: "notifications", group: pages, label: t("nav.notifications"), icon: <Bell />, run: go("/settings/notifications") });
    list.push({ id: "tokens", group: pages, label: t("nav.tokens"), icon: <KeyRound />, run: go("/settings/tokens") });
    return list;
  }, [t, isAdmin, servers.data, sites.data, databases.data, navigate, onClose]);

  const filtered = useMemo(() => {
    const q = query.trim().toLocaleLowerCase();
    if (!q) return items;
    return items.filter((i) => (i.label + " " + (i.hint ?? "") + " " + i.group).toLocaleLowerCase().includes(q));
  }, [items, query]);

  useEffect(() => setActive(0), [query]);

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => Math.min(a + 1, filtered.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      filtered[active]?.run();
    }
  };

  let lastGroup = "";
  return (
    <dialog
      ref={ref}
      onClose={onClose}
      onClick={(e) => e.target === ref.current && onClose()}
      className="mx-auto mt-[12vh] w-[calc(100%-2rem)] max-w-xl overflow-hidden rounded-2xl border border-line bg-surface p-0 text-ink shadow-2xl backdrop:bg-black/40"
    >
      {open && (
        <div onKeyDown={onKeyDown}>
          <div className="flex items-center gap-3 border-b border-line px-4">
            <Search className="size-5 text-muted" aria-hidden />
            <input
              autoFocus
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={t("palette.placeholder")}
              aria-label={t("palette.placeholder")}
              role="combobox"
              aria-expanded
              aria-controls="palette-list"
              aria-activedescendant={filtered[active] ? "pi-" + filtered[active].id : undefined}
              className="h-14 flex-1 bg-transparent text-base outline-none placeholder:text-muted"
            />
            <kbd className="rounded border border-line px-1.5 font-mono text-[11px] text-muted">Esc</kbd>
          </div>
          <ul id="palette-list" role="listbox" className="max-h-[50vh] overflow-y-auto p-2">
            {filtered.length === 0 && <li className="px-3 py-6 text-center text-muted">{t("palette.empty")}</li>}
            {filtered.map((item, i) => {
              const header = item.group !== lastGroup ? item.group : null;
              lastGroup = item.group;
              return (
                <li key={item.id} role="presentation">
                  {header && <p className="px-3 pt-2 pb-1 text-xs font-semibold tracking-wider text-muted uppercase">{header}</p>}
                  <div
                    id={"pi-" + item.id}
                    role="option"
                    aria-selected={i === active}
                    onMouseMove={() => setActive(i)}
                    onClick={item.run}
                    className={cx(
                      "flex cursor-pointer items-center gap-3 rounded-lg px-3 py-2 [&>svg]:size-4 [&>svg]:text-muted",
                      i === active && "bg-accent-soft",
                    )}
                  >
                    {item.icon}
                    <span className="truncate">{item.label}</span>
                    {item.hint && <span className="truncate text-sm text-muted">{item.hint}</span>}
                    {i === active && <CornerDownLeft className="ms-auto size-4 text-muted rtl:-scale-x-100" aria-hidden />}
                  </div>
                </li>
              );
            })}
          </ul>
        </div>
      )}
    </dialog>
  );
}

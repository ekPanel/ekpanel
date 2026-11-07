import { useEffect, useState, type ReactNode } from "react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { Activity, ArchiveRestore, Bell, Boxes, CloudCog, Database, Package, Users, Gauge, Globe, Languages, LogOut, Mail, Menu, Monitor, Moon, ScrollText, Search, Server, ShieldCheck, Sun, X } from "lucide-react";
import { api, type User } from "../lib/api";
import { meKey } from "../lib/session";
import { useTheme, type Theme } from "../lib/theme";
import { languages } from "../i18n";
import { CommandPalette } from "./CommandPalette";
import { Logo } from "./Brand";

export { Logo };
import { TaskTray } from "./Tasks";
import { ImpersonationBanner } from "../pages/CustomersPage";
import { NotificationBell } from "../pages/NotificationsPage";
import { cx } from "./ui";

function NavItem({ to, icon, children }: { to: string; icon: ReactNode; children: ReactNode }) {
  return (
    <NavLink
      to={to}
      end={to === "/"}
      className={({ isActive }) =>
        cx(
          "flex items-center gap-3 rounded-lg px-3 py-2 text-[15px] transition-colors",
          isActive ? "bg-surface font-semibold text-ink shadow-[0_0_0_1px_var(--line)]" : "text-muted hover:bg-surface/60 hover:text-ink",
        )
      }
    >
      <span className="[&>svg]:size-[18px]">{icon}</span>
      {children}
    </NavLink>
  );
}

function Sidebar({ user, onNavigate }: { user: User; onNavigate?: () => void }) {
  const { t } = useTranslation();
  return (
    <nav className="grid content-start gap-1 p-3" onClick={onNavigate} aria-label={t("nav.main")}>
      <NavItem to="/" icon={<Gauge />}>{t("nav.dashboard")}</NavItem>
      <NavItem to="/sites" icon={<Globe />}>{t("nav.sites")}</NavItem>
      <NavItem to="/databases" icon={<Database />}>{t("nav.databases")}</NavItem>
      <NavItem to="/apps" icon={<Boxes />}>{t("nav.apps")}</NavItem>
      <NavItem to="/mail" icon={<Mail />}>{t("nav.mail")}</NavItem>
      <NavItem to="/dns" icon={<CloudCog />}>{t("nav.dns")}</NavItem>
      {user.role === "admin" && (
        <>
          <NavItem to="/backups" icon={<ArchiveRestore />}>{t("nav.backups")}</NavItem>
          <NavItem to="/servers" icon={<Server />}>{t("nav.servers")}</NavItem>
          <p className="px-3 pt-5 pb-1 text-xs font-semibold tracking-wider text-muted uppercase">{t("nav.admin")}</p>
          <NavItem to="/customers" icon={<Users />}>{t("nav.customers")}</NavItem>
          <NavItem to="/packages" icon={<Package />}>{t("nav.packages")}</NavItem>
          <NavItem to="/audit" icon={<ScrollText />}>{t("nav.audit")}</NavItem>
        </>
      )}
      <p className="px-3 pt-5 pb-1 text-xs font-semibold tracking-wider text-muted uppercase">{t("nav.account")}</p>
      <NavItem to="/settings/security" icon={<ShieldCheck />}>{t("nav.security")}</NavItem>
      <NavItem to="/settings/notifications" icon={<Bell />}>{t("nav.notifications")}</NavItem>
    </nav>
  );
}

export function LanguageMenu({ user }: { user?: User | null }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  return (
    <label className="relative flex items-center">
      <span className="sr-only">{t("common.language")}</span>
      <Languages className="pointer-events-none absolute start-2.5 size-4 text-muted" aria-hidden />
      <select
        value={i18n.resolvedLanguage}
        onChange={async (e) => {
          const code = e.target.value;
          await i18n.changeLanguage(code);
          if (user) {
            try {
              qc.setQueryData(meKey, await api.patch<User>("/auth/me", { locale: code }));
            } catch {
              // the language still changes for this browser
            }
          }
        }}
        className="h-9 appearance-none rounded-lg border border-line bg-surface ps-8 pe-3 text-sm text-ink hover:bg-sunk"
      >
        {languages.map((l) => (
          <option key={l.code} value={l.code}>
            {l.name}
          </option>
        ))}
      </select>
    </label>
  );
}

export function ThemeMenu() {
  const { t } = useTranslation();
  const { theme, setTheme } = useTheme();
  const options: { value: Theme; icon: ReactNode; label: string }[] = [
    { value: "light", icon: <Sun />, label: t("theme.light") },
    { value: "dark", icon: <Moon />, label: t("theme.dark") },
    { value: "system", icon: <Monitor />, label: t("theme.system") },
  ];
  return (
    <div role="radiogroup" aria-label={t("theme.label")} className="flex rounded-lg border border-line bg-surface p-0.5">
      {options.map((o) => (
        <button
          key={o.value}
          role="radio"
          aria-checked={theme === o.value}
          title={o.label}
          onClick={() => setTheme(o.value)}
          className={cx("rounded-md p-1.5 [&>svg]:size-4", theme === o.value ? "bg-accent-soft text-accent" : "text-muted hover:text-ink")}
        >
          {o.icon}
          <span className="sr-only">{o.label}</span>
        </button>
      ))}
    </div>
  );
}

export function AppShell({ user }: { user: User }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const location = useLocation();
  const [drawer, setDrawer] = useState(false);
  const [palette, setPalette] = useState(false);

  useEffect(() => setDrawer(false), [location.pathname]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const logout = async () => {
    try {
      await api.post("/auth/logout");
    } finally {
      qc.setQueryData(meKey, null);
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== "me" && q.queryKey[0] !== "setup-status" });
      navigate("/login");
    }
  };

  const initials = user.name
    .split(/\s+/)
    .map((p) => p[0])
    .slice(0, 2)
    .join("")
    .toUpperCase();

  return (
    <div className="flex min-h-full">
      {/* Sidebar: fixed column on desktop, slide-over drawer on phones */}
      <aside className="sticky top-0 hidden h-screen w-60 shrink-0 flex-col border-e border-line bg-sunk lg:flex">
        <div className="flex h-16 items-center px-5">
          <Logo />
        </div>
        <Sidebar user={user} />
      </aside>
      {drawer && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div className="absolute inset-0 bg-black/40" onClick={() => setDrawer(false)} />
          <aside className="absolute inset-y-0 start-0 flex w-72 max-w-[85vw] flex-col bg-sunk shadow-xl">
            <div className="flex h-16 items-center justify-between px-5">
              <Logo />
              <button onClick={() => setDrawer(false)} className="rounded-md p-1 text-muted" aria-label={t("common.close")}>
                <X className="size-5" />
              </button>
            </div>
            <Sidebar user={user} />
          </aside>
        </div>
      )}

      <div className="flex min-w-0 flex-1 flex-col">
        <ImpersonationBanner user={user} />
        <header className="sticky top-0 z-30 flex h-16 items-center gap-3 border-b border-line bg-bg/90 px-4 backdrop-blur sm:px-6">
          <button onClick={() => setDrawer(true)} className="rounded-md p-1.5 text-muted lg:hidden" aria-label={t("nav.open_menu")}>
            <Menu className="size-5" />
          </button>
          <button
            onClick={() => setPalette(true)}
            className="flex h-9 min-w-0 flex-1 items-center gap-2 rounded-lg border border-line bg-surface px-3 text-sm text-muted hover:bg-sunk sm:max-w-sm"
          >
            <Search className="size-4 shrink-0" aria-hidden />
            <span className="truncate">{t("palette.placeholder")}</span>
            <kbd className="ms-auto hidden rounded border border-line px-1.5 font-mono text-[11px] sm:inline">Ctrl K</kbd>
          </button>
          <div className="ms-auto flex items-center gap-2">
            <div className="hidden items-center gap-2 md:flex">
              <LanguageMenu user={user} />
              <ThemeMenu />
            </div>
            <div className="flex items-center gap-2 ps-1">
              <NotificationBell />
              <span className="grid size-9 place-items-center rounded-full bg-amber-soft text-xs font-bold text-amber" title={user.email} aria-hidden>
                {initials || <Activity className="size-4" />}
              </span>
              <button onClick={logout} className="rounded-md p-1.5 text-muted hover:bg-sunk hover:text-ink" title={t("nav.sign_out")}>
                <LogOut className="size-5 rtl:-scale-x-100" aria-hidden />
                <span className="sr-only">{t("nav.sign_out")}</span>
              </button>
            </div>
          </div>
        </header>
        <main className="mx-auto w-full max-w-7xl flex-1 px-4 py-6 sm:px-6 sm:py-8">
          <Outlet />
        </main>
        {/* On phones the language and theme controls live at the bottom of the page. */}
        <footer className="flex flex-wrap items-center justify-center gap-2 border-t border-line px-4 py-4 md:hidden">
          <LanguageMenu user={user} />
          <ThemeMenu />
        </footer>
      </div>
      <CommandPalette open={palette} onClose={() => setPalette(false)} user={user} />
      <TaskTray />
    </div>
  );
}

export function PageHeader({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-2xl font-bold tracking-tight text-balance">{title}</h1>
        {description && <p className="mt-1 max-w-2xl text-muted">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
    </div>
  );
}

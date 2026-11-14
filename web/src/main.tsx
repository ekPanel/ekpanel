import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { createBrowserRouter, Link, Navigate, Outlet, RouterProvider, useLocation } from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import "./i18n";
import "./index.css";
import { useMe, useSetupStatus } from "./lib/session";
import { AppShell } from "./components/AppShell";
import { ErrorAlert, Spinner } from "./components/ui";
import { LoginPage, SetupPage } from "./pages/AuthPages";
import { DashboardPage } from "./pages/DashboardPage";
import { ServersPage } from "./pages/ServersPage";
import { ServerDetailPage } from "./pages/ServerDetailPage";
import { AuditPage, SecurityPage } from "./pages/AccountPages";
import { SitesPage } from "./pages/SitesPage";
import { SiteDetailPage } from "./pages/SiteDetailPage";
import { DatabaseDetailPage, DatabasesPage } from "./pages/DatabasesPage";
import { FileManagerPage } from "./pages/FileManagerPage";
import { BackupRepoPage, BackupsPage } from "./pages/BackupsPage";
import { AppDetailPage, AppsPage } from "./pages/AppsPage";
import { CatalogPage } from "./pages/CatalogPage";
import { DNSPage } from "./pages/DNSPage";
import { ZonePage } from "./pages/ZonePage";
import { HostedZonePage } from "./pages/HostedZonePage";
import { MailDomainPage, MailPage } from "./pages/MailPage";
import { WordPressPage } from "./pages/WordPressPage";
import { ServerSecurityPage } from "./pages/SecurityPage";
import { CustomersPage, PackagesPage } from "./pages/CustomersPage";
import { ResellersPage } from "./pages/ResellersPage";
import { BrandingPage } from "./pages/BrandingPage";
import { NotificationsPage } from "./pages/NotificationsPage";
import { UpdatesPage } from "./pages/UpdatesPage";
import { ImportPage } from "./pages/ImportPage";
import { TokensPage } from "./pages/TokensPage";
import { AppTerminalPage, ServerTerminalPage } from "./pages/TerminalPage";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: true } },
});

/** Sends visitors to setup or login as needed, then renders the app shell. */
function SignedIn() {
  const { t } = useTranslation();
  const location = useLocation();
  const status = useSetupStatus();
  const me = useMe();
  if (status.isPending || me.isPending) return <Spinner />;
  if (status.error || me.error) {
    return (
      <div className="mx-auto max-w-md p-8">
        <ErrorAlert>{t("errors.network")}</ErrorAlert>
      </div>
    );
  }
  if (status.data.needs_setup) return <Navigate to="/setup" replace />;
  if (!me.data) {
    const next = location.pathname + location.search;
    return <Navigate to={next === "/" ? "/login" : `/login?next=${encodeURIComponent(next)}`} replace />;
  }
  return <AppShell user={me.data} />;
}

function AdminOnly() {
  const me = useMe();
  return me.data?.role === "admin" ? <Outlet /> : <Navigate to="/" replace />;
}

// Administrators and resellers manage customers and packages (a reseller only their own).
function ManagesCustomers() {
  const me = useMe();
  return me.data?.role === "admin" || me.data?.role === "reseller" ? <Outlet /> : <Navigate to="/" replace />;
}

function NotFound() {
  const { t } = useTranslation();
  return (
    <div className="grid justify-items-start gap-3">
      <h1 className="text-2xl font-bold">{t("not_found.title")}</h1>
      <p className="text-muted">{t("not_found.body")}</p>
      <Link to="/" className="text-accent hover:underline">
        {t("not_found.home")}
      </Link>
    </div>
  );
}

const router = createBrowserRouter([
  { path: "/setup", element: <SetupPage /> },
  { path: "/login", element: <LoginPage /> },
  {
    element: <SignedIn />,
    children: [
      { path: "/", element: <DashboardPage /> },
      { path: "/settings/security", element: <SecurityPage /> },
      { path: "/settings/notifications", element: <NotificationsPage /> },
      { path: "/settings/tokens", element: <TokensPage /> },
      // Customers use these too; the API shows them only what they own.
      { path: "/sites", element: <SitesPage /> },
      { path: "/sites/:id", element: <SiteDetailPage /> },
      { path: "/sites/:id/files", element: <FileManagerPage /> },
      { path: "/databases", element: <DatabasesPage /> },
      { path: "/databases/:id", element: <DatabaseDetailPage /> },
      { path: "/apps", element: <AppsPage /> },
      { path: "/wordpress", element: <WordPressPage /> },
      { path: "/mail", element: <MailPage /> },
      { path: "/mail/:id", element: <MailDomainPage /> },
      { path: "/dns", element: <DNSPage /> },
      { path: "/dns/:id", element: <ZonePage /> },
      { path: "/dns/hosted/:id", element: <HostedZonePage /> },
      { path: "/apps/catalog", element: <CatalogPage /> },
      { path: "/apps/:id", element: <AppDetailPage /> },
      { path: "/apps/:id/terminal", element: <AppTerminalPage /> },
      {
        element: <ManagesCustomers />,
        children: [
          { path: "/customers", element: <CustomersPage /> },
          { path: "/packages", element: <PackagesPage /> },
          { path: "/settings/branding", element: <BrandingPage /> },
        ],
      },
      {
        element: <AdminOnly />,
        children: [
          { path: "/resellers", element: <ResellersPage /> },
          { path: "/backups", element: <BackupsPage /> },
          { path: "/backups/:id", element: <BackupRepoPage /> },
          { path: "/security", element: <ServerSecurityPage /> },
          { path: "/servers", element: <ServersPage /> },
          { path: "/servers/:id", element: <ServerDetailPage /> },
          { path: "/servers/:id/terminal", element: <ServerTerminalPage /> },
          { path: "/audit", element: <AuditPage /> },
          { path: "/settings/updates", element: <UpdatesPage /> },
          { path: "/import", element: <ImportPage /> },
        ],
      },
      { path: "*", element: <NotFound /> },
    ],
  },
]);

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);

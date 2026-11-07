import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, ExternalLink, Search } from "lucide-react";
import { api, errorMessage, type App, type CatalogTemplate, type Server } from "../lib/api";
import { formatBytes } from "../lib/format";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Dialog, ErrorAlert, Field, Spinner, cx } from "../components/ui";
import { EmptyState } from "./ServerParts";
import { OwnerField } from "./CustomersPage";

const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

/** One colour per category, so the grid reads at a glance without anyone's logo. */
const categoryTone: Record<string, string> = {
  monitoring: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  automation: "bg-orange-500/15 text-orange-600 dark:text-orange-400",
  development: "bg-sky-500/15 text-sky-600 dark:text-sky-400",
  security: "bg-rose-500/15 text-rose-600 dark:text-rose-400",
  analytics: "bg-violet-500/15 text-violet-600 dark:text-violet-400",
  publishing: "bg-amber-500/15 text-amber-700 dark:text-amber-400",
  productivity: "bg-teal-500/15 text-teal-600 dark:text-teal-400",
  notifications: "bg-fuchsia-500/15 text-fuchsia-600 dark:text-fuchsia-400",
  database: "bg-red-500/15 text-red-600 dark:text-red-400",
};

export function TemplateMark({ template, className }: { template: Pick<CatalogTemplate, "name" | "category">; className?: string }) {
  const letters = template.name
    .split(/[\s-]+/)
    .map((w) => w[0])
    .join("")
    .slice(0, 2);
  return (
    <span
      className={cx("grid size-11 shrink-0 place-items-center rounded-xl text-base font-bold", categoryTone[template.category] ?? "bg-sunk", className)}
      aria-hidden
    >
      {letters.length > 1 ? letters.toUpperCase() : template.name.slice(0, 2)}
    </span>
  );
}

export function useCatalog(enabled = true) {
  return useQuery({ queryKey: ["catalog"], queryFn: () => api.get<CatalogTemplate[]>("/catalog"), staleTime: Infinity, enabled });
}

export function CatalogPage() {
  const { t, i18n } = useTranslation();
  const catalog = useCatalog();
  const [query, setQuery] = useState("");
  const [category, setCategory] = useState("");
  const [chosen, setChosen] = useState<CatalogTemplate | null>(null);

  const categories = useMemo(() => [...new Set((catalog.data ?? []).map((c) => c.category))], [catalog.data]);
  const q = query.trim().toLowerCase();
  const shown = (catalog.data ?? []).filter(
    (c) =>
      (!category || c.category === category) &&
      (!q || c.name.toLowerCase().includes(q) || t(`catalog.apps.${c.id}.description`).toLowerCase().includes(q) || c.id.includes(q)),
  );

  return (
    <>
      <Link to="/apps" className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        {t("nav.apps")}
      </Link>
      <PageHeader title={t("catalog.title")} description={t("catalog.intro")} />
      {catalog.isPending && <Spinner />}
      {catalog.error && <ErrorAlert>{errorMessage(t, catalog.error)}</ErrorAlert>}
      {catalog.data && (
        <div className="grid gap-5">
          <div className="flex flex-wrap items-center gap-3">
            <label className="relative min-w-60 flex-1 sm:max-w-sm">
              <span className="sr-only">{t("catalog.search")}</span>
              <Search className="pointer-events-none absolute start-3 top-1/2 size-4 -translate-y-1/2 text-muted" aria-hidden />
              <input
                type="search"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder={t("catalog.search")}
                className="h-10 w-full rounded-lg border border-line bg-surface ps-9 pe-3 text-ink"
              />
            </label>
            <div className="flex flex-wrap gap-1.5" role="group" aria-label={t("catalog.category")}>
              {["", ...categories].map((c) => (
                <button
                  key={c || "all"}
                  type="button"
                  aria-pressed={category === c}
                  onClick={() => setCategory(c)}
                  className={cx(
                    "rounded-full border px-3 py-1 text-sm",
                    category === c ? "border-accent bg-accent-soft text-accent" : "border-line text-muted hover:border-accent hover:text-ink",
                  )}
                >
                  {c ? t(`catalog.categories.${c}`) : t("catalog.all")}
                </button>
              ))}
            </div>
          </div>

          {shown.length === 0 ? (
            <EmptyState icon={<Search />} title={t("catalog.none")} body={t("catalog.none_body")} />
          ) : (
            <ul className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
              {shown.map((c) => (
                <li key={c.id}>
                  <button
                    type="button"
                    onClick={() => setChosen(c)}
                    disabled={!c.allowed}
                    aria-labelledby={`tpl-${c.id}`}
                    aria-describedby={`tpl-${c.id}-about`}
                    className="grid h-full w-full content-start gap-3 rounded-xl border border-line bg-surface p-4 text-start transition-colors hover:border-accent focus-visible:border-accent disabled:cursor-not-allowed disabled:opacity-60 disabled:hover:border-line"
                  >
                    <span className="flex items-center gap-3">
                      <TemplateMark template={c} />
                      <span className="grid min-w-0">
                        <span id={`tpl-${c.id}`} className="truncate font-semibold">
                          {c.name}
                        </span>
                        <span className="text-sm text-muted">{t(`catalog.categories.${c.category}`)}</span>
                      </span>
                    </span>
                    <span id={`tpl-${c.id}-about`} className="line-clamp-2 text-sm text-muted">
                      {t(`catalog.apps.${c.id}.description`)}
                    </span>
                    <span className="mt-auto flex flex-wrap items-center gap-2 text-xs text-muted">
                      <span>{t("catalog.memory", { size: formatBytes(c.memory_mb * 1024 * 1024, i18n.language) })}</span>
                      {c.images.length > 1 && <span>· {t("catalog.services", { count: c.images.length })}</span>}
                      {!c.allowed && <Badge tone="warn">{t("catalog.not_allowed")}</Badge>}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {chosen && <InstallDialog template={chosen} onClose={() => setChosen(null)} />}
    </>
  );
}

function InstallDialog({ template, onClose }: { template: CatalogTemplate; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers") });
  // Connected servers first, so the default choice is one that can act now.
  const ready = (servers.data ?? []).filter((s) => s.enrolled_at).sort((a, b) => Number(b.online) - Number(a.online));
  const [server, setServer] = useState("");
  const [owner, setOwner] = useState("");
  const [name, setName] = useState(template.id);
  const [domain, setDomain] = useState("");
  const [tls, setTls] = useState<App["tls"]>("auto");
  const [memory, setMemory] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!server && ready.length) setServer(ready[0].id);
  }, [ready, server]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const app = await api.post<App>("/apps", {
        server_id: server,
        owner_id: owner,
        name,
        template: template.id,
        domain: template.needs_domain ? domain : "",
        tls,
        memory_mb: memory ? Number(memory) : null,
      });
      qc.invalidateQueries({ queryKey: ["apps"] });
      navigate(`/apps/${app.id}`);
    } catch (err) {
      setError(errorMessage(t, err));
      setBusy(false);
    }
  };

  return (
    <Dialog open onClose={onClose} title={t("catalog.install_title", { name: template.name })} wide>
      <form onSubmit={submit} className="grid gap-5">
        <div className="flex items-start gap-3">
          <TemplateMark template={template} />
          <div className="grid gap-1 text-sm">
            <p className="text-muted">{t(`catalog.apps.${template.id}.description`)}</p>
            <a href={template.website} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-accent hover:underline">
              {t("catalog.website")}
              <ExternalLink className="size-3.5" aria-hidden />
            </a>
          </div>
        </div>
        <ErrorAlert>{error}</ErrorAlert>
        {ready.length === 0 && servers.data && <ErrorAlert>{t("catalog.no_servers")}</ErrorAlert>}
        <div className={cx("grid gap-4", ready.length > 1 && "sm:grid-cols-2")}>
          <Field
            label={t("apps.name")}
            hint={t("apps.name_hint")}
            value={name}
            onChange={(e) => setName(e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, ""))}
            dir="ltr"
            maxLength={32}
            required
          />
          {ready.length > 1 && (
            <label className="grid content-start gap-1.5">
              <span className="text-sm font-medium">{t("sites.server")}</span>
              <select value={server} onChange={(e) => setServer(e.target.value)} className={select}>
                {ready.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
        <OwnerField value={owner} onChange={setOwner} />
        {template.needs_domain && (
          <div className="grid gap-4 sm:grid-cols-[1fr_auto]">
            <Field
              label={t("apps.domain")}
              hint={t("catalog.domain_hint")}
              value={domain}
              onChange={(e) => setDomain(e.target.value)}
              dir="ltr"
              placeholder={`${template.id}.example.com`}
              required
            />
            <label className="grid content-start gap-1.5">
              <span className="text-sm font-medium">{t("sites.tls.label")}</span>
              <select value={tls} onChange={(e) => setTls(e.target.value as App["tls"])} className={select}>
                <option value="auto">{t("sites.tls.auto")}</option>
                <option value="internal">{t("sites.tls.internal")}</option>
              </select>
            </label>
          </div>
        )}
        <p className="rounded-lg bg-sunk px-3 py-2 text-sm text-muted">{t("catalog.secrets_note")}</p>
        <details className="rounded-lg border border-line px-3 py-2">
          <summary className="cursor-pointer text-sm font-medium">{t("catalog.what_runs")}</summary>
          <div className="grid gap-3 pt-3">
            <Field
              label={t("apps.memory")}
              hint={t("catalog.memory_hint", { size: template.memory_mb })}
              value={memory}
              onChange={(e) => setMemory(e.target.value.replace(/\D/g, ""))}
              inputMode="numeric"
              dir="ltr"
              placeholder={t("apps.no_limit")}
            />
            <div className="grid gap-1.5 text-sm">
              <span className="font-medium">{t("catalog.images")}</span>
              <ul className="flex flex-wrap gap-1.5" dir="ltr">
                {template.images.map((i) => (
                  <li key={i} className="rounded-md bg-sunk px-2 py-0.5 font-mono text-xs">
                    {i}
                  </li>
                ))}
              </ul>
            </div>
            <pre className="max-h-72 overflow-auto rounded-lg bg-sunk p-3 font-mono text-xs leading-relaxed" dir="ltr">
              {template.compose}
            </pre>
          </div>
        </details>
        <p className="text-sm text-muted">{t("apps.first_run")}</p>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!server || !name}>
            {t("catalog.install")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowRightLeft, CheckCircle2, Loader2, Trash2 } from "lucide-react";
import { api, ApiError, errorMessage, type BackupRepo, type Database, type Server, type Site, type SiteMove } from "../lib/api";
import { formatBytes, formatDateTime } from "../lib/format";
import { Badge, Button, Card, Dialog, ErrorAlert, type Tone } from "./ui";

const phaseTone: Record<SiteMove["phase"], Tone> = {
  copying: "accent",
  switching: "accent",
  moved: "warn",
  cleaning: "accent",
  done: "ok",
  failed: "crit",
};

/** The text key for a kind of thing: "intro" for websites, "intro_app", "intro_database". */
const kindKey = (thing: { kind: string }, key: string) => (thing.kind === "site" ? key : `${key}_${thing.kind}`);

const running = (m: SiteMove) => m.phase === "copying" || m.phase === "switching" || m.phase === "cleaning";

/** What can move: a website or an app. */
export interface Movable {
  kind: "site" | "app" | "database";
  id: string;
  name: string;
  server_id: string;
  server_name: string;
  state: string;
  owner_id: string | null;
}

/** Moving a website to another server (administrators). */
export function SiteMoveCard({ site }: { site: Site }) {
  return (
    <MoveCard
      thing={{ kind: "site", id: site.id, name: site.domain_display, server_id: site.server_id, server_name: site.server_name, state: site.state, owner_id: site.owner_id }}
    />
  );
}

/** Moving a website or an app to another server (administrators). */
export function MoveCard({ thing }: { thing: Movable }) {
  const site = thing;
  const base = thing.kind === "site" ? "sites" : thing.kind === "app" ? "apps" : "databases";
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const moves = useQuery({
    queryKey: ["moves", base, site.id],
    queryFn: () => api.get<SiteMove[]>(`/${base}/${site.id}/moves`),
    refetchInterval: (q) => (q.state.data?.some(running) ? 2000 : 30000),
  });
  const latest = moves.data?.[0];
  const [rebuilding, setRebuilding] = useState(false);
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers"), refetchInterval: 30000 });
  const source = servers.data?.find((s) => s.id === thing.server_id);
  // A website whose server is offline can be rebuilt elsewhere from a backup.
  const sourceDown = thing.kind === "site" && source !== undefined && !source.online;
  const target = servers.data?.find((s) => s.id === latest?.to_server);
  const targetIP = target?.public_ipv4 ?? target?.addresses?.[0];

  const cleanup = async (m: SiteMove) => {
    if (!window.confirm(t(`move.${kindKey(thing, "cleanup_confirm")}`, { server: m.from_server_name }))) return;
    setBusy(true);
    setError("");
    try {
      try {
        await api.post(`/moves/${m.id}/cleanup`);
      } catch (e) {
        // The old databases may still be used by things that stayed on the old server.
        if (!(e instanceof ApiError) || e.code !== "old_copy_in_use") throw e;
        const users = (e.data.users as string[] | undefined)?.join(", ") ?? "";
        if (!window.confirm(t("move.in_use_confirm", { users }))) return;
        await api.post(`/moves/${m.id}/cleanup?force=1`);
      }
      qc.invalidateQueries({ queryKey: ["moves", base, site.id] });
    } catch (e) {
      setError(errorMessage(t, e));
    } finally {
      setBusy(false);
    }
  };

  const canMove = site.state === "active" && (!latest || latest.phase === "done" || latest.phase === "failed");

  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">{t("move.title")}</h2>
          <p className="text-sm text-muted">{t(`move.${kindKey(thing, "intro")}`, { server: site.server_name })}</p>
        </div>
        <div className="flex flex-wrap gap-2">
          {canMove && !sourceDown && (
            <Button onClick={() => setOpen(true)}>
              <ArrowRightLeft className="size-4" aria-hidden /> {t("move.start")}
            </Button>
          )}
          {sourceDown && (!latest || latest.phase === "done" || latest.phase === "failed") && (
            <Button variant="primary" onClick={() => setRebuilding(true)}>
              <ArrowRightLeft className="size-4" aria-hidden /> {t("move.rebuild.start")}
            </Button>
          )}
        </div>
      </div>
      {sourceDown && <p className="rounded-lg bg-warn-soft px-3 py-2 text-sm text-warn">{t("move.rebuild.offline", { server: thing.server_name })}</p>}
      {error && <ErrorAlert>{error}</ErrorAlert>}
      {latest && (
        <div className="grid gap-3 rounded-lg border border-line p-4 text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone={phaseTone[latest.phase]}>
              {running(latest) && <Loader2 className="size-3.5 animate-spin" aria-hidden />}
              {t(`move.phase.${latest.phase}`)}
            </Badge>
            <span>{t("move.route", { from: latest.from_server_name, to: latest.to_server_name })}</span>
            <span className="text-muted">{formatDateTime(latest.created_at, i18n.language)}</span>
          </div>
          {latest.bytes > 0 && <p className="text-muted">{t("move.copied", { size: formatBytes(latest.bytes, i18n.language) })}</p>}
          {latest.error && <ErrorAlert>{latest.error}</ErrorAlert>}
          {(latest.phase === "moved" || (latest.phase === "cleaning" && latest.error)) && (
            <>
              <p className="flex items-start gap-2">
                <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-ok" aria-hidden />
                {t(`move.${kindKey(thing, "moved")}`, { to: latest.to_server_name, from: latest.from_server_name })}
              </p>
              {thing.kind !== "database" && <p className="text-muted">{targetIP ? t("move.dns_hint", { ip: targetIP }) : t("move.dns_hint_noip")}</p>}
              <div>
                <Button variant="danger" size="sm" loading={busy} onClick={() => cleanup(latest)}>
                  <Trash2 className="size-4" aria-hidden /> {t("move.cleanup", { server: latest.from_server_name })}
                </Button>
              </div>
            </>
          )}
        </div>
      )}
      {open && <MoveDialog thing={thing} onClose={() => setOpen(false)} />}
      {rebuilding && <RebuildDialog thing={thing} onClose={() => setRebuilding(false)} />}
    </Card>
  );
}

function MoveDialog({ thing, onClose }: { thing: Movable; onClose: () => void }) {
  const site = thing;
  const base = thing.kind === "site" ? "sites" : thing.kind === "app" ? "apps" : "databases";
  const { t } = useTranslation();
  const qc = useQueryClient();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers") });
  const dbs = useQuery({ queryKey: ["databases"], queryFn: () => api.get<Database[]>("/databases"), enabled: thing.kind === "site" });
  const [target, setTarget] = useState("");
  const [chosen, setChosen] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const candidates = (servers.data ?? []).filter((s) => s.id !== site.server_id && s.enrolled_at);
  const siteDBs = (dbs.data ?? []).filter((d) => d.server_id === site.server_id && (d.owner_id ?? null) === (site.owner_id ?? null));

  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await api.post(`/${base}/${site.id}/move`, thing.kind === "site" ? { server_id: target, database_ids: chosen } : { server_id: target });
      qc.invalidateQueries({ queryKey: ["moves", base, site.id] });
      onClose();
    } catch (e) {
      setError(errorMessage(t, e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open onClose={onClose} title={t("move.dialog_title", { domain: site.name })}>
      <div className="grid gap-4">
        {error && <ErrorAlert>{error}</ErrorAlert>}
        <label className="grid gap-1.5 text-sm">
          <span className="font-medium">{t("move.target")}</span>
          <select className="h-10 rounded-lg border border-line bg-surface px-3 text-ink" value={target} onChange={(e) => setTarget(e.target.value)}>
            <option value="">{t("move.choose_server")}</option>
            {candidates.map((s) => (
              <option key={s.id} value={s.id} disabled={!s.online}>
                {s.name}
                {!s.online ? ` (${t("servers.state.offline")})` : ""}
              </option>
            ))}
          </select>
        </label>
        {thing.kind === "site" && (
        <fieldset className="grid gap-2 text-sm">
          <legend className="mb-1 font-medium">{t("move.databases")}</legend>
          {siteDBs.length === 0 && <p className="text-muted">{t("move.no_databases")}</p>}
          {siteDBs.map((d) => (
            <label key={d.id} className="flex items-center gap-2.5">
              <input
                type="checkbox"
                className="size-4 accent-[var(--accent)]"
                checked={chosen.includes(d.id)}
                onChange={(e) => setChosen((c) => (e.target.checked ? [...c, d.id] : c.filter((x) => x !== d.id)))}
              />
              <span className="font-mono">{d.name}</span>
              <span className="text-muted">{d.engine === "mariadb" ? "MariaDB" : "PostgreSQL"}</span>
            </label>
          ))}
          {siteDBs.length > 0 && <p className="text-muted">{t("move.databases_hint")}</p>}
        </fieldset>
        )}
        <ol className="grid list-decimal gap-1 ps-5 text-sm text-muted">
          <li>{t(`move.${kindKey(thing, "step_copy")}`)}</li>
          <li>{t("move.step_switch")}</li>
          {thing.kind !== "database" && <li>{t("move.step_dns")}</li>}
          <li>{t(`move.${kindKey(thing, "step_cleanup")}`)}</li>
        </ol>
        <p className="rounded-lg bg-warn-soft px-3 py-2 text-sm text-warn">{t(`move.${thing.kind === "site" ? "writes_warning" : thing.kind === "app" ? "app_warning" : "database_warning"}`)}</p>
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>{t("common.cancel")}</Button>
          <Button variant="primary" loading={busy} disabled={!target} onClick={submit}>
            <ArrowRightLeft className="size-4" aria-hidden /> {t("move.confirm")}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

/** Rebuilds a website on another server from a backup, for a server that is gone. */
function RebuildDialog({ thing, onClose }: { thing: Movable; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const servers = useQuery({ queryKey: ["servers"], queryFn: () => api.get<Server[]>("/servers") });
  const repos = useQuery({ queryKey: ["backups"], queryFn: () => api.get<BackupRepo[]>("/backups") });
  const dbs = useQuery({ queryKey: ["databases"], queryFn: () => api.get<Database[]>("/databases") });
  const [target, setTarget] = useState("");
  const [repo, setRepo] = useState("");
  const [chosen, setChosen] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const candidates = (servers.data ?? []).filter((s) => s.id !== thing.server_id && s.enrolled_at && s.online);
  const targetRepos = (repos.data ?? []).filter((r) => r.server_id === target && r.kind === "s3");
  const siteDBs = (dbs.data ?? []).filter((d) => d.server_id === thing.server_id && (d.owner_id ?? null) === (thing.owner_id ?? null));
  const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await api.post(`/sites/${thing.id}/rebuild`, { server_id: target, repo_id: repo, database_ids: chosen });
      qc.invalidateQueries({ queryKey: ["moves", "sites", thing.id] });
      onClose();
    } catch (e) {
      setError(errorMessage(t, e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open onClose={onClose} title={t("move.rebuild.title", { domain: thing.name })}>
      <div className="grid gap-4">
        {error && <ErrorAlert>{error}</ErrorAlert>}
        <p className="text-sm text-muted">{t("move.rebuild.intro")}</p>
        <label className="grid gap-1.5 text-sm">
          <span className="font-medium">{t("move.target")}</span>
          <select className={select} value={target} onChange={(e) => (setTarget(e.target.value), setRepo(""))}>
            <option value="">{t("move.choose_server")}</option>
            {candidates.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name}
              </option>
            ))}
          </select>
        </label>
        <label className="grid gap-1.5 text-sm">
          <span className="font-medium">{t("move.rebuild.repo")}</span>
          <select className={select} value={repo} onChange={(e) => setRepo(e.target.value)} disabled={!target}>
            <option value="">{t("move.rebuild.choose_repo")}</option>
            {targetRepos.map((r) => (
              <option key={r.id} value={r.id}>
                {r.name} · {r.location}
              </option>
            ))}
          </select>
          {target && targetRepos.length === 0 && <span className="text-muted">{t("move.rebuild.no_repo")}</span>}
        </label>
        {siteDBs.length > 0 && (
          <fieldset className="grid gap-2 text-sm">
            <legend className="mb-1 font-medium">{t("move.databases")}</legend>
            {siteDBs.map((d) => (
              <label key={d.id} className="flex items-center gap-2.5">
                <input
                  type="checkbox"
                  className="size-4 accent-[var(--accent)]"
                  checked={chosen.includes(d.id)}
                  onChange={(e) => setChosen((c) => (e.target.checked ? [...c, d.id] : c.filter((x) => x !== d.id)))}
                />
                <span className="font-mono">{d.name}</span>
              </label>
            ))}
          </fieldset>
        )}
        <p className="rounded-lg bg-warn-soft px-3 py-2 text-sm text-warn">{t("move.rebuild.warning")}</p>
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>{t("common.cancel")}</Button>
          <Button variant="primary" loading={busy} disabled={!target || !repo} onClick={submit}>
            {t("move.rebuild.confirm")}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

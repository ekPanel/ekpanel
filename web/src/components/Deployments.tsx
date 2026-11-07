import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Eye, EyeOff, GitBranch, Rocket, Undo2, Webhook } from "lucide-react";
import { api, errorMessage, type App, type Deployment } from "../lib/api";
import { formatElapsed, formatRelative } from "../lib/format";
import { Badge, Button, Card, CopyButton, ErrorAlert, Spinner, cx, type Tone } from "./ui";

const active = (d: Deployment) => d.status === "queued" || d.status === "building" || d.status === "releasing";

const statusTone: Record<Deployment["status"], Tone> = {
  queued: "neutral",
  building: "warn",
  releasing: "warn",
  live: "ok",
  failed: "crit",
  superseded: "neutral",
  cancelled: "neutral",
};

export function useDeployments(app: App) {
  return useQuery({
    queryKey: ["deployments", app.id],
    queryFn: () => api.get<Deployment[]>(`/apps/${app.id}/deployments`),
    enabled: app.kind === "git",
    refetchInterval: (q) => (q.state.data?.some(active) ? 2000 : 30000),
  });
}

/** Starts a build and release of the app's branch. */
export function DeployButton({ app, onError }: { app: App; onError: (msg: string) => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const deployments = useDeployments(app);
  const [busy, setBusy] = useState(false);
  const running = deployments.data?.some(active);
  return (
    <Button
      size="sm"
      variant="primary"
      loading={busy || running}
      disabled={busy || running || app.state === "deleting"}
      onClick={async () => {
        setBusy(true);
        onError("");
        try {
          await api.post<Deployment>(`/apps/${app.id}/deploy`);
          await qc.invalidateQueries({ queryKey: ["deployments", app.id] });
        } catch (err) {
          onError(errorMessage(t, err));
        } finally {
          setBusy(false);
        }
      }}
    >
      <Rocket className="size-4" aria-hidden />
      {running ? t("deploys.deploying") : t("deploys.deploy")}
    </Button>
  );
}

/** Where the app is built from, and how a forge tells the panel about pushes. */
export function GitCard({ app }: { app: App }) {
  const { t } = useTranslation();
  const info = useQuery({
    queryKey: ["git-info", app.id],
    queryFn: () => api.get<{ webhook_url: string; webhook_secret: string; deploy_key_public: string | null }>(`/apps/${app.id}/git`),
  });
  const [showSecret, setShowSecret] = useState(false);
  const ssh = !!app.git_url && !app.git_url.startsWith("https://");
  const row = (label: string, value: string, shown = value) => (
    <div className="grid min-w-0 grid-cols-1 gap-1">
      <span className="text-sm font-medium">{label}</span>
      <div className="flex items-center gap-2 rounded-lg bg-sunk px-3 py-2">
        <span className="min-w-0 flex-1 truncate font-mono text-xs" dir="ltr">
          {shown}
        </span>
        <CopyButton text={value} className="-my-1 shrink-0" />
      </div>
    </div>
  );
  return (
    <Card className="grid gap-4 p-5">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <GitBranch className="size-5 text-muted" aria-hidden />
        <span className="min-w-0 truncate font-mono text-sm" dir="ltr">
          {app.git_url}
        </span>
        <Badge>{app.git_branch}</Badge>
        <span className="text-sm text-muted">{app.auto_deploy ? t("deploys.auto_on") : t("deploys.auto_off")}</span>
      </div>
      {info.error && <ErrorAlert>{errorMessage(t, info.error)}</ErrorAlert>}
      {info.data && (
        <div className="grid gap-4 lg:grid-cols-2">
          <div className="grid min-w-0 grid-cols-1 content-start gap-3">
            <h3 className="flex items-center gap-2 text-sm font-semibold">
              <Webhook className="size-4" aria-hidden />
              {t("deploys.webhook")}
            </h3>
            <p className="text-sm text-muted">{t("deploys.webhook_hint")}</p>
            {row(t("deploys.webhook_url"), info.data.webhook_url)}
            <div className="grid min-w-0 grid-cols-1 gap-1">
              <span className="flex items-center gap-2 text-sm font-medium">
                {t("deploys.webhook_secret")}
                <Button size="sm" variant="ghost" onClick={() => setShowSecret(!showSecret)} aria-pressed={showSecret}>
                  {showSecret ? <EyeOff className="size-4" aria-hidden /> : <Eye className="size-4" aria-hidden />}
                  {showSecret ? t("catalog.hide") : t("catalog.show")}
                </Button>
              </span>
              <div className="flex items-center gap-2 rounded-lg bg-sunk px-3 py-2">
                <span className="min-w-0 flex-1 truncate font-mono text-xs" dir="ltr">
                  {showSecret ? info.data.webhook_secret : "•".repeat(24)}
                </span>
                <CopyButton text={info.data.webhook_secret} className="-my-1 shrink-0" />
              </div>
            </div>
          </div>
          {info.data.deploy_key_public && (
            <div className="grid min-w-0 grid-cols-1 content-start gap-3">
              <h3 className="text-sm font-semibold">{t("deploys.deploy_key")}</h3>
              <p className="text-sm text-muted">{ssh ? t("deploys.deploy_key_hint") : t("deploys.deploy_key_https")}</p>
              {row(t("deploys.public_key"), info.data.deploy_key_public)}
            </div>
          )}
        </div>
      )}
    </Card>
  );
}

/** The app's deployments, newest first, with the log of the chosen one. */
export function DeploymentsCard({ app }: { app: App }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const list = useDeployments(app);
  const [chosen, setChosen] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const shown = list.data?.find((d) => d.id === chosen) ?? list.data?.[0];

  const rollback = async (d: Deployment) => {
    setBusy(d.id);
    setError("");
    try {
      const nd = await api.post<Deployment>(`/apps/${app.id}/deployments/${d.id}/rollback`);
      await qc.invalidateQueries({ queryKey: ["deployments", app.id] });
      setChosen(nd.id);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy("");
    }
  };
  const took = (d: Deployment) =>
    d.started_at && d.finished_at ? formatElapsed((Date.parse(d.finished_at) - Date.parse(d.started_at)) / 1000, i18n.language) : null;

  return (
    <Card className="overflow-hidden">
      <h2 className="border-b border-line px-4 py-3 font-semibold">{t("deploys.title")}</h2>
      {list.isPending && <Spinner />}
      {(list.error || error) && (
        <div className="p-4">
          <ErrorAlert>{error || errorMessage(t, list.error)}</ErrorAlert>
        </div>
      )}
      {list.data?.length === 0 && <p className="px-4 py-6 text-center text-sm text-muted">{t("deploys.none")}</p>}
      {!!list.data?.length && (
        <div className="grid xl:grid-cols-[2fr_3fr]">
          <ul className="max-h-[32rem] divide-y divide-line overflow-y-auto border-line xl:border-e">
            {list.data.map((d) => (
              <li key={d.id} className={cx("flex items-start gap-2 px-4 py-3", shown?.id === d.id && "bg-sunk")}>
                <button type="button" onClick={() => setChosen(d.id)} className="grid min-w-0 flex-1 gap-1 text-start" aria-pressed={shown?.id === d.id}>
                  <span className="flex flex-wrap items-center gap-2 text-sm">
                    <span className="font-semibold tabular-nums">#{d.number}</span>
                    <Badge tone={statusTone[d.status]}>{t(`deploys.status.${d.status}`)}</Badge>
                    {d.commit_sha && (
                      <span className="font-mono text-xs text-muted" dir="ltr">
                        {d.commit_sha.slice(0, 7)}
                      </span>
                    )}
                  </span>
                  <span className="truncate text-sm" dir="auto">
                    {d.commit_message ?? t("deploys.branch_head", { branch: d.branch })}
                  </span>
                  <span className="text-xs text-muted">
                    {t(`deploys.trigger.${d.trigger}`, { name: d.created_by_name ?? "" })} · {formatRelative(d.created_at, i18n.language)}
                    {took(d) && ` · ${took(d)}`}
                  </span>
                </button>
                {d.rollback_possible && (
                  <Button size="sm" variant="ghost" loading={busy === d.id} disabled={!!busy || list.data.some(active)} onClick={() => rollback(d)}>
                    <Undo2 className="size-4 rtl:-scale-x-100" aria-hidden />
                    {t("deploys.rollback")}
                  </Button>
                )}
              </li>
            ))}
          </ul>
          {shown && <DeploymentLog app={app} deployment={shown} />}
        </div>
      )}
    </Card>
  );
}

/** The log of one deployment; while it runs, only new output is fetched. */
function DeploymentLog({ app, deployment }: { app: App; deployment: Deployment }) {
  const { t } = useTranslation();
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const offset = useRef(0);
  const pre = useRef<HTMLPreElement>(null);
  const stick = useRef(true);
  const running = active(deployment);

  useEffect(() => {
    offset.current = 0;
    setText("");
    setError("");
    stick.current = true;
  }, [deployment.id]);

  useEffect(() => {
    let stop = false;
    let timer: number | undefined;
    const fetchMore = async () => {
      try {
        const r = await api.get<{ log: string; length: number; status: Deployment["status"] }>(
          `/apps/${app.id}/deployments/${deployment.id}/log?offset=${offset.current}`,
        );
        if (stop) return;
        if (r.log) {
          offset.current = r.length;
          setText((s) => s + r.log);
        }
        if (active({ ...deployment, status: r.status })) timer = window.setTimeout(fetchMore, 1500);
      } catch (err) {
        if (!stop) setError(errorMessage(t, err));
      }
    };
    fetchMore();
    return () => {
      stop = true;
      window.clearTimeout(timer);
    };
    // Restart following when the deployment changes, or when it starts running again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [app.id, deployment.id, running]);

  useEffect(() => {
    if (stick.current && pre.current) pre.current.scrollTop = pre.current.scrollHeight;
  }, [text]);

  return (
    <div className="grid min-w-0 content-start">
      {deployment.error && <p className="border-b border-line px-4 py-2 text-sm break-words text-crit">{deployment.error}</p>}
      {error && (
        <div className="p-4">
          <ErrorAlert>{error}</ErrorAlert>
        </div>
      )}
      <pre
        ref={pre}
        onScroll={(e) => {
          const el = e.currentTarget;
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
        className="h-[32rem] overflow-auto bg-term-bg p-4 font-mono text-xs leading-relaxed whitespace-pre-wrap text-term-fg"
        dir="ltr"
        aria-label={t("deploys.log", { number: deployment.number })}
        aria-busy={running}
      >
        {text || (running ? t("deploys.waiting") : t("deploys.no_log"))}
      </pre>
    </div>
  );
}

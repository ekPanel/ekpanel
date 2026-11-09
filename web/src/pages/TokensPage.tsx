import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus, Trash2 } from "lucide-react";
import { api, errorMessage, type APIToken, type CreatedAPIToken } from "../lib/api";
import { formatDateTime, formatRelative } from "../lib/format";
import { useMe } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { Badge, Button, Card, CopyButton, Dialog, ErrorAlert, Field, Spinner } from "../components/ui";

const adminScopes = ["read", "sites", "databases", "apps", "dns", "mail", "backups", "servers", "customers", "settings"];
const customerScopes = ["read", "sites", "databases", "apps", "dns", "mail", "settings"];
const expiries = [30, 90, 365, 0];

/** Account → API tokens: keys for scripts and the ekctl CLI. */
export function TokensPage() {
  const { t, i18n } = useTranslation();
  const me = useMe();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");
  const acting = !!me.data?.impersonator_id;
  const q = useQuery({ queryKey: ["tokens"], queryFn: () => api.get<APIToken[]>("/tokens"), enabled: !acting });

  const revoke = async (tok: APIToken) => {
    if (!window.confirm(t("tokens.revoke_confirm", { name: tok.name }))) return;
    setError("");
    try {
      await api.del(`/tokens/${tok.id}`);
      qc.invalidateQueries({ queryKey: ["tokens"] });
    } catch (e) {
      setError(errorMessage(t, e));
    }
  };

  return (
    <>
      <PageHeader
        title={t("tokens.title")}
        description={t("tokens.intro")}
        actions={
          !acting && (
            <Button variant="primary" onClick={() => setAdding(true)}>
              <Plus className="size-4" aria-hidden /> {t("tokens.add")}
            </Button>
          )
        }
      />
      <div className="grid max-w-4xl gap-6">
        {acting && <ErrorAlert>{t("security.not_while_acting")}</ErrorAlert>}
        {error && <ErrorAlert>{error}</ErrorAlert>}
        {q.isPending && !acting && <Spinner />}
        {q.data && q.data.length === 0 && (
          <Card className="grid justify-items-start gap-2 p-6">
            <KeyRound className="size-6 text-muted" aria-hidden />
            <p className="font-medium">{t("tokens.empty")}</p>
            <p className="text-sm text-muted">{t("tokens.empty_hint")}</p>
          </Card>
        )}
        {q.data && q.data.length > 0 && (
          <Card className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-muted">
                <tr className="border-b border-line">
                  <th className="px-4 py-2.5 text-start font-medium">{t("tokens.name")}</th>
                  <th className="px-4 py-2.5 text-start font-medium">{t("tokens.scopes")}</th>
                  <th className="px-4 py-2.5 text-start font-medium">{t("tokens.last_used")}</th>
                  <th className="px-4 py-2.5 text-start font-medium">{t("tokens.expires")}</th>
                  <th className="px-4 py-2.5" />
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {q.data.map((tok) => {
                  const expired = tok.expires_at !== null && new Date(tok.expires_at).getTime() < Date.now();
                  return (
                    <tr key={tok.id}>
                      <td className="px-4 py-3">
                        <p className="font-medium">{tok.name}</p>
                        <p className="font-mono text-xs text-muted">{tok.prefix}…</p>
                      </td>
                      <td className="px-4 py-3">
                        <span className="flex flex-wrap gap-1">
                          {tok.scopes.map((s) => (
                            <Badge key={s}>{t(`tokens.scope.${s}`)}</Badge>
                          ))}
                        </span>
                      </td>
                      <td className="px-4 py-3 text-muted">
                        {tok.last_used_at ? (
                          <span title={tok.last_used_ip}>{formatRelative(tok.last_used_at, i18n.language)}</span>
                        ) : (
                          t("tokens.never_used")
                        )}
                      </td>
                      <td className="px-4 py-3">
                        {tok.expires_at === null ? (
                          <span className="text-muted">{t("tokens.never")}</span>
                        ) : expired ? (
                          <Badge tone="crit">{t("tokens.expired")}</Badge>
                        ) : (
                          <span className="text-muted">{formatDateTime(tok.expires_at, i18n.language)}</span>
                        )}
                      </td>
                      <td className="px-4 py-3 text-end">
                        <Button variant="danger" size="sm" onClick={() => revoke(tok)}>
                          <Trash2 className="size-4" aria-hidden /> {t("tokens.revoke")}
                        </Button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </Card>
        )}
        <Card className="grid gap-2 p-5 text-sm">
          <h2 className="font-semibold">{t("tokens.how_title")}</h2>
          <p className="text-muted">{t("tokens.how_body")}</p>
          <pre className="overflow-x-auto rounded-lg bg-sunk px-3 py-2 font-mono text-xs">
            {`curl -H "Authorization: Bearer ekp_…" ${window.location.origin}/api/v1/sites`}
          </pre>
          <p className="text-muted">{t("tokens.how_cli")}</p>
        </Card>
      </div>
      {adding && <AddTokenDialog onClose={() => setAdding(false)} admin={me.data?.role === "admin"} />}
    </>
  );
}

function AddTokenDialog({ onClose, admin }: { onClose: () => void; admin: boolean }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const scopes = admin ? adminScopes : customerScopes;
  const [name, setName] = useState("");
  const [chosen, setChosen] = useState<string[]>(["read"]);
  const [days, setDays] = useState(90);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [created, setCreated] = useState<CreatedAPIToken | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      setCreated(await api.post<CreatedAPIToken>("/tokens", { name, scopes: chosen, expires_in_days: days }));
      qc.invalidateQueries({ queryKey: ["tokens"] });
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open onClose={onClose} title={created ? t("tokens.created_title") : t("tokens.add")}>
      {created ? (
        <div className="grid gap-4">
          <p className="text-sm">{t("tokens.created_once")}</p>
          <div className="flex items-center gap-2 rounded-lg border border-line bg-sunk px-3 py-2">
            <code className="min-w-0 flex-1 font-mono text-sm break-all">{created.token}</code>
            <CopyButton text={created.token} />
          </div>
          <Button variant="primary" onClick={onClose} className="justify-self-end">
            {t("tokens.done")}
          </Button>
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-4">
          {error && <ErrorAlert>{error}</ErrorAlert>}
          <Field label={t("tokens.name")} hint={t("tokens.name_hint")} value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} autoFocus />
          <fieldset className="grid gap-2">
            <legend className="mb-1 text-sm font-medium">{t("tokens.scopes")}</legend>
            {scopes.map((s) => (
              <label key={s} className="flex items-start gap-2.5 text-sm">
                <input
                  type="checkbox"
                  className="mt-0.5 size-4 accent-[var(--accent)]"
                  checked={chosen.includes(s)}
                  onChange={(e) => setChosen((c) => (e.target.checked ? [...c, s] : c.filter((x) => x !== s)))}
                />
                <span>
                  <span className="font-medium">{t(`tokens.scope.${s}`)}</span>
                  <span className="block text-muted">{t(`tokens.scope_hint.${s}`)}</span>
                </span>
              </label>
            ))}
          </fieldset>
          <label className="grid gap-1.5 text-sm">
            <span className="font-medium">{t("tokens.expires")}</span>
            <select className="h-10 rounded-lg border border-line bg-surface px-3 text-ink" value={days} onChange={(e) => setDays(Number(e.target.value))}>
              {expiries.map((d) => (
                <option key={d} value={d}>
                  {d === 0 ? t("tokens.never") : t("tokens.days", { count: d })}
                </option>
              ))}
            </select>
          </label>
          <p className="text-sm text-muted">{t("tokens.never_with_token")}</p>
          <div className="flex justify-end gap-2">
            <Button type="button" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy} disabled={!name.trim() || chosen.length === 0}>
              {t("tokens.create")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Bell, BellRing, Mail, MessageCircle, Pencil, Plus, Send, Trash2, Webhook } from "lucide-react";
import { api, ApiError, errorMessage, type Channel, type ChannelKind, type NotificationItem } from "../lib/api";
import { formatRelative } from "../lib/format";
import { useMe } from "../lib/session";
import { PageHeader } from "../components/AppShell";
import { Button, Card, CopyButton, Dialog, ErrorAlert, Field, Spinner, cx } from "../components/ui";

const select = "h-10 rounded-lg border border-line bg-surface px-3 text-ink";

// ---- Bell in the header ----

export function NotificationBell() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const q = useQuery({
    queryKey: ["notifications"],
    queryFn: () => api.get<{ notifications: NotificationItem[]; unread: number }>("/notifications?limit=20"),
    refetchInterval: 30000,
  });
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false);
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
    };
  }, [open]);
  const unread = q.data?.unread ?? 0;
  const readAll = async () => {
    await api.post("/notifications/read", {});
    qc.invalidateQueries({ queryKey: ["notifications"] });
  };
  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen((o) => !o)}
        className="relative rounded-md p-1.5 text-muted hover:bg-sunk hover:text-ink"
        aria-label={t("notifications.bell", { count: unread })}
        aria-expanded={open}
      >
        {unread > 0 ? <BellRing className="size-5" aria-hidden /> : <Bell className="size-5" aria-hidden />}
        {unread > 0 && (
          <span className="absolute -end-0.5 -top-0.5 grid min-w-4 place-items-center rounded-full bg-crit px-1 text-[10px] leading-4 font-bold text-white">
            {unread > 99 ? "99+" : unread}
          </span>
        )}
      </button>
      {open && (
        <div className="absolute end-0 top-11 z-50 w-[22rem] max-w-[calc(100vw-2rem)] overflow-hidden rounded-xl border border-line bg-surface shadow-xl">
          <div className="flex items-center gap-2 border-b border-line px-4 py-2.5">
            <h2 className="font-semibold">{t("notifications.title")}</h2>
            {unread > 0 && (
              <button onClick={readAll} className="ms-auto text-sm text-accent hover:underline">
                {t("notifications.read_all")}
              </button>
            )}
          </div>
          <ul className="max-h-[26rem] divide-y divide-line overflow-auto">
            {q.data?.notifications.length === 0 && <li className="px-4 py-6 text-center text-sm text-muted">{t("notifications.empty")}</li>}
            {q.data?.notifications.map((n) => {
              const path = n.link ? new URL(n.link).pathname : "";
              const body = (
                <>
                  <span className="flex items-start gap-2">
                    {!n.read_at && <span className="mt-1.5 size-2 shrink-0 rounded-full bg-accent" aria-label={t("notifications.unread")} />}
                    <span className={cx("font-medium", n.severity === "critical" && "text-crit")}>{n.title}</span>
                  </span>
                  <span className="line-clamp-2 text-muted">{n.body}</span>
                  <span className="text-xs text-muted">{formatRelative(n.created_at, i18n.language)}</span>
                </>
              );
              return (
                <li key={n.id}>
                  {path && path !== "/" ? (
                    <Link to={path} onClick={() => setOpen(false)} className="grid gap-0.5 px-4 py-3 text-sm hover:bg-sunk">
                      {body}
                    </Link>
                  ) : (
                    <div className="grid gap-0.5 px-4 py-3 text-sm">{body}</div>
                  )}
                </li>
              );
            })}
          </ul>
          <Link
            to="/settings/notifications"
            onClick={() => setOpen(false)}
            className="block border-t border-line px-4 py-2.5 text-center text-sm text-accent hover:bg-sunk"
          >
            {t("notifications.settings")}
          </Link>
        </div>
      )}
    </div>
  );
}

// ---- Channels ----

const kindIcon: Record<ChannelKind, React.ReactNode> = {
  email: <Mail className="size-4" aria-hidden />,
  telegram: <Send className="size-4" aria-hidden />,
  discord: <MessageCircle className="size-4" aria-hidden />,
  slack: <MessageCircle className="size-4" aria-hidden />,
  webhook: <Webhook className="size-4" aria-hidden />,
};

function target(c: Channel) {
  return c.config.address || c.config.chat_id || c.config.host || "";
}

function ChannelDialog({ open, channel, events, onClose }: { open: boolean; channel: Channel | null; events: string[]; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [kind, setKind] = useState<ChannelKind>("email");
  const [name, setName] = useState("");
  const [address, setAddress] = useState("");
  const [chatId, setChatId] = useState("");
  const [token, setToken] = useState("");
  const [url, setUrl] = useState("");
  const [chosen, setChosen] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [key, setKey] = useState("");
  useEffect(() => {
    if (!open) return;
    setKind(channel?.kind ?? "email");
    setName(channel?.name ?? "");
    setAddress(channel?.config.address ?? "");
    setChatId(channel?.config.chat_id ?? "");
    setToken("");
    setUrl("");
    setChosen(channel?.events ?? []);
    setError("");
    setKey("");
  }, [open, channel]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const body = { kind, name, address, chat_id: chatId, token, url, events: chosen, enabled: channel?.enabled ?? true };
    try {
      const r = channel ? await api.put<Channel>(`/notifications/channels/${channel.id}`, body) : await api.post<Channel>("/notifications/channels", body);
      qc.invalidateQueries({ queryKey: ["channels"] });
      if (r.key) setKey(r.key);
      else onClose();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  const keep = channel ? t("notifications.keep_secret") : undefined;

  return (
    <Dialog open={open} onClose={onClose} title={channel ? t("notifications.edit") : t("notifications.add")} wide>
      {key ? (
        <div className="grid gap-4">
          <div className="grid gap-2 rounded-lg bg-warn-soft p-4 text-warn">
            <p className="text-sm font-medium">{t("notifications.key_once")}</p>
            <div className="flex items-center gap-2">
              <code className="rounded bg-surface px-2 py-1 font-mono text-sm break-all text-ink" dir="ltr">
                {key}
              </code>
              <CopyButton text={key} />
            </div>
          </div>
          <p className="text-sm text-muted">{t("notifications.key_hint")}</p>
          <div className="flex justify-end">
            <Button variant="primary" onClick={onClose}>
              {t("backups.saved_it")}
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="grid gap-4">
          <ErrorAlert>{error}</ErrorAlert>
          <div className="grid gap-4 sm:grid-cols-2">
            <label className="grid gap-1.5">
              <span className="text-sm font-medium">{t("notifications.kind")}</span>
              <select value={kind} onChange={(e) => setKind(e.target.value as ChannelKind)} className={select} disabled={!!channel}>
                {(["email", "telegram", "discord", "slack", "webhook"] as const).map((k) => (
                  <option key={k} value={k}>
                    {t(`notifications.kinds.${k}`)}
                  </option>
                ))}
              </select>
            </label>
            <Field
              label={t("notifications.name")}
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={60}
              placeholder={t(`notifications.kinds.${kind}`)}
            />
          </div>
          {kind === "email" && (
            <Field label={t("notifications.address")} type="email" value={address} onChange={(e) => setAddress(e.target.value)} dir="ltr" required />
          )}
          {kind === "telegram" && (
            <div className="grid gap-4 sm:grid-cols-2">
              <Field
                label={t("notifications.bot_token")}
                hint={keep ?? t("notifications.bot_token_hint")}
                value={token}
                onChange={(e) => setToken(e.target.value)}
                dir="ltr"
                autoComplete="off"
                required={!channel}
              />
              <Field
                label={t("notifications.chat_id")}
                hint={t("notifications.chat_id_hint")}
                value={chatId}
                onChange={(e) => setChatId(e.target.value)}
                dir="ltr"
                required
              />
            </div>
          )}
          {(kind === "discord" || kind === "slack" || kind === "webhook") && (
            <Field
              label={t("notifications.url")}
              hint={keep ?? t(`notifications.url_hint.${kind}`)}
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              dir="ltr"
              autoComplete="off"
              required={!channel}
              placeholder={channel ? channel.config.host : "https://"}
            />
          )}
          <fieldset className="grid gap-1.5">
            <legend className="mb-1 text-sm font-medium">{t("notifications.events")}</legend>
            <div className="grid gap-x-4 gap-y-1.5 sm:grid-cols-2">
              {events.map((ev) => (
                <label key={ev} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={chosen.includes(ev)}
                    onChange={(e) => setChosen(e.target.checked ? [...chosen, ev] : chosen.filter((x) => x !== ev))}
                    className="accent-[var(--accent)]"
                  />
                  {t(`notifications.event_names.${ev}`)}
                </label>
              ))}
            </div>
            <span className="text-sm text-muted">{t("notifications.events_hint")}</span>
          </fieldset>
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="primary" loading={busy}>
              {channel ? t("sites.save") : t("notifications.create")}
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

function ChannelsCard() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["channels"], queryFn: () => api.get<{ channels: Channel[]; events: string[] }>("/notifications/channels") });
  const [editing, setEditing] = useState<Channel | null>(null);
  const [adding, setAdding] = useState(false);
  const [message, setMessage] = useState<{ id: string; ok: boolean; text: string } | null>(null);
  const act = async (run: () => Promise<unknown>) => {
    try {
      await run();
    } finally {
      qc.invalidateQueries({ queryKey: ["channels"] });
    }
  };
  return (
    <Card className="overflow-hidden">
      <div className="flex items-center gap-3 border-b border-line px-4 py-3">
        <h2 className="font-semibold">{t("notifications.channels")}</h2>
        <Button size="sm" className="ms-auto" onClick={() => setAdding(true)}>
          <Plus className="size-4" aria-hidden />
          {t("notifications.add")}
        </Button>
      </div>
      {q.isPending && <Spinner />}
      {q.error && (
        <div className="p-4">
          <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert>
        </div>
      )}
      {q.data?.channels.length === 0 && <p className="px-4 py-6 text-center text-sm text-muted">{t("notifications.no_channels")}</p>}
      <ul className="divide-y divide-line">
        {q.data?.channels.map((c) => (
          <li key={c.id} className={cx("grid gap-1 px-4 py-3 text-sm", !c.enabled && "opacity-60")}>
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-muted">{kindIcon[c.kind]}</span>
              <span className="font-medium">{c.name}</span>
              <span className="truncate text-muted" dir="ltr">
                {target(c)}
              </span>
              <span className="ms-auto flex items-center gap-1">
                <label className="me-1 flex items-center gap-1.5 text-xs text-muted">
                  <input
                    type="checkbox"
                    checked={c.enabled}
                    onChange={(e) =>
                      act(() =>
                        api.put(`/notifications/channels/${c.id}`, {
                          kind: c.kind,
                          name: c.name,
                          address: c.config.address ?? "",
                          chat_id: c.config.chat_id ?? "",
                          events: c.events,
                          enabled: e.target.checked,
                        }),
                      )
                    }
                    className="accent-[var(--accent)]"
                  />
                  {t("cron.on")}
                </label>
                <Button
                  size="sm"
                  variant="ghost"
                  title={t("notifications.test")}
                  aria-label={t("notifications.test")}
                  onClick={async () => {
                    setMessage(null);
                    try {
                      await api.post(`/notifications/channels/${c.id}/test`);
                      setMessage({ id: c.id, ok: true, text: t("notifications.test_sent") });
                    } catch (err) {
                      // What the service answered is the useful part of a failed test.
                      const detail = err instanceof ApiError && err.code === "channel_failed" ? ` ${err.message}` : "";
                      setMessage({ id: c.id, ok: false, text: errorMessage(t, err) + detail });
                    }
                    qc.invalidateQueries({ queryKey: ["channels"] });
                  }}
                >
                  <Send className="size-4" aria-hidden />
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setEditing(c)} title={t("notifications.edit")} aria-label={t("notifications.edit")}>
                  <Pencil className="size-4" aria-hidden />
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => act(() => api.del(`/notifications/channels/${c.id}`))}
                  title={t("notifications.delete")}
                  aria-label={t("notifications.delete")}
                >
                  <Trash2 className="size-4" aria-hidden />
                </Button>
              </span>
            </div>
            <p className="text-xs text-muted">
              {c.events.length ? c.events.map((e) => t(`notifications.event_names.${e}`)).join(", ") : t("notifications.all_events")}
              {c.last_sent_at && " · " + t("notifications.last_sent", { when: formatRelative(c.last_sent_at, i18n.language) })}
            </p>
            {c.last_error && <p className="text-xs break-words text-crit">{c.last_error}</p>}
            {message?.id === c.id && <p className={cx("text-xs", message.ok ? "text-ok" : "text-crit")}>{message.text}</p>}
          </li>
        ))}
      </ul>
      <ChannelDialog open={adding || editing !== null} channel={editing} events={q.data?.events ?? []} onClose={() => (setAdding(false), setEditing(null))} />
    </Card>
  );
}

// ---- Settings (administrators) ----

interface NotifySettings {
  panel_url: string;
  smtp: { host: string; port: number; username: string; from: string; tls: string; password?: string; password_set: boolean };
}

function MailSettingsCard() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["notify-settings"], queryFn: () => api.get<NotifySettings>("/settings/notifications") });
  const [f, setF] = useState<NotifySettings | null>(null);
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    if (q.data && !f) {
      setF({
        ...q.data,
        panel_url: q.data.panel_url || window.location.origin,
        smtp: { ...q.data.smtp, port: q.data.smtp.port || 587, tls: q.data.smtp.tls || "starttls" },
      });
    }
  }, [q.data, f]);
  if (!f) return q.error ? <ErrorAlert>{errorMessage(t, q.error)}</ErrorAlert> : <Spinner />;
  const set = (k: keyof NotifySettings["smtp"], v: string | number) => setF({ ...f, smtp: { ...f.smtp, [k]: v } });
  return (
    <Card className="p-6">
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          setSaved(false);
          try {
            const r = await api.put<NotifySettings>("/settings/notifications", { ...f, smtp: { ...f.smtp, password } });
            qc.setQueryData(["notify-settings"], r);
            setF({ ...f, smtp: { ...f.smtp, password_set: r.smtp.password_set } });
            setPassword("");
            setSaved(true);
          } catch (err) {
            setError(errorMessage(t, err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <div>
          <h2 className="font-semibold">{t("notifications.mail.title")}</h2>
          <p className="text-sm text-muted">{t("notifications.mail.body")}</p>
        </div>
        <ErrorAlert>{error}</ErrorAlert>
        <Field
          label={t("notifications.mail.panel_url")}
          hint={t("notifications.mail.panel_url_hint")}
          value={f.panel_url}
          onChange={(e) => setF({ ...f, panel_url: e.target.value })}
          dir="ltr"
        />
        <div className="grid gap-4 sm:grid-cols-[2fr_1fr_1fr]">
          <Field
            label={t("notifications.mail.host")}
            value={f.smtp.host}
            onChange={(e) => set("host", e.target.value)}
            dir="ltr"
            placeholder="smtp.example.com"
          />
          <Field
            label={t("notifications.mail.port")}
            value={String(f.smtp.port)}
            onChange={(e) => set("port", Number(e.target.value.replace(/\D/g, "")) || 0)}
            inputMode="numeric"
            dir="ltr"
          />
          <label className="grid content-start gap-1.5">
            <span className="text-sm font-medium">{t("notifications.mail.security")}</span>
            <select value={f.smtp.tls} onChange={(e) => set("tls", e.target.value)} className={select}>
              {(["starttls", "tls", "none"] as const).map((x) => (
                <option key={x} value={x}>
                  {t(`notifications.mail.tls.${x}`)}
                </option>
              ))}
            </select>
          </label>
        </div>
        <div className="grid gap-4 sm:grid-cols-3">
          <Field
            label={t("notifications.mail.from")}
            value={f.smtp.from}
            onChange={(e) => set("from", e.target.value)}
            dir="ltr"
            placeholder="ekPanel <panel@example.com>"
          />
          <Field
            label={t("notifications.mail.username")}
            value={f.smtp.username}
            onChange={(e) => set("username", e.target.value)}
            dir="ltr"
            autoComplete="off"
          />
          <Field
            label={t("notifications.mail.password")}
            hint={f.smtp.password_set ? t("notifications.keep_secret") : undefined}
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
          />
        </div>
        <div className="flex items-center gap-3">
          <Button type="submit" loading={busy}>
            {t("sites.save")}
          </Button>
          {saved && <span className="text-sm text-ok">{t("notifications.mail.saved")}</span>}
        </div>
      </form>
    </Card>
  );
}

export function NotificationsPage() {
  const { t } = useTranslation();
  const me = useMe();
  return (
    <>
      <PageHeader title={t("nav.notifications")} description={t("notifications.intro")} />
      <div className="grid max-w-4xl gap-6">
        {me.data?.impersonator_id ? <ErrorAlert>{t("security.not_while_acting")}</ErrorAlert> : <ChannelsCard />}
        {me.data?.role === "admin" && !me.data.impersonator_id && <MailSettingsCard />}
        <p className="text-sm text-muted">{t("notifications.inbox_note")}</p>
      </div>
    </>
  );
}

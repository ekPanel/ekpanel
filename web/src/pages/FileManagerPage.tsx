import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type FormEvent } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft, Archive, ChevronRight, Download, File, FilePlus, Folder, FolderPlus, FolderOpen, Home, Link2, Loader2,
  PackageOpen, Pencil, RefreshCw, Trash2, Upload, X,
} from "lucide-react";
import { api, ApiError, errorMessage, type FileEntry, type FilesResult, type Site } from "../lib/api";
import { formatBytes, formatRelative } from "../lib/format";
import { Button, Card, Dialog, ErrorAlert, Field, Spinner, cx } from "../components/ui";

const CodeEditor = lazy(() => import("../components/CodeEditor"));

const CHUNK = 512 * 1024;
const TRASH = "/.trash";
const EDIT_LIMIT = 2 * 1024 * 1024;
const ARCHIVE = /\.(zip|tar|tar\.gz|tgz)$/i;

const join = (dir: string, name: string) => (dir === "/" ? "" : dir) + "/" + name;
const parent = (p: string) => p.slice(0, p.lastIndexOf("/")) || "/";

function toBase64(bytes: Uint8Array): string {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(s);
}
function fromBase64(b64: string): Uint8Array {
  const s = atob(b64);
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
  return out;
}

// ---- Uploads ----

interface Upload {
  id: number;
  file: File;
  path: string;
  sent: number;
  state: "queued" | "sending" | "done" | "exists" | "error";
  error?: string;
}

function useUploads(siteId: string, onDone: () => void) {
  const { t } = useTranslation();
  const [uploads, setUploads] = useState<Upload[]>([]);
  const running = useRef(false);
  const nextId = useRef(1);
  const update = (id: number, patch: Partial<Upload>) => setUploads((u) => u.map((x) => (x.id === id ? { ...x, ...patch } : x)));

  const send = useCallback(
    async (u: Upload, replace: boolean) => {
      update(u.id, { state: "sending", sent: 0, error: undefined });
      try {
        let offset = 0;
        do {
          const piece = new Uint8Array(await u.file.slice(offset, offset + CHUNK).arrayBuffer());
          const final = offset + piece.length >= u.file.size;
          const q = new URLSearchParams({ path: u.path, offset: String(offset), ...(final ? { final: "1" } : {}), ...(replace ? { replace: "1" } : {}) });
          const res = await fetch(`/api/v1/sites/${siteId}/files/upload?${q}`, {
            method: "POST",
            credentials: "same-origin",
            headers: { "Content-Type": "application/octet-stream" },
            body: piece,
          });
          if (!res.ok) {
            const e = (await res.json().catch(() => ({}))) as { code?: string; message?: string };
            if (e.code === "file_exists") {
              update(u.id, { state: "exists" });
              return;
            }
            throw new ApiError(res.status, e.code ?? "internal", e.message ?? res.statusText);
          }
          offset += piece.length;
          update(u.id, { sent: offset });
        } while (offset < u.file.size);
        update(u.id, { state: "done" });
        onDone();
      } catch (err) {
        update(u.id, { state: "error", error: errorMessage(t, err) });
      }
    },
    [siteId, onDone, t],
  );

  // One upload at a time, in order.
  useEffect(() => {
    if (running.current) return;
    const next = uploads.find((u) => u.state === "queued");
    if (!next) return;
    running.current = true;
    send(next, false).finally(() => {
      running.current = false;
      setUploads((u) => [...u]);
    });
  }, [uploads, send]);

  const add = (files: FileList | File[], dir: string) =>
    setUploads((u) => [...u, ...Array.from(files).map((file) => ({ id: nextId.current++, file, path: join(dir, file.name), sent: 0, state: "queued" as const }))]);
  const replace = (u: Upload) => send(u, true);
  const clear = () => setUploads((u) => u.filter((x) => x.state === "queued" || x.state === "sending"));
  return { uploads, add, replace, clear };
}

// ---- Page ----

export function FileManagerPage() {
  const { id = "" } = useParams();
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const dir = params.get("path") || "/public_html";
  const site = useQuery({ queryKey: ["site", id], queryFn: () => api.get<Site>(`/sites/${id}`) });
  const listing = useQuery({
    queryKey: ["files", id, dir],
    queryFn: () => api.post<FilesResult>(`/sites/${id}/files`, { action: "list", path: dir }),
    retry: false,
  });
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [dialog, setDialog] = useState<null | { kind: "mkdir" | "newfile" | "rename" | "compress" | "chmod"; entry?: FileEntry }>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);

  const refresh = useCallback(() => qc.invalidateQueries({ queryKey: ["files", id] }), [qc, id]);
  const { uploads, add, replace, clear } = useUploads(id, refresh);
  const entries = useMemo(() => listing.data?.entries ?? [], [listing.data]);
  const inTrash = dir === TRASH || dir.startsWith(TRASH + "/");

  useEffect(() => setSelected(new Set()), [dir]);
  const go = (path: string) => setParams({ path }, { replace: false });

  const run = async (body: object, after?: () => void) => {
    setBusy(true);
    setError("");
    try {
      await api.post<FilesResult>(`/sites/${id}/files`, body);
      refresh();
      setSelected(new Set());
      after?.();
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };

  const selectedPaths = [...selected].map((n) => join(dir, n));
  const single = selected.size === 1 ? entries.find((e) => selected.has(e.name)) : undefined;

  const crumbs = dir.split("/").filter(Boolean);
  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    if (e.dataTransfer.files.length) add(e.dataTransfer.files, dir);
  };

  if (site.isPending) return <Spinner />;
  if (site.error || !site.data) return <ErrorAlert>{errorMessage(t, site.error)}</ErrorAlert>;

  return (
    <>
      <Link to={`/sites/${id}`} className="mb-4 inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink">
        <ArrowLeft className="size-4 rtl:-scale-x-100" aria-hidden />
        <bdi>{site.data.domain_display}</bdi>
      </Link>
      <div className="mb-4 flex flex-wrap items-end justify-between gap-3">
        <h1 className="text-2xl font-bold tracking-tight">{t("files.title")}</h1>
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="ghost" onClick={() => go(TRASH)}>
            <Trash2 className="size-4" aria-hidden />
            {t("files.trash")}
          </Button>
        </div>
      </div>

      {/* Breadcrumb: paths are file system names, always left to right */}
      <nav aria-label={t("files.location")} dir="ltr" className="mb-3 flex flex-wrap items-center gap-1 rounded-lg border border-line bg-surface px-3 py-2 font-mono text-sm">
        <button onClick={() => go("/")} className="rounded p-1 text-muted hover:bg-sunk hover:text-ink" title={t("files.home")}>
          <Home className="size-4" aria-hidden />
          <span className="sr-only">{t("files.home")}</span>
        </button>
        {crumbs.map((c, i) => (
          <span key={i} className="flex items-center gap-1">
            <ChevronRight className="size-3.5 text-muted" aria-hidden />
            <button onClick={() => go("/" + crumbs.slice(0, i + 1).join("/"))} className="rounded px-1.5 py-0.5 hover:bg-sunk">
              {c === ".trash" ? t("files.trash") : c}
            </button>
          </span>
        ))}
      </nav>

      {/* Toolbar */}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        {!inTrash ? (
          <>
            <Button size="sm" variant="primary" onClick={() => fileInput.current?.click()}>
              <Upload className="size-4" aria-hidden />
              {t("files.upload")}
            </Button>
            <input ref={fileInput} type="file" multiple hidden onChange={(e) => e.target.files && (add(e.target.files, dir), (e.target.value = ""))} />
            <Button size="sm" onClick={() => setDialog({ kind: "mkdir" })}>
              <FolderPlus className="size-4" aria-hidden />
              {t("files.new_folder")}
            </Button>
            <Button size="sm" onClick={() => setDialog({ kind: "newfile" })}>
              <FilePlus className="size-4" aria-hidden />
              {t("files.new_file")}
            </Button>
          </>
        ) : (
          <Button size="sm" variant="danger" onClick={() => run({ action: "empty_trash" })} disabled={entries.length === 0 || busy}>
            <Trash2 className="size-4" aria-hidden />
            {t("files.empty_trash")}
          </Button>
        )}
        <Button size="sm" variant="ghost" onClick={refresh} title={t("files.refresh")}>
          <RefreshCw className={cx("size-4", listing.isFetching && "animate-spin")} aria-hidden />
          <span className="sr-only">{t("files.refresh")}</span>
        </Button>
        {selected.size > 0 && (
          <div className="ms-auto flex flex-wrap items-center gap-2 rounded-lg bg-accent-soft px-2 py-1">
            <span className="px-1 text-sm font-medium text-accent">{t("files.selected", { count: selected.size })}</span>
            {single?.type === "file" && (
              <a className="inline-flex h-8 items-center gap-2 rounded-lg px-3 text-sm font-medium hover:bg-surface" href={`/api/v1/sites/${id}/files/download?path=${encodeURIComponent(join(dir, single.name))}`}>
                <Download className="size-4" aria-hidden />
                {t("files.download")}
              </a>
            )}
            {single && !inTrash && (
              <Button size="sm" variant="ghost" onClick={() => setDialog({ kind: "rename", entry: single })}>
                <Pencil className="size-4" aria-hidden />
                {t("files.rename")}
              </Button>
            )}
            {inTrash && single && (
              <Button size="sm" variant="ghost" onClick={() => run({ action: "rename", path: join(dir, single.name), to: "/public_html/" + single.name.replace(/^\d{8}-\d{6}-\d+-/, "") })}>
                <FolderOpen className="size-4" aria-hidden />
                {t("files.restore")}
              </Button>
            )}
            {single?.type === "file" && ARCHIVE.test(single.name) && !inTrash && (
              <Button size="sm" variant="ghost" onClick={() => run({ action: "extract", path: join(dir, single.name) })} loading={busy}>
                <PackageOpen className="size-4" aria-hidden />
                {t("files.extract")}
              </Button>
            )}
            {!inTrash && (
              <Button size="sm" variant="ghost" onClick={() => setDialog({ kind: "compress" })}>
                <Archive className="size-4" aria-hidden />
                {t("files.compress")}
              </Button>
            )}
            <Button size="sm" variant="danger" onClick={() => run({ action: "delete", paths: selectedPaths })} loading={busy}>
              <Trash2 className="size-4" aria-hidden />
              {inTrash ? t("files.delete_forever") : t("files.delete")}
            </Button>
            <button onClick={() => setSelected(new Set())} className="rounded p-1 text-muted hover:text-ink" aria-label={t("files.clear_selection")}>
              <X className="size-4" />
            </button>
          </div>
        )}
      </div>

      <div className="grid gap-3">
        <ErrorAlert>{error}</ErrorAlert>
        {inTrash && <p className="text-sm text-muted">{t("files.trash_hint")}</p>}

        {uploads.length > 0 && (
          <Card className="grid gap-2 p-3">
            {uploads.map((u) => (
              <div key={u.id} className="grid gap-1 text-sm">
                <div className="flex items-center gap-2">
                  {u.state === "sending" ? <Loader2 className="size-4 animate-spin text-accent" aria-hidden /> : <Upload className="size-4 text-muted" aria-hidden />}
                  <span className="min-w-0 truncate font-mono" dir="ltr">{u.file.name}</span>
                  <span className="ms-auto shrink-0 text-xs text-muted tabular-nums">
                    {formatBytes(u.sent, i18n.language)} / {formatBytes(u.file.size, i18n.language)}
                  </span>
                </div>
                <div className="h-1 overflow-hidden rounded bg-sunk">
                  <div className={cx("h-full", u.state === "error" ? "bg-crit" : u.state === "done" ? "bg-ok" : "bg-accent")} style={{ width: `${u.file.size ? (u.sent / u.file.size) * 100 : 100}%` }} />
                </div>
                {u.state === "exists" && (
                  <div className="flex items-center gap-2 text-warn">
                    {t("files.exists")}
                    <Button size="sm" variant="ghost" onClick={() => replace(u)}>
                      {t("files.replace")}
                    </Button>
                  </div>
                )}
                {u.error && <span className="text-crit">{u.error}</span>}
              </div>
            ))}
            {uploads.every((u) => u.state !== "queued" && u.state !== "sending") && (
              <div>
                <Button size="sm" variant="ghost" onClick={clear}>
                  {t("files.clear_uploads")}
                </Button>
              </div>
            )}
          </Card>
        )}

        <div
          className={cx("overflow-x-auto rounded-xl border border-line bg-surface transition-colors", dragging && "border-accent bg-accent-soft")}
          onDragOver={(e) => (e.preventDefault(), !inTrash && setDragging(true))}
          onDragLeave={() => setDragging(false)}
          onDrop={(e) => (inTrash ? e.preventDefault() : onDrop(e))}
        >
          {listing.isPending && <Spinner />}
          {listing.error && (
            <div className="p-4">
              <ErrorAlert>{errorMessage(t, listing.error)}</ErrorAlert>
            </div>
          )}
          {listing.data && (
            <table className="w-full min-w-[640px] text-sm">
              <thead>
                <tr className="border-b border-line bg-sunk text-xs tracking-wider text-muted uppercase">
                  <th className="w-10 px-3 py-2.5">
                    <input
                      type="checkbox"
                      aria-label={t("files.select_all")}
                      checked={entries.length > 0 && selected.size === entries.length}
                      onChange={(e) => setSelected(e.target.checked ? new Set(entries.map((x) => x.name)) : new Set())}
                      className="accent-[var(--accent)]"
                    />
                  </th>
                  <th className="px-3 py-2.5 text-start font-semibold">{t("files.name")}</th>
                  <th className="px-3 py-2.5 text-end font-semibold">{t("files.size")}</th>
                  <th className="px-3 py-2.5 text-start font-semibold">{t("files.modified")}</th>
                  <th className="px-3 py-2.5 text-start font-semibold">{t("files.permissions")}</th>
                </tr>
              </thead>
              <tbody>
                {dir !== "/" && (
                  <tr className="border-b border-line hover:bg-sunk/60">
                    <td />
                    <td className="px-3 py-2" colSpan={4}>
                      <button onClick={() => go(parent(dir))} className="flex items-center gap-2 text-muted hover:text-ink">
                        <Folder className="size-4" aria-hidden />
                        ..
                      </button>
                    </td>
                  </tr>
                )}
                {entries.length === 0 && (
                  <tr>
                    <td colSpan={5} className="px-3 py-10 text-center text-muted">
                      {inTrash ? t("files.trash_empty") : t("files.empty")}
                    </td>
                  </tr>
                )}
                {entries.map((e) => {
                  const full = join(dir, e.name);
                  const editable = e.type === "file" && e.size <= EDIT_LIMIT && !inTrash;
                  return (
                    <tr key={e.name} className={cx("border-b border-line last:border-0", selected.has(e.name) ? "bg-accent-soft/60" : "hover:bg-sunk/60")}>
                      <td className="px-3 py-2">
                        <input
                          type="checkbox"
                          aria-label={e.name}
                          checked={selected.has(e.name)}
                          onChange={(ev) => {
                            const next = new Set(selected);
                            if (ev.target.checked) next.add(e.name);
                            else next.delete(e.name);
                            setSelected(next);
                          }}
                          className="accent-[var(--accent)]"
                        />
                      </td>
                      <td className="px-3 py-2">
                        <span className="flex min-w-0 items-center gap-2">
                          {e.type === "dir" ? (
                            <Folder className="size-4 shrink-0 text-accent" aria-hidden />
                          ) : e.type === "link" ? (
                            <Link2 className="size-4 shrink-0 text-muted" aria-hidden />
                          ) : (
                            <File className="size-4 shrink-0 text-muted" aria-hidden />
                          )}
                          {e.type === "dir" ? (
                            <button onClick={() => go(full)} className="truncate font-medium hover:text-accent" dir="auto">
                              {e.name}
                            </button>
                          ) : editable ? (
                            <button onClick={() => setEditing(full)} className="truncate hover:text-accent" dir="auto" title={t("files.edit")}>
                              {e.name}
                            </button>
                          ) : (
                            <span className="truncate" dir="auto">
                              {e.name}
                            </span>
                          )}
                          {e.type === "link" && e.target && (
                            <span className="truncate font-mono text-xs text-muted" dir="ltr">
                              → {e.target}
                            </span>
                          )}
                        </span>
                      </td>
                      <td className="px-3 py-2 text-end text-muted tabular-nums">{e.type === "file" ? formatBytes(e.size, i18n.language) : ""}</td>
                      <td className="px-3 py-2 text-muted" title={new Date(e.modified).toLocaleString(i18n.language)}>
                        {formatRelative(e.modified, i18n.language)}
                      </td>
                      <td className="px-3 py-2">
                        {e.type !== "link" && !inTrash ? (
                          <button onClick={() => setDialog({ kind: "chmod", entry: e })} className="rounded px-1 font-mono text-muted hover:bg-sunk hover:text-ink" dir="ltr">
                            {e.mode}
                          </button>
                        ) : (
                          <span className="font-mono text-muted" dir="ltr">
                            {e.mode}
                          </span>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
        {!inTrash && <p className="text-center text-xs text-muted">{t("files.drop_hint")}</p>}
      </div>

      <NameDialog
        state={dialog}
        dir={dir}
        onClose={() => setDialog(null)}
        onSubmit={(kind, value, entry) => {
          if (kind === "mkdir") return run({ action: "mkdir", path: join(dir, value) }, () => setDialog(null));
          if (kind === "newfile")
            return run({ action: "write", path: join(dir, value), data: "" }, () => {
              setDialog(null);
              setEditing(join(dir, value));
            });
          if (kind === "rename" && entry) return run({ action: "rename", path: join(dir, entry.name), to: join(dir, value) }, () => setDialog(null));
          if (kind === "compress") return run({ action: "compress", paths: selectedPaths, to: join(dir, value) }, () => setDialog(null));
          if (kind === "chmod" && entry) return run({ action: "chmod", path: join(dir, entry.name), mode: value }, () => setDialog(null));
        }}
        busy={busy}
        error={dialog ? error : ""}
      />
      {editing && <EditorDialog siteId={id} path={editing} onClose={() => (setEditing(null), refresh())} />}
    </>
  );
}

// ---- Small dialogs ----

function NameDialog({
  state, dir, onClose, onSubmit, busy, error,
}: {
  state: null | { kind: "mkdir" | "newfile" | "rename" | "compress" | "chmod"; entry?: FileEntry };
  dir: string;
  onClose: () => void;
  onSubmit: (kind: "mkdir" | "newfile" | "rename" | "compress" | "chmod", value: string, entry?: FileEntry) => void;
  busy: boolean;
  error: string;
}) {
  const { t } = useTranslation();
  const [value, setValue] = useState("");
  useEffect(() => {
    if (!state) return;
    setValue(
      state.kind === "rename" ? state.entry!.name
        : state.kind === "chmod" ? state.entry!.mode
          : state.kind === "compress" ? `archive-${new Date().toISOString().slice(0, 10)}.zip`
            : "",
    );
  }, [state]);
  if (!state) return <Dialog open={false} onClose={onClose} title="">{null}</Dialog>;
  const valid = state.kind === "chmod" ? /^[0-7]{3}$/.test(value) : value.trim() !== "" && !value.includes("/");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (valid) onSubmit(state.kind, value.trim(), state.entry);
  };
  return (
    <Dialog open onClose={onClose} title={t(`files.dialog.${state.kind}`)}>
      <form onSubmit={submit} className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        <Field
          label={state.kind === "chmod" ? t("files.permissions") : t("files.name")}
          hint={state.kind === "chmod" ? t("files.chmod_hint") : <span dir="ltr" className="font-mono">{join(dir, value || "…")}</span>}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          className="[&_input]:font-mono"
          autoComplete="off"
          spellCheck={false}
        />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button type="submit" variant="primary" loading={busy} disabled={!valid}>
            {t("files.dialog.ok")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function EditorDialog({ siteId, path, onClose }: { siteId: string; path: string; onClose: () => void }) {
  const { t } = useTranslation();
  const [text, setText] = useState<string | null>(null);
  const [original, setOriginal] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const dirty = text !== null && text !== original;

  useEffect(() => {
    api
      .post<FilesResult>(`/sites/${siteId}/files`, { action: "read", path })
      .then((r) => {
        try {
          const s = new TextDecoder("utf-8", { fatal: true }).decode(fromBase64(r.data ?? ""));
          setText(s);
          setOriginal(s);
        } catch {
          setError(t("files.binary"));
        }
      })
      .catch((err) => setError(errorMessage(t, err)));
  }, [siteId, path, t]);

  const save = async () => {
    if (text === null) return;
    setSaving(true);
    setError("");
    try {
      await api.post(`/sites/${siteId}/files`, { action: "write", path, data: toBase64(new TextEncoder().encode(text)) });
      setOriginal(text);
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setSaving(false);
    }
  };
  const close = () => {
    if (dirty && !window.confirm(t("files.unsaved"))) return;
    onClose();
  };

  return (
    <div className="fixed inset-0 z-50 flex flex-col bg-bg" role="dialog" aria-modal aria-label={path}>
      <div className="flex flex-wrap items-center gap-3 border-b border-line px-4 py-3">
        <Pencil className="size-4 text-muted" aria-hidden />
        <span className="min-w-0 truncate font-mono text-sm" dir="ltr">
          {path}
        </span>
        {dirty && <span className="text-xs text-warn">● {t("files.modified_unsaved")}</span>}
        {saved && <span className="text-xs text-ok">{t("files.saved")}</span>}
        <div className="ms-auto flex gap-2">
          <Button size="sm" variant="primary" onClick={save} loading={saving} disabled={!dirty}>
            {t("files.save")}
            <kbd className="hidden font-mono text-[11px] opacity-70 sm:inline">Ctrl S</kbd>
          </Button>
          <Button size="sm" variant="ghost" onClick={close}>
            <X className="size-4" aria-hidden />
            {t("common.close")}
          </Button>
        </div>
      </div>
      <div className="min-h-0 flex-1 p-3">
        {error && <ErrorAlert>{error}</ErrorAlert>}
        {text === null && !error && <Spinner />}
        {text !== null && (
          <Suspense fallback={<Spinner />}>
            <CodeEditor name={path} value={text} onChange={setText} onSave={save} />
          </Suspense>
        )}
      </div>
    </div>
  );
}

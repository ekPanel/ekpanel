import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { X } from "lucide-react";
import { api, ApiError, errorMessage, type Server } from "../lib/api";
import type { SecurityView } from "../pages/SecurityPage";
import { Badge, Button, Card, ErrorAlert } from "./ui";

type Mode = SecurityView["countries"]["mode"];

/** Country blocking of a server: block some countries, or allow only some. */
export function CountriesCard({ server, sec }: { server: Server; sec: SecurityView }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [mode, setMode] = useState<Mode>(sec.countries.mode);
  const [chosen, setChosen] = useState<string[]>(sec.countries.countries);
  const [search, setSearch] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const st = sec.status?.countries;

  const names = useMemo(() => {
    let dn: Intl.DisplayNames | null = null;
    try {
      dn = new Intl.DisplayNames([i18n.language, "en"], { type: "region" });
    } catch {
      // very old browsers: codes only
    }
    return new Map(sec.country_codes.map((c) => [c, dn?.of(c) ?? c]));
  }, [sec.country_codes, i18n.language]);
  const name = (c: string) => names.get(c) ?? c;
  const q = search.trim().toLowerCase();
  const matches = q ? sec.country_codes.filter((c) => !chosen.includes(c) && (c.toLowerCase() === q || name(c).toLowerCase().includes(q))).slice(0, 8) : [];

  const changed = mode !== sec.countries.mode || (mode !== "off" && chosen.join() !== sec.countries.countries.join());
  const save = async () => {
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      const v = await api.put<SecurityView>(`/servers/${server.id}/security/countries`, { mode, countries: mode === "off" ? [] : chosen });
      qc.setQueryData(["security", server.id], v);
      setChosen(v.countries.countries);
      setSaved(true);
      qc.invalidateQueries({ queryKey: ["tasks"] });
    } catch (err) {
      if (err instanceof ApiError && err.code === "blocks_you") {
        setError(t("serversec.countries.blocks_you", { ip: String(err.data.ip ?? ""), country: name(String(err.data.country ?? "")) }));
      } else {
        setError(errorMessage(t, err));
      }
    } finally {
      setBusy(false);
    }
  };
  const add = (c: string) => {
    setChosen([...chosen, c].sort());
    setSearch("");
    setSaved(false);
  };

  return (
    <Card className="grid gap-4 p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <h2 className="text-lg font-semibold">{t("serversec.countries.title")}</h2>
        {st?.active ? <Badge tone="ok">{t("serversec.countries.active", { count: st.ranges ?? 0 })}</Badge> : <Badge>{t("serversec.crowdsec.off")}</Badge>}
      </div>
      <p className="text-sm text-muted">{t("serversec.countries.intro")}</p>
      <ErrorAlert>{error || (sec.countries.mode !== "off" ? st?.error : "")}</ErrorAlert>
      <fieldset className="grid gap-2 text-sm">
        <legend className="sr-only">{t("serversec.countries.title")}</legend>
        {(["off", "block", "allow"] as const).map((m) => (
          <label key={m} className="flex items-start gap-2">
            <input
              type="radio"
              name={`countries-${server.id}`}
              checked={mode === m}
              onChange={() => {
                setMode(m);
                setSaved(false);
              }}
              className="mt-1 accent-[var(--accent)]"
            />
            <span>
              <span className="font-medium">{t(`serversec.countries.m_${m}`)}</span>
              {m !== "off" && <span className="block text-muted">{t(`serversec.countries.m_${m}_hint`)}</span>}
            </span>
          </label>
        ))}
      </fieldset>
      {mode !== "off" && (
        <div className="grid gap-2">
          {chosen.length > 0 && (
            <ul className="flex flex-wrap gap-1.5">
              {chosen.map((c) => (
                <li key={c} className="flex items-center gap-1 rounded-full border border-line bg-sunk py-0.5 ps-2.5 pe-1 text-sm">
                  {name(c)}
                  <button
                    type="button"
                    onClick={() => {
                      setChosen(chosen.filter((x) => x !== c));
                      setSaved(false);
                    }}
                    className="rounded-full p-0.5 text-muted hover:bg-surface hover:text-ink"
                    aria-label={t("serversec.countries.remove", { name: name(c) })}
                  >
                    <X className="size-3.5" aria-hidden />
                  </button>
                </li>
              ))}
            </ul>
          )}
          <div className="relative">
            <input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && matches.length > 0) {
                  e.preventDefault();
                  add(matches[0]);
                }
              }}
              placeholder={t("serversec.countries.search")}
              aria-label={t("serversec.countries.search")}
              className="h-10 w-full rounded-lg border border-line bg-surface px-3 text-ink"
            />
            {matches.length > 0 && (
              <ul className="absolute inset-x-0 top-full z-10 mt-1 overflow-hidden rounded-lg border border-line bg-surface shadow-lg">
                {matches.map((c) => (
                  <li key={c}>
                    <button
                      type="button"
                      onClick={() => add(c)}
                      className="flex w-full items-center justify-between px-3 py-2 text-start text-sm hover:bg-sunk"
                    >
                      <span>{name(c)}</span>
                      <span className="text-xs text-muted">{c}</span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      )}
      <div className="flex flex-wrap items-center gap-3">
        <Button onClick={save} loading={busy} disabled={!changed || (mode !== "off" && chosen.length === 0)}>
          {t("common.save")}
        </Button>
        {saved && <span className="text-sm text-ok">{t("serversec.allowlist.saved")}</span>}
      </div>
      <p className="text-xs text-muted">
        {t("serversec.countries.notes")} {st?.list && t("serversec.countries.list", { month: st.list })}{" "}
        <a href="https://db-ip.com" target="_blank" rel="noreferrer" className="text-accent hover:underline">
          IP Geolocation by DB-IP
        </a>
      </p>
    </Card>
  );
}

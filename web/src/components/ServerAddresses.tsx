import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { api, errorMessage, type Server } from "../lib/api";
import { Button, Card, ErrorAlert, Field } from "./ui";

/** The addresses DNS records point at: found from the server, or set by hand. */
export function ServerAddressesCard({ server }: { server: Server }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [v4, setV4] = useState("");
  const [v6, setV6] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    setV4(server.public_ipv4 ?? "");
    setV6(server.public_ipv6 ?? "");
  }, [server.public_ipv4, server.public_ipv6]);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    setSaved(false);
    try {
      await api.put(`/servers/${server.id}/addresses`, { public_ipv4: v4, public_ipv6: v6 });
      qc.invalidateQueries({ queryKey: ["server", server.id] });
      setSaved(true);
    } catch (err) {
      setError(errorMessage(t, err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card className="grid gap-4 p-5">
      <div>
        <h2 className="font-semibold">{t("addresses.title")}</h2>
        <p className="text-sm text-muted">{t("addresses.intro")}</p>
      </div>
      <p className="text-sm">
        {t("addresses.found")}{" "}
        <span className="font-mono text-muted" dir="ltr">
          {(server.addresses ?? []).join(", ") || "—"}
        </span>
      </p>
      <form onSubmit={submit} className="grid gap-4">
        <ErrorAlert>{error}</ErrorAlert>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="IPv4" value={v4} onChange={(e) => setV4(e.target.value.trim())} placeholder={t("addresses.automatic")} dir="ltr" />
          <Field label="IPv6" value={v6} onChange={(e) => setV6(e.target.value.trim())} placeholder={t("addresses.automatic")} dir="ltr" />
        </div>
        <p className="text-sm text-muted">{t("addresses.hint")}</p>
        <div className="flex items-center gap-3">
          <Button type="submit" variant="primary" loading={busy}>
            {t("sites.save")}
          </Button>
          {saved && <span className="text-sm text-ok">{t("sites.saved")}</span>}
        </div>
      </form>
    </Card>
  );
}

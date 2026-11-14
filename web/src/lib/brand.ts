import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type Branding } from "./api";
import i18n from "../i18n";

declare global {
  interface Window {
    /** Defined by public/theme-init.js: sets the accent colors from "#rrggbb" ("" resets). */
    ekpanelBrandColor?: (hex: string) => void;
    /** Defined by public/theme-init.js: the accent tokens of both themes for "#rrggbb". */
    ekpanelBrandPalette?: (hex: string) => BrandPalette | null;
  }
}

export interface BrandTokens {
  accent: string;
  ink: string;
  soft: string;
}

export interface BrandPalette {
  light: BrandTokens;
  dark: BrandTokens;
}

export const brandingKey = ["branding"] as const;

const ekPanel: Branding = { name: "ekPanel", color: "", logo_url: null, custom: false, url: null, source_url: "https://github.com/ekPanel/ekpanel" };

function useBrandingQuery() {
  return useQuery({ queryKey: brandingKey, queryFn: () => api.get<Branding>("/branding"), staleTime: 5 * 60_000 });
}

/** How the panel looks: for the signed-in account, or for the domain it was opened at. */
export function useBranding() {
  return useBrandingQuery().data ?? ekPanel;
}

let applied = "";

/** Puts the brand on the page: colors, the tab's title and icon, and the name in texts.
 * Returns the brand, or null while it is loading (show no logo then, not ekPanel's). */
export function useApplyBranding() {
  const q = useBrandingQuery();
  const b = q.data ?? ekPanel;
  useEffect(() => {
    // Until the brand is known, the page keeps what the server put in it (its title, icon
    // and color for the domain), rather than flashing ekPanel's.
    if (!q.data) return;
    const key = [b.name, b.color, b.logo_url].join("|");
    if (key === applied) return;
    applied = key;
    window.ekpanelBrandColor?.(b.color);
    document.title = b.name;
    const icon = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
    if (icon) {
      icon.type = b.logo_url ? "image/png" : "image/svg+xml";
      icon.href = b.logo_url ?? "/favicon.svg";
    }
    const vars = (i18n.options.interpolation ??= {});
    if (vars.defaultVariables?.brand !== b.name) {
      vars.defaultVariables = { ...vars.defaultVariables, brand: b.name };
      // Texts already on the page are drawn again with the new name.
      i18n.changeLanguage(i18n.language);
    }
  }, [q.data, b.name, b.color, b.logo_url]);
  return q.data ?? (q.isError ? ekPanel : null);
}

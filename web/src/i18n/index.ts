import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import en from "./locales/en.json";
import bn from "./locales/bn.json";

// To add a language: create locales/<code>.json from en.json, add it here, and add the code
// to supportedLocales in internal/api/auth.go. `npm run i18n:check` reports missing keys.
export const languages = [
  { code: "en", name: "English" },
  { code: "bn", name: "বাংলা" },
] as const;

export type LanguageCode = (typeof languages)[number]["code"];

// Scripts written right to left. The layout mirrors automatically for these.
const rtlLanguages = new Set(["ar", "fa", "he", "ur", "ps", "sd", "yi", "dv", "ug", "ckb"]);

const STORAGE_KEY = "hostpanel.lang";

function isSupported(code: string): code is LanguageCode {
  return languages.some((l) => l.code === code);
}

function detectLanguage(): LanguageCode {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved && isSupported(saved)) return saved;
  } catch {
    // storage blocked: fall through to the browser's languages
  }
  for (const tag of navigator.languages ?? [navigator.language]) {
    const base = tag.toLowerCase().split("-")[0];
    if (isSupported(base)) return base;
  }
  return "en";
}

export function directionOf(code: string): "rtl" | "ltr" {
  return rtlLanguages.has(code.split("-")[0]) ? "rtl" : "ltr";
}

function applyToDocument(code: string) {
  document.documentElement.lang = code;
  document.documentElement.dir = directionOf(code);
}

i18n.use(initReactI18next).init({
  resources: { en: { translation: en }, bn: { translation: bn } },
  lng: detectLanguage(),
  fallbackLng: "en",
  interpolation: { escapeValue: false }, // React already escapes
});

applyToDocument(i18n.language);
i18n.on("languageChanged", (code) => {
  applyToDocument(code);
  try {
    localStorage.setItem(STORAGE_KEY, code);
  } catch {
    // not persisted; the choice still applies for this visit
  }
});

export default i18n;

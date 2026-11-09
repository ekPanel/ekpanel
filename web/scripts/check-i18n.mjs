// Reports keys that are missing from, or extra in, each translation compared to English.
// Run: npm run i18n:check   (exits non-zero if anything is missing, for CI)
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

// The web UI's strings, and the notifications the panel sends.
const dirs = ["../src/i18n/locales/", "../../internal/notify/locales/"].map((d) => fileURLToPath(new URL(d, import.meta.url)));
const flatten = (obj, prefix = "") => Object.entries(obj).flatMap(([k, v]) => (v && typeof v === "object" ? flatten(v, prefix + k + ".") : [prefix + k]));
// Plural forms differ by language (_one, _few, _many…), so compare their base key.
const base = (k) => k.replace(/_(zero|one|two|few|many|other)$/, "");
let failed = false;
for (const dir of dirs) {
  const load = (f) => new Set(flatten(JSON.parse(readFileSync(join(dir, f), "utf8"))).map(base));
  const source = load("en.json");
  for (const file of readdirSync(dir).filter((f) => f.endsWith(".json") && f !== "en.json")) {
    const keys = load(file);
    const missing = [...source].filter((k) => !keys.has(k));
    const extra = [...keys].filter((k) => !source.has(k));
    const pct = Math.round(((source.size - missing.length) / source.size) * 100);
    console.log(
      `${dir.includes("notify") ? "notifications " : "ui "}${file}: ${pct}% translated` +
        (missing.length ? `, missing ${missing.length}` : "") +
        (extra.length ? `, ${extra.length} unused` : ""),
    );
    for (const k of missing) console.log("  missing: " + k);
    for (const k of extra) console.log("  unused:  " + k);
    if (missing.length) failed = true;
  }
}
// Every key the UI asks for by a fixed name must exist in English. Keys built at run time
// (template strings) are not checked.
const uiDir = fileURLToPath(new URL("../src/", import.meta.url));
const english = new Set(flatten(JSON.parse(readFileSync(join(dirs[0], "en.json"), "utf8"))));
const sources = (d) =>
  readdirSync(d, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? sources(join(d, e.name)) : /\.tsx?$/.test(e.name) ? [join(d, e.name)] : []));
const unknown = new Set();
for (const file of sources(uiDir)) {
  for (const m of readFileSync(file, "utf8").matchAll(/\bt\(\s*"([a-z0-9_]+(?:\.[a-z0-9_]+)+)"/g)) {
    const k = m[1];
    if (!english.has(k) && !english.has(k + "_one") && !english.has(k + "_other")) unknown.add(`${k} (${file.slice(uiDir.length)})`);
  }
}
for (const k of unknown) console.log("  not in en.json: " + k);
if (unknown.size) failed = true;
process.exit(failed ? 1 : 0);

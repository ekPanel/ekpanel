# Translating ekPanel

ekPanel should feel native in every language, not only in English. Translations are plain
JSON files, so you do not need to know how to program.

## Add a new language

1. Copy `web/src/i18n/locales/en.json` to `web/src/i18n/locales/<code>.json`, where `<code>` is
   the [ISO 639-1 code](https://en.wikipedia.org/wiki/List_of_ISO_639_language_codes)
   (`es`, `ar`, `hi`, `id`, `tr`, …).
2. Translate the values. Keep the keys and anything inside `{{double braces}}` unchanged.
3. Register the language in `web/src/i18n/index.ts` (the `languages` list and `resources`),
   using the language's own name for itself, e.g. `{ code: "es", name: "Español" }`.
4. Add the code to `supportedLocales` in `internal/api/auth.go`, so the panel remembers it
   as a user's preference.
5. Run `cd web && npm run i18n:check` to confirm nothing is missing, then open a pull request.

## Things to know

- **Plurals.** Keys ending in `_one` and `_other` are plural forms. Your language may need
  different forms (`_zero`, `_two`, `_few`, `_many`); see the
  [CLDR plural rules](https://www.unicode.org/cldr/charts/latest/supplemental/language_plural_rules.html).
  Provide the forms your language uses. The checker accepts any set.
- **Numbers, dates and units** (GB, Mbps, "3 minutes ago") are formatted by the browser for
  your language. Do not put numbers or units into translations by hand.
- **Right-to-left languages** (Arabic, Persian, Urdu, Hebrew…) mirror the layout
  automatically. If something looks wrong in RTL, please open an issue with a screenshot.
- **`<code>` tags** in a value (for example in `setup.token_hint`) must stay around the same text.
- **Tone.** Write the way a helpful person speaks: short, clear sentences, addressing the user
  politely (formal "you" where your language has one). Prefer everyday words over jargon,
  but keep standard technical terms such as CPU, SSL, DNS and IP as your users know them.
- Error messages say what happened and what to do next.

A web-based translation platform (Weblate) will be set up before the first public release, so
translators can work in the browser without Git.

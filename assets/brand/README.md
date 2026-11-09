# ekPanel logo

A terminal-black tile with **ek** (এক, "one") and a green cursor, followed by **Panel**.
The letters are outlined (Audiowide, slanted 12°), so the files need no font.

| File | Use |
|---|---|
| `ekpanel-logo.svg` | Main logo on light backgrounds: README, website header, documentation |
| `ekpanel-logo-dark.svg` | On dark backgrounds: dark mode, slides, terminal themes |
| `ekpanel-logo-stacked.svg` | Icon above the name: splash screens, stickers, shirts |
| `ekpanel-icon.svg` | Icon alone: favicon, app icon, GitHub organisation picture |
| `ekpanel-logo-mono.svg` | One colour: print and embossing |
| `ekpanel-logo-reverse.svg` | White, on colours and photos |

The web UI draws the same shapes from `web/src/components/Brand.tsx`; the favicon is
`web/public/favicon.svg`.

## Colours

| Name | Hex | Where |
|---|---|---|
| Terminal black | `#0F1513` | the tile; "Panel" on light backgrounds |
| Cursor green | `#36D27E` | the cursor only, and things that are running |
| Screen white | `#EAF2EE` | "ek"; "Panel" on dark backgrounds |
| Border | `#28332F` | the thin line around the tile |

The UI's accent is the same green: `#13804A` on light themes, `#36D27E` on dark.

## Rules

- Keep free space around the logo of at least half the icon's height.
- Write the name **ekPanel**: small e, capital P. In the logo, "ek" is only inside the tile.
- Smallest sizes: the icon alone 16 px, the full logo 96 px wide.
- On photos or patterns, use the one-colour or reversed version.
- The cursor is always green; it is the only colour in the logo.
- Do not stretch or squash it, change the tile's or cursor's colour, or write "ekPanel" again
  next to the icon.

Fonts for headings around the logo: Audiowide for Latin, Anek Bangla for Bangla (both free on
Google Fonts). The logo files themselves need neither.

(function () {
  try {
    var t = localStorage.getItem("ekpanel.theme");
    var dark = t === "dark" || (t !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
    if (dark) document.documentElement.classList.add("dark");
    var l = localStorage.getItem("ekpanel.lang");
    if (l) document.documentElement.lang = l;
  } catch (e) {}

  // White-label color: the accent tokens for light and dark themes from one "#rrggbb",
  // darkened or lightened until text in it stays readable (contrast 4.5:1) on the page.
  // The app calls it again when the signed-in account has another brand; "" goes back to
  // ekPanel's green.
  function rgb(hex) {
    var n = parseInt(hex.slice(1), 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
  }
  function toHex(c) {
    return "#" + c.map(function (v) { return ("0" + Math.round(v).toString(16)).slice(-2); }).join("");
  }
  function mix(a, b, w) {
    return a.map(function (v, i) { return v + (b[i] - v) * w; });
  }
  function lum(c) {
    var s = c.map(function (v) { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); });
    return 0.2126 * s[0] + 0.7152 * s[1] + 0.0722 * s[2];
  }
  function contrast(a, b) {
    var x = lum(a), y = lum(b);
    return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
  }
  function readable(c, bg, toward) {
    for (var i = 0; i < 20 && contrast(c, bg) < 4.5; i++) c = mix(c, toward, 0.12);
    return c;
  }
  // The accent tokens for both themes, or null for a value that is not "#rrggbb".
  window.ekpanelBrandPalette = function (hex) {
    if (!/^#[0-9a-f]{6}$/i.test(hex || "")) return null;
    var c = rgb(hex), white = [255, 255, 255], black = [0, 0, 0];
    var lightBg = rgb("#ffffff"), darkBg = rgb("#131d1b"), darkInk = rgb("#0f1513");
    var light = readable(c, lightBg, black);
    var dark = readable(c, darkBg, white);
    return {
      light: { accent: toHex(light), ink: "#ffffff", soft: toHex(mix(c, white, 0.86)) },
      dark: { accent: toHex(dark), ink: contrast(dark, darkInk) >= 4.5 ? "#0f1513" : "#ffffff", soft: toHex(mix(c, darkBg, 0.8)) },
    };
  };
  window.ekpanelBrandColor = function (hex) {
    var el = document.getElementById("ekpanel-brand-color");
    var p = window.ekpanelBrandPalette(hex);
    if (!p) {
      if (el) el.remove();
      return;
    }
    // html:root and html.dark outrank the stylesheet's :root and .dark, wherever it loads.
    var css =
      "html:root{--accent:" + p.light.accent + ";--accent-ink:" + p.light.ink + ";--accent-soft:" + p.light.soft + "}" +
      "html.dark{--accent:" + p.dark.accent + ";--accent-ink:" + p.dark.ink + ";--accent-soft:" + p.dark.soft + "}";
    if (!el) {
      el = document.createElement("style");
      el.id = "ekpanel-brand-color";
      document.head.appendChild(el);
    }
    el.textContent = css;
  };
  var meta = document.querySelector('meta[name="ekpanel-brand-color"]');
  if (meta) window.ekpanelBrandColor(meta.getAttribute("content"));
})();

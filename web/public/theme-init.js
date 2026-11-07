(function () {
  try {
    var t = localStorage.getItem("ekpanel.theme");
    var dark = t === "dark" || (t !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
    if (dark) document.documentElement.classList.add("dark");
    var l = localStorage.getItem("ekpanel.lang");
    if (l) document.documentElement.lang = l;
  } catch (e) {}
})();

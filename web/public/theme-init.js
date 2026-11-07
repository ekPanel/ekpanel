(function () {
  try {
    var t = localStorage.getItem("hostpanel.theme");
    var dark = t === "dark" || (t !== "light" && window.matchMedia("(prefers-color-scheme: dark)").matches);
    if (dark) document.documentElement.classList.add("dark");
    var l = localStorage.getItem("hostpanel.lang");
    if (l) document.documentElement.lang = l;
  } catch (e) {}
})();

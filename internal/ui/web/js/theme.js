// Theme: follows the system until the user picks one. The pick is kept in localStorage.
// A classic script in the head, so the theme is set before the first paint.
(function () {
  var KEY = "pipesmith-theme";
  var root = document.documentElement;

  function stored() {
    try {
      var v = localStorage.getItem(KEY);
      return v === "light" || v === "dark" ? v : null;
    } catch (e) {
      return null;
    }
  }

  function system() {
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }

  function apply(theme) {
    root.setAttribute("data-theme", theme);
  }

  function pick(theme) {
    try {
      localStorage.setItem(KEY, theme);
    } catch (e) {
      /* storage may be refused; the theme still applies for this visit */
    }
    apply(theme);
  }

  apply(stored() || system());

  window.pipesmithTheme = {
    current: function () { return root.getAttribute("data-theme"); },
    stored: stored,
    pick: pick
  };
})();

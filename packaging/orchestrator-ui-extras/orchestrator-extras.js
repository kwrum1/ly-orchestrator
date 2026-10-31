/* Orchestrator UI extras: injected into the frozen baseline index.html.
 * Adds two menu entries without touching app.js. */
(function () {
  "use strict";

  var ITEMS = [
    {
      id: "extras-mirror",
      after: 'a[href="/topology"]',
      href: "/mirror.html",
      label: "镜像口",
      title: "物理口镜像到专用抓包口",
      icon:
        '<path d="M17 21v-2a1 1 0 0 1-1-1v-1a2 2 0 0 1 2-2h2a2 2 0 0 1 2 2v1a1 1 0 0 1-1 1"></path>' +
        '<path d="M19 15V6.5a1 1 0 0 0-7 0v11a1 1 0 0 1-7 0V9"></path>' +
        '<path d="M21 21v-2h-4"></path><path d="M3 5h4V3"></path>' +
        '<path d="M7 5a1 1 0 0 1 1 1v1a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V6a1 1 0 0 1 1-1V3"></path>'
    },
    {
      id: "extras-bypass",
      after: 'a[href="/groups"]',
      href: "/bypass.html",
      label: "故障旁路",
      title: "按组检测故障并仅旁路该组策略",
      icon:
        '<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"></path>' +
        '<path d="M12 8v4"></path><path d="M12 16h.01"></path>'
    }
  ];

  function makeLink(item) {
    var a = document.createElement("a");
    a.id = item.id;
    a.className = "nav-item";
    a.href = item.href;
    a.title = item.title;
    a.innerHTML =
      '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" ' +
      'fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" ' +
      'stroke-linejoin="round" class="lucide">' + item.icon + "</svg><span>" + item.label + "</span>";
    return a;
  }

  function applyFilter(input) {
    var query = (input && input.value ? input.value : "").trim().toLowerCase();
    ITEMS.forEach(function (item) {
      var link = document.getElementById(item.id);
      if (!link) return;
      var match = !query || item.label.toLowerCase().indexOf(query) !== -1;
      link.style.display = match ? "" : "none";
    });
  }

  function inject() {
    var nav = document.querySelector("nav.side-nav");
    if (!nav) return;
    ITEMS.forEach(function (item) {
      if (document.getElementById(item.id)) return;
      var anchor = nav.querySelector(item.after);
      var group = anchor ? anchor.parentElement : nav.querySelector("section.nav-group");
      if (!group) return;
      var link = makeLink(item);
      if (anchor && anchor.parentElement === group) anchor.insertAdjacentElement("afterend", link);
      else group.appendChild(link);
    });
    var filter = document.querySelector('.side-search input[placeholder="筛选菜单"]');
    if (filter && !filter.dataset.extrasBound) {
      filter.dataset.extrasBound = "1";
      filter.addEventListener("input", function () { applyFilter(filter); });
      applyFilter(filter);
    }
  }

  function start() {
    inject();
    var nav = document.querySelector("nav.side-nav");
    if (!nav || !window.MutationObserver) return;
    var observer = new MutationObserver(function () { inject(); });
    observer.observe(nav, { childList: true, subtree: false });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
  // The React bundle renders the shell after this file runs; retry briefly.
  var tries = 0;
  var timer = setInterval(function () {
    inject();
    if (++tries > 40 && document.querySelector("nav.side-nav")) clearInterval(timer);
  }, 250);
})();

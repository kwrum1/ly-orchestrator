/* Orchestrator UI extras: injected into the frozen baseline index.html.
 * Adds two menu entries and renders their pages inside the baseline content
 * area, keeping the sidebar/topbar (no full page reload, no new window). */
(function () {
  "use strict";

  var ITEMS = [
    {
      kind: "mirror",
      id: "extras-mirror",
      after: 'a[href="/topology"]',
      path: "/mirror",
      label: "镜像口",
      title: "物理口镜像到专用抓包口",
      icon:
        '<path d="M17 21v-2a1 1 0 0 1-1-1v-1a2 2 0 0 1 2-2h2a2 2 0 0 1 2 2v1a1 1 0 0 1-1 1"></path>' +
        '<path d="M19 15V6.5a1 1 0 0 0-7 0v11a1 1 0 0 1-7 0V9"></path>' +
        '<path d="M21 21v-2h-4"></path><path d="M3 5h4V3"></path>' +
        '<path d="M7 5a1 1 0 0 1 1 1v1a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V6a1 1 0 0 1 1-1V3"></path>'
    },
    {
      kind: "bypass",
      id: "extras-bypass",
      after: 'a[href="/groups"]',
      path: "/bypass",
      label: "故障旁路",
      title: "按组检测故障并仅旁路该组策略",
      icon:
        '<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"></path>' +
        '<path d="M12 8v4"></path><path d="M12 16h.01"></path>'
    }
  ];

  var VIEWS = {
    mirror: { html: "/mirror.html", script: "/mirror.js", factory: "LyExtrasMirrorView" },
    bypass: { html: "/bypass.html", script: "/bypass.js", factory: "LyExtrasBypassView" }
  };

  var active = null;      // { kind, container, hidden, cleanup }
  var loading = false;

  function itemByKind(kind) {
    return ITEMS.find(function (item) { return item.kind === kind; }) || null;
  }

  function kindFromPath(path) {
    if (path === "/mirror" || path === "/mirror.html") return "mirror";
    if (path === "/bypass" || path === "/bypass.html") return "bypass";
    return "";
  }

  function makeLink(item) {
    var a = document.createElement("a");
    a.id = item.id;
    a.className = "nav-item";
    a.href = item.path;
    a.title = item.title;
    a.setAttribute("data-extras-kind", item.kind);
    a.innerHTML =
      '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" ' +
      'fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" ' +
      'stroke-linejoin="round" class="lucide">' + item.icon + "</svg><span>" + item.label + "</span>";
    return a;
  }

  function setActive(kind) {
    document.querySelectorAll("nav.side-nav a.nav-item").forEach(function (link) {
      var matches = link.getAttribute("data-extras-kind") === kind;
      link.classList.toggle("is-active", matches);
      if (matches) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    });
  }

  function loadScript(src) {
    return new Promise(function (resolve, reject) {
      var existing = Array.prototype.slice.call(document.scripts).find(function (script) {
        return (script.getAttribute("src") || "").indexOf(src) === 0;
      });
      if (existing) { resolve(); return; }
      var script = document.createElement("script");
      script.src = src;
      script.onload = function () { resolve(); };
      script.onerror = function () { reject(new Error("加载失败：" + src)); };
      document.head.appendChild(script);
    });
  }

  function addActions(container, kind) {
    var head = container.querySelector(".page-head");
    if (!head) return;
    var actions = document.createElement("div");
    actions.className = "page-actions";
    var refresh = document.createElement("button");
    refresh.className = "btn btn-sm";
    refresh.id = "refresh";
    refresh.textContent = "刷新";
    var save = document.createElement("button");
    save.className = "btn btn-sm btn-primary";
    save.id = kind === "mirror" ? "save" : "save-config";
    save.textContent = kind === "mirror" ? "保存并应用" : "保存探测配置";
    actions.appendChild(refresh);
    actions.appendChild(save);
    head.appendChild(actions);
  }

  function closeView() {
    if (!active) return;
    var current = active;
    active = null;
    if (current.cleanup) {
      try { current.cleanup(); } catch (err) { /* ignore */ }
    }
    if (current.container && current.container.parentNode) current.container.parentNode.removeChild(current.container);
    (current.hidden || []).forEach(function (pair) {
      if (pair.node) pair.node.style.display = pair.display || "";
    });
    document.querySelectorAll("nav.side-nav a.nav-item[data-extras-kind]").forEach(function (link) {
      link.classList.remove("is-active");
      link.removeAttribute("aria-current");
    });
  }

  function openView(kind, updateHistory) {
    var item = itemByKind(kind);
    if (!item) return;
    if (active && active.kind === kind) return;
    if (loading) return;
    var mainInner = document.querySelector(".main-inner");
    if (!mainInner) return;

    loading = true;
    closeView();
    var hidden = [];
    Array.prototype.slice.call(mainInner.children).forEach(function (child) {
      hidden.push({ node: child, display: child.style.display });
      child.style.display = "none";
    });

    var container = document.createElement("div");
    container.id = "extras-view";
    container.className = "page extras-embedded";
    mainInner.appendChild(container);
    active = { kind: kind, container: container, hidden: hidden, cleanup: null };
    setActive(kind);
    if (updateHistory) {
      var path = item.path;
      if (window.location.pathname !== path) window.history.pushState({ extrasView: kind }, "", path);
    }

    fetch(VIEWS[kind].html, { credentials: "same-origin" })
      .then(function (response) { return response.text(); })
      .then(function (html) {
        var parsed = new DOMParser().parseFromString(html, "text/html");
        var wrap = parsed.querySelector(".extras-wrap");
        if (!wrap) throw new Error("页面模板缺失");
        while (wrap.firstChild) container.appendChild(wrap.firstChild);
        addActions(container, kind);
        return loadScript(VIEWS[kind].script);
      })
      .then(function () {
        if (!active || active.kind !== kind) return;
        var factory = window[VIEWS[kind].factory];
        if (typeof factory !== "function") throw new Error("页面脚本未加载");
        var cleanup = factory(container);
        if (active && active.kind === kind) active.cleanup = typeof cleanup === "function" ? cleanup : null;
      })
      .catch(function (error) {
        if (!active || active.kind !== kind) return;
        var box = container.querySelector("#error");
        if (box) {
          box.hidden = false;
          box.textContent = String(error && error.message ? error.message : error);
        }
      })
      .then(function () { loading = false; });
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
    if (!document.getElementById("extras-view-style")) {
      var style = document.createElement("style");
      style.id = "extras-view-style";
      style.textContent =
        "[hidden]{display:none!important}" +
        "#extras-view{display:flex;flex-direction:column;gap:16px}" +
        "#extras-view .extras-wrap{max-width:none;margin:0;padding:0}" +
        "#extras-view .page-head{margin:0}";
      document.head.appendChild(style);
    }
    inject();
    var nav = document.querySelector("nav.side-nav");
    if (nav && window.MutationObserver) {
      var observer = new MutationObserver(function () { inject(); });
      observer.observe(nav, { childList: true, subtree: false });
    }
    var wanted = kindFromPath(window.location.pathname);
    if (wanted) openView(wanted, false);
  }

  // Keep the baseline shell: only the content area is swapped. Clicking any
  // baseline menu removes the extra view and lets the React router navigate.
  document.addEventListener("click", function (event) {
    var link = event.target.closest ? event.target.closest("a.nav-item") : null;
    if (!link) return;
    var kind = link.getAttribute("data-extras-kind");
    if (kind) {
      event.preventDefault();
      openView(kind, true);
      return;
    }
    closeView();
  }, true);

  window.addEventListener("popstate", function () {
    var kind = kindFromPath(window.location.pathname);
    if (kind) openView(kind, false);
    else closeView();
  });

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
  var tries = 0;
  var timer = setInterval(function () {
    inject();
    if (!active) {
      var wanted = kindFromPath(window.location.pathname);
      if (wanted) openView(wanted, false);
    }
    if (++tries > 40 && document.querySelector("nav.side-nav")) clearInterval(timer);
  }, 250);
})();

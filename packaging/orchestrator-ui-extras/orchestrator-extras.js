/* Orchestrator UI extras: injected into the frozen baseline index.html.
 * Keeps the baseline shell and content area. Adds the 流量镜像 page and two
 * in-place enhancements:
 *   - 网卡设置 → 角色配置 gains 镜像口 / 未配置
 *   - 新增/编辑编排组 gains 接口故障旁路 / 业务故障旁路
 */
(function () {
  "use strict";

  /* --- gateway-parity interaction rules ---------------------------------
   * 1. The shell's toolbar dispatches `lyroute:refresh`, but the frozen
   *    baseline never listens. Reload the active route so the operator sees
   *    the committed generation instead of a toast with stale data.
   * 2. The shell has an in-flight guard for the main ApplyProvider; the
   *    policy page and these extras also issue async policy PUTs. Serialize
   *    those mutations so a second save cannot race the first VPP
   *    transaction (the gateway's useApply queue behaviour).
   * 3. Policy/topology applies are committed through `set ly-route
   *    orchestrator candidate/commit` inside VPP: no vpp.service restart and
   *    no control-plane restart. The UI must show "配置应用中", never a
   *    generic "system restarting" state for a policy delete.
   */
  window.addEventListener("lyroute:refresh", function () {
    window.setTimeout(function () { window.location.reload(); }, 80);
  });

  var applyQueue = Promise.resolve();
  var nativeFetch = window.fetch;
  if (nativeFetch) {
    window.fetch = function (input, init) {
      var url = typeof input === "string" ? input : (input && input.url) || "";
      var method = ((init && init.method) || (input && input.method) || "GET").toUpperCase();
      var isApply = /\/api\/v1\/(runtime\/apply|orchestrator\/policy|orchestrator\/service-chains\/apply)(\?|$)/.test(url) &&
                    (method === "POST" || method === "PUT" || method === "PATCH" || method === "DELETE");
      if (!isApply) return nativeFetch.call(window, input, init);
      var run = function () { return nativeFetch.call(window, input, init); };
      var next = applyQueue.then(run, run);
      applyQueue = next.catch(function () {});
      return next;
    };
  }

  // First-paint styles. The console re-renders this page from the frozen shell
  // and only then compacts it; hiding the wide blocks here (before React mounts)
  // makes the compact layout the *only* layout the operator ever sees.
  (function injectBaseStyles() {
    if (document.getElementById("extras-base-style")) return;
    var style = document.createElement("style");
    style.id = "extras-base-style";
    style.textContent = [
      "/* 网卡设置：直接按紧凑口径绘制，避免先铺开再收起的抖动 */",
      ".card .port-grid{display:none !important}",
      ".card .takeover-panel p{display:none !important}",
      ".card .card-actions .badge{display:none !important}",
      ".nic-summary{color:var(--text-tertiary);font-size:12px;line-height:1.6}",
      "/* 固定列宽会在宽卡片里留下大片空白，统一交给表格自适应 */",
      ".extras-stretch-table{table-layout:auto !important;width:100% !important}",
      ".extras-stretch-table th[style*='width']{width:auto !important}",
      ".mirror-summary{display:flex;flex-wrap:wrap;gap:6px 18px;margin:0 0 10px;color:var(--text-tertiary);font-size:11px}",
      ".mirror-summary b{color:var(--text-secondary);font-weight:600}",
      "/* The shell prints its own wording first (设备正常 / 需要处理); keep it",
      "   hidden until normalised so the operator never sees two sentences. */",
      ".health-pill>span:not(.health-reason),.sidebar-foot>span:last-child{visibility:hidden}",
      "html.extras-status-ready .health-pill>span:not(.health-reason),html.extras-status-ready .sidebar-foot>span:last-child{visibility:visible}",
      "/* A page that ends after one short card left a bare block under it. The",
      "   shell's table board fills the viewport on 编排组; give the same shape to",
      "   every single-card page, and let an empty state centre inside the card. */",
      "@media (min-width:900px){",
      "  .main-inner .page:not(:has(.card ~ .card))>.card:last-child{min-height:calc(100vh - 196px);display:flex;flex-direction:column}",
      "  .main-inner .page:not(:has(.card ~ .card))>.card:last-child>.card-body{flex:1 1 auto;display:flex;flex-direction:column}",
      "  .main-inner .page:not(:has(.card ~ .card))>.card:last-child>.card-body>.table-wrap{flex:1 1 auto}",
      "  .main-inner .page:not(:has(.card ~ .card))>.card:last-child>.card-body>.empty-state{flex:1 1 auto;justify-content:center}",
      "/* One empty-state shape everywhere: same box, same centring, whichever",
      "   component rendered it. */",
      ".card-body .empty-state{flex:1 1 auto;display:flex;flex-direction:column;align-items:center;justify-content:center;min-height:240px}",
      "  .extras-embedded #no-target{margin:auto 0}",
      "}"
    ].join("\n");
    document.head.appendChild(style);
  })();

  // The console is served with a week-long asset cache. Carry this script's own
  // content hash into every page it loads, otherwise a shipped fix stays hidden
  // behind the browser's cached copy.
  var assetVersion = (function () {
    var source = (document.currentScript && document.currentScript.src) || "";
    var match = /[?&]v=([^&]+)/.exec(source);
    return match ? match[1] : "";
  })();

  function assetUrl(path) {
    if (!assetVersion || !path || path.indexOf("?") !== -1) return path;
    return path + "?v=" + assetVersion;
  }

  var ITEMS = [
    {
      kind: "mirror",
      id: "extras-mirror",
      after: 'a[href="/topology"]',
      path: "/mirror",
      label: "流量镜像",
      title: "把物理口流量镜像到镜像口",
      icon:
        '<path d="M17 21v-2a1 1 0 0 1-1-1v-1a2 2 0 0 1 2-2h2a2 2 0 0 1 2 2v1a1 1 0 0 1-1 1"></path>' +
        '<path d="M19 15V6.5a1 1 0 0 0-7 0v11a1 1 0 0 1-7 0V9"></path>' +
        '<path d="M21 21v-2h-4"></path><path d="M3 5h4V3"></path>' +
        '<path d="M7 5a1 1 0 0 1 1 1v1a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V6a1 1 0 0 1 1-1V3"></path>'
    }
  ];

  var VIEWS = {
    mirror: { html: "/mirror.html", script: "/mirror.js", factory: "LyExtrasMirrorView" }
  };

  var active = null;      // { kind, container, hidden, cleanup }
  var loading = false;
  var roleModal = null;   // { port, role, previous, sources }
  var groupModal = null;  // { name, editing }
  var pendingGroupBypass = null; // { name, state, at } set when a group save is clicked
  var skipRoleInject = false;    // guards internal topology writes from the role interceptor

  function api(path, options) {
    var opts = options || {};
    var headers = Object.assign({ Accept: "application/json" }, opts.headers || {});
    if (opts.body !== undefined && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
    return fetch(path, {
      method: opts.method || "GET",
      credentials: "same-origin",
      headers: headers,
      body: opts.body === undefined ? undefined : (typeof opts.body === "string" ? opts.body : JSON.stringify(opts.body))
    }).then(function (response) {
      return response.text().then(function (text) {
        var body = null;
        try { body = text ? JSON.parse(text) : null; } catch (err) { body = text; }
        if (!response.ok) {
          var detail = body && body.error ? (body.error.message || body.error.code) : null;
          var error = new Error(detail || body || ("HTTP " + response.status));
          error.status = response.status;
          throw error;
        }
        return body;
      });
    });
  }

  function itemByKind(kind) {
    return ITEMS.find(function (item) { return item.kind === kind; }) || null;
  }

  function kindFromPath(path) {
    if (path === "/mirror" || path === "/mirror.html") return "mirror";
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
    // The mirror page inserts its own single primary action and refreshes
    // itself after every write, so it gets no generic buttons here.
    if (kind !== "mirror") {
      var refresh = document.createElement("button");
      refresh.className = "btn btn-sm";
      refresh.id = "refresh";
      refresh.textContent = "刷新";
      actions.appendChild(refresh);
      var save = document.createElement("button");
      save.className = "btn btn-sm btn-primary";
      save.id = "save";
      save.textContent = "保存并应用";
      actions.appendChild(save);
    }
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
    if (!mainInner) {
      // The extras script can run before React mounts the shell.
      if (!openView.retries) openView.retries = 0;
      if (openView.retries < 15) {
        openView.retries += 1;
        window.setTimeout(function () { openView(kind, false); }, 300);
      }
      return;
    }
    openView.retries = 0;

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
    if (kind === "mirror") {
      // The template is injected before its data-driven page script runs, so a
      // visible container would show the card head and an empty table first.
      // Hold it until the page script has written the first render.
      container.style.visibility = "hidden";
      window.setTimeout(function () {
        if (container.style.visibility === "hidden") container.style.visibility = "";
      }, 2500);
    }
    mainInner.appendChild(container);
    active = { kind: kind, container: container, hidden: hidden, cleanup: null };
    setActive(kind);
    if (updateHistory) {
      var path = item.path;
      if (window.location.pathname !== path) window.history.pushState({ extrasView: kind }, "", path);
    }

    fetch(assetUrl(VIEWS[kind].html), { credentials: "same-origin" })
      .then(function (response) { return response.text(); })
      .then(function (html) {
        var parsed = new DOMParser().parseFromString(html, "text/html");
        var wrap = parsed.querySelector(".extras-wrap");
        if (!wrap) throw new Error("页面模板缺失");
        // The page template keeps its layout in <style>; when embedded only
        // .extras-wrap is copied, so carry the styles into the view as well.
        parsed.querySelectorAll("style").forEach(function (node) {
          container.appendChild(node.cloneNode(true));
        });
        while (wrap.firstChild) container.appendChild(wrap.firstChild);
        addActions(container, kind);
        return loadScript(assetUrl(VIEWS[kind].script));
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

  // ---------- shared port helpers ----------

  function portName(item) {
    return item.name || item.id;
  }

  function portLabel(item) {
    // eth-only display rule: the console speaks product port names, never the
    // kernel/system name (ens*/enp*/eno*). Backend ids stay untouched.
    var id = String(item.id || "");
    if (/^eth\d+$/i.test(id)) return id;
    var name = String(item.name || id);
    if (/^eth\d+$/i.test(name)) return name;
    return name;
  }

  function isManagement(item) {
    return item.management === true || item.gateway_role === "management" || item.takeover_state === "management";
  }

  function currentPorts() {
    return api("/api/v1/interfaces").then(function (data) { return (data && data.items) || []; });
  }

  function currentTopology() {
    return api("/api/v1/orchestrator/topology").then(function (data) { return (data && data.item) || {}; });
  }

  // Marking a port as the mirror destination is a two-part change: the console
  // role (topology port_roles) and the interface resource that the dataplane
  // compiles into a SPAN session.
  function setPortMirror(port, enabled) {
    return currentTopology().then(function (topo) {
      var roles = Object.assign({}, topo.port_roles || {});
      if (enabled) roles[port] = "mirror";
      else if (roles[port] === "mirror") delete roles[port];
      topo.port_roles = roles;
      delete topo.checksum;
      return api("/api/v1/orchestrator/topology", { method: "PUT", body: topo });
    }).then(function () {
      return currentPorts();
    }).then(function (ports) {
      var item = ports.find(function (p) { return p.id === port || p.name === port; });
      if (!item) return null;
      if (enabled) {
        return api("/api/v1/interfaces/" + encodeURIComponent(item.id), {
          method: "PATCH",
          body: { id: item.id, name: portName(item), mirror: true }
        });
      }
      return api("/api/v1/interfaces/" + encodeURIComponent(item.id), {
        method: "PATCH",
        body: { id: item.id, name: portName(item), mirror: false, mirror_sources: [] }
      });
    }).then(function () {
      return api("/api/v1/runtime/apply", { method: "POST", body: {} }).catch(function () { return null; });
    });
  }

  window.LyOrchExtras = window.LyOrchExtras || {};
  window.LyOrchExtras.setPortMirror = setPortMirror;
  window.LyOrchExtras.currentPorts = currentPorts;
  window.LyOrchExtras.portLabel = portLabel;
  window.LyOrchExtras.isManagement = isManagement;

  // ---------- first-run topology ----------
  //
  // A factory appliance has no topology document, so GET answers 404 and the
  // frozen console refuses every role save with "拓扑未加载". The API also
  // rejects a document that lacks a boundary, which makes the very first
  // 入口/出口 assignment impossible in one step. The console therefore works
  // against a local draft: the half-configured document is kept here and is
  // written for real as soon as both boundaries exist.

  var draftStorageKey = "lyroute.extras.topologyDraft";
  var topologyDraft = null;

  function loadDraft() {
    if (topologyDraft) return topologyDraft;
    try {
      var raw = window.localStorage.getItem(draftStorageKey);
      if (raw) topologyDraft = JSON.parse(raw);
    } catch (err) { /* ignore a broken draft */ }
    return topologyDraft;
  }

  function rememberDraft(doc) {
    topologyDraft = doc || null;
    try {
      if (doc) window.localStorage.setItem(draftStorageKey, JSON.stringify(doc));
      else window.localStorage.removeItem(draftStorageKey);
    } catch (err) { /* storage is optional */ }
  }

  function emptyTopologyDraft() {
    return currentPorts().then(function (ports) {
      var mgmt = (ports || []).find(isManagement);
      return {
        schema_version: 1,
        management_interface: (mgmt && (mgmt.id || mgmt.name)) || "eth0",
        interfaces: [],
        orchestration_groups: [],
        port_roles: {}
      };
    }).catch(function () {
      return { schema_version: 1, management_interface: "eth0", interfaces: [], orchestration_groups: [], port_roles: {} };
    });
  }

  function boundaryRoles(doc) {
    var roles = {};
    ((doc && doc.interfaces) || []).forEach(function (item) {
      if (item && (item.role === "lan" || item.role === "wan")) roles[item.role] = item;
    });
    return roles;
  }

  function mergeTopologyDraft(base, incoming) {
    var out = JSON.parse(JSON.stringify(base || {}));
    out.schema_version = (incoming && incoming.schema_version) || out.schema_version || 1;
    out.management_interface = (incoming && incoming.management_interface) || out.management_interface || "eth0";
    out.interfaces = (incoming && incoming.interfaces && incoming.interfaces.length)
      ? incoming.interfaces
      : (out.interfaces || []);
    out.orchestration_groups = (incoming && incoming.orchestration_groups && incoming.orchestration_groups.length)
      ? incoming.orchestration_groups
      : (out.orchestration_groups || []);
    out.port_roles = Object.assign({}, out.port_roles || {}, (incoming && incoming.port_roles) || {});
    delete out.checksum;
    return out;
  }

  // ---------- 角色配置: LAN / WAN / 镜像口 / 未配置 ----------

  function enhanceRoleModal(modal) {
    var heading = modal.querySelector(".modal-head h2");
    if (!heading || heading.textContent.trim() !== "角色配置") return;
    if (modal.dataset.roleEnhanced === "1") return;
    var subtitle = (modal.querySelector(".modal-head p") || {}).textContent || "";
    var port = subtitle.split("·")[0].trim();
    var select = modal.querySelector("select");
    if (!port || !select) return;
    modal.dataset.roleEnhanced = "1";
    // The baseline renders WAN/LAN. Append the two extra choices instead of
    // rebuilding the select: replacing React-owned <option> nodes makes the
    // baseline crash when it unmounts the modal.
    [["mirror", "镜像口"], ["", "未配置"]].forEach(function (pair) {
      if (select.querySelector('option[value="' + pair[0] + '"]')) return;
      var option = document.createElement("option");
      option.value = pair[0];
      option.textContent = pair[1];
      option.setAttribute("data-extras-option", "1");
      select.appendChild(option);
    });
    ["lan", "wan", "mirror", ""].forEach(function (value) {
      var option = select.querySelector('option[value="' + value + '"]');
      if (option) select.appendChild(option);
    });
    roleModal = { port: port, role: select.value, previous: "", sources: [] };
    var touched = false;
    Promise.all([currentTopology(), currentPorts()]).then(function (results) {
      if (modal.dataset.roleEnhanced !== "1" || !document.body.contains(modal)) return;
      var topo = results[0] || {};
      var ports = results[1] || [];
      var item = ports.find(function (p) { return p.id === port || p.name === port || p.system_name === port; });
      var previous = (topo.port_roles || {})[port] || "";
      var role = previous;
      if (!role && item && item.mirror === true) role = "mirror";
      // A fast operator can change the select before this prefill lands; the
      // device read must never overwrite that choice.
      if (!touched) select.value = role;
      roleModal = {
        port: port,
        role: touched ? select.value : role,
        previous: previous,
        sources: (item && item.mirror_sources) || []
      };
    }).catch(function () { /* keep the baseline selection */ });
    select.addEventListener("change", function () {
      touched = true;
      if (roleModal) roleModal.role = select.value;
    });
  }

  // ---------- 编排组: 接口 / 业务故障旁路 ----------

  function bypassBlock() {
    var wrap = document.createElement("div");
    wrap.className = "field span-2";
    wrap.dataset.bypassBlock = "1";
    wrap.innerHTML =
      '<span class="field-label">故障旁路</span>' +
      '<div class="bypass-options">' +
      '  <label class="check-card"><input type="checkbox" data-bypass-iface>' +
      "    <span><strong>接口故障旁路</strong><small>网口 down 时自动旁路该组，其它组不受影响</small></span></label>" +
      '  <label class="check-card"><input type="checkbox" data-bypass-biz>' +
      "    <span><strong>业务故障旁路</strong><small>被动探测请求无响应时自动旁路该组</small></span></label>" +
      "</div>" +
      '<div class="bypass-fields" data-bypass-fields hidden>' +
      '  <label class="field"><span class="field-label">目的地址</span><input class="input" data-bypass-ip placeholder="例如 192.168.10.82"></label>' +
      '  <label class="field"><span class="field-label">目的端口</span><input class="input" data-bypass-port type="number" min="1" max="65535" placeholder="例如 80"></label>' +
      '  <label class="field"><span class="field-label">协议</span><select class="select" data-bypass-proto><option value="tcp">TCP</option><option value="udp">UDP</option></select></label>' +
      '  <label class="field"><span class="field-label">故障阈值（秒）</span><input class="input" data-bypass-threshold type="number" min="1" max="300" value="10"><span class="field-hint">窗口内所有请求都没有收到答复才旁路</span></label>' +
      "</div>" +
      '<span class="field-hint">接口旁路看网口 down；业务旁路在设定秒数内所有请求都没有答复（拒绝、重置、丢包都不算答复）才旁路。两类可独立开启，业务旁路需填写目的地址与端口。</span>';
    return wrap;
  }

  function enhanceGroupModal(modal) {
    var heading = modal.querySelector(".modal-head h2");
    if (!heading) return;
    var title = heading.textContent.trim();
    if (!/编排组/.test(title)) return;
    if (modal.dataset.bypassEnhanced === "1") return;
    var body = modal.querySelector(".modal-body");
    if (!body) return;
    modal.dataset.bypassEnhanced = "1";
    var block = bypassBlock();
    body.appendChild(block);

    var nameInput = body.querySelector('input.input');
    var editing = /编辑/.test(title);
    var name = editing && nameInput ? nameInput.value.trim() : "";
    groupModal = { name: name, editing: editing, modal: modal };

    var iface = block.querySelector("[data-bypass-iface]");
    var biz = block.querySelector("[data-bypass-biz]");
    var fields = block.querySelector("[data-bypass-fields]");
    var ip = block.querySelector("[data-bypass-ip]");
    var port = block.querySelector("[data-bypass-port]");
    var proto = block.querySelector("[data-bypass-proto]");
    var threshold = block.querySelector("[data-bypass-threshold]");

    biz.addEventListener("change", function () { fields.hidden = !biz.checked; });

    if (editing && name) {
      api("/api/v1/orchestrator/bypass").then(function (data) {
        if (!document.body.contains(modal)) return;
        var config = (data && data.config) || {};
        var entry = (config.groups || []).find(function (item) { return item.name === name; });
        if (!entry) return;
        // interface_bypass is optional in older configurations; omitted means on.
        iface.checked = entry.interface_bypass !== false;
        var target = (entry.flow_targets || [])[0];
        if (target) {
          biz.checked = true;
          fields.hidden = false;
          ip.value = target.dst_ip || "";
          port.value = target.dst_port || "";
          proto.value = (target.protocol || "tcp").toLowerCase();
        }
        if (entry.window_seconds) threshold.value = String(entry.window_seconds);
      }).catch(function () { /* the group still saves without prefill */ });
    }

    function readState() {
      return {
        iface: iface.checked,
        biz: biz.checked,
        ip: String(ip.value || "").trim(),
        port: Number(port.value),
        proto: proto.value,
        threshold: Number(threshold.value) || 10
      };
    }

    // Block the save when business bypass is enabled without a target, and
    // remember the switches so the group-save response can persist them.
    var footer = modal.querySelector(".modal-footer");
    var primary = footer ? footer.querySelector(".btn-primary") : null;
    if (primary) {
      primary.addEventListener("click", function (event) {
        if (biz.checked) {
          var missing = [];
          if (!String(ip.value || "").trim()) missing.push("目的地址");
          var p = Number(port.value);
          if (!Number.isInteger(p) || p < 1 || p > 65535) missing.push("目的端口");
          if (missing.length) {
            event.preventDefault();
            event.stopImmediatePropagation();
            var warning = block.querySelector(".bypass-error");
            if (!warning) {
              warning = document.createElement("div");
              warning.className = "inline-warning bypass-error";
              block.appendChild(warning);
            }
            warning.textContent = "业务故障旁路缺少" + missing.join("、") + "；补全或关闭业务旁路后再保存。";
            return;
          }
        }
        var groupName = String((nameInput && nameInput.value) || "").trim();
        if (groupName) {
          pendingGroupBypass = { name: groupName, state: readState(), at: Date.now() };
        }
      }, true);
    }

    groupModal.read = function () {
      return readState();
    };
  }

  function saveBypassForGroup(name, state, enabled) {
    if (!name) return Promise.resolve(null);
    return api("/api/v1/orchestrator/bypass").then(function (data) {
      var config = Object.assign({}, (data && data.config) || {});
      var existing = (config.groups || []).find(function (item) { return item.name === name; });
      var groups = (config.groups || []).filter(function (item) { return item.name !== name; });
      if (enabled && state && (state.iface || state.biz)) {
        var entry = {
          name: name,
          interface_bypass: Boolean(state.iface),
          window_seconds: state.threshold || 10,
          failure_threshold: (existing && existing.failure_threshold) || 3,
          recovery_threshold: (existing && existing.recovery_threshold) || 3
        };
        if (state.biz) {
          entry.flow_targets = [{
            dst_ip: state.ip,
            dst_port: state.port,
            protocol: state.proto === "udp" ? "udp" : "tcp"
          }];
        }
        groups.push(entry);
      }
      config.groups = groups;
      return api("/api/v1/orchestrator/bypass/config", { method: "PUT", body: config });
    });
  }

  function removeBypassGroup(name) {
    if (!name) return Promise.resolve(null);
    return api("/api/v1/orchestrator/bypass").then(function (data) {
      var config = Object.assign({}, (data && data.config) || {});
      var groups = (config.groups || []).filter(function (item) { return item.name !== name; });
      if (groups.length === (config.groups || []).length) return null;
      config.groups = groups;
      return api("/api/v1/orchestrator/bypass/config", { method: "PUT", body: config });
    });
  }

  // ---------- toast + mirror role column ----------

  function showToast(message, tone) {
    var node = document.getElementById("extras-toast");
    if (!node) {
      node = document.createElement("div");
      node.id = "extras-toast";
      node.className = "extras-toast";
      document.body.appendChild(node);
    }
    node.textContent = String(message);
    node.setAttribute("data-tone", tone || "info");
    node.hidden = false;
    window.clearTimeout(showToast.timer);
    showToast.timer = window.setTimeout(function () {
      node.hidden = true;
    }, 9000);
  }

  var mirrorIds = {};
  var mirrorIdsAt = 0;
  var mirrorIdsPromise = null;

  function refreshMirrorIds(force) {
    if (!force && Date.now() - mirrorIdsAt < 5000) return Promise.resolve(mirrorIds);
    if (mirrorIdsPromise) return mirrorIdsPromise;
    mirrorIdsPromise = currentPorts().then(function (ports) {
      var ids = {};
      ports.forEach(function (item) {
        if (item.mirror !== true) return;
        [item.id, item.name, item.system_name].forEach(function (value) {
          if (value) ids[String(value)] = true;
        });
      });
      mirrorIds = ids;
      mirrorIdsAt = Date.now();
      mirrorIdsPromise = null;
      return ids;
    }).catch(function () {
      mirrorIdsPromise = null;
      return mirrorIds;
    });
    return mirrorIdsPromise;
  }

  // The baseline maps lan/wan to labels and renders everything else as
  // 未配置, so a mirror port would read as unconfigured. Rewrite only that
  // cell; rows showing 入口/出口, 聚合组 or 编排组 are left alone.
  function relabelMirrorRoles() {
    var ids = Object.keys(mirrorIds);
    if (!ids.length) return;
    document.querySelectorAll("table").forEach(function (table) {
      var headers = Array.prototype.slice.call(table.querySelectorAll("thead th"));
      var index = -1;
      headers.forEach(function (th) {
        if (index === -1 && th.textContent.trim() === "方向") index = th.cellIndex;
      });
      if (index < 0) return;
      table.querySelectorAll("tbody tr").forEach(function (row) {
        var first = row.cells[0];
        if (!first || !row.cells[index]) return;
        var text = first.innerText.trim();
        var isMirror = ids.some(function (id) {
          return text === id || text.indexOf(id + " ") === 0 || text.indexOf("· " + id) !== -1;
        });
        if (!isMirror) return;
        var cell = row.cells[index];
        var current = cell.innerText.trim();
        if (current === "镜像口") return;
        if (current !== "未配置" && current !== "mirror") return;
        var badge = cell.querySelector(".badge") || cell.firstElementChild;
        if (badge) {
          badge.textContent = "镜像口";
          if (badge.className && badge.className.indexOf("badge") !== -1) badge.className = "badge badge-info";
        } else {
          cell.textContent = "镜像口";
        }
      });
    });
  }

  function scheduleRelabel() {
    refreshMirrorIds(false).then(function () {
      relabelMirrorRoles();
      decorateInterfacesPage();
    });
    refreshPortNameMap().then(rewriteSystemNames);
  }

  // eth-only display rule: the frozen baseline renders the kernel name in a few
  // places (orchestration-group port cells, hints). Rewrite the text node only;
  // React state and API payloads keep the kernel names.
  var portNameMap = { at: 0, map: {} };

  function refreshPortNameMap() {
    if (Date.now() - portNameMap.at < 10000) return Promise.resolve(portNameMap.map);
    return currentPorts().then(function (ports) {
      var map = {};
      (ports || []).forEach(function (item) {
        var id = String(item.id || "");
        var system = String(item.system_name || "");
        if (/^eth\d+$/i.test(id) && system && system !== id) map[system] = id;
      });
      portNameMap = { at: Date.now(), map: map };
      return map;
    }).catch(function () { return portNameMap.map; });
  }

  function rewriteSystemNames() {
    var keys = Object.keys(portNameMap.map);
    if (!keys.length) return;
    var root = document.querySelector(".main-inner");
    if (!root) return;
    var pattern = new RegExp("\\b(" + keys.map(function (key) {
      return key.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    }).join("|") + ")\\b", "g");
    var walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, null);
    var node = walker.nextNode();
    while (node) {
      var parent = node.parentElement;
      var tag = parent ? parent.tagName : "";
      if (tag !== "SCRIPT" && tag !== "STYLE" && tag !== "TEXTAREA" && /ens|enp|eno/.test(node.nodeValue)) {
        var replaced = node.nodeValue.replace(pattern, function (match) {
          return portNameMap.map[match] || match;
        });
        if (replaced !== node.nodeValue) node.nodeValue = replaced;
      }
      node = walker.nextNode();
    }
  }

  // ---------- 网卡设置: compact overview + table columns ----------

  var nicFacts = { at: 0, ports: [], topology: {}, groups: [] };

  function refreshNicFacts() {
    if (Date.now() - nicFacts.at < 8000) return Promise.resolve(nicFacts);
    return Promise.all([
      currentPorts(),
      currentTopology().catch(function () { return {}; }),
      api("/api/v1/orchestrator/orchestration-groups").catch(function () { return { items: [] }; })
    ]).then(function (results) {
      nicFacts = {
        at: Date.now(),
        ports: results[0] || [],
        topology: results[1] || {},
        groups: (results[2] && results[2].items) || []
      };
      return nicFacts;
    }).catch(function () { return nicFacts; });
  }

  function portLookup(ports, key) {
    return ports.find(function (item) {
      return item.id === key || item.name === key || item.system_name === key;
    }) || null;
  }

  function interfacePage() {
    var main = document.querySelector(".main-inner");
    if (!main) return null;
    var h1 = main.querySelector("h1");
    if (!h1 || h1.textContent.trim() !== "网卡设置") return null;
    return main.querySelector(".page");
  }

  function overviewSummary(facts) {
    var ports = facts.ports || [];
    var topology = facts.topology || {};
    var roles = topology.port_roles || {};
    var members = {};
    ((topology.interfaces) || []).forEach(function (item) {
      var port = item.port || (item.bond && item.bond.name) || "";
      if (port) members[port] = item.role === "wan" ? "出口" : "入口";
    });
    (facts.groups || []).forEach(function (group) {
      (group.ports || []).forEach(function (port) {
        if (port.interface) members[port.interface] = "编排组";
      });
    });
    var counts = { 管理口: 0, 入口: 0, 出口: 0, 编排组: 0, 镜像口: 0, 其他: 0 };
    ports.forEach(function (item) {
      var keys = [item.id, item.name, item.system_name].filter(Boolean);
      var role = "";
      keys.some(function (key) { if (roles[key]) { role = roles[key]; return true; } return false; });
      var group = keys.some(function (key) { return members[key] === "编排组"; });
      var logical = "";
      keys.some(function (key) { if (members[key] && members[key] !== "编排组") { logical = members[key]; return true; } return false; });
      if (isManagement(item)) counts["管理口"] += 1;
      else if (group) counts["编排组"] += 1;
      else if (logical) counts[logical] += 1;
      else if (item.mirror === true || role === "mirror") counts["镜像口"] += 1;
      else counts["其他"] += 1;
    });
    var parts = [ports.length + " 个端口"];
    ["管理口", "入口", "出口", "编排组", "镜像口", "其他"].forEach(function (key) {
      if (counts[key]) parts.push(key + " " + counts[key]);
    });
    return parts.join(" · ");
  }

  // The page is revealed once, after its data is written. Filling a visible page
  // in place made the summary and the extra column appear under the operator's
  // eyes; a single reveal is the only way to keep the compact layout without it.
  function holdInterfacePageUntilReady() {
    var page = interfacePage();
    if (!page || page.dataset.extrasReady === "1") return;
    if (page.style.visibility !== "hidden") page.style.visibility = "hidden";
    if (page.dataset.extrasHold === "1") return;
    page.dataset.extrasHold = "1";
    // Safety net: a failed read must never leave the page invisible.
    window.setTimeout(function () {
      if (page.dataset.extrasReady !== "1") page.style.visibility = "";
    }, 2500);
  }

  function revealInterfacePage() {
    document.querySelectorAll(".main-inner .page").forEach(function (node) {
      node.dataset.extrasReady = "1";
      node.style.visibility = "";
    });
  }

  function ensureOverviewCard(page, facts) {
    var cards = Array.prototype.slice.call(page.querySelectorAll("section.card"));
    var card = cards.find(function (node) {
      var h2 = node.querySelector(".card-head h2");
      return h2 && h2.textContent.trim() === "接口总览";
    });
    if (!card) return;
    // The stylesheet hides .port-grid from the first paint; nothing to remove.

    var body = card.querySelector(".card-body");
    if (!body) return;
    var summary = body.querySelector(".nic-summary");
    if (!summary) {
      summary = document.createElement("div");
      summary.className = "nic-summary";
      body.appendChild(summary);
    }
    var text = overviewSummary(facts);
    if (summary.textContent !== text) summary.textContent = text;
    card.style.marginBottom = "10px";
  }

  function ensureInterfaceTable(page, facts) {
    var table = page.querySelector("table");
    if (!table) return;
    var headers = Array.prototype.slice.call(table.querySelectorAll("thead th"));
    var takeoverTh = headers.find(function (th) { return th.textContent.trim() === "接管能力"; });
    if (takeoverTh && takeoverTh.dataset.extrasMode !== "1") {
      takeoverTh.dataset.extrasMode = "1";
      // Keep the shell's own header text: renaming it after paint is exactly
      // the "first wording, then a different wording" flicker operators see.

    }
    // Exactly one canonical shape: insert the column only when the header is
    // absent. No stored flag and no cleanup pass, so a scan can never add a
    // second column or fight with its own previous output.
    if (takeoverTh && !table.querySelector("th[data-extras-status]")) {
      var statusTh = document.createElement("th");
      statusTh.dataset.extrasStatus = "1";
      statusTh.style.width = "84px";
      statusTh.innerHTML = '<button type="button" class="th-button">网卡状态</button>';
      takeoverTh.parentNode.insertBefore(statusTh, takeoverTh.nextSibling);
      table.classList.add("extras-auto-table");
    }
    var index = takeoverTh ? takeoverTh.cellIndex : -1;
    var rows = table.querySelectorAll("tbody tr");
    Array.prototype.forEach.call(rows, function (row) {
      var first = row.cells[0];
      if (!first) return;
      var key = first.innerText.trim().split("\n")[0];
      var item = portLookup(facts.ports || [], key);
      if (index < 0) return;
      var existing = row.querySelector("td[data-extras-status]");
      if (!existing) {
        existing = document.createElement("td");
        existing.dataset.extrasStatus = "1";
        row.insertBefore(existing, row.cells[index + 1] || null);
      }
      var state = item && item.link_state ? String(item.link_state).toLowerCase() : "";
      var text = state === "up" ? "UP" : state === "down" ? "DOWN" : "—";
      var tone = state === "up" ? "badge-success" : state === "down" ? "badge-danger" : "badge-neutral";
      var current = existing.querySelector(".badge");
      if (!current) {
        current = document.createElement("span");
        current.className = "badge " + tone;
        existing.appendChild(current);
      }
      if (current.textContent !== text) current.textContent = text;
      if (current.className !== "badge " + tone) current.className = "badge " + tone;
    });
  }

  function enhanceBondModal(page, facts) {
    var modal = Array.prototype.slice.call(document.querySelectorAll(".modal")).find(function (node) {
      var h2 = node.querySelector(".modal-head h2");
      return h2 && h2.textContent.trim() === "创建聚合";
    });
    if (!modal || modal.dataset.extrasCompat === "1") return;
    var grid = modal.querySelector(".check-grid");
    if (!grid) return;
    modal.dataset.extrasCompat = "1";
    var cards = Array.prototype.slice.call(grid.querySelectorAll(".check-card"));
    var blocked = 0;
    cards.forEach(function (card) {
      var label = card.querySelector("strong");
      var name = label ? label.textContent.trim() : "";
      var item = portLookup(facts.ports || [], name);
      var compat = item && (item.takeover_class === "compatibility" || item.takeover_mode === "af_packet");
      if (!compat) return;
      blocked += 1;
      card.style.opacity = "0.55";
      card.title = "兼容模式（af_packet）的网卡不能加入聚合组";
      var box = card.querySelector('input[type="checkbox"]');
      if (box) { box.disabled = true; box.checked = false; }
    });
    if (!blocked) return;
    var body = modal.querySelector(".modal-body");
    if (body && !body.querySelector(".bond-compat-warning")) {
      var warning = document.createElement("div");
      warning.className = "inline-warning bond-compat-warning";
      warning.style.marginBottom = "10px";
      warning.textContent =
        "兼容模式（af_packet）的网卡无法证明聚合能力，已在下方禁用；请先把网卡切换到高性能接管，再创建聚合。";
      body.insertBefore(warning, body.firstChild);
    }
    var usable = cards.length - blocked;
    var primary = modal.querySelector(".modal-footer .btn-primary");
    if (primary && usable === 0) {
      primary.disabled = true;
      primary.title = "没有可用于聚合的网卡：当前所有物理口都在兼容模式";
    }
  }

  function decorateInterfacesPage() {
    if (!interfacePage()) return;
    refreshNicFacts().then(function (facts) {
      // The read takes a moment and React can replace the page node meanwhile
      // (the previous code held a stale reference and silently skipped every
      // enhancement, which is why the summary sometimes never appeared).
      var current = interfacePage();
      if (!current) return;
      ensureOverviewCard(current, facts);
      ensureInterfaceTable(current, facts);
      enhanceBondModal(current, facts);
      revealInterfacePage();
    }).catch(function () {
      // Never leave the page hidden because a read failed.
      revealInterfacePage();
    });
  }

  // The role save only writes topology metadata; the dataplane needs the
  // interface mirror flag (and an apply) to actually create or drop the SPAN.
  function putTopologyInternal(doc) {
    skipRoleInject = true;
    return api("/api/v1/orchestrator/topology", { method: "PUT", body: doc }).then(function (result) {
      skipRoleInject = false;
      return result;
    }, function (error) {
      skipRoleInject = false;
      throw error;
    });
  }

  function syncMirrorAfterRole(port, role, sources) {
    // The baseline save may not carry the new role (its React state can be
    // replaced when the confirm dialog opens), so verify and repair it here.
    return currentTopology().then(function (topo) {
      var roles = Object.assign({}, topo.port_roles || {});
      var current = roles[port] || "";
      if (current === role) return null;
      if (role === "") delete roles[port];
      else roles[port] = role;
      var doc = Object.assign({}, topo, { port_roles: roles });
      delete doc.checksum;
      return putTopologyInternal(doc);
    }).then(function () {
      return currentPorts();
    }).then(function (ports) {
      var item = ports.find(function (p) { return p.id === port || p.name === port || p.system_name === port; });
      if (!item) return null;
      if (role === "mirror") {
        // Always send the current source list: the topology save can drop it,
        // and a role re-save must not erase the mirror policies.
        var keep = sources && sources.length ? sources : (item.mirror_sources || []);
        return api("/api/v1/interfaces/" + encodeURIComponent(item.id), {
          method: "PATCH",
          body: { id: item.id, name: portName(item), mirror: true, mirror_sources: keep }
        });
      }
      if (role !== "mirror" && item.mirror === true) {
        return api("/api/v1/interfaces/" + encodeURIComponent(item.id), {
          method: "PATCH",
          body: { id: item.id, name: portName(item), mirror: false, mirror_sources: [] }
        });
      }
      return null;
    }).then(function () {
      return api("/api/v1/runtime/apply", { method: "POST", body: {} }).catch(function () { return null; });
    }).then(function () {
      return refreshMirrorIds(true);
    }).then(function () {
      relabelMirrorRoles();
    });
  }

  // A mirror-role change that the dataplane rejects (second mirror port, a port
  // owned by a group, ...) must not leave the console ahead of the device.
  function revertRole(snapshot, error) {
    showToast("镜像口设置失败：" + (error && error.message ? error.message : error), "error");
    currentTopology().then(function (topo) {
      var roles = Object.assign({}, topo.port_roles || {});
      if (snapshot.previous) roles[snapshot.port] = snapshot.previous;
      else delete roles[snapshot.port];
      var doc = Object.assign({}, topo, { port_roles: roles });
      delete doc.checksum;
      return putTopologyInternal(doc);
    }).then(function () {
      return refreshMirrorIds(true);
    }).catch(function () { /* the next reload repairs the table */ });
  }

  function roleConflictResponse(message) {
    return new Response(JSON.stringify({ error: { code: "mirror_conflict", message: message } }), {
      status: 409,
      headers: { "Content-Type": "application/json" }
    });
  }

  function groupWriteRequest(url, method) {
    if (method === "POST" && /\/api\/v1\/orchestrator\/orchestration-groups$/.test(url)) return true;
    if (method === "PUT" && /\/api\/v1\/orchestrator\/orchestration-groups\/[^/]+$/.test(url)) return true;
    return false;
  }

  // The IP-group page writes /api/v1/objects/ip-groups, while the policy
  // editor only offers the policy document's inline ip_objects. Merge the
  // stored groups into the policy payload the editor reads so a group created
  // on that page is selectable in a rule (inline CIDRs stay untouched).
  function mergeIPGroups(doc) {
    return api("/api/v1/objects/ip-groups").then(function (data) {
      var item = doc && doc.item;
      if (!item) return doc;
      var merged = {};
      (item.ip_objects || []).forEach(function (object) {
        if (object && object.id) merged[object.id] = object;
      });
      ((data && data.items) || []).forEach(function (group) {
        var kind = group.kind || group.group_type || group.type;
        if (kind && kind !== "ip") return;
        var id = group.name || group.id;
        if (!id) return;
        var prefixes = group.entries || group.members || [];
        merged[id] = { id: id, prefixes: prefixes };
      });
      item.ip_objects = Object.keys(merged).sort().map(function (key) { return merged[key]; });
      return doc;
    }).catch(function () { return doc; });
  }

  // ---------- fetch interception ----------

  var rawFetch = window.fetch.bind(window);
  window.fetch = function (input, init) {
    var url = typeof input === "string" ? input : (input && input.url) || "";
    var opts = init || {};
    var method = String(opts.method || "GET").toUpperCase();
    var bodyText = typeof opts.body === "string" ? opts.body : "";
    var topologySave = method === "PUT" && /\/api\/v1\/orchestrator\/topology$/.test(url);
    var roleSnapshot = topologySave && !skipRoleInject && roleModal
      ? {
          port: roleModal.port,
          role: roleModal.role,
          previous: roleModal.previous || "",
          sources: roleModal.sources || []
        }
      : null;

    if (method === "GET" && /\/api\/v1\/orchestrator\/topology(\?|$)/.test(url)) {
      return rawFetch(input, opts).then(function (response) {
        if (response.status !== 404) return response;
        var draft = loadDraft();
        var ready = draft ? Promise.resolve(draft) : emptyTopologyDraft();
        return ready.then(function (doc) {
          return new Response(JSON.stringify({ item: doc, checksum: "" }), {
            status: 200,
            statusText: "OK",
            headers: { "Content-Type": "application/json" }
          });
        });
      });
    }

    if (method === "GET" && /\/api\/v1\/orchestrator\/policy$/.test(url)) {
      return rawFetch(input, opts).then(function (response) {
        if (!response.ok) return response;
        return response.clone().json().then(function (doc) {
          return mergeIPGroups(doc).then(function (merged) {
            return new Response(JSON.stringify(merged), {
              status: response.status,
              statusText: response.statusText,
              headers: { "Content-Type": "application/json" }
            });
          });
        }).catch(function () { return response; });
      });
    }

    if (roleSnapshot && bodyText) {
      try {
        var doc = JSON.parse(bodyText);
        doc.port_roles = doc.port_roles || {};
        if (roleSnapshot.role === "") delete doc.port_roles[roleSnapshot.port];
        else doc.port_roles[roleSnapshot.port] = roleSnapshot.role;
        var pending = loadDraft();
        if (pending) doc = mergeTopologyDraft(pending, doc);
        var boundaries = boundaryRoles(doc);
        if (!boundaries.lan || !boundaries.wan) {
          rememberDraft(doc);
          var missing = !boundaries.lan ? "入口（LAN）" : "出口（WAN）";
          showToast("已记录当前角色，还需要配置" + missing, "info");
          return Promise.resolve(new Response(JSON.stringify({ item: doc, checksum: "" }), {
            status: 200,
            statusText: "OK",
            headers: { "Content-Type": "application/json" }
          }));
        }
        rememberDraft(null);
        opts = Object.assign({}, opts, { body: JSON.stringify(doc) });
      } catch (err) { /* keep the original body */ }
    }

    // Reject a second mirror port before the console records the role; the
    // synthetic 409 carries a message the baseline shows in its toast.
    if (roleSnapshot && roleSnapshot.role === "mirror") {
      return currentPorts().then(function (ports) {
        var other = ports.find(function (item) {
          return item.mirror === true && item.id !== roleSnapshot.port && item.name !== roleSnapshot.port;
        });
        if (other) {
          return roleConflictResponse(
            "接口 " + portLabel(other) + " 已经是镜像口；请先把它的角色改回 LAN/WAN/未配置，再设置本端口。"
          );
        }
        return rawFetch(input, opts).then(function (response) {
          if (response.ok) {
            syncMirrorAfterRole(roleSnapshot.port, roleSnapshot.role, roleSnapshot.sources).catch(function (error) {
              revertRole(roleSnapshot, error);
            });
          }
          return response;
        });
      });
    }

    if (roleSnapshot) {
      return rawFetch(input, opts).then(function (response) {
        if (response.ok) {
          syncMirrorAfterRole(roleSnapshot.port, roleSnapshot.role, roleSnapshot.sources).catch(function (error) {
            revertRole(roleSnapshot, error);
          });
        }
        return response;
      });
    }

    if (groupWriteRequest(url, method)) {
      return rawFetch(input, opts).then(function (response) {
        if (response.ok && pendingGroupBypass && Date.now() - pendingGroupBypass.at < 120000) {
          var pending = pendingGroupBypass;
          var name = "";
          try { name = String((JSON.parse(bodyText || "{}") || {}).name || ""); } catch (err) { name = ""; }
          if (!name) name = decodeURIComponent(url.split("/").pop() || "");
          if (name && name === pending.name) {
            pendingGroupBypass = null;
            saveBypassForGroup(pending.name, pending.state, true).catch(function (error) {
              showToast("故障旁路保存失败：" + (error && error.message ? error.message : error), "error");
            });
          }
        } else if (!response.ok) {
          pendingGroupBypass = null;
        }
        return response;
      });
    }

    if (method === "DELETE" && /\/api\/v1\/orchestrator\/orchestration-groups\/[^/]+$/.test(url)) {
      return rawFetch(input, opts).then(function (response) {
        if (response.ok) {
          var name = decodeURIComponent(url.split("/").pop() || "");
          removeBypassGroup(name).catch(function () { /* the prune loop retries */ });
        }
        return response;
      });
    }

    return rawFetch(input, opts);
  };

  // ---------- DOM observation ----------

  function relabelConfirmTitle() {    if (!roleModal) return;
    document.querySelectorAll(".modal").forEach(function (modal) {
      var heading = modal.querySelector(".modal-head h2");
      if (!heading) return;
      var text = heading.textContent.trim();
      if (!/^将 .+ 配置为 /.test(text)) return;
      var port = roleModal.port;
      var wanted = roleModal.role === "mirror"
        ? "将 " + port + " 配置为 镜像口？"
        : roleModal.role === ""
          ? "清除 " + port + " 的角色配置？"
          : "";
      if (wanted && text !== wanted) heading.textContent = wanted;
    });
  }

  // Routing-consistent feedback: routine apply success ("顺序已生效（3.3s）")
  // stays out of the toast stack, but explicit save/upgrade/rollback messages
  // remain visible.
  function keepSuccessToasts() {
    document.querySelectorAll(".toast-stack .toast-success").forEach(function (node) {
      if (node.classList.contains("extras-keep")) return;
      var text = node.textContent || "";
      if (/保存成功|已保存|升级完成|回滚完成|导入成功|导出成功|已创建|已删除|已更新|已恢复/.test(text)) {
        node.classList.add("extras-keep");
      }
    });
  }

  var STATUS_COPY = {
    "运行正常": { label: "运行正常", tone: "success" },
    "设备正常": { label: "运行正常", tone: "success" },
    "配置下发中": { label: "配置下发中", tone: "info" },
    "配置应用中": { label: "配置下发中", tone: "info" },
    "读取中": { label: "配置下发中", tone: "info" },
    "状态读取中": { label: "配置下发中", tone: "info" },
    "配置异常": { label: "配置异常", tone: "danger" },
    "需要处理": { label: "配置异常", tone: "danger" },
    "配置未生效": { label: "配置异常", tone: "danger" },
    "状态未知": { label: "配置异常", tone: "danger" }
  };

  // The frozen shell still uses neutral labels (需要处理 / 配置未生效 /
  // 设备正常). The product exposes three states only: 运行正常 / 配置下发中 /
  // 配置异常. Rewrite the copy and the tone without touching React's nodes.
  function normalizeStatusCopy() {
    document.querySelectorAll(".health-pill").forEach(function (pill) {
      var span = pill.querySelector("span:not(.health-reason)");
      if (!span) return;
      var mapped = STATUS_COPY[(span.textContent || "").trim()];
      if (!mapped) return;
      if (span.textContent !== mapped.label) span.textContent = mapped.label;
      pill.classList.remove("health-success", "health-warning", "health-danger", "health-info");
      pill.classList.add("health-" + mapped.tone);
      var dot = pill.querySelector(".status-dot");
      if (dot) dot.className = "status-dot status-dot-" + mapped.tone;
    });
    document.querySelectorAll(".sidebar-foot").forEach(function (foot) {
      var spans = foot.querySelectorAll("span");
      var label = spans[spans.length - 1];
      if (!label) return;
      var mapped = STATUS_COPY[(label.textContent || "").trim()];
      if (!mapped) return;
      if (label.textContent !== mapped.label) label.textContent = mapped.label;
      var dot = foot.querySelector(".status-dot");
      if (dot) dot.className = "status-dot status-dot-" + mapped.tone;
    });
    document.querySelectorAll(".metric-bottom, .metric").forEach(function (box) {
      var walker = document.createTreeWalker(box, NodeFilter.SHOW_TEXT, null);
      var node = walker.nextNode();
      while (node) {
        if ((node.nodeValue || "").trim() === "available") node.nodeValue = "可用";
        node = walker.nextNode();
      }
    });
    document.documentElement.classList.add("extras-status-ready");
  }

  // Safety net: if this script never gets to run its pass, the status wording
  // must still be visible.
  window.setTimeout(function () {
    document.documentElement.classList.add("extras-status-ready");
  }, 3000);

  // 模式 字段整体下线：管理口只支持独立模式，一个只有单一选项的下拉只会让人
  // 以为「与 LAN 共享」还在。MutationObserver 的回调是微任务，先于浏览器绘制执行，
  // 所以新渲染出来的字段也会在绘制前被隐藏，不会出现闪烁。
  function hideManagementModeField() {
    document.querySelectorAll("label.field").forEach(function (field) {
      var label = field.querySelector(".field-label");
      if (!label) return;
      if (label.textContent.replace(/\*/g, "").trim() !== "模式") return;
      if (!field.querySelector("select")) return;
      if (field.style.display === "none") return;
      field.style.display = "none";
    });
  }

  var managementModeObserver = new MutationObserver(function () {
    hideManagementModeField();
    holdInterfacePageUntilReady();
  });
  if (document.body) {
    managementModeObserver.observe(document.body, { childList: true, subtree: true });
  }
  hideManagementModeField();

  // 模式 下拉里的「与 LAN 共享」已经下线：管理口只支持独立模式。
  function dropSharedManagement() {
    document.querySelectorAll('select option[value="shared_lan"]').forEach(function (option) {
      var select = option.parentElement;
      // Never auto-flip a device that is currently sharing the management port:
      // that would change the very interface the operator is connected through.
      // An existing shared setup stays visible (but not selectable) so it can be
      // migrated deliberately; on everything else the option is simply gone.
      if (select && select.value === "shared_lan") {
        option.disabled = true;
        option.setAttribute("data-extras-removed", "1");
        if (option.textContent.indexOf("已下线") === -1) option.textContent = option.textContent + "（已下线）";
        return;
      }
      option.hidden = true;
      option.disabled = true;
      option.setAttribute("data-extras-removed", "1");
    });
  }

  // 流量编排 表格用固定列宽，宽屏下右侧会空一大片；让表格自适应卡片宽度。
  function stretchPolicyTable() {
    var h1 = document.querySelector(".main-inner h1");
    if (!h1 || h1.textContent.trim() !== "流量编排") return;
    document.querySelectorAll(".main-inner table.data-table").forEach(function (table) {
      if (table.dataset.extrasStretch === "1") return;
      table.dataset.extrasStretch = "1";
      table.classList.add("extras-stretch-table");
      table.querySelectorAll("th").forEach(function (th) {
        if (th.style.width) th.style.width = "";
        if (th.style.minWidth) th.style.minWidth = "";
      });
    });
  }

  function scan() {
    document.querySelectorAll(".modal").forEach(function (modal) {
      var heading = modal.querySelector(".modal-head h2");
      if (!heading) return;
      if (heading.textContent.trim() === "角色配置") enhanceRoleModal(modal);
      else if (/编排组/.test(heading.textContent)) enhanceGroupModal(modal);
    });
    if (!document.querySelector(".modal[data-role-enhanced='1']")) roleModal = null;
    if (!document.querySelector(".modal[data-bypass-enhanced='1']")) groupModal = null;
    relabelConfirmTitle();
    keepSuccessToasts();
    inject();
    hideManagementModeField();
    dropSharedManagement();
    stretchPolicyTable();
    normalizeStatusCopy();
    scheduleRelabel();
  }

  function pruneBypassGroups() {
    if (document.hidden) return;
    api("/api/v1/orchestrator/orchestration-groups").then(function (groups) {
      var names = {};
      ((groups && groups.items) || []).forEach(function (item) { names[item.name] = true; });
      return api("/api/v1/orchestrator/bypass").then(function (data) {
        var config = (data && data.config) || {};
        var existing = config.groups || [];
        var kept = existing.filter(function (item) { return names[item.name]; });
        if (kept.length === existing.length) return null;
        config.groups = kept;
        return api("/api/v1/orchestrator/bypass/config", { method: "PUT", body: config });
      });
    }).catch(function () { /* best effort */ });
  }

  var scanTimer = null;
  function scheduleScan() {
    if (scanTimer) return;
    scanTimer = window.setTimeout(function () {
      scanTimer = null;
      scan();
    }, 120);
  }

  function start() {
    if (!document.getElementById("extras-view-style")) {
      var style = document.createElement("style");
      style.id = "extras-view-style";
      style.textContent =
        "[hidden]{display:none!important}" +
        "#extras-view{display:flex;flex-direction:column;gap:16px}" +
        "#extras-view .extras-wrap{max-width:none;margin:0;padding:0}" +
        "#extras-view .page-head{margin:0}" +
        ".bypass-options{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px;margin-top:6px}" +
        ".bypass-options .check-card{align-items:flex-start}" +
        ".bypass-fields{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px 14px;margin-top:10px;padding:12px;border:1px solid var(--border);border-radius:var(--radius-md);background:var(--surface-sunken,#f8fafc)}" +
        ".bypass-error{margin-top:8px}" +
        ".extras-toast{position:fixed;right:24px;bottom:24px;z-index:1000;max-width:420px;padding:11px 14px;border-radius:var(--radius-md);border:1px solid var(--border);background:#fff;box-shadow:0 12px 30px rgba(15,23,42,.16);font-size:12px;line-height:1.5;color:var(--text-secondary)}" +
        ".extras-toast[data-tone=error]{border-color:#fecaca;background:#fff7f7;color:#b42318}" +
        ".nic-summary{display:flex;flex-wrap:wrap;gap:6px 16px;font-size:12px;color:var(--text-secondary)}" +
        ".takeover-facts{flex-wrap:wrap;gap:6px 16px}" +
        ".takeover-facts>span{font-size:12px}" +
        ".toast-stack .toast-success{display:none!important}" +
        ".toast-stack .toast-success.extras-keep{display:flex!important}" +
        ".metric-grid{grid-template-columns:repeat(auto-fit,minmax(180px,1fr))}" +
        ".extras-auto-table{table-layout:auto}" +
        ".metric{min-height:112px;padding:14px 16px}" +
        ".metric strong{font-size:24px}" +
        ".traffic-chart{min-height:clamp(160px,20vh,240px)}" +
        ".trend-empty{min-height:clamp(200px,26vh,300px)}" +
        ".port-grid-large{grid-template-columns:repeat(4,minmax(0,1fr))}" +
        "@media(min-width:1181px){.overview-main-grid .span-8{grid-column:span 8}.overview-main-grid .span-4{grid-column:span 4}.overview-secondary-grid .span-7,.overview-bottom-grid .span-7{grid-column:span 7}.overview-secondary-grid .span-5,.overview-bottom-grid .span-5{grid-column:span 5}}" +
        "@media(max-width:1180px){.overview-main-grid .span-8,.overview-main-grid .span-4,.overview-secondary-grid .span-7,.overview-secondary-grid .span-5,.overview-bottom-grid .span-7,.overview-bottom-grid .span-5{grid-column:span 12}}" +
        "@media(max-width:1500px){.metric{padding:14px 16px}.metric strong{font-size:24px}.metric-grid{grid-template-columns:repeat(auto-fit,minmax(180px,1fr))}}" +
        "@media(max-width:1280px){.metric-grid{grid-template-columns:repeat(auto-fit,minmax(180px,1fr))}}" +
        ".health-pill.health-info{border-color:#c4e9f7;background:var(--info-soft);color:#0369a1}";
      document.head.appendChild(style);
    }
    inject();
    var nav = document.querySelector("nav.side-nav");
    if (nav && window.MutationObserver) {
      var observer = new MutationObserver(function () { inject(); });
      observer.observe(nav, { childList: true, subtree: false });
    }
    // Sorting and the column picker rebuild the table cells; re-apply the
    // synthetic 网卡状态 column after those interactions.
    document.addEventListener("click", function (event) {
      var target = event.target && event.target.closest
        ? event.target.closest(".th-button, .column-menu label, .column-picker button")
        : null;
      if (!target) return;
      window.setTimeout(decorateInterfacesPage, 350);
      window.setTimeout(decorateInterfacesPage, 1200);
    }, true);
    if (window.MutationObserver) {
      var bodyObserver = new MutationObserver(scheduleScan);
      bodyObserver.observe(document.body, { childList: true, subtree: true });
    }
    var wanted = kindFromPath(window.location.pathname);
    if (wanted) openView(wanted, false);
    scheduleScan();
    window.setInterval(pruneBypassGroups, 30000);
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
    if (active) closeView();
  });

  window.addEventListener("popstate", function () {
    var kind = kindFromPath(window.location.pathname);
    if (kind) openView(kind, false);
    else if (active) closeView();
  });

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();

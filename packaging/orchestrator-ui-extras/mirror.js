/* Port mirror page for the frozen orchestrator UI. Uses the orchestrator
 * interface resource + runtime apply contract; never talks to VPP directly. */
window.LyExtrasMirrorView = function (root) {
  "use strict";

  var state = {
    items: [],
    targetId: "",
    sources: {}, // port id -> direction
    mirror: null,
    component: null,
    busy: false
  };

  var $ = function (id) { return root ? root.querySelector("#" + id) : document.getElementById(id); };

  function api(path, options) {
    var opts = options || {};
    var headers = Object.assign({ Accept: "application/json" }, opts.headers || {});
    if (opts.body !== undefined && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
    return fetch(path, {
      method: opts.method || "GET",
      credentials: "same-origin",
      headers: headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body)
    }).then(function (response) {
      if (response.status === 401) {
        window.location.href = "/";
        throw new Error("登录状态已失效");
      }
      return response.text().then(function (text) {
        var body = null;
        try { body = text ? JSON.parse(text) : null; } catch (err) { body = text; }
        if (!response.ok) {
          var detail = body && body.error ? (body.error.message || body.error.code) : null;
          throw new Error(detail || body || ("HTTP " + response.status));
        }
        return body;
      });
    });
  }

  function showError(message) {
    var box = $("error");
    if (!message) { box.hidden = true; box.textContent = ""; return; }
    box.hidden = false;
    box.innerHTML = "";
    var strong = document.createElement("strong");
    strong.textContent = "操作失败";
    var p = document.createElement("p");
    p.textContent = String(message);
    box.appendChild(strong);
    box.appendChild(p);
  }

  function isManagement(item) {
    return item.management === true || item.gateway_role === "management" || item.takeover_state === "management";
  }

  function isPhysical(item) {
    return Boolean(item.system_name) && !isManagement(item);
  }

  function portName(item) {
    return item.name || item.id;
  }

  function portLabel(item) {
    var name = portName(item);
    return item.system_name && item.system_name !== name ? name + " · " + item.system_name : name;
  }

  function portByReference(reference) {
    return state.items.find(function (item) {
      return item.id === reference || item.name === reference || item.system_name === reference;
    }) || null;
  }

  function currentMirror() {
    return state.items.find(function (item) { return item.mirror === true; }) || null;
  }

  function sourceDirections(item) {
    var result = {};
    (item && item.mirror_sources ? item.mirror_sources : []).forEach(function (source) {
      if (!source || !source.interface) return;
      result[source.interface] = source.direction || "both";
    });
    return result;
  }

  function renderTargetOptions() {
    var select = $("target");
    select.innerHTML = "";
    var none = document.createElement("option");
    none.value = "";
    none.textContent = "未选择";
    select.appendChild(none);
    state.items.filter(isPhysical).forEach(function (item) {
      var role = String(item.gateway_role || "").toLowerCase();
      if (role === "lan" || role === "wan") return;
      var option = document.createElement("option");
      option.value = item.id;
      option.textContent = portLabel(item) + (item.mirror ? "（当前镜像口）" : "");
      select.appendChild(option);
    });
    select.value = state.targetId;
  }

  function renderSources() {
    var list = $("sources");
    list.innerHTML = "";
    var candidates = state.items.filter(function (item) {
      return isPhysical(item) && item.id !== state.targetId;
    });
    if (!candidates.length) {
      var empty = document.createElement("div");
      empty.className = "muted";
      empty.textContent = "没有可用作镜像源的已接管物理网卡。";
      list.appendChild(empty);
      return;
    }
    candidates.forEach(function (item) {
      var reference = state.sources.hasOwnProperty(item.id)
        ? item.id
        : (state.sources.hasOwnProperty(item.system_name) ? item.system_name : item.name);
      var checked = Boolean(reference && state.sources.hasOwnProperty(reference));
      var direction = checked ? state.sources[reference] : "both";

      var row = document.createElement("label");
      row.className = "source-row" + (checked ? " checked" : "");

      var box = document.createElement("input");
      box.type = "checkbox";
      box.checked = checked;
      box.addEventListener("change", function () {
        if (box.checked) state.sources[item.id] = direction;
        else { delete state.sources[item.id]; delete state.sources[item.system_name]; }
        renderSources();
      });

      var name = document.createElement("span");
      name.className = "name";
      var strong = document.createElement("strong");
      strong.textContent = portLabel(item);
      var small = document.createElement("small");
      var role = String(item.gateway_role || "").toLowerCase();
      small.textContent = (item.link_state || "未知") + (role === "lan" ? " · LAN" : role === "wan" ? " · WAN" : "");
      name.appendChild(strong);
      name.appendChild(small);

      var select = document.createElement("select");
      select.className = "select";
      [["both", "双向"], ["rx", "仅接收"], ["tx", "仅发送"]].forEach(function (pair) {
        var option = document.createElement("option");
        option.value = pair[0];
        option.textContent = pair[1];
        select.appendChild(option);
      });
      select.value = direction;
      select.disabled = !box.checked;
      select.addEventListener("click", function (event) { event.preventDefault(); event.stopPropagation(); });
      select.addEventListener("change", function () {
        if (box.checked) state.sources[item.id] = select.value;
      });

      row.appendChild(box);
      row.appendChild(name);
      row.appendChild(select);
      list.appendChild(row);
    });
  }

  function renderStatus() {
    var mirror = currentMirror();
    var status = $("status");
    var badge = $("state-badge");
    status.innerHTML = "";

    if (!mirror) {
      badge.className = "badge badge-neutral";
      badge.textContent = "未配置";
      var empty = document.createElement("div");
      empty.className = "muted";
      empty.textContent = "尚未配置镜像口。选择目标口与源口后点击“保存并应用”。";
      status.appendChild(empty);
      return;
    }

    var component = state.component;
    var running = component && component.state === "running";
    badge.className = "badge " + (running ? "badge-success" : "badge-warning");
    badge.textContent = running ? "已生效" : "未生效";

    var head = document.createElement("div");
    head.className = "notice-block";
    var title = document.createElement("strong");
    title.textContent = "镜像目标口 " + portLabel(mirror);
    var detail = document.createElement("span");
    detail.textContent = "链路 " + (mirror.link_state || "未知") + " · MAC " + (mirror.mac || "无") +
      " · 源口 " + ((mirror.mirror_sources || []).length);
    head.appendChild(title);
    head.appendChild(detail);
    status.appendChild(head);

    var list = document.createElement("div");
    (mirror.mirror_sources || []).forEach(function (source) {
      var item = portByReference(source.interface);
      var row = document.createElement("div");
      row.className = "session";
      var meta = document.createElement("div");
      meta.className = "meta";
      var strong = document.createElement("strong");
      strong.textContent = (item ? portLabel(item) : source.interface) + " → " + portLabel(mirror);
      var small = document.createElement("small");
      var direction = source.direction || "both";
      small.textContent = "方向 " + (direction === "rx" ? "仅接收" : direction === "tx" ? "仅发送" : "双向") + " · 三层镜像（l2 关闭）";
      meta.appendChild(strong);
      meta.appendChild(small);
      var stateBadge = document.createElement("span");
      stateBadge.className = "badge " + (running ? "badge-success" : "badge-warning");
      stateBadge.textContent = running ? "已生效" : "未生效";
      row.appendChild(meta);
      row.appendChild(stateBadge);
      list.appendChild(row);
    });
    if (!(mirror.mirror_sources || []).length) {
      var none = document.createElement("div");
      none.className = "inline-warning";
      none.textContent = "镜像口还没有镜像源。";
      list.appendChild(none);
    }
    status.appendChild(list);

    if (component && component.reason) {
      var reason = document.createElement("div");
      reason.className = running ? "notice-block" : "inline-warning";
      reason.style.marginTop = "10px";
      var reasonTitle = document.createElement("strong");
      reasonTitle.textContent = running ? "运行时回执" : "运行时未确认会话";
      var reasonText = document.createElement("span");
      reasonText.textContent = component.reason;
      reason.appendChild(reasonTitle);
      reason.appendChild(reasonText);
      status.appendChild(reason);
    }
  }

  function render() {
    renderTargetOptions();
    renderSources();
    renderStatus();
  }

  function load() {
    return Promise.all([
      api("/api/v1/interfaces"),
      api("/api/v1/runtime/status").catch(function () { return null; })
    ]).then(function (results) {
      state.items = (results[0] && results[0].items) || [];
      var components = (results[1] && results[1].components) || [];
      state.component = components.find(function (component) { return component.name === "mirror"; }) || null;
      var mirror = currentMirror();
      state.mirror = mirror;
      state.targetId = mirror ? mirror.id : "";
      state.sources = sourceDirections(mirror);
      render();
    }).catch(function (error) {
      showError(error.message || error);
    });
  }

  function waitForApply() {
    var tries = 0;
    return new Promise(function (resolve) {
      var timer = setInterval(function () {
        tries += 1;
        api("/api/v1/runtime/status").then(function (status) {
          var components = (status && status.components) || [];
          var component = components.find(function (item) { return item.name === "mirror"; });
          var desired = currentMirror();
          var settled = desired ? (component && component.state === "running") : !component;
          if (settled || tries >= 40) {
            clearInterval(timer);
            resolve();
          }
        }).catch(function () {
          if (tries >= 40) { clearInterval(timer); resolve(); }
        });
      }, 1500);
    });
  }

  function apply() {
    return api("/api/v1/runtime/apply", { method: "POST", body: {} }).then(function (result) {
      var status = result && (result.status || result.runtime_state);
      if (status && /fail|error|locked/.test(String(status))) {
        throw new Error(result.reason || ("应用失败：" + status));
      }
      return waitForApply();
    });
  }

  function setBusy(busy) {
    state.busy = busy;
    $("save").disabled = busy;
    $("cancel").disabled = busy;
    $("refresh").disabled = busy;
    $("save").textContent = busy ? "应用中…" : "保存并应用";
  }

  function save() {
    var target = state.items.find(function (item) { return item.id === state.targetId; });
    if (!target) { showError("请先选择镜像目标口"); return; }
    var sources = Object.keys(state.sources).map(function (reference) {
      var item = portByReference(reference) || { id: reference };
      return { interface: item.id, direction: state.sources[reference] || "both" };
    });
    setBusy(true);
    showError("");
    api("/api/v1/interfaces/" + encodeURIComponent(target.id), {
      method: "PATCH",
      body: { id: target.id, name: portName(target), mirror: true, mirror_sources: sources }
    }).then(apply).then(function () {
      return load();
    }).catch(function (error) {
      showError(error.message || error);
    }).then(function () {
      setBusy(false);
    });
  }

  function cancelMirror() {
    var mirror = currentMirror();
    if (!mirror) { showError("当前没有镜像口"); return; }
    if (!window.confirm("取消 " + portLabel(mirror) + " 的镜像口并清除全部会话？")) return;
    setBusy(true);
    showError("");
    api("/api/v1/interfaces/" + encodeURIComponent(mirror.id), {
      method: "PATCH",
      body: { id: mirror.id, name: portName(mirror), mirror: false, mirror_sources: [] }
    }).then(apply).then(function () {
      return load();
    }).catch(function (error) {
      showError(error.message || error);
    }).then(function () {
      setBusy(false);
    });
  }

  $("target").addEventListener("change", function (event) {
    state.targetId = event.target.value;
    state.sources = {};
    var mirror = currentMirror();
    if (mirror && mirror.id === state.targetId) state.sources = sourceDirections(mirror);
    render();
  });
  $("save").addEventListener("click", save);
  $("cancel").addEventListener("click", cancelMirror);
  $("refresh").addEventListener("click", function () { load(); });

  load();
  return function () {};
};

if (document.body && document.body.dataset.extrasPage === "mirror") {
  window.LyExtrasMirrorView(document);
}

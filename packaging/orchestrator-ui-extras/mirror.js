/* 流量镜像 page for the frozen orchestrator UI.
 *
 * One row in the table is one physical source port mirrored into the single
 * mirror destination port (selected in 网卡设置 → 角色配置). The add/edit
 * dialog mirrors the 流量编排 policy dialog: a modal-xl with .policy-form
 * sections, .form-grid fields, native <select> controls and the same footer.
 */
window.LyExtrasMirrorView = function (root) {
  "use strict";

  var state = {
    ports: [],
    component: null,
    busy: false,
    // Nothing is written to the page before the first read finishes, otherwise
    // the operator sees "未配置 / 0 条" and then the real values.
    ready: false
  };

  var $ = function (id) { return root ? root.querySelector("#" + id) : document.getElementById(id); };
  var helpers = window.LyOrchExtras || {};
  var portLabel = helpers.portLabel || function (item) { return item.name || item.id; };
  var isManagement = helpers.isManagement || function (item) {
    return item.management === true || item.gateway_role === "management" || item.takeover_state === "management";
  };
  var iconClose =
    '<svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" ' +
    'stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-x">' +
    '<path d="M18 6 6 18"></path><path d="m6 6 12 12"></path></svg>';

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
    if (!box) return;
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

  function isPhysical(item) {
    return Boolean(item.system_name) && !isManagement(item);
  }

  function mirrorPort() {
    return state.ports.find(function (item) { return item.mirror === true; }) || null;
  }

  function portByReference(reference) {
    return state.ports.find(function (item) {
      return item.id === reference || item.name === reference || item.system_name === reference;
    }) || null;
  }

  function sources() {
    var port = mirrorPort();
    return port && port.mirror_sources ? port.mirror_sources : [];
  }

  function running() {
    return Boolean(state.component && state.component.state === "running");
  }

  function directionLabel(value) {
    return value === "rx" ? "仅接收" : value === "tx" ? "仅发送" : "双向";
  }

  function renderTable() {
    if (!state.ready) return;
    var port = mirrorPort();
    var rows = $("policy-rows");
    if (!rows) return;
    rows.innerHTML = "";
    var list = sources();
    var empty = $("empty");
    if (empty) empty.hidden = Boolean(list.length) || !port;

    var applying = state.busy || Boolean(state.component && /applying|recovering|pending/.test(String(state.component.state)));
    list.forEach(function (source) {
      var item = portByReference(source.interface) || { id: source.interface, name: source.interface };
      var tr = document.createElement("tr");

      var name = document.createElement("td");
      var strong = document.createElement("strong");
      strong.textContent = portLabel(item);
      name.appendChild(strong);

      var dir = document.createElement("td");
      dir.textContent = directionLabel(source.direction);

      var out = document.createElement("td");
      out.textContent = port ? portLabel(port) : "—";

      var status = document.createElement("td");
      var badge = document.createElement("span");
      badge.className = "badge " + (applying ? "badge-info" : running() ? "badge-success" : "badge-warning");
      badge.textContent = applying ? "下发中" : running() ? "已生效" : "未生效";
      status.appendChild(badge);

      var actions = document.createElement("td");
      var wrap = document.createElement("div");
      wrap.className = "row-actions";
      var edit = document.createElement("button");
      edit.className = "btn btn-sm btn-text";
      edit.textContent = "编辑";
      edit.disabled = state.busy || applying;
      edit.addEventListener("click", function () { openDialog(source.interface); });
      var remove = document.createElement("button");
      remove.className = "btn btn-sm btn-text";
      remove.textContent = "删除";
      remove.disabled = state.busy || applying;
      remove.addEventListener("click", function () { removeSource(source.interface); });
      wrap.appendChild(edit);
      wrap.appendChild(remove);
      actions.appendChild(wrap);

      tr.appendChild(name);
      tr.appendChild(dir);
      tr.appendChild(out);
      tr.appendChild(status);
      tr.appendChild(actions);
      rows.appendChild(tr);
    });
  }

  function closeDialog() {
    var backdrops = document.querySelectorAll(".modal-backdrop[data-mirror-dialog]");
    Array.prototype.forEach.call(backdrops, function (node) { node.remove(); });
  }

  function candidates() {
    var port = mirrorPort();
    return state.ports.filter(function (item) {
      return isPhysical(item) && (!port || item.id !== port.id);
    });
  }

  function openDialog(editInterface) {
    if (!mirrorPort()) { showError("请先在网卡设置里把一个端口配置为镜像口"); return; }
    closeDialog();
    var editSource = editInterface ? sources().find(function (item) { return item.interface === editInterface; }) : null;

    var backdrop = document.createElement("div");
    backdrop.className = "modal-backdrop";
    backdrop.setAttribute("data-mirror-dialog", "1");
    var modal = document.createElement("div");
    modal.className = "modal modal-xl";
    modal.setAttribute("role", "dialog");
    modal.setAttribute("aria-modal", "true");
    modal.innerHTML =
      '<header class="modal-head"><div><h2>' + (editInterface ? "编辑镜像策略" : "添加镜像策略") + '</h2>' +
      '<p>源口流量复制到镜像口；保存后立即下发 SPAN</p></div>' +
      '<button class="icon-btn" type="button" data-close aria-label="关闭" title="关闭">' + iconClose + '</button></header>' +
      '<div class="modal-body"><div class="policy-form">' +
      '<section><h3>镜像规则</h3><div class="form-grid">' +
      '  <label class="field"><span class="field-label">源接口</span>' +
      '    <select class="select" data-source></select>' +
      '    <span class="field-hint">每个源接口一条策略；管理口和镜像口不在列表中</span></label>' +
      '  <label class="field"><span class="field-label">方向</span><select class="select" data-direction>' +
      '    <option value="both">双向</option><option value="rx">仅接收</option><option value="tx">仅发送</option></select></label>' +
      '  <label class="field"><span class="field-label">输出口</span><select class="select" data-output></select>' +
      '    <span class="field-hint">只列出角色为「镜像口」的端口</span></label>' +
      '</div></section></div></div>' +
      '<footer class="modal-footer"><button class="btn btn-secondary" type="button" data-close>取消</button>' +
      '<button class="btn btn-primary" type="button" data-save>保存策略</button></footer>';
    backdrop.appendChild(modal);
    document.body.appendChild(backdrop);

    var source = modal.querySelector("[data-source]");
    var options = candidates();
    if (!options.length) {
      var none = document.createElement("option");
      none.value = "";
      none.textContent = "没有可用的物理数据口";
      source.appendChild(none);
      source.disabled = true;
    }
    options.forEach(function (item) {
      var option = document.createElement("option");
      option.value = item.id;
      option.textContent = portLabel(item);
      source.appendChild(option);
    });
    if (editSource) {
      source.value = editSource.interface;
      if (!source.value) {
        var current = document.createElement("option");
        current.value = editSource.interface;
        current.textContent = portLabel(portByReference(editSource.interface) || { id: editSource.interface });
        source.appendChild(current);
        source.value = editSource.interface;
      }
      source.disabled = true;
      source.title = "源接口是这条策略的主键，编辑时不可修改";
    } else if (options.length) {
      var used = sources().map(function (item) { return item.interface; });
      var firstFree = options.find(function (item) { return used.indexOf(item.id) === -1; });
      source.value = (firstFree || options[0]).id;
    }

    var direction = modal.querySelector("[data-direction]");
    direction.value = editSource ? (editSource.direction || "both") : "both";

    var output = modal.querySelector("[data-output]");
    var port = mirrorPort();
    state.ports.filter(function (item) { return item.mirror === true && isPhysical(item); }).forEach(function (item) {
      var option = document.createElement("option");
      option.value = item.id;
      option.textContent = portLabel(item);
      output.appendChild(option);
    });
    output.value = port ? port.id : "";

    modal.querySelectorAll("[data-close]").forEach(function (node) {
      node.addEventListener("click", closeDialog);
    });
    backdrop.addEventListener("mousedown", function (event) {
      if (event.target === backdrop) closeDialog();
    });
    var onKey = function (event) {
      if (event.key === "Escape") { closeDialog(); document.removeEventListener("keydown", onKey); }
    };
    document.addEventListener("keydown", onKey);

    modal.querySelector("[data-save]").addEventListener("click", function () {
      var id = source.value;
      if (!id) { showDialogError(modal, "请选择一个源接口"); return; }
      saveSource(id, direction.value, modal);
    });
  }

  function setBusy(busy, modal) {
    state.busy = busy;
    if (modal) {
      var save = modal.querySelector("[data-save]");
      if (save) {
        save.disabled = busy;
        save.textContent = busy ? "应用中…" : "保存策略";
      }
    }
    renderTable();
    syncAddButton();
  }

  function waitForApply() {
    var tries = 0;
    return new Promise(function (resolve) {
      var timer = setInterval(function () {
        tries += 1;
        api("/api/v1/runtime/status").then(function (status) {
          var components = (status && status.components) || [];
          var component = components.find(function (item) { return item.name === "mirror"; });
          if ((component && component.state === "running") || tries >= 40) {
            clearInterval(timer);
            resolve();
          }
        }).catch(function () {
          if (tries >= 40) { clearInterval(timer); resolve(); }
        });
      }, 700);
    });
  }

  function delay(ms) {
    return new Promise(function (resolve) { setTimeout(resolve, ms); });
  }

  function runtimeIdle() {
    return api("/api/v1/runtime/status").then(function (status) {
      var components = (status && status.components) || [];
      return !components.some(function (item) {
        return /applying|recovering|pending/.test(String(item.state));
      });
    }).catch(function () { return true; });
  }

  function waitRuntimeIdle(tries) {
    return runtimeIdle().then(function (idle) {
      if (idle || tries <= 0) return null;
      return delay(800).then(function () { return waitRuntimeIdle(tries - 1); });
    });
  }

  function apply(tries) {
    var attempt = tries || 0;
    return api("/api/v1/runtime/apply", { method: "POST", body: {} }).then(function (result) {
      var status = result && (result.status || result.runtime_state);
      var text = String(status || "");
      if (/apply_in_progress|applying/i.test(text)) {
        // Another apply owns the device; our PATCH is newer than that
        // generation, so re-issue instead of waiting on the stale one.
        if (attempt >= 20) throw new Error("设备仍在处理上一次下发，请稍后重试");
        var wait = Math.max(1, Number(result.retry_after_seconds) || 1) * 1000;
        return delay(wait).then(function () { return apply(attempt + 1); });
      }
      if (status && /fail|error|locked/.test(text)) {
        throw new Error(result.reason || ("应用失败：" + status));
      }
      return waitForApply();
    });
  }

  function patchSources(next) {
    var port = mirrorPort();
    if (!port) throw new Error("请先在网卡设置里配置镜像口");
    return api("/api/v1/interfaces/" + encodeURIComponent(port.id), {
      method: "PATCH",
      body: { id: port.id, name: port.name || port.id, mirror: true, mirror_sources: next }
    });
  }

  function nextSources(interfaceID, direction) {
    var list = sources().map(function (source) {
      return { interface: source.interface, direction: source.direction || "both" };
    });
    var found = list.find(function (item) { return item.interface === interfaceID; });
    if (found) found.direction = direction;
    else list.push({ interface: interfaceID, direction: direction });
    return list;
  }

  function saveSource(interfaceID, direction, modal) {
    setBusy(true, modal);
    showError("");
    waitRuntimeIdle(15).then(function () {
      return patchSources(nextSources(interfaceID, direction));
    }).then(function () {
      // The row is already saved on the device: close the dialog and show it
      // as 未生效 instead of blocking the operator for the whole apply.
      closeDialog();
      return load();
    }).then(function () {
      return apply();
    }).then(function () {
      return load();
    }).catch(function (error) {
      showError(error.message || String(error));
    }).then(function () { setBusy(false, modal); });
  }

  function removeSource(interfaceID) {
    if (state.busy) return;
    var next = sources().filter(function (source) { return source.interface !== interfaceID; }).map(function (source) {
      return { interface: source.interface, direction: source.direction || "both" };
    });
    setBusy(true);
    showError("");
    waitRuntimeIdle(15).then(function () {
      return patchSources(next);
    }).then(function () {
      return load();
    }).then(function () {
      return apply();
    }).then(function () {
      return load();
    }).catch(function (error) {
      showError(error.message || error);
    }).then(function () { setBusy(false); });
  }

  function load() {
    return Promise.all([
      api("/api/v1/interfaces"),
      api("/api/v1/runtime/status").catch(function () { return null; })
    ]).then(function (results) {
      state.ports = (results[0] && results[0].items) || [];
      var components = (results[1] && results[1].components) || [];
      state.component = components.find(function (component) { return component.name === "mirror"; }) || null;
      state.ready = true;
      renderTable();
      if (root && root.style) root.style.visibility = "";
      syncAddButton();
    }).catch(function (error) {
      showError(error.message || error);
    });
  }

  function syncAddButton() {
    var add = $("add-policy");
    if (!add) return;
    var hasTarget = Boolean(mirrorPort());
    add.disabled = !hasTarget || state.busy;
    add.title = hasTarget
      ? (state.busy ? "正在下发，请稍候" : "添加一条镜像策略")
      : "还没有镜像口：请先在网卡设置里把一个端口设为镜像口";
  }

  function mountHeaderButton() {
    var head = root.querySelector(".page-head");
    if (!head) return;
    var actions = head.querySelector(".page-actions");
    if (!actions) {
      actions = document.createElement("div");
      actions.className = "page-actions";
      head.appendChild(actions);
    }
    if (actions.querySelector("#add-policy")) return;
    var add = document.createElement("button");
    add.id = "add-policy";
    add.className = "btn btn-sm btn-primary";
    add.textContent = "添加镜像策略";
    add.addEventListener("click", function () { openDialog(""); });
    actions.insertBefore(add, actions.firstChild);
  }

  mountHeaderButton();
  // Reveal the page only after the first read has been written, so nothing
  // appears in stages.
  if (root && root.style) root.style.visibility = "hidden";
  window.setTimeout(function () { if (root && root.style) root.style.visibility = ""; }, 4000);
  load();
  return function () { closeDialog(); };
};

if (document.body && document.body.dataset.extrasPage === "mirror") {
  window.LyExtrasMirrorView(document);
}

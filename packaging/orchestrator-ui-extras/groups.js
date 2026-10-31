/* Inline orchestration-group editor for the frozen baseline shell. */
window.LyExtrasGroupsView = function (root) {
  "use strict";
  var state = { topology: null, ports: [], rows: [], busy: false, error: "" };
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
      if (response.status === 401) { window.location.href = "/"; throw new Error("登录状态已失效"); }
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
    state.error = message || "";
    var box = $("error");
    if (!box) return;
    box.hidden = !state.error;
    box.innerHTML = "";
    if (!state.error) return;
    var strong = document.createElement("strong");
    strong.textContent = "保存失败";
    var p = document.createElement("p");
    p.textContent = state.error;
    box.appendChild(strong);
    box.appendChild(p);
  }

  function direction(value) { return String(value || "").trim().toLowerCase().replace(/-/g, "_"); }
  function portOf(group, dir) {
    var port = (group.ports || []).find(function (item) { return direction(item.direction) === dir; });
    return port ? (port.interface || (port.bond && port.bond.name) || "") : "";
  }

  function load() {
    return Promise.all([api("/api/v1/orchestrator/topology"), api("/api/v1/interfaces")]).then(function (results) {
      state.topology = results[0] && results[0].item;
      var management = state.topology ? state.topology.management_interface : "";
      state.ports = ((results[1] && results[1].items) || []).filter(function (item) {
        return item.system_name && item.system_name !== management &&
          item.takeover_state !== "management" && item.gateway_role !== "management";
      });
      state.rows = ((state.topology && state.topology.orchestration_groups) || []).map(function (group) {
        return { name: group.name, lan: portOf(group, "lan_facing"), wan: portOf(group, "wan_facing") };
      });
      render();
      showError("");
    }).catch(function (error) { showError(error.message || error); });
  }

  function portLabel(item) {
    var name = item.name || item.id;
    return item.system_name && item.system_name !== name ? name + " · " + item.system_name : name;
  }

  function portOptions(selected) {
    var values = state.ports.map(function (item) { return { value: item.system_name, label: portLabel(item) }; });
    if (selected && !values.some(function (item) { return item.value === selected; })) values.push({ value: selected, label: selected });
    return values;
  }

  function field(label, control) {
    var wrap = document.createElement("div");
    wrap.className = "field";
    var span = document.createElement("span");
    span.className = "field-label";
    span.textContent = label;
    wrap.appendChild(span);
    wrap.appendChild(control);
    return wrap;
  }

  function selectPorts(selected, rowIndex, key) {
    var select = document.createElement("select");
    select.className = "select";
    var none = document.createElement("option");
    none.value = "";
    none.textContent = "请选择";
    select.appendChild(none);
    portOptions(selected).forEach(function (item) {
      var option = document.createElement("option");
      option.value = item.value;
      option.textContent = item.label;
      select.appendChild(option);
    });
    select.value = selected || "";
    select.addEventListener("change", function () {
      state.rows[rowIndex][key] = select.value;
    });
    return select;
  }

  function render() {
    var container = $("groups");
    if (!container) return;
    container.innerHTML = "";
    if (!state.rows.length) {
      var empty = document.createElement("div");
      empty.className = "group-empty";
      empty.textContent = "暂无编排组。没有编排组时只能做 LAN/WAN 直通策略。";
      container.appendChild(empty);
    }
    state.rows.forEach(function (row, index) {
      var line = document.createElement("div");
      line.className = "group-row";
      var name = document.createElement("input");
      name.className = "input";
      name.value = row.name;
      name.addEventListener("input", function () { row.name = name.value; });
      var remove = document.createElement("button");
      remove.className = "btn btn-sm btn-danger";
      remove.textContent = "移除";
      remove.addEventListener("click", function () {
        state.rows.splice(index, 1);
        render();
      });
      line.appendChild(field("组名称", name));
      line.appendChild(field("LAN-facing 端口", selectPorts(row.lan, index, "lan")));
      line.appendChild(field("WAN-facing 端口", selectPorts(row.wan, index, "wan")));
      line.appendChild(remove);
      container.appendChild(line);
    });
  }

  function save() {
    if (!state.topology) { showError("拓扑未加载"); return; }
    var names = {};
    for (var i = 0; i < state.rows.length; i += 1) {
      var row = state.rows[i];
      row.name = String(row.name || "").trim();
      if (!row.name) { showError("请填写每个编排组的名称"); return; }
      if (names[row.name]) { showError("编排组名称重复：" + row.name); return; }
      names[row.name] = true;
      if (!row.lan || !row.wan) { showError("编排组 " + row.name + " 需要 LAN-facing 与 WAN-facing 端口"); return; }
      if (row.lan === row.wan) { showError("编排组 " + row.name + " 的 LAN/WAN 端口不能相同"); return; }
    }
    var body = {
      schema_version: state.topology.schema_version || 1,
      management_interface: state.topology.management_interface,
      interfaces: (state.topology.interfaces || []).map(function (item) {
        return item.bond && item.bond.members && item.bond.members.length
          ? { name: item.name, role: item.role, bond: item.bond }
          : { name: item.name, role: item.role, port: item.port };
      }),
      port_roles: state.topology.port_roles || {},
      orchestration_groups: state.rows.map(function (row) {
        return { name: row.name, ports: [
          { interface: row.lan, direction: "lan_facing" },
          { interface: row.wan, direction: "wan_facing" }
        ] };
      })
    };
    setBusy(true);
    showError("");
    api("/api/v1/orchestrator/topology", { method: "PUT", body: body }).then(function () {
      return load();
    }).catch(function (error) {
      showError(error.message || error);
    }).then(function () { setBusy(false); });
  }

  function setBusy(busy) {
    state.busy = busy;
    ["save-groups", "refresh", "add-group"].forEach(function (id) {
      var button = $(id);
      if (button) button.disabled = busy;
    });
    var save = $("save-groups");
    if (save) save.textContent = busy ? "保存中…" : "保存编排组";
  }

  $("add-group").addEventListener("click", function () {
    state.rows.push({ name: "grp-" + (state.rows.length + 1), lan: "", wan: "" });
    render();
  });
  $("save-groups").addEventListener("click", save);
  $("refresh").addEventListener("click", function () { load(); });

  load();
  return function () {};
};

if (document.body && document.body.dataset.extrasPage === "groups") {
  window.LyExtrasGroupsView(document);
}

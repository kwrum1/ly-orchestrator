/* Per-group fault bypass page for the frozen orchestrator UI. */
(function () {
  "use strict";

  var state = { data: null, edits: {}, dirty: false, busy: false };
  var $ = function (id) { return document.getElementById(id); };

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

  function healthLabel(health) {
    switch (health) {
      case "healthy": return "正常";
      case "faulty": return "故障";
      case "bypass": return "旁路中";
      default: return "无流量 / 未知";
    }
  }

  function healthTone(health) {
    switch (health) {
      case "healthy": return "badge-success";
      case "faulty": return "badge-warning";
      case "bypass": return "badge-danger";
      default: return "badge-neutral";
    }
  }

  function flowText(targets) {
    return (targets || []).map(function (target) {
      return target.dst_ip + ":" + target.dst_port + "/" + (target.protocol || "tcp");
    }).join("\n");
  }

  function tcpText(targets) {
    return (targets || []).map(function (target) {
      return target.source ? target.target + " @ " + target.source : target.target;
    }).join("\n");
  }

  function parseFlow(text) {
    var result = [];
    String(text || "").split(/\r?\n/).forEach(function (raw) {
      var line = raw.trim();
      if (!line) return;
      var match = line.match(/^([0-9a-fA-F:.]+):(\d{1,5})(?:\/(tcp|udp))?$/);
      if (!match) throw new Error("无法解析被动探测目标：" + line + "（格式 ip:port/tcp 或 ip:port/udp）");
      var port = Number(match[2]);
      if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error("端口超出范围：" + line);
      result.push({ dst_ip: match[1], dst_port: port, protocol: match[3] || "tcp" });
    });
    return result;
  }

  function parseTCP(text) {
    var result = [];
    String(text || "").split(/\r?\n/).forEach(function (raw) {
      var line = raw.trim();
      if (!line) return;
      var parts = line.split("@");
      var target = parts[0].trim();
      var source = parts.length > 1 ? parts[1].trim() : "";
      if (!/^[^\s]+:\d{1,5}$/.test(target)) throw new Error("无法解析主动探测目标：" + line + "（格式 host:port 或 host:port @ source）");
      result.push(source ? { target: target, source: source } : { target: target });
    });
    return result;
  }

  function seedEdits() {
    var config = (state.data && state.data.config) || {};
    var edits = {};
    (groups() || []).forEach(function (group) {
      var stored = (config.groups || []).find(function (item) { return item.name === group.group; }) || {};
      edits[group.group] = {
        window: String(stored.window_seconds || group.window_seconds || 10),
        flow: flowText(stored.flow_targets || group.flow_targets),
        tcp: tcpText(stored.tcp_targets || group.tcp_targets),
        interfaces: (stored.interfaces || group.interfaces || []).join(", ")
      };
    });
    state.edits = edits;
    state.dirty = false;
  }

  function groups() {
    return (state.data && state.data.groups) || [];
  }

  function renderAggregate() {
    var data = state.data || {};
    var badge = $("aggregate-badge");
    if (data.enabled) {
      badge.className = "badge badge-danger";
      badge.textContent = data.forced ? "手工旁路中" : "有组处于旁路";
    } else {
      badge.className = "badge badge-success";
      badge.textContent = "策略强制中";
    }
    var box = $("aggregate");
    box.innerHTML = "";
    [
      ["策略代", data.generation || "未知"],
      ["旁路包数", String(data.bypass_packets || 0)],
      ["自动探测", data.config && data.config.enabled ? "开启" : "关闭"],
      ["原因", data.reason || "—"]
    ].forEach(function (pair) {
      var span = document.createElement("span");
      span.innerHTML = "";
      var label = document.createTextNode(pair[0] + " ");
      var value = document.createElement("b");
      value.textContent = pair[1];
      span.appendChild(label);
      span.appendChild(value);
      box.appendChild(span);
    });
  }

  function field(label, input, hint) {
    var wrap = document.createElement("div");
    wrap.className = "field";
    var span = document.createElement("span");
    span.className = "field-label";
    span.textContent = label;
    wrap.appendChild(span);
    wrap.appendChild(input);
    if (hint) {
      var h = document.createElement("span");
      h.className = "field-hint";
      h.textContent = hint;
      wrap.appendChild(h);
    }
    return wrap;
  }

  function renderGroups() {
    var container = $("groups");
    container.innerHTML = "";
    if (!groups().length) {
      var empty = document.createElement("section");
      empty.className = "card";
      empty.innerHTML = '<div class="empty-state"><strong>拓扑中还没有编排组</strong><p>先保存带编排组的拓扑，故障旁路会为每个组分别检测。</p></div>';
      container.appendChild(empty);
      return;
    }

    groups().forEach(function (group) {
      var edit = state.edits[group.group] || { window: "10", flow: "", tcp: "" };
      var card = document.createElement("section");
      card.className = "card group-card";

      var head = document.createElement("div");
      head.className = "card-head";
      var title = document.createElement("div");
      title.className = "card-title";
      var h2 = document.createElement("h2");
      h2.textContent = group.group;
      var sub = document.createElement("p");
      sub.textContent = "窗口 " + (group.window_seconds || edit.window) + "s · 请求 " + (group.flow_attempts || 0) +
        " · 成功 " + (group.flow_success || 0) + " · RST " + (group.flow_rst || 0) + " · 超时 " + (group.flow_timeouts || 0);
      title.appendChild(h2);
      title.appendChild(sub);
      var actions = document.createElement("div");
      actions.className = "group-head-actions";
      var health = document.createElement("span");
      health.className = "badge " + healthTone(group.health);
      health.textContent = healthLabel(group.health);
      actions.appendChild(health);
      if (group.enabled) {
        var bypass = document.createElement("span");
        bypass.className = "badge badge-danger";
        bypass.textContent = "旁路中";
        actions.appendChild(bypass);
      }
      if (group.forced) {
        var forced = document.createElement("span");
        forced.className = "badge badge-warning";
        forced.textContent = "手工强制";
        actions.appendChild(forced);
      }
      var toggle = document.createElement("button");
      toggle.className = "btn btn-sm " + (group.enabled ? "btn-primary" : "btn-danger");
      toggle.textContent = group.enabled ? "恢复该组策略" : "强制该组旁路";
      toggle.addEventListener("click", function () {
        if (state.busy) return;
        setBusy(true);
        showError("");
        api("/api/v1/orchestrator/bypass", {
          method: "POST",
          body: { group: group.group, enabled: !group.enabled, reason: "ui manual" }
        }).then(function (result) {
          state.data = result;
          seedEdits();
          render();
        }).catch(function (error) {
          showError(error.message || error);
        }).then(function () { setBusy(false); });
      });
      actions.appendChild(toggle);
      head.appendChild(title);
      head.appendChild(actions);

      var body = document.createElement("div");
      body.className = "card-body";

      if (group.reason || group.cause) {
        var reason = document.createElement("div");
        reason.className = group.enabled ? "inline-warning" : "notice-block";
        reason.innerHTML = "";
        var rTitle = document.createElement("strong");
        rTitle.textContent = group.enabled ? "当前旁路原因" : "最近状态";
        var rText = document.createElement("span");
        rText.textContent = (group.reason || group.cause) + (group.since ? " · 自 " + group.since : "");
        reason.appendChild(rTitle);
        reason.appendChild(rText);
        body.appendChild(reason);
      }

      var kv = document.createElement("div");
      kv.className = "kv";
      [
        ["失败/成功轮数", (group.consecutive_failures || 0) + " / " + (group.consecutive_successes || 0)],
        ["旁路包数", String(group.bypass_packets || 0)],
        ["探测网口", (group.interfaces || []).join(", ") || "默认取组端口"]
      ].forEach(function (pair) {
        var span = document.createElement("span");
        span.appendChild(document.createTextNode(pair[0] + " "));
        var b = document.createElement("b");
        b.textContent = pair[1];
        span.appendChild(b);
        kv.appendChild(span);
      });
      body.appendChild(kv);

      var windowInput = document.createElement("input");
      windowInput.className = "input";
      windowInput.value = edit.window;
      windowInput.addEventListener("input", function () { edit.window = windowInput.value; state.dirty = true; });

      var flow = document.createElement("textarea");
      flow.className = "textarea";
      flow.rows = 3;
      flow.value = edit.flow;
      flow.placeholder = "192.168.10.82:80/tcp\n192.168.10.82:53/udp";
      flow.addEventListener("input", function () { edit.flow = flow.value; state.dirty = true; });

      var tcp = document.createElement("textarea");
      tcp.className = "textarea";
      tcp.rows = 2;
      tcp.value = edit.tcp;
      tcp.placeholder = "10.1.18.10:443";
      tcp.addEventListener("input", function () { edit.tcp = tcp.value; state.dirty = true; });

      var fields = document.createElement("div");
      fields.className = "form-grid";
      fields.appendChild(field("探测窗口（秒）", windowInput));
      fields.appendChild(field("探测网口（逗号分隔，可留空）", (function () {
        var input = document.createElement("input");
        input.className = "input";
        input.value = edit.interfaces !== undefined ? edit.interfaces : (group.interfaces || []).join(", ");
        input.addEventListener("input", function () { edit.interfaces = input.value; state.dirty = true; });
        return input;
      })()));
      body.appendChild(fields);
      body.appendChild(field("被动探测目标", flow, "每行一个 ip:port/protocol；仅用于请求/响应型业务"));
      body.appendChild(field("主动 TCP 探测（可选）", tcp, "每行 host:port，或 host:port @ source"));

      card.appendChild(head);
      card.appendChild(body);
      container.appendChild(card);
    });
  }

  function setBusy(busy) {
    state.busy = busy;
    $("save-config").disabled = busy;
    $("refresh").disabled = busy;
    $("save-config").textContent = busy ? "保存中…" : "保存探测配置";
  }

  function render() {
    renderAggregate();
    renderGroups();
  }

  function load() {
    return api("/api/v1/orchestrator/bypass").then(function (data) {
      state.data = data;
      seedEdits();
      render();
    }).catch(function (error) {
      showError(error.message || error);
    });
  }

  function saveConfig() {
    if (!state.data) return;
    setBusy(true);
    showError("");
    try {
      var config = JSON.parse(JSON.stringify(state.data.config || {}));
      var existing = config.groups || [];
      config.groups = groups().map(function (group) {
        var edit = state.edits[group.group] || {};
        var stored = existing.find(function (item) { return item.name === group.group; }) || {};
        var windowSeconds = Number(edit.window);
        if (!Number.isFinite(windowSeconds) || windowSeconds < 1) throw new Error("探测窗口必须是 ≥ 1 的数字");
        var next = Object.assign({}, stored, {
          name: group.group,
          window_seconds: windowSeconds,
          flow_targets: parseFlow(edit.flow),
          tcp_targets: parseTCP(edit.tcp)
        });
        if (edit.interfaces !== undefined) {
          next.interfaces = String(edit.interfaces).split(",").map(function (item) { return item.trim(); }).filter(Boolean);
        }
        return next;
      });
      api("/api/v1/orchestrator/bypass/config", { method: "PUT", body: config }).then(function (result) {
        state.data = result;
        seedEdits();
        render();
        showError("");
      }).catch(function (error) {
        showError(error.message || error);
      }).then(function () { setBusy(false); });
    } catch (error) {
      setBusy(false);
      showError(error.message || error);
    }
  }

  $("save-config").addEventListener("click", saveConfig);
  $("refresh").addEventListener("click", function () { load(); });

  // Poll status, but never overwrite an operator's unsaved edits.
  setInterval(function () {
    if (state.dirty || state.busy || document.hidden) return;
    api("/api/v1/orchestrator/bypass").then(function (data) {
      state.data = data;
      seedEdits();
      render();
    }).catch(function () { /* keep the last view */ });
  }, 10000);

  load();
})();

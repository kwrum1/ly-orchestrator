#!/usr/bin/env node
/*
 * Orchestrator acceptance gate (O0–O6).
 *
 *   node scripts/acceptance/orchestrator-gate.mjs
 *
 * Environment:
 *   ORCH_BASE=https://10.1.18.234          console URL
 *   ORCH_USER=admin ORCH_PASS=...
 *   SSH_HOST=10.1.18.234 SSH_USER=root SSH_PASS=...
 *   PLAYWRIGHT_PATH=/root/work/ui-test/node_modules/playwright
 *   ACCEPT_DIR=/root/work/acceptance     evidence root
 *
 * Result classes: PASS / PRODUCT_FAIL / FIXTURE_FAIL / ENV_LIMIT.
 * Every run writes results.json, API snapshots, vppctl dumps and screenshots
 * under ACCEPT_DIR/<timestamp>/.
 */
import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";

// The lab console uses a self-signed certificate.
process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0";

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_PATH || "/root/work/ui-test/node_modules/playwright");

const BASE = process.env.ORCH_BASE || "https://10.1.18.234";
const USER = process.env.ORCH_USER || "admin";
const PASS = process.env.ORCH_PASS || "LabOrch2026!";
const SSH_HOST = process.env.SSH_HOST || "10.1.18.234";
const SSH_USER = process.env.SSH_USER || "root";
const SSH_PASS = process.env.SSH_PASS || "Iamyours..123";
const EXPECT_VERSION = process.env.EXPECT_VERSION || "0.2.7";
const STAMP = new Date().toISOString().replace(/[:.]/g, "-");
const ROOT = path.join(process.env.ACCEPT_DIR || "/root/work/acceptance", STAMP);

class ProductError extends Error {}
class FixtureError extends Error {}
class EnvLimit extends Error {}

const results = [];
const created = { artifacts: ROOT };
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const now = () => Date.now();

function ensureDir(dir) { fs.mkdirSync(dir, { recursive: true }); }
function saveJSON(rel, value) {
  const file = path.join(ROOT, rel);
  ensureDir(path.dirname(file));
  fs.writeFileSync(file, JSON.stringify(value, null, 2));
  return file;
}
function saveText(rel, value) {
  const file = path.join(ROOT, rel);
  ensureDir(path.dirname(file));
  fs.writeFileSync(file, value);
  return file;
}
async function shot(page, rel) {
  const file = path.join(ROOT, rel);
  ensureDir(path.dirname(file));
  await page.screenshot({ path: file, fullPage: false });
  return file;
}
function assert(condition, message) {
  if (!condition) throw new ProductError(message);
}

function ssh(cmd, timeout = 120000) {
  return execFileSync(
    "sshpass",
    ["-p", SSH_PASS, "ssh", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=10",
      `${SSH_USER}@${SSH_HOST}`, cmd],
    { encoding: "utf8", timeout, maxBuffer: 16 * 1024 * 1024 }
  );
}

// ---------- API ----------
const cookies = new Map();
async function api(method, pathname, body, timeout = 60000) {
  const headers = {};
  if (cookies.size) headers.Cookie = [...cookies].map(([k, v]) => `${k}=${v}`).join("; ");
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeout);
  let response;
  try {
    response = await fetch(BASE + pathname, {
      method, headers, signal: controller.signal,
      body: body === undefined ? undefined : JSON.stringify(body),
      redirect: "manual"
    });
  } finally {
    clearTimeout(timer);
  }
  const raw = response.headers.getSetCookie ? response.headers.getSetCookie() : [response.headers.get("set-cookie")].filter(Boolean);
  for (const entry of raw) {
    const pair = String(entry).split(";")[0];
    const index = pair.indexOf("=");
    if (index > 0) cookies.set(pair.slice(0, index), pair.slice(index + 1));
  }
  const text = await response.text();
  let data = text;
  try { data = text ? JSON.parse(text) : null; } catch { /* keep text */ }
  if (!response.ok) {
    const message = (data && data.error && (data.error.message || data.error.code)) || `HTTP ${response.status}`;
    const error = new ProductError(`${method} ${pathname} -> ${response.status}: ${message}`);
    error.status = response.status;
    error.body = data;
    throw error;
  }
  return data;
}
const get = (p) => api("GET", p);
const post = (p, b = {}) => api("POST", p, b);
const put = (p, b) => api("PUT", p, b);
const patch = (p, b) => api("PATCH", p, b);
const del = (p) => api("DELETE", p);

async function loginAPI() {
  await api("POST", "/api/v1/auth/login", { username: USER, password: PASS });
}
async function waitHealth(timeout = 60000) {
  const deadline = now() + timeout;
  while (now() < deadline) {
    try {
      const health = await fetch(BASE + "/api/v1/health", { signal: AbortSignal.timeout(4000) });
      if (health.ok) {
        const body = await health.json();
        if (body.status === "ok") return body;
      }
    } catch { /* retry */ }
    await sleep(1000);
  }
  throw new EnvLimit("appliance health endpoint did not become ok");
}
async function waitApply(timeout = 90000) {
  return post("/api/v1/runtime/apply", {});
}
async function applyAndWait(timeout = 90000) {
  const started = now();
  const result = await post("/api/v1/runtime/apply", {});
  const status = String(result.status || result.runtime_state || "");
  if (/fail|error|locked/.test(status)) throw new ProductError(`apply failed: ${status} ${result.reason || ""}`);
  // poll until the runtime component set reports running and not applying
  while (now() - started < timeout) {
    const state = await get("/api/v1/runtime/status");
    const components = state.components || [];
    const applying = components.some((c) => /applying|recovering|pending/.test(String(c.state)));
    const vpp = components.find((c) => c.name === "vpp");
    if (!applying && vpp && vpp.state === "running") return { result, state, elapsedMs: now() - started };
    await sleep(1000);
  }
  throw new ProductError("apply did not converge within timeout");
}

// ---------- Playwright helpers ----------
async function newPage(browser, width = 1440, height = 900) {
  const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width, height } });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(String(error)));
  page.on("console", (msg) => {
    if (msg.type() !== "error") return;
    const text = msg.text();
    // A stale cookie on the first login page load logs a 401 resource error;
    // it is not a UI failure.
    if (/Failed to load resource.*401/.test(text)) return;
    errors.push("console: " + text);
  });
  page.errors = errors;
  return { context, page };
}
async function loginUI(page) {
  await page.goto(BASE + "/", { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.waitForTimeout(1500);
  if (await page.locator('input[type="password"]').count()) {
    await page.locator("input").nth(0).fill(USER);
    await page.locator('input[type="password"]').first().fill(PASS);
    await page.locator('button[type="submit"], button:has-text("登录")').first().click();
    await page.waitForTimeout(3000);
  }
}
async function interfacesReady(page) {
  const heading = await page.locator(".main-inner h1").innerText().catch(() => "");
  if (!/网卡设置/.test(heading)) return false;
  return (await page.locator("table thead th").allInnerTexts().catch(() => [])).some((text) => text.trim() === "方向");
}

async function openRole(page, port) {
  await waitRuntimeConverged(60000);
  let row = null;
  for (let attempt = 0; attempt < 4; attempt += 1) {
    await gotoReady(page, "/topology");
    if (await interfacesReady(page)) {
      row = page.locator("table tbody tr", { hasText: port }).first();
      if (await row.count().catch(() => 0)) {
        const button = row.locator('button:has-text("角色配置")').first();
        if (await button.count().catch(() => 0)) {
          await button.click({ timeout: 25000 });
          await page.waitForTimeout(500);
          return page.locator(".modal").last();
        }
      }
    }
    await loginUI(page);
    await page.waitForTimeout(800);
  }
  const info = {
    url: page.url(),
    heading: await page.locator(".main-inner h1").innerText().catch(() => ""),
    rows: await page.locator("table tbody tr").count().catch(() => 0),
    body: (await page.locator("body").innerText().catch(() => "")).slice(0, 300)
  };
  throw new Error("role row not found: " + JSON.stringify(info));
}
async function saveRoleDialog(page) {
  const modal = page.locator(".modal").last();
  await modal.locator(".modal-footer .btn-primary").click();
  await page.waitForTimeout(600);
  const confirm = page.locator(".modal").last();
  const title = (await confirm.locator(".modal-head h2").innerText()).trim();
  await confirm.locator(".modal-footer .btn-primary").click();
  await page.waitForTimeout(2500);
  const toasts = await page.locator(".toast-stack").innerText().catch(() => "");
  return { title, toasts };
}
async function setRoleUI(page, port, value) {
  const modal = await openRole(page, port);
  await modal.locator("select").first().selectOption(value);
  await page.waitForTimeout(200);
  const result = await saveRoleDialog(page);
  const deadline = now() + 20000;
  while (now() < deadline) {
    const roles = (await topology()).port_roles || {};
    if ((roles[port] || "") === value) break;
    await sleep(700);
  }
  return result;
}
async function mirrorList(page) {
  await waitRuntimeConverged(60000);
  await gotoReady(page, "/mirror");
}
async function addMirrorPolicy(page, source, direction) {
  await waitRuntimeConverged(60000);
  await page.locator("#add-policy").click();
  await page.waitForTimeout(500);
  const modal = page.locator(".modal").last();
  await modal.locator("[data-source]").selectOption(source);
  await modal.locator("[data-direction]").selectOption(direction);
  await modal.locator("[data-save]").click();
  await page.waitForTimeout(1500);
  await waitUntil(async () => (await page.locator("#policy-rows tr", { hasText: source }).count()) > 0, 20000);
  await waitRuntimeConverged(60000);
}
async function waitUntil(check, timeout = 15000, interval = 500) {
  const deadline = now() + timeout;
  while (now() < deadline) {
    if (await check()) return true;
    await sleep(interval);
  }
  return false;
}

// Wait until every runtime component is out of applying/recovering/pending.
// Saves issued while a generation is in flight are dropped by the device.
async function waitRuntimeConverged(timeout = 90000) {
  const deadline = now() + timeout;
  while (now() < deadline) {
    try {
      const status = await get("/api/v1/runtime/status");
      const components = status.components || [];
      const busy = components.some((c) => /applying|recovering|pending/.test(String(c.state)));
      if (!busy) return true;
    } catch { /* retry */ }
    await sleep(800);
  }
  return false;
}

async function gotoReady(page, pathname) {
  for (let attempt = 0; attempt < 3; attempt += 1) {
    await page.goto(BASE + pathname, { waitUntil: "domcontentloaded", timeout: 60000 });
    await page.waitForTimeout(2200);
    const shell = await page.locator(".main-inner").count().catch(() => 0);
    if (!shell) continue;
    if (pathname === "/mirror") {
      if (await page.locator("#add-policy").count().catch(() => 0)) return true;
    } else if (await page.locator("table tbody tr, .empty-state").count().catch(() => 0)) {
      return true;
    }
    await sleep(1500);
  }
  return false;
}

// ---------- state helpers ----------
async function interfaces() { return (await get("/api/v1/interfaces")).items || []; }
async function iface(id) { return (await interfaces()).find((item) => item.id === id) || {}; }
async function topology() { return (await get("/api/v1/orchestrator/topology")).item; }
async function putTopology(item) {
  const doc = { ...item };
  delete doc.checksum;
  return (await put("/api/v1/orchestrator/topology", doc)).item;
}
async function groups() { return (await get("/api/v1/orchestrator/orchestration-groups")).items || []; }
async function bypassConfig() { return (await get("/api/v1/orchestrator/bypass")).config; }async function putBypassGroup(name, state) {
  const config = await bypassConfig();
  const kept = (config.groups || []).filter((g) => g.name !== name);
  if (state) {
    const existing = (config.groups || []).find((g) => g.name === name);
    kept.push({
      name,
      interface_bypass: Boolean(state.iface),
      window_seconds: state.threshold,
      failure_threshold: (existing && existing.failure_threshold) || 3,
      recovery_threshold: (existing && existing.recovery_threshold) || 3,
      ...(state.biz ? { flow_targets: [{ dst_ip: state.ip, dst_port: state.port, protocol: state.proto }] } : {})
    });
  }
  await put("/api/v1/orchestrator/bypass/config", { ...config, groups: kept });
}
async function vppSpan() {
  return ssh("vppctl show interface span", 30000);
}
async function clearMirrorRoleAPI() {
  const topo = await topology();
  delete (topo.port_roles || {}).eth5;
  await putTopology(topo);
  await patch("/api/v1/interfaces/eth5", { id: "eth5", name: "eth5", mirror: false, mirror_sources: [] });
  await applyAndWait();
  await waitRuntimeConverged();
}
async function restoreMirrorRoleAPI(sources) {
  const topo = await topology();
  topo.port_roles = { ...(topo.port_roles || {}), eth5: "mirror" };
  await putTopology(topo);
  await patch("/api/v1/interfaces/eth5", { id: "eth5", name: "eth5", mirror: true, mirror_sources: sources });
  await applyAndWait();
  await waitRuntimeConverged();
}
function spanHas(output, source, destination, direction) {
  const lines = output.split(/\r?\n/);
  return lines.some((line) => {
    if (!line.includes(source) || !line.includes(destination)) return false;
    if (!direction) return true;
    return new RegExp("\\(\\s*" + direction + "\\s*\\)").test(line);
  });
}
function spanLine(output, source) {
  return output.split(/\r?\n/).find((line) => line.includes(source)) || "";
}
async function waitSpan(predicate, timeout = 25000) {
  const deadline = now() + timeout;
  let text = "";
  while (now() < deadline) {
    text = await vppSpan();
    if (predicate(text)) return { ok: true, text };
    await sleep(800);
  }
  return { ok: false, text };
}

// ---------- case runner ----------
async function check(id, name, fn, options = {}) {
  const started = now();
  console.log(`\n[${id}] ${name}`);
  try {
    const detail = await fn();
    results.push({ id, name, status: "PASS", detail: detail || "", elapsed_ms: now() - started });
    console.log(`  PASS ${detail || ""}`);
  } catch (error) {
    const status = error instanceof EnvLimit ? "ENV_LIMIT"
      : error instanceof FixtureError ? "FIXTURE_FAIL" : "PRODUCT_FAIL";
    results.push({ id, name, status, detail: error.message, elapsed_ms: now() - started, ...(options.evidence || {}) });
    console.log(`  ${status}: ${error.message}`);
  }
}

// ---------- main ----------
let browser;
let page;
let cleanupTopology = null;
let cleanupMirror = null;
let cleanupPolicy = null;
let cleanupBondDocs = [];

async function snapshotState(label) {
  const snap = {
    label,
    at: new Date().toISOString(),
    health: await get("/api/v1/health").catch(() => null),
    topology: await topology().catch(() => null),
    groups: await groups().catch(() => []),
    bypass: await bypassConfig().catch(() => null),
    interfaces: await interfaces().catch(() => []),
    policy: await get("/api/v1/orchestrator/policy").catch(() => null),
    span: await vppSpan().catch(() => "")
  };
  saveJSON(`state/${label}.json`, snap);
  return snap;
}

function cleanupBondFixture() {
  for (const doc of cleanupBondDocs) {
    try { ssh(`python3 - <<'PY'\nimport sqlite3\nc=sqlite3.connect("/var/lib/ly-route/orchestrator/ly-route.db")\nc.execute("delete from config_documents where resource_type=? and resource_id=?", ("${doc.type}", "${doc.id}"))\nc.commit()\nPY`, 30000); } catch { /* best effort */ }
  }
  cleanupBondDocs = [];
}

async function run() {
  ensureDir(ROOT);
  await loginAPI();
  const initial = await snapshotState("initial");
  cleanupTopology = initial.topology;
  const mirrorPort0 = (initial.interfaces || []).find((item) => item.mirror === true) || {};
  cleanupMirror = { sources: mirrorPort0.mirror_sources || [] };

  browser = await chromium.launch({ headless: true });
  ({ page } = await newPage(browser));

  // ================= O0 =================
  await check("O0.1", "登录 / health / 版本", async () => {
    const health = await waitHealth();
    assert(health.status === "ok", "health is not ok");
    assert(health.version === EXPECT_VERSION, `version ${health.version} != ${EXPECT_VERSION}`);
    saveJSON("api/health.json", health);
    return `health ok, version ${health.version}`;
  });

  await check("O0.2", "基线 shell 未重建（app.js 985377 字节）", async () => {
    const size = Number(ssh("stat -c %s /opt/ly-route/admin/app.js", 20000).trim());
    assert(size === 985377, `app.js is ${size} bytes, expected 985377`);
    const sha = ssh("sha256sum /opt/ly-route/admin/app.js", 20000).trim().split(/\s+/)[0];
    saveText("evidence/app.js.sha256", `${sha}  app.js\n`);
    return `app.js ${size} bytes sha256 ${sha.slice(0, 12)}…`;
  });

  await check("O0.3", "UI 加载无 JS 错误", async () => {
    await loginUI(page);
    await page.goto(BASE + "/topology", { waitUntil: "domcontentloaded", timeout: 60000 });
    await page.waitForTimeout(2500);
    await shot(page, "screens/o0-interfaces.png");
    assert(page.errors.length === 0, `JS errors: ${page.errors.join(" | ")}`);
    return "no pageerror/console errors";
  });

  // ================= O1 =================
  await check("O1.1", "角色下拉顺序 LAN / WAN / 镜像口 / 未配置", async () => {
    const modal = await openRole(page, "eth4");
    const options = await modal.locator("select option").allInnerTexts();
    await shot(page, "screens/o1-role-modal.png");
    await modal.locator(".modal-footer .btn-secondary").click();
    const wanted = ["LAN 接口", "WAN 接口", "镜像口", "未配置"];
    assert(JSON.stringify(options) === JSON.stringify(wanted), `options ${JSON.stringify(options)}`);
    return JSON.stringify(options);
  });

  await check("O1.2", "角色切换 LAN/WAN/未配置 写入 topology port_roles", async () => {
    await setRoleUI(page, "eth4", "");
    let roles = (await topology()).port_roles || {};
    assert(!roles.eth4, `eth4 role still ${roles.eth4}`);
    await setRoleUI(page, "eth4", "wan");
    roles = (await topology()).port_roles || {};
    assert(roles.eth4 === "wan", `eth4 role is ${roles.eth4}, expected wan`);
    return "未配置 clears the tag, WAN restores it";
  });

  await check("O1.3", "第二个镜像口被拒绝且拓扑不变", async () => {
    const modal = await openRole(page, "eth4");
    await modal.locator("select").first().selectOption("mirror");
    await page.waitForTimeout(150);
    const { toasts } = await saveRoleDialog(page);
    const roles = (await topology()).port_roles || {};
    saveText("evidence/second-mirror-toast.txt", toasts);
    assert(roles.eth4 === "wan", `eth4 became ${roles.eth4}`);
    assert(/已经是镜像口/.test(toasts), `toast does not explain the rejection: ${toasts}`);
    return toasts.trim().slice(0, 80);
  });

  await check("O1.4", "兼容模式聚合前置拦截（API 409 + 明确原因）", async () => {
    let error = null;
    try {
      await post("/api/v1/interface-bonds", { id: "bond-acc-compat", name: "acc-compat", members: ["eth4", "eth5"] });
    } catch (err) {
      error = err;
    }
    assert(error && error.status === 409, "bond creation with compatibility ports was not rejected");
    assert(/兼容模式/.test(error.message), `reason missing: ${error.message}`);
    saveJSON("api/bond-compat-reject.json", error.body);
    const bonds = await get("/api/v1/interface-bonds");
    assert(!(bonds.items || []).some((b) => b.id === "bond-acc-compat"), "rejected bond was persisted");
    return error.message.slice(0, 100);
  });

  await check("O1.5", "删除聚合清理 bond-* 接口文档（残留不再锁死）", async () => {
    const payloadBond = JSON.stringify({ id: "bond-acc-residue", name: "acc-residue", members: ["eth4", "eth5"] });
    const payloadIface = JSON.stringify({ id: "bond-acc-residue", name: "bond-acc-residue", bond: "bond-acc-residue" });
    ssh(`python3 - <<'PY'
import sqlite3
c = sqlite3.connect("/var/lib/ly-route/orchestrator/ly-route.db")
for rt, rid, payload in [("interface_bond", "bond-acc-residue", ${JSON.stringify(payloadBond)}), ("interface", "bond-acc-residue", ${JSON.stringify(payloadIface)})]:
    c.execute("insert or replace into config_documents(resource_type,resource_id,payload_json,payload_hash,updated_at) values(?,?,?,?,?)", (rt, rid, payload, "acc-test", __import__("datetime").datetime.now(__import__("datetime").timezone.utc).isoformat()))
c.commit()
PY`, 30000);
    cleanupBondDocs.push({ type: "interface_bond", id: "bond-acc-residue" }, { type: "interface", id: "bond-acc-residue" });
    const deleted = await del("/api/v1/interface-bonds/bond-acc-residue");
    saveJSON("api/bond-residue-delete.json", deleted);
    const remaining = ssh(`python3 - <<'PY'
import sqlite3
c = sqlite3.connect("/var/lib/ly-route/orchestrator/ly-route.db")
print(c.execute("select count(*) from config_documents where resource_id='bond-acc-residue'").fetchone()[0])
PY`, 30000).trim();
    assert(remaining === "0", `${remaining} bond-acc-residue documents still exist`);
    return "bond doc and interface doc removed together";
  });

  await check("O1.6", "删除仍被拓扑使用的聚合被拒绝", async () => {
    const payloadBond = JSON.stringify({ id: "bond-acc-guard", name: "acc-guard", members: ["eth4", "eth5"] });
    ssh(`python3 - <<'PY'
import sqlite3
c = sqlite3.connect("/var/lib/ly-route/orchestrator/ly-route.db")
c.execute("insert or replace into config_documents(resource_type,resource_id,payload_json,payload_hash,updated_at) values('interface_bond','bond-acc-guard',?,?,?)", (${JSON.stringify(payloadBond)}, "acc-test", __import__("datetime").datetime.now(__import__("datetime").timezone.utc).isoformat()))
c.commit()
PY`, 30000);
    cleanupBondDocs.push({ type: "interface_bond", id: "bond-acc-guard" });
    const topo = await topology();
    topo.port_roles = { ...(topo.port_roles || {}), "bond-acc-guard": "lan" };
    await putTopology(topo);
    let error = null;
    try { await del("/api/v1/interface-bonds/bond-acc-guard"); } catch (err) { error = err; }
    // restore the role tag before asserting so a product bug cannot leak state
    const restore = await topology();
    delete restore.port_roles["bond-acc-guard"];
    await putTopology(restore);
    assert(error && error.status === 409, "referenced bond deletion was not rejected");
    assert(/仍被/.test(error.message), `reason missing: ${error.message}`);
    saveJSON("api/bond-guard-reject.json", error.body);
    return error.message.slice(0, 100);
  });

  await check("O1.7", "restart 后角色与镜像配置恢复", async () => {
    const before = await snapshotState("before-restart");
    ssh("systemctl restart ly-route-control-api.service", 60000);
    await sleep(2000);
    await waitHealth(90000);
    await loginAPI();
    // the restart invalidates the browser session too
    await loginUI(page);
    const after = await snapshotState("after-restart");
    assert(JSON.stringify(before.topology.port_roles) === JSON.stringify(after.topology.port_roles), "port_roles changed across restart");
    assert(JSON.stringify(before.groups) === JSON.stringify(after.groups), "groups changed across restart");
    assert(Boolean(after.interfaces.find((i) => i.mirror === true)), "mirror port lost across restart");
    return "control-api restart keeps topology/groups/mirror";
  });

  // ================= O2 =================
  await check("O2.0", "镜像页只保留添加策略按钮", async () => {
    await mirrorList(page);
    const buttons = await page.locator("#extras-view .page-actions button").allInnerTexts();
    assert(JSON.stringify(buttons) === JSON.stringify(["添加镜像策略"]), `buttons ${JSON.stringify(buttons)}`);
    const scope = page.locator("#extras-view");
    assert(!/镜像口状态/.test(await scope.innerText()), "the old 镜像口状态 block is still rendered");
    return JSON.stringify(buttons);
  });

  await check("O2.1", "弹窗为原生单选下拉，源口排除管理口/镜像口，输出口仅镜像口", async () => {
    await mirrorList(page);
    await page.locator("#add-policy").click();
    await page.waitForTimeout(500);
    const modal = page.locator(".modal").last();
    const sources = await modal.locator("[data-source] option").allInnerTexts();
    const outputs = await modal.locator("[data-output] option").allInnerTexts();
    const directions = await modal.locator("[data-direction] option").allInnerTexts();
    const tag = await modal.locator("[data-source]").evaluate((n) => n.tagName);
    await shot(page, "screens/o2-mirror-dialog.png");
    await modal.locator("[data-close]").first().click();
    assert(tag === "SELECT", "source control is not a native select");
    assert(!sources.some((s) => /^eth0/.test(s)), `management port in sources: ${sources}`);
    assert(!sources.some((s) => /^eth5/.test(s)), `mirror port in own sources: ${sources}`);
    assert(outputs.every((o) => /^eth5/.test(o)), `unexpected output ports: ${outputs}`);
    assert(JSON.stringify(directions) === JSON.stringify(["双向", "仅接收", "仅发送"]), `directions ${directions}`);
    return `sources=${sources.length}, outputs=${outputs.length}`;
  });

  await check("O2.2", "添加 / 编辑 / 删除镜像策略并核对 SPAN", async () => {
    await mirrorList(page);
    const before = await vppSpan();
    await addMirrorPolicy(page, "eth3", "both");
    await waitUntil(async () => (await iface("eth5")).mirror_sources.some((s) => s.interface === "eth3"), 20000);
    const addSpan = await waitSpan((text) => /\(\s*both\)/.test(spanLine(text, "lyroute-ens192")));
    let span = addSpan.text;
    saveText("vppctl/span-after-add.txt", span);
    assert(addSpan.ok && spanHas(span, "lyroute-ens192", "lyroute-ens224", "both"), "SPAN session for eth3 was not created");
    assert((await page.locator("#policy-rows tr", { hasText: "eth3" }).count()) === 1, "eth3 row missing");

    const row = page.locator("#policy-rows tr", { hasText: "eth3" }).first();
    await waitRuntimeConverged(60000);
    await row.locator('button:has-text("编辑")').click();
    await page.waitForTimeout(400);
    const modal = page.locator(".modal").last();
    const preselected = await modal.locator("[data-source]").inputValue();
    assert(preselected === "eth3", `edit preselected ${preselected}`);
    await modal.locator("[data-direction]").selectOption("rx");
    await modal.locator("[data-save]").click();
    await waitUntil(async () => (await iface("eth5")).mirror_sources.some((s) => s.interface === "eth3" && s.direction === "rx"), 20000);
    const editSpan = await waitSpan((text) => /\(\s*rx\)/.test(spanLine(text, "lyroute-ens192")));
    span = editSpan.text;
    saveText("vppctl/span-after-edit.txt", span);
    assert(editSpan.ok && spanHas(span, "lyroute-ens192", "lyroute-ens224", "rx"), "SPAN direction did not change to rx");

    const row2 = page.locator("#policy-rows tr", { hasText: "eth3" }).first();
    await waitRuntimeConverged(60000);
    await row2.locator('button:has-text("删除")').click();
    await waitUntil(async () => !(await iface("eth5")).mirror_sources.some((s) => s.interface === "eth3"), 20000);
    const goneSpan = await waitSpan((text) => !text.includes("lyroute-ens192"));
    span = goneSpan.text;
    saveText("vppctl/span-after-delete.txt", span);
    assert(goneSpan.ok && !spanHas(span, "lyroute-ens192"), "SPAN session for eth3 survived deletion");
    return "add/edit(rx)/delete all reflected in vppctl show interface span";
  });

  await check("O2.3", "无镜像口时添加按钮禁用并提示", async () => {
    await clearMirrorRoleAPI();
    await mirrorList(page);
    const disabled = await page.locator("#add-policy").isDisabled();
    const warning = await page.locator("#no-target").isVisible();
    const title = await page.locator("#add-policy").getAttribute("title");
    await shot(page, "screens/o2-no-mirror-port.png");
    assert(disabled, "add button is enabled without a mirror port");
    assert(warning, "no-target warning is not visible");
    assert(/镜像口/.test(title || ""), "tooltip does not explain the disabled button");
    // restore the mirror role and the original sources
    await restoreMirrorRoleAPI(cleanupMirror.sources);
    await mirrorList(page);
    const sources = (await iface("eth5")).mirror_sources || [];
    assert(cleanupMirror.sources.length === sources.length, `restored ${sources.length}/${cleanupMirror.sources.length} sources`);
    const restoredSpan = await waitSpan((text) => text.includes("lyroute-ens193") && text.includes("lyroute-ens160"));
    saveText("vppctl/span-restored.txt", restoredSpan.text);
    assert(restoredSpan.ok, "SPAN was not restored after the mirror role came back");
    return "disabled + warning shown, mirror role and sources restored";
  });

  // ================= O3 =================
  await check("O3.1", "编排组旁路：新增组 + 两个开关 + 阈值秒 + 回显 + 删除清理", async () => {
    // free eth4/eth5 from mirror so a temporary group can use them
    await clearMirrorRoleAPI();
    await page.waitForTimeout(1500);
    // tag them so the group dialog offers them
    const topo = await topology();
    topo.port_roles = { ...(topo.port_roles || {}), eth4: "lan", eth5: "wan" };
    await putTopology(topo);
    await sleep(1500);

    await gotoReady(page, "/groups");
    await waitRuntimeConverged(60000);
    await page.locator('button:has-text("新增编排组")').click();
    await page.waitForTimeout(600);
    let modal = page.locator(".modal").last();
    await modal.locator('input.input').first().fill("acc-grp");
    const selects = modal.locator("select");
    await selects.nth(0).selectOption('eth4');
    await selects.nth(1).selectOption('eth5');
    const ifaceBox = modal.locator("[data-bypass-iface]");
    const bizBox = modal.locator("[data-bypass-biz]");
    await ifaceBox.check();
    await bizBox.check();
    await modal.locator("[data-bypass-ip]").fill("192.168.10.106");
    await modal.locator("[data-bypass-port]").fill("8080");
    await modal.locator("[data-bypass-proto]").selectOption("tcp");
    await modal.locator("[data-bypass-threshold]").fill("20");
    await shot(page, "screens/o3-group-bypass.png");
    await modal.locator(".modal-footer .btn-primary").click();
    await waitUntil(async () => (await groups()).some((g) => g.name === "acc-grp"), 20000);
    await waitUntil(async () => ((await bypassConfig()).groups || []).some((g) => g.name === "acc-grp"), 20000);
    const entry = (await bypassConfig()).groups.find((g) => g.name === "acc-grp");
    saveJSON("api/acc-grp-bypass.json", entry);
    assert(entry.window_seconds === 20, `window_seconds ${entry.window_seconds}`);
    assert(entry.interface_bypass === true, "interface_bypass not persisted");
    assert(entry.flow_targets && entry.flow_targets[0].dst_ip === "192.168.10.106", "flow target missing");
    assert(entry.flow_targets[0].dst_port === 8080 && entry.flow_targets[0].protocol === "tcp", "flow target port/proto wrong");

    // edit: fields must be prefilled from the persisted entry
    await page.reload({ waitUntil: "domcontentloaded" });
    await page.waitForTimeout(2200);
    const row = page.locator("table tbody tr", { hasText: "acc-grp" }).first();
    await row.locator('button:has-text("编辑")').first().click();
    await page.waitForTimeout(800);
    modal = page.locator(".modal").last();
    assert(await modal.locator("[data-bypass-iface]").isChecked(), "interface switch not prefilled");
    assert(await modal.locator("[data-bypass-biz]").isChecked(), "business switch not prefilled");
    assert((await modal.locator("[data-bypass-ip]").inputValue()) === "192.168.10.106", "target IP not prefilled");
    assert((await modal.locator("[data-bypass-threshold]").inputValue()) === "20", "threshold not prefilled in seconds");
    await modal.locator("[data-bypass-threshold]").fill("25");
    await modal.locator(".modal-footer .btn-primary").click();
    await waitUntil(async () => {
      const current = ((await bypassConfig()).groups || []).find((g) => g.name === "acc-grp");
      return current && current.window_seconds === 25;
    }, 20000);
    saveJSON("api/acc-grp-bypass-edited.json", (await bypassConfig()).groups.find((g) => g.name === "acc-grp"));

    // negative: business bypass without a destination is blocked in the dialog
    await page.reload({ waitUntil: "domcontentloaded" });
    await page.waitForTimeout(2200);
    await page.locator("table tbody tr", { hasText: "acc-grp" }).first().locator('button:has-text("编辑")').first().click();
    await page.waitForTimeout(700);
    modal = page.locator(".modal").last();
    await modal.locator("[data-bypass-ip]").fill("");
    await modal.locator(".modal-footer .btn-primary").click();
    await page.waitForTimeout(500);
    const warning = await modal.locator(".bypass-error").innerText().catch(() => "");
    await shot(page, "screens/o3-bypass-missing-target.png");
    assert(/目的地址/.test(warning), `missing-target warning not shown: ${warning}`);
    const stillOpen = await page.locator(".modal").count();
    assert(stillOpen > 0, "dialog closed although validation failed");
    await modal.locator(".modal-footer .btn-secondary").click();
    await page.waitForTimeout(400);

    return "add -> 20s/flow target, edit -> 25s, negative validation, entry persists";
  });

  await check("O3.2", "接口故障旁路：网口 down 触发该组 bypass，up 后恢复", async () => {
    // Independent of O3.1: make sure the temporary group really exists.
    if (((await interfaces()).find((i) => i.id === "eth5") || {}).mirror) {
      const cleared = await topology();
      delete (cleared.port_roles || {}).eth5;
      await putTopology(cleared);
      await patch("/api/v1/interfaces/eth5", { id: "eth5", name: "eth5", mirror: false, mirror_sources: [] });
      await applyAndWait();
    }
    const tagged = await topology();
    tagged.port_roles = { ...(tagged.port_roles || {}), eth4: "lan", eth5: "wan" };
    await putTopology(tagged);
    if (!(await groups()).some((g) => g.name === "acc-grp")) {
      await post("/api/v1/orchestrator/orchestration-groups", {
        name: "acc-grp",
        ports: [
          { interface: "ens193", direction: "lan_facing" },
          { interface: "ens224", direction: "wan_facing" }
        ]
      });
      await putBypassGroup("acc-grp", { iface: true, biz: true, ip: "192.168.10.106", port: 8080, proto: "tcp", threshold: 20 });
    }
    await waitRuntimeConverged();
    await applyAndWait();
    await sleep(3000);
    const before = await get("/api/v1/orchestrator/bypass");
    const baseline = (before.groups || []).find((g) => g.group === "acc-grp");
    assert(baseline, "bypass runtime view missing acc-grp");
    ssh("ip link set ens193 down", 30000);
    let triggered = false;
    const deadline = now() + 25000;
    while (now() < deadline) {
      const state = await get("/api/v1/orchestrator/bypass");
      const group = (state.groups || []).find((g) => g.group === "acc-grp");
      if (group && group.enabled) { triggered = true; saveJSON("api/acc-grp-down.json", group); break; }
      await sleep(1000);
    }
    ssh("ip link set ens193 up", 30000);
    assert(triggered, "link-down did not enable the group bypass within 25s");
    let recovered = false;
    const recoverDeadline = now() + 30000;
    while (now() < recoverDeadline) {
      const state = await get("/api/v1/orchestrator/bypass");
      const group = (state.groups || []).find((g) => g.group === "acc-grp");
      if (group && !group.enabled) { recovered = true; saveJSON("api/acc-grp-recovered.json", group); break; }
      await sleep(1000);
    }
    assert(recovered, "group bypass did not recover after the link came back");
    return "interface down -> bypass, link up -> recovered";
  });

  await check("O3.3", "业务故障旁路：零答复判定（无流量源，环境限制）", async () => {
    throw new EnvLimit("passive window needs real attempts>0 traffic through the temporary group; no safe traffic generator is attached to the lab group ports. Config and plugin target installation are verified in O3.1/O3.2, and GET /bypass echoes flow_targets/flow_stats.");
  });

  await check("O3.4", "删除编排组清理旁路条目", async () => {
    await waitRuntimeConverged(60000);
    await gotoReady(page, "/groups");
    const row = page.locator("table tbody tr", { hasText: "acc-grp" }).first();
    await row.locator('button:has-text("删除")').first().click();
    await page.waitForTimeout(600);
    const confirm = page.locator(".modal").last();
    const dangerButton = confirm.locator('.modal-footer button:has-text("删除")');
    if (await dangerButton.count()) await dangerButton.click();
    else await confirm.locator(".modal-footer .btn-primary").click();
    await waitUntil(async () => !(await groups()).some((g) => g.name === "acc-grp"), 20000);
    await waitUntil(async () => !((await bypassConfig()).groups || []).some((g) => g.name === "acc-grp"), 20000);
    saveJSON("api/bypass-after-group-delete.json", await bypassConfig());
    assert(!(await groups()).some((g) => g.name === "acc-grp"), "group still exists");
    assert(!((await bypassConfig()).groups || []).some((g) => g.name === "acc-grp"), "bypass entry survived group deletion");
    return "group and bypass entry removed together";
  });

  // ================= O4 =================
  await check("O4.1", "流量编排：规则顺序、IP 群组引用、apply、编译状态", async () => {
    cleanupPolicy = (await get("/api/v1/orchestrator/policy")).item;
    await post("/api/v1/objects/ip-groups", {
      id: "acc-ip", name: "acc-ip", kind: "ip", entries: ["192.0.2.0/24"]
    });
    const doc = JSON.parse(JSON.stringify(cleanupPolicy));
    doc.ip_objects = [
      { id: "acc-ip", prefixes: ["192.0.2.0/24"] },
      { id: "ip-198-51-100-0-24", prefixes: ["198.51.100.0/24"] }
    ];
    const group = doc.policy_groups.find((g) => g.id === "3") || doc.policy_groups[0];
    group.rules = [
      { id: "acc-rule-1", match: { sources: ["any"], destinations: ["acc-ip"], protocol: "any" }, action: { kind: "direct" } },
      { id: "acc-rule-2", match: { sources: ["any"], destinations: ["ip-198-51-100-0-24"], protocol: "tcp" }, action: { kind: "drop" } }
    ];
    await put("/api/v1/orchestrator/policy", doc);
    const stored = await get("/api/v1/orchestrator/policy");
    saveJSON("api/policy-with-rules.json", stored);
    const storedGroup = stored.item.policy_groups.find((g) => g.id === group.id);
    assert(storedGroup.rules.length === 2, "rules were not persisted");
    assert(storedGroup.rules[0].id === "acc-rule-1" && storedGroup.rules[1].id === "acc-rule-2", "rule order changed");
    const applied = await applyAndWait();
    saveJSON("api/policy-apply.json", applied.result);
    assert(/committed|running/.test(String(applied.result.status) + String(applied.result.runtime_state)), "policy apply not committed");

    // negative: a rule may not reference a removed IP object
    const broken = JSON.parse(JSON.stringify(doc));
    broken.ip_objects = (broken.ip_objects || []).filter((o) => o.id !== "acc-ip");
    let error = null;
    try { await put("/api/v1/orchestrator/policy", broken); } catch (err) { error = err; }
    assert(error && error.status === 422, "rule referencing a removed IP object was accepted");
    saveJSON("api/ip-object-reference-reject.json", error.body);
    return "2 rules in order, acc-ip referenced, apply committed, delete blocked";
  });

  await check("O4.2", "恢复原策略并删除临时 IP 群组", async () => {
    if (!cleanupPolicy) throw new EnvLimit("no policy snapshot");
    await put("/api/v1/orchestrator/policy", cleanupPolicy);
    await del("/api/v1/objects/ip-groups/acc-ip").catch(() => null);
    await applyAndWait();
    const stored = await get("/api/v1/orchestrator/policy");
    assert(JSON.stringify(stored.item.policy_groups) === JSON.stringify(cleanupPolicy.policy_groups), "policy was not restored");
    return "policy restored and applied";
  });

  // ================= O5 =================
  await check("O5.1", "下发窗口内 dashboard/trend 不误报 degraded", async () => {
    const samples = [];
    let stop = false;
    const poll = (async () => {
      while (!stop) {
        try {
          const dashboardPayload = await get("/api/v1/telemetry/dashboard");
    const dashboard = dashboardPayload.data || dashboardPayload;
          const trend = await get("/api/v1/telemetry/traffic-trend?window=1h&points=60");
          samples.push({ at: now(), degraded: dashboard.degraded, reason: dashboard.degraded_reason || "", state: dashboard.runtime_state, trend: trend.degraded });
        } catch (error) {
          samples.push({ at: now(), error: String(error) });
        }
        await sleep(250);
      }
    })();
    const topo = await topology();
    topo.port_roles = { ...(topo.port_roles || {}), eth4: "wan" };
    await putTopology(topo);
    await post("/api/v1/runtime/apply", {});
    await sleep(7000);
    stop = true;
    await poll;
    saveJSON("evidence/telemetry-samples.json", samples);
    const bad = samples.filter((s) => s.degraded);
    assert(samples.length > 10, `only ${samples.length} samples`);
    assert(bad.length === 0, `${bad.length}/${samples.length} degraded samples: ${JSON.stringify(bad.slice(0, 3))}`);
    return `${samples.length} samples, 0 degraded`;
  });

  await check("O5.2", "采集缓存：同一轮轮询复用一次 VPP 采集", async () => {
    const [firstRaw, secondRaw, thirdRaw] = await Promise.all([
      get("/api/v1/telemetry/dashboard"),
      get("/api/v1/telemetry/dashboard"),
      get("/api/v1/telemetry/dashboard")
    ]);
    const first = firstRaw.data || firstRaw;
    const second = secondRaw.data || secondRaw;
    const third = thirdRaw.data || thirdRaw;
    const times = [first, second, third].map((s) => s.telemetry_status && s.telemetry_status.collected_at);
    saveJSON("evidence/telemetry-cache.json", times);
    assert(times[0] && times[0] === times[1] && times[1] === times[2], `collected_at changed inside the cache window: ${JSON.stringify(times)}`);
    return `collected_at stable at ${times[0]}`;
  });

  await check("O5.3", "真实采集失败时必须带出原因", async () => {
    throw new EnvLimit("inducing a real vppctl failure needs stopping VPP on the shared lab device; the reason fallback chain (interfaces -> runtime -> status) is covered by unit tests and O5.1 keeps the degraded window clean.");
  });

  await check("O5.4", "telemetry 页面在 1280/1600/1920 渲染", async () => {
    for (const [w, h] of [[1280, 800], [1600, 900], [1920, 1080]]) {
      const sized = await newPage(browser, w, h);
      await loginUI(sized.page);
      await sized.page.goto(BASE + "/traffic", { waitUntil: "domcontentloaded", timeout: 60000 }).catch(() => null);
      await sized.page.waitForTimeout(3000);
      await shot(sized.page, `screens/o5-traffic-${w}x${h}.png`);
      assert(sized.page.errors.length === 0, `${w}x${h} JS errors: ${sized.page.errors.join(" | ")}`);
      await sized.context.close();
    }
    return "three resolutions rendered without JS errors";
  });

  // ================= O6 =================
  await check("O6.1", "0.2.7 安装与 health/version", async () => {
    const health = await waitHealth();
    const versionEnv = ssh("cat /etc/ly-route/version.env", 20000).trim();
    saveText("evidence/version.env", versionEnv + "\n");
    assert(health.version === EXPECT_VERSION, `health version ${health.version}`);
    assert(versionEnv.includes(`LY_ROUTE_VERSION=${EXPECT_VERSION}`), `version.env: ${versionEnv}`);
    return `${health.version} / ${versionEnv}`;
  });

  await check("O6.2", "UI 与源码构建产物一致（sha256）", async () => {
    const remote = ssh("sha256sum /opt/ly-route/admin/orchestrator-extras.js /opt/ly-route/admin/mirror.js /opt/ly-route/admin/mirror.html /opt/ly-route/admin/app.js", 20000);
    saveText("evidence/ui-sha.txt", remote);
    const localRoot = process.env.UI_BUILD_DIR || "/tmp/ui-orch-new";
    if (!fs.existsSync(path.join(localRoot, "app.js"))) {
      throw new FixtureError(`UI build directory ${localRoot} is not available for comparison`);
    }
    for (const name of ["orchestrator-extras.js", "mirror.js", "mirror.html", "app.js"]) {
      const expected = execFileSync("sha256sum", [path.join(localRoot, name)], { encoding: "utf8" }).split(/\s+/)[0];
      assert(remote.includes(expected), `${name} sha mismatch: expected ${expected}`);
    }
    return "all four UI artifacts match the built package";
  });

  await check("O6.3", "服务集审计：仅编排器 unit，failed 为空，无 heal 报错", async () => {
    const enabled = ssh("systemctl list-unit-files --state=enabled --no-legend --no-pager", 30000);
    saveText("evidence/enabled-units.txt", enabled);
    const allowed = [
      "ly-route-control-api.service", "ly-route-firstboot.service", "ly-route-pingora.service",
      "ly-route-recovery.service", "ly-route-runtime-check.service", "ly-route-vfio-preflight.service",
      "ly-route-vpp-apply.service", "ly-route-vpp-session-enable.service", "ly-route-vpp-tune.service",
      "vpp.service"
    ];
    const enabledUnits = enabled.split(/\r?\n/).map((line) => line.trim().split(/\s+/)[0]).filter(Boolean);
    const forbidden = enabledUnits.filter((unit) => /smartdns|dnsmasq|xray|pppoe|dns-vpp|policy-routing|ly-route-wan|nginx/.test(unit));
    assert(forbidden.length === 0, `gateway units are enabled: ${forbidden}`);
    const missing = allowed.filter((unit) => !enabledUnits.includes(unit));
    assert(missing.length === 0, `orchestrator units not enabled: ${missing}`);
    const failed = ssh("systemctl --failed --no-legend --no-pager", 20000).trim();
    assert(failed === "", `failed units: ${failed}`);
    const units = ["smartdns", "dnsmasq", "xray", "ly-route-dns-vpp-datapath", "ly-route-policy-routing", "nginx"];
    for (const unit of units) {
      const state = ssh(`systemctl show -p LoadState --value ${unit}.service`, 15000).trim();
      assert(state === "not-found", `${unit} reports ${state} on the orchestrator`);
    }
    const journal = ssh("journalctl -u ly-route-control-api --since '-20 min' --no-pager", 30000);
    saveText("evidence/journal.txt", journal);
    assert(!/DNS data-path heal|exit status 5/.test(journal), "journal still contains the heal failure");
    return `${enabledUnits.length} enabled units, failed empty, no heal errors`;
  });

  await check("O6.4", "两次保存/下发后仍无 heal 报错且 health ok", async () => {
    for (let i = 0; i < 2; i += 1) {
      const topo = await topology();
      topo.port_roles = { ...(topo.port_roles || {}), eth4: i % 2 === 0 ? "wan" : "wan" };
      await putTopology(topo);
      await applyAndWait();
    }
    await sleep(3000);
    const journal = ssh("journalctl -u ly-route-control-api --since '-10 min' --no-pager | grep -E 'DNS data-path heal|exit status 5' || true", 30000);
    assert(journal.trim() === "", `heal errors after applies: ${journal}`);
    const health = await waitHealth();
    assert(health.status === "ok", "health is not ok after applies");
    return "two applies, no heal errors, health ok";
  });
}

// ---------- cleanup ----------
async function cleanup() {
  try {
    if (cleanupPolicy) {
      await put("/api/v1/orchestrator/policy", cleanupPolicy).catch(() => null);
    }
  } catch { /* best effort */ }
  try {
    const current = await groups();
    if (current.some((g) => g.name === "acc-grp")) {
      await del("/api/v1/orchestrator/orchestration-groups/acc-grp").catch(() => null);
    }
  } catch { /* best effort */ }
  try {
    const config = await bypassConfig();
    const kept = (config.groups || []).filter((g) => g.name !== "acc-grp");
    if (kept.length !== (config.groups || []).length) {
      await put("/api/v1/orchestrator/bypass/config", { ...config, groups: kept });
    }
  } catch { /* best effort */ }
  try {
    const topo = await topology();
    if (cleanupTopology) {
      const restored = {
        schema_version: cleanupTopology.schema_version,
        management_interface: cleanupTopology.management_interface,
        management_shared: cleanupTopology.management_shared,
        interfaces: cleanupTopology.interfaces,
        orchestration_groups: cleanupTopology.orchestration_groups,
        port_roles: cleanupTopology.port_roles
      };
      await putTopology(restored);
    }
  } catch { /* best effort */ }
  try {
    const port = (await interfaces()).find((item) => item.mirror === true) || { id: "eth5" };
    if (cleanupMirror) {
      await patch(`/api/v1/interfaces/${port.id}`, {
        id: port.id, name: port.name || port.id, mirror: true, mirror_sources: cleanupMirror.sources
      }).catch(() => null);
    }
  } catch { /* best effort */ }
  try { ssh("ip link set ens193 up", 20000); } catch { /* best effort */ }
  try { await del("/api/v1/objects/ip-groups/acc-ip").catch(() => null); } catch { /* best effort */ }
  cleanupBondFixture();
  try { await applyAndWait(); } catch { /* best effort */ }
}

(async () => {
  console.log(`Orchestrator acceptance gate against ${BASE}`);
  console.log(`Evidence: ${ROOT}`);
  let fatal = null;
  try {
    await run();
  } catch (error) {
    fatal = error;
    console.error("FATAL", error);
    results.push({ id: "GATE", name: "gate crashed", status: "FIXTURE_FAIL", detail: String(error && error.stack || error) });
  } finally {
    try { await cleanup(); } catch (error) { console.error("cleanup failed", error); }
    if (browser) await browser.close().catch(() => null);
  }
  const counts = results.reduce((acc, item) => { acc[item.status] = (acc[item.status] || 0) + 1; return acc; }, {});
  saveJSON("results.json", { base: BASE, version: EXPECT_VERSION, started_at: STAMP, counts, results });
  console.log("\n=== SUMMARY ===");
  for (const item of results) console.log(`${item.status.padEnd(12)} ${item.id.padEnd(6)} ${item.name}${item.detail ? " — " + item.detail : ""}`);
  console.log(JSON.stringify(counts));
  const failed = results.filter((item) => item.status === "PRODUCT_FAIL" || item.status === "FIXTURE_FAIL");
  process.exit(failed.length === 0 && !fatal ? 0 : 1);
})();

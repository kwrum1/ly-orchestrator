// Release-image UI sweep for the merged Orchestrator build.
//
// The 506cc4c release image ships the source-tree controller-shell bundle
// (frontend/orchestrator), not the legacy 11-page Vite bundle accepted at
// e2c16ef.  That shell is a stateful SPA: pages are sidebar buttons
// (`#sideMenu [data-page]`) and there are no per-page URLs.  This sweep
// navigates those buttons in real Chromium, screenshots every page, and
// records HTTP failures and console errors.
//
//   ORCH_BASE=https://10.1.18.244 ORCH_PASS_FILE=... ORCH_OUT=... \
//     node ui-sweep-release.mjs
//
// Exit code is non-zero on a missing page, a 404, or an unexpected console
// error (the pre-auth session probe 401 is expected).

import { chromium } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';

const BASE = process.env.ORCH_BASE || 'https://127.0.0.1';
const USER = process.env.ORCH_USER || 'admin';
const PASS = fs.readFileSync(process.env.ORCH_PASS_FILE, 'utf8').trim();
const OUT = process.env.ORCH_OUT || './ui-release-evidence';
fs.mkdirSync(OUT, { recursive: true });

const pages = [
  ['系统概况', 'dashboard/overview'],
  ['流量概况', 'telemetry/flow-summary'],
  ['在线用户', 'telemetry/online-users'],
  ['Top连接', 'telemetry/top-connections'],
  ['网卡设置', 'orchestrator/nic-settings'],
  ['编排设置', 'orchestrator/group-settings'],
  ['流量编排', 'orchestrator/policy'],
  ['流量控制', 'flow-control/traffic'],
  ['安全控制', 'security/policies'],
  ['IP管理', 'object/ip'],
  ['系统用户管理', 'system/users'],
  ['配置管理', 'system/config'],
];

const result = {
  base: BASE,
  browser: 'chromium',
  bundle: 'released controller-shell (not the legacy 11-page Vite bundle)',
  started_at: new Date().toISOString(),
  total: pages.length,
  passed: 0,
  failed: [],
  pages: [],
  console_errors: [],
  http_failures: [],
  api_requests: new Set(),
};
const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } });
const page = await context.newPage();
let current = 'login';
page.on('console', (m) => { if (m.type() === 'error') result.console_errors.push({ page: current, text: m.text() }); });
page.on('pageerror', (e) => result.console_errors.push({ page: current, text: 'pageerror: ' + e.message }));
page.on('response', (r) => {
  const url = r.url();
  if (url.includes('/api/v1/')) result.api_requests.add(r.request().method() + ' ' + url.replace(BASE, ''));
  if (r.status() >= 400) result.http_failures.push({ page: current, status: r.status(), method: r.request().method(), url });
});

await page.goto(BASE + '/', { waitUntil: 'domcontentloaded', timeout: 60000 });
await page.waitForTimeout(1500);
const inputs = page.locator('input');
if (await inputs.count() >= 2) {
  await inputs.nth(0).fill(USER);
  await inputs.nth(1).fill(PASS);
  await page.locator('button[type="submit"], button:has-text("登录")').first().click();
  await page.waitForTimeout(3000);
}
const shellVisible = await page.locator('#appShell:not(.is-hidden)').count() > 0;
const menu = await page.locator('#sideMenu [data-page]').evaluateAll((nodes) => nodes.map((n) => ({ id: n.dataset.page, title: n.innerText.trim() })));
result.login = { url: page.url(), shell_visible: shellVisible, menu_count: menu.length, menu };
result.menu_matches_expected = pages.every(([title, id]) => menu.some((item) => item.id === id && item.title === title));

for (const [label, id] of pages) {
  current = label;
  const before = { f: result.http_failures.length, e: result.console_errors.length };
  const button = page.locator(`#sideMenu [data-page="${id}"]`);
  let clicked = false;
  try { await button.click({ timeout: 5000 }); clicked = true; } catch { clicked = false; }
  await page.waitForTimeout(1800);
  const workspace = await page.locator('#workspace').innerText().catch(() => '');
  const body = await page.locator('body').innerText();
  const titleShown = workspace.includes(label) || body.includes(label);
  const file = path.join(OUT, `${String(result.pages.length + 1).padStart(2, '0')}-${label.replace(/[\/\\ ]+/g, '_')}.png`);
  await page.screenshot({ path: file, fullPage: true });
  const raw404 = /(^|\s)404(\s|$)|Not Found|未找到/.test(workspace);
  const ok = clicked && titleShown && workspace.replace(/\s+/g, '').length > 40 && !raw404;
  result.pages.push({
    label, id, clicked, title: await page.title(), ok, raw_error_text: raw404,
    workspace_chars: workspace.replace(/\s+/g, '').length,
    http_failures: result.http_failures.slice(before.f),
    console_errors: result.console_errors.slice(before.e),
    screenshot: file,
  });
  if (ok) result.passed++; else result.failed.push(label);
}

result.finished_at = new Date().toISOString();
result.api_requests = [...result.api_requests].sort();
const http404 = result.http_failures.filter((f) => f.status === 404).length;
const unexpectedConsole = result.console_errors.filter((e) => !e.text.includes('401'));
result.summary = {
  passed: result.passed,
  failed: result.failed,
  http_404: http404,
  http_failures: result.http_failures.length,
  pre_auth_session_probe_401: result.http_failures.filter((f) => f.status === 401 && f.url.endsWith('/api/v1/auth/session')).length,
  console_errors: result.console_errors.length,
  unexpected_console_errors: unexpectedConsole.length,
  menu_matches_expected: result.menu_matches_expected,
};
result.passed_all = result.failed.length === 0 && http404 === 0 && unexpectedConsole.length === 0 && result.menu_matches_expected;
fs.writeFileSync(path.join(OUT, 'result.json'), JSON.stringify(result, null, 2));
console.log(JSON.stringify(result.summary, null, 2));
await browser.close();
process.exit(result.passed_all ? 0 : 1);

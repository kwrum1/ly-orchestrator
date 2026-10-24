// Orchestrator 11-page UI acceptance sweep.
//
// Real Chromium, authenticated, sidebar navigation, per-page screenshot, and a
// machine-readable result that records every 4xx and every console error.
//
//   ORCH_BASE=https://10.1.18.203 ORCH_PASS_FILE=... ORCH_OUT=... \
//     node ui-sweep.mjs
//
// Requires `playwright` with a Chromium build. Exit code is non-zero when a
// page fails, when any 404 is observed, or on a non-401 console error.

import { chromium } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';

const BASE = process.env.ORCH_BASE || 'https://127.0.0.1';
const USER = process.env.ORCH_USER || 'admin';
const PASS = fs.readFileSync(process.env.ORCH_PASS_FILE, 'utf8').trim();
const OUT = process.env.ORCH_OUT || './ui-evidence';
fs.mkdirSync(OUT, { recursive: true });

const pages = [
  ['单机总览', '/'],
  ['单机拓扑', '/topology'],
  ['编排组', '/groups'],
  ['流量编排', '/policy'],
  ['透明编排', '/transparent'],
  ['遥测', '/telemetry'],
  ['运行态', '/runtime'],
  ['配置与快照', '/config'],
  ['升级 / 回滚', '/upgrades'],
  ['产品转换', '/conversions'],
  ['用户管理', '/system/users'],
];

const result = {
  base: BASE,
  browser: 'chromium',
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
result.login = { url: page.url(), ok: !/请输入密码/.test(await page.locator('body').innerText()) };

for (const [label, route] of pages) {
  current = label;
  const before = { f: result.http_failures.length, e: result.console_errors.length };
  const link = page.locator(`a[href="${route}"]`).first();
  let clicked = false;
  try { if (await link.count()) { await link.click({ timeout: 5000 }); clicked = true; } } catch { clicked = false; }
  if (!clicked) await page.goto(BASE + route, { waitUntil: 'domcontentloaded', timeout: 60000 });
  await page.waitForTimeout(3000);
  const h1s = await page.locator('h1').allInnerTexts();
  const body = await page.locator('body').innerText();
  const file = path.join(OUT, `${String(result.pages.length + 1).padStart(2, '0')}-${label.replace(/[\/\\ ]+/g, '_')}.png`);
  await page.screenshot({ path: file, fullPage: true });
  const raw404 = /(^|\s)404(\s|$)|Not Found|未找到|请求失败|加载失败/.test(body);
  const ok = h1s.includes(label) && body.replace(/\s+/g, '').length > 80 && !raw404;
  result.pages.push({
    label, path: route, url: page.url(), clicked, title: await page.title(),
    h1: h1s[0] || '', ok, raw_error_text: raw404,
    chars: body.replace(/\s+/g, '').length,
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
};
result.passed_all = result.failed.length === 0 && http404 === 0 && unexpectedConsole.length === 0;
fs.writeFileSync(path.join(OUT, 'result.json'), JSON.stringify(result, null, 2));
console.log(JSON.stringify(result.summary, null, 2));
await browser.close();
process.exit(result.passed_all ? 0 : 1);

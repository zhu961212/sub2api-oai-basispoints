#!/usr/bin/env node
// Real Chromium regression for the Sub2API sandbox. No npm dependencies.
// Run: node tools/test-ui-browser.mjs
// Optional: CHROME_PATH=/path/to/chrome node tools/test-ui-browser.mjs
// Reproduce an old UI snapshot: --ui-root /path/to/ui --expect-submit-blocked
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { access, mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { randomBytes } from 'node:crypto';

const projectRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const args = process.argv.slice(2);
let uiRoot = join(projectRoot, 'ui');
let expectBlocked = false;
for (let index = 0; index < args.length; index++) {
  if (args[index] === '--ui-root' && args[index + 1]) uiRoot = resolve(args[++index]);
  else if (args[index] === '--expect-submit-blocked') expectBlocked = true;
  else throw new Error(`Unknown or incomplete argument: ${args[index]}`);
}

async function findBrowser() {
  const candidates = [
    process.env.CHROME_PATH,
    process.env.PROGRAMFILES && join(process.env.PROGRAMFILES, 'Google', 'Chrome', 'Application', 'chrome.exe'),
    process.env['PROGRAMFILES(X86)'] && join(process.env['PROGRAMFILES(X86)'], 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
    process.env.PROGRAMFILES && join(process.env.PROGRAMFILES, 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
    process.env.LOCALAPPDATA && join(process.env.LOCALAPPDATA, 'Google', 'Chrome', 'Application', 'chrome.exe'),
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
    '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome', '/usr/bin/microsoft-edge',
  ].filter(Boolean);
  for (const candidate of candidates) {
    try { await access(candidate); return candidate; } catch { /* Try the next installed browser. */ }
  }
  throw new Error('Chrome/Edge/Chromium was not found; set CHROME_PATH to its executable.');
}

// This driver runs INSIDE the opaque-origin iframe. The parent never reads its DOM.
// It loads after the unchanged production app.js and uses real DOM click defaults.
const driver = String.raw`
(async function () {
  const token = new URLSearchParams(location.hash.slice(1)).get('bridge_token');
  const params = new URLSearchParams(location.search);
  const stage = params.get('stage');
  const blocked = params.get('blocked') === '1';
  const send = (type, data = {}) => parent.postMessage({ source: 'bps-ui-browser-test', bridge_token: token, stage, type, ...data }, '*');
  const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));
  const selected = () => Array.from(document.querySelectorAll('#account-list input:checked')).map((box) => Number(box.value));
  const selectedModels = () => Array.from(document.querySelectorAll('#model-list input:checked')).map((box) => box.value);
  const modelIDs = ['gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna'];
  const defaultModels = ['gpt-6-astra', 'gpt-5.6-sol'];
  const subset = ['gpt-6-sol', 'gpt-5.6-luna'];
  const same = (actual, expected) => JSON.stringify(actual) === JSON.stringify(expected);
  async function until(check, label, milliseconds = 6000) {
    const end = performance.now() + milliseconds;
    while (performance.now() < end) {
      if (check()) return;
      await sleep(25);
    }
    throw new Error(label + '; hint=' + document.getElementById('form-hint').textContent);
  }
  try {
    await until(() => document.querySelectorAll('#account-list input').length === 2 &&
      document.getElementById('form-hint').textContent === '' &&
      !document.getElementById('save-button').disabled, 'Configuration did not become ready');
    if (!blocked) {
      if (document.querySelector('[id^="image-relay"]') || document.querySelector('input:not(#account-list input):not(#model-list input), select, details'))
        throw new Error('Images still require additional configuration controls');
      if (!document.body.textContent.includes('支持直接发送图片和截图，无需额外配置'))
        throw new Error('Automatic image support is not described');
      if (!same(Array.from(document.querySelectorAll('#model-list input')).map((box) => box.value), modelIDs))
        throw new Error('The model picker does not group GPT-6 before GPT-5.6');
      const initialModels = stage === 'select' ? defaultModels : stage === 'all' ? subset : stage === 'clear' ? modelIDs : [];
      if (!same(selectedModels(), initialModels))
        throw new Error('Reopened models were ' + JSON.stringify(selectedModels()) + ', expected ' + JSON.stringify(initialModels));
      if (stage === 'reopen-empty' && !document.getElementById('model-hint').textContent.includes('所有模型原样透传'))
        throw new Error('Clearing models does not explain pass-through behavior');
      if (/图片中转|公网 HTTPS|反向代理|监听地址|存储目录/.test(document.body.textContent))
        throw new Error('Obsolete image hosting instructions are still visible');
    }
    const initial = stage === 'clear' || stage === 'all' ? [101, 202] : [];
    if (!same(selected(), initial)) throw new Error('Reopened selection was ' + JSON.stringify(selected()) + ', expected ' + JSON.stringify(initial));
    send('opened', { selected: selected(), models: selectedModels() });
    if (stage === 'reopen-empty') { send('done', { selected: selected(), models: selectedModels() }); return; }
    if (stage === 'select' && !blocked) {
      const selectAll = document.getElementById('select-all-button');
      if (!selectAll || selectAll.disabled) throw new Error('Select-all button did not become ready');
      selectAll.click();
      if (!selectAll.disabled) throw new Error('Select-all button stayed enabled after every account was selected');
    } else if (stage !== 'all') {
      for (const box of document.querySelectorAll('#account-list input')) box.click();
    }
    const expected = stage === 'select' || stage === 'all' ? [101, 202] : [];
    if (!same(selected(), expected)) throw new Error('Account selection controls did not change the selected accounts');
    const expectedModels = stage === 'select' ? subset : stage === 'all' ? modelIDs : [];
    if (!blocked) {
      for (const box of document.querySelectorAll('#model-list input')) {
        if (box.checked !== expectedModels.includes(box.value)) box.click();
      }
      if (!same(selectedModels(), expectedModels)) throw new Error('Model checkboxes did not change the selected models');
    }
    const button = document.getElementById('save-button');
    let clickCount = 0;
    let submitCount = 0;
    button.addEventListener('click', () => { clickCount++; });
    document.getElementById('config-form').addEventListener('submit', () => { submitCount++; }, true);
    button.click();
    if (!blocked && !document.getElementById('model-fields').disabled) throw new Error('Model fields were not locked during save');
    send('clicked', { selected: selected(), clickCount, submitCount, buttonType: button.type });
    if (blocked) {
      await sleep(800);
      send('blocked-evidence', { selected: selected(), clickCount, submitCount, buttonType: button.type,
        hint: document.getElementById('form-hint').textContent });
      return;
    }
    await until(() => /已保存/.test(document.getElementById('form-hint').textContent) && !button.disabled,
      'Save button did not finish a confirmed save');
    if (!same(selected(), expected)) throw new Error('Selection changed after save');
    if (!same(selectedModels(), expectedModels)) throw new Error('Model selection changed after save');
    send('done', { selected: selected(), models: selectedModels(), hint: document.getElementById('form-hint').textContent });
  } catch (error) { send('failure', { error: error.message }); }
})();`;

const secret = randomBytes(18).toString('hex');
const harness = `<!doctype html><html><head><meta charset="utf-8"><title>Sub2API sandbox regression</title></head>
<body><pre id="result">Running sandbox regression…</pre><script src="/__test/host.js"></script></body></html>`;
const host = `
(function () {
  const secret = ${JSON.stringify(secret)};
  const expectBlocked = ${JSON.stringify(expectBlocked)};
  const clone = (value) => JSON.parse(JSON.stringify(value));
  const same = (left, right) => JSON.stringify(left) === JSON.stringify(right);
  const models = ['gpt-6-astra', 'gpt-5.6-sol', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-terra', 'gpt-5.6-luna'];
  const defaultModels = ['gpt-6-astra', 'gpt-5.6-sol'];
  const subset = ['gpt-6-sol', 'gpt-5.6-luna'];
  let config = { account_ids: [], enabled_models: defaultModels,
    timeout_seconds: 123, auth_mode: 'chatgpt', rewrite_tools: false };
  let frame;
  let token;
  let stage;
  let finished = false;
  let loadCount = 0;
  let saveCount = 0;
  const events = [];
  const accounts = [
    { id: 101, name: 'Regression account A', schedulable: true, status: 'active' },
    { id: 202, name: 'Regression account B', schedulable: true, status: 'active' },
  ];
  async function finish(ok, details) {
    if (finished) return;
    finished = true;
    const result = { ok, ...details, saveCount, loadCount, persisted: config, events };
    document.getElementById('result').textContent = JSON.stringify(result, null, 2);
    await fetch('/__test/result/' + secret, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(result) });
  }
  function reopen(nextStage) {
    if (frame) frame.remove();
    stage = nextStage;
    token = crypto.randomUUID();
    frame = document.createElement('iframe');
    frame.setAttribute('sandbox', 'allow-scripts');
    frame.src = '/ui/index.html?stage=' + stage + '&blocked=' + (expectBlocked ? '1' : '0') + '#bridge_token=' + encodeURIComponent(token);
    document.body.appendChild(frame);
  }
  window.addEventListener('message', function (event) {
    if (finished || !frame || event.source !== frame.contentWindow) return;
    const data = event.data;
    if (!data || typeof data !== 'object' || data.bridge_token !== token) return;
    if (event.origin !== 'null') return void finish(false, { error: 'Iframe was not an opaque-origin sandbox: ' + event.origin });
    if (data.source === 'bps-ui-browser-test') {
      if (data.stage !== stage) return;
      const { bridge_token, source, ...details } = data;
      events.push({ ...details, origin: event.origin });
      if (data.type === 'failure') return void finish(false, { error: data.error });
      if (data.type === 'blocked-evidence') {
        const reproduced = expectBlocked && saveCount === 0 && data.clickCount === 1 && data.submitCount === 0 && data.buttonType === 'submit';
        return void finish(reproduced, { reproducedSubmitBlocked: reproduced, evidence: details,
          error: reproduced ? undefined : 'Expected a blocked native form submission with no config.save' });
      }
      if (data.type !== 'done') return;
      if (stage === 'select') {
        if (saveCount !== 1 || !same(config.account_ids, [101, 202])) return void finish(false, { error: 'Selected accounts were not persisted by the host' });
        if (!same(config.enabled_models, subset)) return void finish(false, { error: 'Model subset was not persisted by the host' });
        return reopen('all');
      }
      if (stage === 'all') {
        if (saveCount !== 2 || !same(config.account_ids, [101, 202]) || !same(config.enabled_models, models))
          return void finish(false, { error: 'All six models were not persisted or accounts changed' });
        return reopen('clear');
      }
      if (stage === 'clear') {
        if (saveCount !== 3 || !same(config.account_ids, []) || !same(config.enabled_models, []))
          return void finish(false, { error: 'Empty account and model selections were not persisted by the host' });
        return reopen('reopen-empty');
      }
      return void finish(true, { checkedSelectionSurvivedReopen: true, emptySelectionSurvivedReopen: true,
        automaticImagesWithoutSettings: true,
        defaultModelsSelected: true, modelSubsetSurvivedReopen: true, allSixModelsSurvivedReopen: true,
        emptyModelSelectionSurvivedReopen: true, otherConfigurationPreserved: true });
    }
    if (data.source !== 'sub2api-plugin-ui') return;
    if (data.type === 'ui.resize' || data.type === 'sub2api.plugin.ready') return;
    if (typeof data.request_id !== 'string' || !data.request_id) return void finish(false, { error: 'Bridge request has no request_id' });
    events.push({ stage, type: data.type, origin: event.origin });
    const respond = (payload) => event.source.postMessage({ source: 'sub2api-plugin-host', bridge_token: token,
      type: data.type + '.result', request_id: data.request_id, ...payload }, '*');
    if (data.type === 'config.load') { loadCount++; return respond({ ok: true, config: clone(config) }); }
    if (data.type === 'plugin.status') return respond({ ok: true, result: { healthy: true, message: 'ready',
      status_json: JSON.stringify({ plugin_version: 'browser-test', accounts, account_ids: config.account_ids,
        available_models: models, enabled_models: config.enabled_models }) } });
    if (data.type === 'config.save') {
      saveCount++;
      if (data.config.timeout_seconds !== 123 || data.config.auth_mode !== 'chatgpt' || data.config.rewrite_tools !== false)
        return void finish(false, { error: 'Saving account selection overwrote unrelated configuration' });
      config = clone(data.config);
      return respond({ ok: true, config: clone(config) });
    }
    respond({ ok: false, error: 'Unknown bridge request: ' + data.type });
  });
  window.addEventListener('error', (event) => { void finish(false, { error: 'Host harness error: ' + event.message }); });
  setTimeout(() => { void finish(false, { error: 'Browser regression timed out' }); }, 18000);
  reopen('select');
})();`;

const browser = await findBrowser();
const html = await readFile(join(uiRoot, 'index.html'), 'utf8');
const profile = await mkdtemp(join(tmpdir(), 'bps-ui-browser-'));
let child;
let timer;
let resultResolve;
let resultReject;
let stderr = '';
const resultPromise = new Promise((resolveResult, rejectResult) => { resultResolve = resolveResult; resultReject = rejectResult; });
const contentTypes = { '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8' };
const server = createServer(async (request, response) => {
  try {
    const url = new URL(request.url, 'http://127.0.0.1');
    response.setHeader('Cache-Control', 'no-store');
    response.setHeader('Content-Security-Policy', "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; form-action 'none'");
    if (request.method === 'POST' && url.pathname === '/__test/result/' + secret) {
      let body = '';
      for await (const chunk of request) {
        body += chunk;
        if (body.length > 65536) throw new Error('Oversized test result');
      }
      const result = JSON.parse(body);
      response.writeHead(200, { 'Content-Type': 'text/plain' });
      response.end('ok');
      resultResolve(result);
      return;
    }
    if (request.method !== 'GET') { response.writeHead(405); response.end(); return; }
    if (url.pathname === '/') { response.setHeader('Content-Type', 'text/html; charset=utf-8'); response.end(harness); return; }
    if (url.pathname === '/__test/host.js' || url.pathname === '/__test/driver.js') {
      response.setHeader('Content-Type', 'text/javascript; charset=utf-8');
      response.end(url.pathname.endsWith('host.js') ? host : driver);
      return;
    }
    if (url.pathname === '/ui/index.html') {
      response.setHeader('Content-Type', 'text/html; charset=utf-8');
      response.end(html.replace('</body>', '<script src="/__test/driver.js"></script></body>'));
      return;
    }
    if (url.pathname.startsWith('/ui/assets/')) {
      const file = resolve(uiRoot, '.' + decodeURIComponent(url.pathname.slice(3)));
      if (!file.startsWith(resolve(uiRoot, 'assets') + sep)) { response.writeHead(403); response.end(); return; }
      const extension = file.slice(file.lastIndexOf('.'));
      if (!contentTypes[extension]) { response.writeHead(404); response.end(); return; }
      response.setHeader('Content-Type', contentTypes[extension]);
      response.end(await readFile(file));
      return;
    }
    response.writeHead(404); response.end();
  } catch (error) { response.writeHead(500); response.end('Test harness error'); resultReject(error); }
});

try {
  await new Promise((resolveListening, rejectListening) => {
    server.once('error', rejectListening);
    server.listen(0, '127.0.0.1', resolveListening);
  });
  const url = `http://127.0.0.1:${server.address().port}/`;
  console.log(`Browser: ${browser}\nUI: ${uiRoot}\nSandbox: allow-scripts; CSP: form-action 'none'`);
  child = spawn(browser, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--disable-background-networking', '--disable-component-update', '--disable-sync',
    '--metrics-recording-only', '--password-store=basic', '--no-proxy-server',
    '--remote-debugging-pipe', `--user-data-dir=${profile}`, url,
  ], { stdio: ['ignore', 'ignore', 'pipe', 'pipe', 'pipe'], windowsHide: true });
  child.stderr.on('data', (chunk) => { stderr = (stderr + chunk).slice(-12000); });
  child.once('error', resultReject);
  child.once('exit', (code) => { resultReject(new Error(`Browser exited before a result (code ${code})\n${stderr}`)); });
  timer = setTimeout(() => resultReject(new Error(`Browser did not report within 25 seconds\n${stderr}`)), 25000);
  const result = await resultPromise;
  console.log(JSON.stringify(result, null, 2));
  if (!result.ok) throw new Error(result.error || 'Browser regression failed');
  console.log(expectBlocked ? 'PASS: reproduced blocked submit with zero config.save requests.' : 'PASS: account choices and model subset, all-six, and empty choices persist across iframe destruction and reopening.');
} catch (error) {
  console.error(error.stack || error.message);
  process.exitCode = 1;
} finally {
  clearTimeout(timer);
  if (child && child.exitCode === null && child.pid) {
    // Close only this test's browser. Never connect to a user's existing profile.
    child.stdio[3]?.on('error', () => {});
    child.stdio[3]?.write(JSON.stringify({ id: 1, method: 'Browser.close' }) + '\0');
    await Promise.race([new Promise((done) => child.once('exit', done)), new Promise((done) => setTimeout(done, 1500))]);
    if (child.exitCode === null) {
      if (process.platform === 'win32') {
        const killer = spawn('taskkill.exe', ['/PID', String(child.pid), '/T', '/F'], { stdio: 'ignore', windowsHide: true });
        await new Promise((done) => { killer.once('error', done); killer.once('exit', done); });
      } else child.kill('SIGTERM');
    }
  }
  server.closeAllConnections();
  await new Promise((done) => server.close(done));
  // mkdtemp owns this exact directory; do not recursively remove any caller path.
  await rm(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
}

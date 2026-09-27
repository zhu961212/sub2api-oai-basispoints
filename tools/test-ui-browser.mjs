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

const browserAccounts = [
  { id: 1011234567890123, name: '同名账号😀-完整名称', schedulable: true, status: 'active' },
  { id: 2021234567890123, name: '同名账号😀-完整名称', schedulable: true, status: 'active' },
  { id: 3031234567890123, name: '超长账号名称不得截断'.repeat(20) + '<img src=x onerror=window.__accountNameInjected=true>', schedulable: true, status: 'active' },
  { id: 4041234567890123, name: 'Unicode é 账号👩‍💻 العربية', schedulable: true, status: 'active' },
  { id: 5051234567890123, name: '   ', schedulable: true, status: 'active' },
];

// This driver runs INSIDE the opaque-origin iframe. The parent never reads its DOM.
// It loads after the unchanged production app.js and uses real DOM click defaults.
const driver = String.raw`
(async function () {
  const token = new URLSearchParams(location.hash.slice(1)).get('bridge_token');
  const params = new URLSearchParams(location.search);
  const stage = params.get('stage');
  const scoped = stage === 'scoped' || stage === 'scoped-reopen';
  const blocked = params.get('blocked') === '1';
  const send = (type, data = {}) => parent.postMessage({ source: 'bps-ui-browser-test', bridge_token: token, stage, type, ...data }, '*');
  const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));
  const selected = () => Array.from(document.querySelectorAll('#account-list input:checked')).map((box) => Number(box.value));
  const selectedModels = () => Array.from(document.querySelectorAll('#model-list input:checked')).map((box) => box.value);
  const accounts = ${JSON.stringify(browserAccounts)};
  const accountIDs = accounts.map((account) => account.id);
  const displayNames = accounts.map((account) => account.name.trim() ? account.name : String(account.id));
  const modelIDs = ['gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna'];
  const defaultModels = ['gpt-6-astra', 'gpt-5.6-sol'];
  const subset = ['gpt-6-sol', 'gpt-5.6-luna'];
  const same = (actual, expected) => JSON.stringify(actual) === JSON.stringify(expected);
  function assert403Policy(expected, pending = false) {
    const toggle = document.getElementById('bps-403-toggle');
    const label = '403 自动停用：' + (expected ? '已开启' : '已关闭') + (pending ? '（待保存）' : '');
    if (!toggle || toggle.type !== 'button' || toggle.getAttribute('aria-pressed') !== String(expected) || toggle.textContent !== label)
      throw new Error('403 policy button state is incorrect; expected ' + label);
    return toggle;
  }
  function assertDeviceConvergence(expected, pending = false) {
    const toggle = document.getElementById('bps-device-toggle');
    const label = '设备收敛：' + (expected ? '已开启' : '已关闭') + (pending ? '（待保存）' : '');
    if (!toggle || toggle.type !== 'button' || toggle.getAttribute('aria-pressed') !== String(expected) || toggle.textContent !== label)
      throw new Error('Device convergence button state is incorrect; expected ' + label);
    return toggle;
  }
  function assertResponsiveLayout() {
    const width = document.documentElement.clientWidth;
    if (document.documentElement.scrollWidth > width + 1 || document.body.scrollWidth > width + 1)
      throw new Error('Page overflows horizontally at ' + width + 'px');
    for (const element of document.querySelectorAll('.account-name, .account-availability, #degradation-result > span, #bps-403-toggle, #bps-device-toggle')) {
      const bounds = element.getBoundingClientRect();
      if (bounds.left < -1 || bounds.right > width + 1 || element.scrollWidth > element.clientWidth + 1)
        throw new Error('Account name, diagnostic result or policy control overflows at ' + width + 'px');
    }
  }
  async function until(check, label, milliseconds = 6000) {
    const end = performance.now() + milliseconds;
    while (performance.now() < end) {
      if (check()) return;
      await sleep(25);
    }
    throw new Error(label + '; hint=' + document.getElementById('form-hint').textContent);
  }
  try {
    await until(() => document.querySelectorAll('#account-list input').length === accountIDs.length &&
      document.getElementById('form-hint').textContent === '' &&
      !document.getElementById('save-button').disabled, 'Configuration did not become ready');
    if (!blocked) {
      if (!same(Array.from(document.querySelectorAll('.account-name')).map((element) => element.textContent), displayNames))
        throw new Error('Account labels do not preserve complete names or empty-name ID fallback');
      if (document.querySelector('#account-list img, #account-list svg, #account-list script') || window.__accountNameInjected)
        throw new Error('Account names were parsed as HTML');
      for (const row of document.querySelectorAll('.account-row')) {
        const accountID = row.querySelector('input').value;
        if (!row.querySelector('.account-availability').textContent.startsWith('#' + accountID + ' · ') ||
            row.querySelector('.account-name').title !== '账号 ID：' + accountID ||
            row.querySelector('.account-check-button').getAttribute('aria-label') !== '检测账号 ID：' + accountID)
          throw new Error('Account identity or individual check button labels point to the wrong account');
      }
      const initial403Policy = stage === 'select' || stage === 'clear';
      if (assert403Policy(initial403Policy).disabled) throw new Error('403 policy control did not become ready');
      const initialDeviceConvergence = stage === 'all';
      if (assertDeviceConvergence(initialDeviceConvergence).disabled) throw new Error('Device convergence control did not become ready');
      assertResponsiveLayout();
      if (document.querySelector('[id^="image-relay"]') || document.querySelector('input:not(#account-list input):not(#model-list input), select, details'))
        throw new Error('Images still require additional configuration controls');
      if (!document.body.textContent.includes('支持直接发送图片和截图，无需额外配置'))
        throw new Error('Automatic image support is not described');
      if (!same(Array.from(document.querySelectorAll('#model-list input')).map((box) => box.value), modelIDs))
        throw new Error('The model picker does not group GPT-6 before GPT-5.6');
      const initialModels = stage === 'select' ? defaultModels : stage === 'all' || scoped ? subset : stage === 'clear' ? modelIDs : [];
      if (!same(selectedModels(), initialModels))
        throw new Error('Reopened models were ' + JSON.stringify(selectedModels()) + ', expected ' + JSON.stringify(initialModels));
      if (stage === 'reopen-empty' && !document.getElementById('model-hint').textContent.includes('所有模型原样透传'))
        throw new Error('Clearing models does not explain pass-through behavior');
      if (/图片中转|公网 HTTPS|反向代理|监听地址|存储目录/.test(document.body.textContent))
        throw new Error('Obsolete image hosting instructions are still visible');
    }
    const initial = scoped ? (stage === 'scoped' ? [accountIDs[0], accountIDs[1], accountIDs[3]] : [accountIDs[1], accountIDs[2], accountIDs[3]]) :
      stage === 'reopen-empty' || (stage === 'select' && blocked) ? [] : accountIDs;
    if (!same(selected(), initial)) throw new Error('Reopened selection was ' + JSON.stringify(selected()) + ', expected ' + JSON.stringify(initial));
    send('opened', { selected: selected(), models: selectedModels(), viewportWidth: document.documentElement.clientWidth,
      autoDisableOn403: document.getElementById('bps-403-toggle')?.getAttribute('aria-pressed'),
      deviceConvergence: document.getElementById('bps-device-toggle')?.getAttribute('aria-pressed') });
    if (scoped) {
      const bulk = document.getElementById('degradation-check-button');
      const save = document.getElementById('save-button');
      const accountButton = (accountID) => document.querySelector('.account-check-button[value="' + accountID + '"]');
      const hint = () => document.getElementById('form-hint').textContent;
      function assertReady() {
        if (bulk.disabled || save.disabled || document.getElementById('select-all-button').disabled ||
            Array.from(document.querySelectorAll('.account-check-button')).some((button) => button.disabled))
          throw new Error('New host capability did not enable individual, bulk and select-all actions');
      }
      function assertBusy(before) {
        for (const controlID of ['save-button', 'retry-button', 'select-all-button', 'degradation-check-button',
          'bps-403-toggle', 'bps-device-toggle', 'account-fields', 'model-fields']) {
          if (!document.getElementById(controlID).disabled) throw new Error('Diagnostic did not lock ' + controlID);
        }
        if (Array.from(document.querySelectorAll('.account-check-button')).some((button) => !button.disabled))
          throw new Error('Diagnostic did not lock every individual check');
        // Even synthetic events must not bypass the busy guard or dispatch writes.
        for (const button of [save, bulk, accountButton(accountIDs[0]), document.getElementById('select-all-button'),
          document.getElementById('bps-403-toggle'), document.getElementById('bps-device-toggle')]) {
          button.click();
          button.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
        }
        const box = document.querySelector('#account-list input');
        box.checked = !box.checked;
        box.dispatchEvent(new Event('change', { bubbles: true }));
        if (!same(selected(), before) || !same(selectedModels(), subset))
          throw new Error('Busy diagnostic allowed account or model selection changes');
        assert403Policy(false);
        assertDeviceConvergence(false);
      }
      async function diagnose(button, expectedSelection, expectedHint) {
        const before = selected();
        assertReady();
        button.click();
        assertBusy(before);
        await until(() => !save.disabled && hint().includes(expectedHint), 'Scoped diagnostic did not settle: ' + expectedHint);
        if (!same(selected(), expectedSelection) || !same(selectedModels(), subset))
          throw new Error('Scoped diagnostic changed the wrong account or model selection');
        assertReady();
        assertResponsiveLayout();
      }
      assertReady();
      if (stage === 'scoped-reopen') {
        send('done', { selected: selected(), models: selectedModels(), scopedSelectionSurvivedReopen: true });
        return;
      }
      await diagnose(accountButton(accountIDs[1]), initial, '账号选择未改变');
      const checkedRow = accountButton(accountIDs[1]).closest('.account-row');
      const sameNameRow = accountButton(accountIDs[0]).closest('.account-row');
      if (!checkedRow.querySelector('.account-check-result.result-degraded') || sameNameRow.querySelector('.account-check-result.result-degraded'))
        throw new Error('Single-account result leaked to another account with the same name');
      const singleResult = document.getElementById('degradation-result').textContent;
      if (!singleResult.includes('（#' + accountIDs[1] + '）') || singleResult.includes('foreign-result-name'))
        throw new Error('Scoped result lost the selected account ID or trusted an unrelated result name');

      await diagnose(accountButton(accountIDs[0]), initial, '检测失败');
      if (sameNameRow.querySelector('.account-check-result.result-degraded'))
        throw new Error('Mismatched request ID was displayed as another account result');

      const filtered = [accountIDs[1], accountIDs[2], accountIDs[3]];
      await diagnose(bulk, filtered, '失败或跳过的账号保留原选择');
      if (!hint().includes('点击保存后生效')) throw new Error('Bulk selection did not explain the manual save requirement');
      const expectedStatuses = ['ok', 'error', 'degraded', 'skipped', 'ok'];
      for (let index = 0; index < accountIDs.length; index++) {
        const row = accountButton(accountIDs[index]).closest('.account-row');
        if (!row.querySelector('.account-check-result.result-' + expectedStatuses[index]))
          throw new Error('Bulk result was assigned to the wrong account ID');
      }
      await diagnose(bulk, filtered, '检测失败');
      await diagnose(bulk, filtered, '检测未全部完成');
      send('before-scoped-save', { selected: selected(), models: selectedModels() });
      save.click();
      await until(() => !save.disabled && hint().includes('已保存'), 'Scoped local selection did not save on explicit click');
      if (!same(selected(), filtered) || !same(selectedModels(), subset))
        throw new Error('Explicit save changed the diagnostic account/model selection');
      assertReady();
      assertResponsiveLayout();
      send('done', { selected: selected(), models: selectedModels(), scopedDiagnostics: 5,
        individualResultIsolated: true, requestAndTargetMismatchRejected: true,
        busyActionsLocked: true, partialFailureSelectionPreserved: true, incompleteSelectionPreserved: true,
        scopedSelectionSavedOnlyOnClick: true });
      return;
    }
    if (stage === 'reopen-empty') {
      const statuses = ['error', 'error', 'degraded', 'ok', 'skipped'];
      const bulk = document.getElementById('degradation-check-button');
      if (!bulk.disabled || !bulk.title.includes('原子绑定检测账号'))
        throw new Error('Bulk account diagnostics must be disabled with the host limitation explained');
      bulk.click();
      bulk.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
      for (let index = 0; index < accountIDs.length; index++) {
        const accountID = accountIDs[index];
        const button = document.querySelector('.account-check-button[value="' + accountID + '"]');
        if (!button.disabled || !button.title.includes('原子绑定检测账号'))
          throw new Error('Account diagnostics must be disabled with the host limitation explained');
        button.click();
        button.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
        await until(() => document.getElementById('form-hint').textContent.includes('检测已暂停') &&
          !document.getElementById('save-button').disabled, 'Forced diagnostic event did not fail closed');
        if (document.getElementById('bps-403-toggle').disabled || document.getElementById('bps-device-toggle').disabled)
          throw new Error('Paused diagnostics unexpectedly locked policy controls');
        for (let rowIndex = 0; rowIndex < accountIDs.length; rowIndex++) {
          const row = document.querySelector('.account-check-button[value="' + accountIDs[rowIndex] + '"]').closest('.account-row');
          const verdict = row.querySelector('.account-check-result');
          if (!verdict.classList.contains('result-' + statuses[rowIndex]))
            throw new Error('Passive account result leaked across equal account names');
        }
        const label = displayNames[index] === String(accountID) ? '#' + accountID : displayNames[index] + '（#' + accountID + '）';
        if (!document.querySelector('#degradation-result > span:nth-child(' + (index + 2) + ')').textContent.startsWith(label + ' · '))
          throw new Error('Passive diagnostic result lost the complete account name or ID');
        if (document.querySelector('#degradation-result img, #degradation-result svg, #degradation-result script') || window.__accountNameInjected)
          throw new Error('Diagnostic names were parsed as HTML');
        if (!same(selected(), []) || !same(selectedModels(), [])) throw new Error('Individual diagnostic changed account/model routing');
        assert403Policy(false);
        assertDeviceConvergence(false);
        assertResponsiveLayout();
      }
      send('done', { selected: selected(), models: selectedModels(), pausedAccountDiagnostics: accountIDs.length, passiveAccountResults: accountIDs.length });
      return;
    }
    if (stage === 'select' && !blocked) {
      const selectAll = document.getElementById('select-all-button');
      if (!selectAll || selectAll.disabled || selectAll.textContent !== '已全选列表账号')
        throw new Error('Already selected accounts must keep the select-all control clickable');
      selectAll.click();
      if (!same(selected(), accountIDs) || selectAll.disabled) throw new Error('Repeated select-all changed selected accounts or disabled the control');
      for (const box of document.querySelectorAll('#account-list input:checked')) box.click();
      if (!same(selected(), [])) throw new Error('Account checkboxes did not clear the migrated selection');
      if (!selectAll || selectAll.disabled) throw new Error('Select-all button did not become ready');
      selectAll.click();
      if (selectAll.disabled || selectAll.textContent !== '已全选列表账号')
        throw new Error('Select-all must stay clickable and report every account selected');
    } else if (stage !== 'all') {
      for (const box of document.querySelectorAll('#account-list input')) box.click();
    }
    const expected = stage === 'select' || stage === 'all' ? accountIDs : [];
    if (!same(selected(), expected)) throw new Error('Account selection controls did not change the selected accounts');
    const expectedModels = stage === 'select' ? subset : stage === 'all' ? modelIDs : [];
    const expected403Policy = stage === 'all';
    const expectedDeviceConvergence = stage === 'select';
    if (!blocked) {
      assert403Policy(!expected403Policy).click();
      assert403Policy(expected403Policy, true);
      const initialDeviceConvergence = stage === 'all';
      assertDeviceConvergence(initialDeviceConvergence).click();
      assertDeviceConvergence(!initialDeviceConvergence, true);
      assertResponsiveLayout();
      assertDeviceConvergence(!initialDeviceConvergence, true).click();
      assertDeviceConvergence(initialDeviceConvergence);
      if (initialDeviceConvergence !== expectedDeviceConvergence) {
        assertDeviceConvergence(initialDeviceConvergence).click();
        assertDeviceConvergence(expectedDeviceConvergence, true);
      }
      assertResponsiveLayout();
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
    if (!blocked && !document.getElementById('bps-403-toggle').disabled) throw new Error('403 policy control was not locked during save');
    if (!blocked && !document.getElementById('bps-device-toggle').disabled) throw new Error('Device convergence control was not locked during save');
    if (!blocked) {
      document.getElementById('bps-device-toggle').click();
      assertDeviceConvergence(expectedDeviceConvergence, expectedDeviceConvergence !== (stage === 'all'));
    }
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
    if (assert403Policy(expected403Policy).disabled) throw new Error('403 policy control did not unlock after save');
    if (assertDeviceConvergence(expectedDeviceConvergence).disabled) throw new Error('Device convergence control did not unlock after save');
    assertResponsiveLayout();
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
  const sameIDs = (left, right) => Array.isArray(left) && same(left.slice().sort((a, b) => a - b), right.slice().sort((a, b) => a - b));
  // Saved configuration uses catalog order; the UI groups model generations separately.
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
  let testCount = 0;
  let scopedTestCount = 0;
  let scopedInFlight = false;
  let allowScopedSave = false;
  let legacyChecks = {};
  let scopedChecks = {};
  let lastCheck = null;
  const events = [];
  const accounts = ${JSON.stringify(browserAccounts)};
  const accountIDs = accounts.map((account) => account.id);
  async function finish(ok, details) {
    if (finished) return;
    finished = true;
    const result = { ok, ...details, saveCount, loadCount, testCount, scopedTestCount, persisted: config, events };
    document.getElementById('result').textContent = JSON.stringify(result, null, 2);
    await fetch('/__test/result/' + secret, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(result) });
  }
  function reopen(nextStage) {
    if (frame) frame.remove();
    stage = nextStage;
    if (stage === 'reopen-empty') {
      const statuses = ['error', 'error', 'degraded', 'ok', 'skipped'];
      lastCheck = { completed: true, degraded_account_ids: [accountIDs[2]], results: accountIDs.map((id, index) => {
        const result = { account_id: id, name: 'stale-result-name', status: statuses[index] };
        if (index < 2) result.error = index === 0 ? 'HTTP 403' : 'HTTP 429';
        else if (index === 4) result.error = 'temporarily unavailable';
        else result.answer = index === 2 ? '苹果16' : '苹果17';
        return result;
      }) };
    }
    token = crypto.randomUUID();
    frame = document.createElement('iframe');
    frame.style.width = stage === 'select' || stage === 'clear' ? '900px' : '320px';
    frame.style.height = '1600px';
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
      if (data.type === 'before-scoped-save') {
        if (stage !== 'scoped' || scopedTestCount !== 5 || scopedInFlight || saveCount !== 3 ||
            !same(config.account_ids, [accountIDs[0], accountIDs[1], accountIDs[3]]))
          return void finish(false, { error: 'Diagnostics saved or altered persisted selection before an explicit save' });
        allowScopedSave = true;
        return;
      }
      if (data.type !== 'done') return;
      if (stage === 'scoped') {
        if (!allowScopedSave || saveCount !== 4 || testCount !== 0 || scopedTestCount !== 5 || data.scopedDiagnostics !== 5 ||
            !sameIDs(config.account_ids, [accountIDs[1], accountIDs[2], accountIDs[3]]) || !same(config.enabled_models, subset))
          return void finish(false, { error: 'Scoped diagnostic results or explicit persisted selection did not match the requested snapshot' });
        scopedChecks = { singleAndBulkDiagnosticsEnabled: true, requestLocalSnapshotsUsed: true,
          individualResultIsolated: data.individualResultIsolated, requestAndTargetMismatchRejected: data.requestAndTargetMismatchRejected,
          busyActionsLocked: data.busyActionsLocked, partialFailureSelectionPreserved: data.partialFailureSelectionPreserved,
          incompleteSelectionPreserved: data.incompleteSelectionPreserved, scopedSelectionSavedOnlyOnClick: data.scopedSelectionSavedOnlyOnClick };
        return reopen('scoped-reopen');
      }
      if (stage === 'scoped-reopen') {
        if (saveCount !== 4 || scopedTestCount !== 5 || !data.scopedSelectionSurvivedReopen ||
            !same(data.selected, [accountIDs[1], accountIDs[2], accountIDs[3]]))
          return void finish(false, { error: 'Explicit scoped selection did not survive reopening the new host UI' });
        return void finish(true, { ...legacyChecks, ...scopedChecks, scopedSelectionSurvivedReopen: true });
      }
      if (stage === 'select') {
        if (saveCount !== 1 || !same(config.account_ids, accountIDs)) return void finish(false, { error: 'Selected accounts were not persisted by the host' });
        if (config.bps_auto_disable_on_403 !== false) return void finish(false, { error: 'Disabled 403 policy was not persisted by the host' });
        if (config.bps_device_convergence !== true) return void finish(false, { error: 'Enabled device convergence was not persisted by the host' });
        if (config.auto_select_new_accounts !== true || !same(config.excluded_account_ids, []))
          return void finish(false, { error: 'Legacy unrestricted accounts were not migrated to automatic selection' });
        if (!same(config.enabled_models, subset)) return void finish(false, { error: 'Model subset was not persisted by the host' });
        return reopen('all');
      }
      if (stage === 'all') {
        if (saveCount !== 2 || !same(config.account_ids, accountIDs) || !same(config.enabled_models, models))
          return void finish(false, { error: 'All six models were not persisted or accounts changed' });
        if (config.bps_auto_disable_on_403 !== true) return void finish(false, { error: 'Enabled 403 policy was not persisted by the host' });
        if (config.bps_device_convergence !== false) return void finish(false, { error: 'Disabled device convergence was not persisted by the host' });
        return reopen('clear');
      }
      if (stage === 'clear') {
        if (saveCount !== 3 || !same(config.account_ids, []) || !same(config.enabled_models, []))
          return void finish(false, { error: 'Empty account and model selections were not persisted by the host' });
        if (config.auto_select_new_accounts !== true || !same(config.excluded_account_ids, accountIDs))
          return void finish(false, { error: 'Cleared accounts were not persisted as explicit exclusions' });
        if (config.bps_auto_disable_on_403 !== false) return void finish(false, { error: '403 policy changed while clearing accounts' });
        if (config.bps_device_convergence !== false) return void finish(false, { error: 'Device convergence changed while clearing accounts' });
        return reopen('reopen-empty');
      }
      if (testCount !== 0 || saveCount !== 3 || data.pausedAccountDiagnostics !== accountIDs.length || data.passiveAccountResults !== accountIDs.length)
        return void finish(false, { error: 'Paused diagnostics dispatched a request, saved config, or lost passive results' });
      legacyChecks = { checkedSelectionSurvivedReopen: true, emptySelectionSurvivedReopen: true,
        fullAccountNamesDisplayed: true, duplicateNamesDisambiguatedByIDs: true, namesRenderedAsText: true,
        accountDiagnosticsPausedWithoutSideEffects: true, passiveAccountResultsIsolated: true, default403PolicyEnabled: true, disabled403PolicySurvivedReopen: true,
        enabled403PolicySurvivedReopen: true, wideAndNarrowLayoutsWithoutOverflow: true,
        defaultDeviceConvergenceDisabled: true, enabledDeviceConvergenceSurvivedReopen: true,
        disabledDeviceConvergenceSurvivedReopen: true, deviceConvergenceDirtyStateReset: true,
        deviceConvergenceLockedDuringSave: true,
        automaticImagesWithoutSettings: true,
        defaultModelsSelected: true, modelSubsetSurvivedReopen: true, allSixModelsSurvivedReopen: true,
        emptyModelSelectionSurvivedReopen: true, otherConfigurationPreserved: true };
      config = { ...config, account_ids: [accountIDs[0], accountIDs[1], accountIDs[3]],
        excluded_account_ids: [accountIDs[2], accountIDs[4]], enabled_models: subset };
      lastCheck = null;
      return reopen('scoped');
    }
    if (data.source !== 'sub2api-plugin-ui') return;
    if (data.type === 'ui.resize' || data.type === 'sub2api.plugin.ready') return;
    if (typeof data.request_id !== 'string' || !data.request_id) return void finish(false, { error: 'Bridge request has no request_id' });
    events.push({ stage, type: data.type, origin: event.origin });
    const responseWindow = event.source;
    const responseToken = token;
    const respond = (payload) => responseWindow.postMessage({ source: 'sub2api-plugin-host', bridge_token: responseToken,
      type: data.type + '.result', request_id: data.request_id, ...payload }, '*');
    if (data.type === 'config.load') {
      loadCount++;
      const payload = { ok: true, config: clone(config) };
      if (stage === 'scoped' || stage === 'scoped-reopen') payload.capabilities = ['config.testScoped'];
      return respond(payload);
    }
    if (data.type === 'plugin.status') return respond({ ok: true, result: { healthy: true, message: 'ready',
      status_json: JSON.stringify({ plugin_version: 'browser-test', accounts, account_ids: config.account_ids,
        available_models: models, enabled_models: config.enabled_models, degradation_check: lastCheck }) } });
    if (data.type === 'config.test') {
      testCount++;
      return void finish(false, { error: 'Account diagnostics must never dispatch legacy config.test' });
    }
    if (data.type === 'config.testScoped') {
      if (stage !== 'scoped' || scopedInFlight || allowScopedSave || saveCount !== 3)
        return void finish(false, { error: 'Scoped request escaped its supported stage or busy lock' });
      scopedTestCount++;
      if (scopedTestCount > 5) return void finish(false, { error: 'An extra scoped request bypassed the busy lock' });
      const snapshot = data.config;
      const single = scopedTestCount <= 2;
      const targets = single ? [accountIDs[scopedTestCount === 1 ? 1 : 0]] : accountIDs;
      const expectedSelection = scopedTestCount <= 3 ? [accountIDs[0], accountIDs[1], accountIDs[3]] : [accountIDs[1], accountIDs[2], accountIDs[3]];
      if (!snapshot || snapshot.degradation_check !== true || !sameIDs(snapshot.account_ids, expectedSelection) ||
          !same(snapshot.enabled_models, subset) || snapshot.timeout_seconds !== 123 || snapshot.auth_mode !== 'chatgpt' || snapshot.rewrite_tools !== false ||
          (single ? snapshot.degradation_check_account_id !== targets[0] || 'degradation_check_account_ids' in snapshot :
            !same(snapshot.degradation_check_account_ids, targets) || 'degradation_check_account_id' in snapshot))
        return void finish(false, { error: 'Scoped request did not preserve its selected account/model snapshot and explicit targets' });
      if (!same(config.account_ids, [accountIDs[0], accountIDs[1], accountIDs[3]]) || config.degradation_check === true ||
          'degradation_check_account_id' in config || 'degradation_check_account_ids' in config)
        return void finish(false, { error: 'Scoped diagnostic command leaked into saved configuration' });
      const call = scopedTestCount;
      const statuses = single ? ['degraded'] : call === 5 ? ['degraded', 'ok', 'ok', 'error', 'skipped'] : ['ok', 'error', 'degraded', 'skipped', 'ok'];
      const check = { request_id: data.request_id, target_account_ids: targets.slice(), completed: call !== 5,
        degraded_account_ids: targets.filter((_id, index) => statuses[index] === 'degraded'),
        results: targets.map((id, index) => {
          const row = { account_id: id, name: 'foreign-result-name', status: statuses[index] };
          if (row.status === 'degraded' || row.status === 'ok') row.answer = row.status === 'degraded' ? '苹果16' : '苹果17';
          else row.error = row.status === 'error' ? 'fixed diagnostic failure' : 'fixed skipped account';
          return row;
        }) };
      if (call === 2) check.request_id = 'unrelated-request';
      if (call === 4) check.target_account_ids[0] = 999;
      if (call !== 2 && call !== 4) lastCheck = clone(check);
      scopedInFlight = true;
      // Fixed local fixtures only: no account, token, model or upstream network request.
      setTimeout(() => {
        scopedInFlight = false;
        respond({ ok: true, result: { success: call < 3, message: 'fixed scoped diagnostic', latency_ms: 100,
          status_json: JSON.stringify({ degradation_check: check }) } });
      }, 100);
      return;
    }
    if (data.type === 'config.save') {
      saveCount++;
      if (scopedInFlight || (stage === 'scoped' && !allowScopedSave))
        return void finish(false, { error: 'Scoped diagnostics must not save configuration automatically or while busy' });
      if (stage === 'reopen-empty')
        return void finish(false, { error: 'Paused account diagnostics must never save configuration' });
      if (data.config.timeout_seconds !== 123 || data.config.auth_mode !== 'chatgpt' || data.config.rewrite_tools !== false)
        return void finish(false, { error: 'Saving account selection overwrote unrelated configuration' });
      if (stage === 'scoped' && (data.config.degradation_check === true ||
          'degradation_check_account_id' in data.config || 'degradation_check_account_ids' in data.config))
        return void finish(false, { error: 'Explicit save retained a diagnostic command' });
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
  console.log(expectBlocked ? 'PASS: reproduced blocked submit with zero config.save requests.' : 'PASS: old-host diagnostics stay disabled; scoped single/bulk diagnostics preserve request/account ownership, busy locks and failed selections; only explicit save persists choices; select-all remains clickable and policies/models survive reopening.');
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

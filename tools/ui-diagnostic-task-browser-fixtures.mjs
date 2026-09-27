// Serialized into the real opaque-origin sandbox by test-ui-browser.mjs.
// All diagnostic responses are local fixtures, with no upstream requests.
export async function runDiagnosticTaskDriver(stage, accounts) {
  const token = new URLSearchParams(location.hash.slice(1)).get('bridge_token');
  const ids = accounts.map(account => account.id);
  const base = [ids[0], ids[1], ids[3]];
  const models = ['gpt-6-sol', 'gpt-5.6-luna'];
  const editedModels = ['gpt-6-astra', ...models];
  const send = (type, details = {}) => parent.postMessage({ source: 'bps-ui-browser-test', bridge_token: token, stage, type, ...details }, '*');
  const get = id => document.getElementById(id);
  const chosen = selector => Array.from(document.querySelectorAll(selector + ' input:checked'), box => box.value);
  const selected = () => chosen('#account-list').map(Number);
  const button = id => Array.from(document.querySelectorAll('.account-check-button')).find(item => item.value === String(id));
  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
  const assert = (ok, message) => { if (!ok) throw new Error(message); };
  const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
  const hint = () => get('form-hint').textContent;
  const bulk = () => get('degradation-check-button');
  const save = () => get('save-button');
  async function until(check, label) {
    const end = performance.now() + 6000;
    while (performance.now() < end) { if (check()) return; await sleep(15); }
    throw new Error(label + '; hint=' + hint());
  }
  let sequence = 0;
  function control(action) {
    const requestID = ++sequence;
    return new Promise(resolve => {
      function receive(event) {
        if (event.source !== parent || event.data?.source !== 'bps-task-fixture' ||
            event.data.bridge_token !== token || event.data.request_id !== requestID) return;
        window.removeEventListener('message', receive); resolve();
      }
      window.addEventListener('message', receive);
      send('fixture-control', { action, request_id: requestID });
    });
  }
  function selection(expected = base, expectedModels = models) {
    assert(same(selected(), expected), 'Task changed unrelated account selection: ' + JSON.stringify(selected()));
    assert(same(chosen('#model-list'), expectedModels), 'Task replaced unsaved model selection');
  }
  function ready() {
    assert(!bulk().disabled && !save().disabled && !get('select-all-button').disabled, 'Plugin task mode did not restore bulk detection and selectors');
    assert(ids.every(id => !button(id).disabled), 'Plugin task mode did not restore individual detection');
    assert(get('account-check-hint').textContent.includes('无需修改宿主'), 'Compatible mode omitted the unpatched-host explanation');
  }
  function busy(expected = base, expectedModels = models) {
    for (const id of ['save-button', 'retry-button', 'select-all-button', 'degradation-check-button',
      'bps-403-toggle', 'account-fields', 'model-fields']) assert(get(id).disabled, 'Task failed to lock ' + id);
    assert(ids.every(id => button(id).disabled), 'Task failed to lock single-account buttons');
    for (const item of [bulk(), save(), button(ids[0]), get('select-all-button')]) {
      item.click(); item.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    }
    const box = document.querySelector('#account-list input');
    box.checked = !box.checked; box.dispatchEvent(new Event('change', { bubbles: true }));
    selection(expected, expectedModels);
  }
  function pending(expected = base, expectedModels = models) {
    assert(bulk().textContent === '继续查询上次检测' && !bulk().disabled, 'Uncertain outcome did not expose query-only recovery');
    assert(button(ids[1]).disabled, 'Uncertain task permitted another account diagnostic');
    assert(!save().disabled && !get('account-fields').disabled && !get('model-fields').disabled, 'Uncertain task blocked ordinary configuration edits');
    selection(expected, expectedModels);
  }
  async function diagnose(item, expectedHint, expected = base, expectedModels = models) {
    const status = get('diagnostic-task-status');
    const previousID = status.textContent.split(' · ')[0];
    const isQuery = item.textContent.includes('继续查询');
    item.click(); busy(expected, expectedModels);
    await until(() => !save().disabled && hint().includes(expectedHint), 'Task did not settle: ' + expectedHint);
    assert(!status.hidden && status.textContent.startsWith('检测任务 '), 'Task ID is not visible');
    if (isQuery) assert(status.textContent.split(' · ')[0] === previousID, 'Query changed the visible task ID');
  }
  async function saveForm() {
    save().click(); await until(() => !save().disabled && hint().includes('已保存'), 'Explicit task-stage save failed');
  }
  function layout() {
    const width = document.documentElement.clientWidth;
    assert(document.documentElement.scrollWidth <= width + 1, 'Task page overflows horizontally');
    for (const item of document.querySelectorAll('.account-name, .account-check-result, #account-check-hint, #diagnostic-task-status'))
      assert(item.getBoundingClientRect().right <= width + 1 && item.scrollWidth <= item.clientWidth + 1, 'Task result or recovery hint overflows');
  }
  try {
    await until(() => document.querySelectorAll('#account-list input').length === ids.length && !save().disabled && hint() === '', 'Task page did not become ready');
    if (stage === 'task-success-reopen') {
      ready(); selection([ids[1], ids[2], ids[3]], editedModels); layout();
      assert(get('bps-403-toggle').getAttribute('aria-pressed') === 'true', 'Saved diagnostic parameters did not survive reopening');
      send('done'); return;
    }
    selection();
    if (stage === 'task-reopen-busy') {
      assert(bulk().disabled && ids.every(id => button(id).disabled), 'Reopened page ignored an active plugin task');
      assert(!get('select-all-button').disabled && !bulk().textContent.includes('继续查询'), 'Reopened page adopted a foreign task or locked selectors');
      await sleep(120); await control('finish-and-reopen'); return;
    }
    ready();
    if (stage === 'task-reopen-finished') {
      await sleep(120); await saveForm(); selection(); send('done'); return;
    }
    if (stage === 'task-success') {
      get('select-all-button').click(); get('select-all-button').click();
      assert(same(selected(), ids) && !get('select-all-button').disabled, 'Repeated select-all failed in task mode');
      for (const box of document.querySelectorAll('#account-list input')) if (!base.includes(Number(box.value))) box.click();
      Array.from(document.querySelectorAll('#model-list input')).find(box => box.value === 'gpt-6-astra').click();
      get('bps-403-toggle').click();
      await diagnose(button(ids[1]), '未按探针结果调整账号选择', base, editedModels);
      selection(base, editedModels); ready();
      assert(button(ids[1]).closest('.account-row').querySelector('.result-degraded'), 'Single task result missing');
      assert(!button(ids[0]).closest('.account-row').querySelector('.result-degraded'), 'Single result leaked across duplicate account names');
      assert(!get('degradation-result').textContent.includes('foreign-result-name'), 'Result replaced trusted account identity');
      await diagnose(bulk(), '点击保存后生效', base, editedModels);
      selection([ids[1], ids[2], ids[3]], editedModels); ready(); layout();
      await control('before-explicit-save'); await saveForm();
      selection([ids[1], ids[2], ids[3]], editedModels); send('done'); return;
    }
    if (stage === 'task-reopen-running') {
      button(ids[0]).click(); busy(); await control('close-while-running'); return;
    }
    if (stage === 'task-prepare-timeout') {
      await diagnose(button(ids[0]), '准备提交未确认'); pending();
      await diagnose(bulk(), '准备状态'); pending();
      await control('expire'); await diagnose(bulk(), '任务已过期');
      ready(); selection(); send('done'); return;
    }
    if (stage === 'task-commit-timeout') {
      await diagnose(button(ids[0]), '未按探针结果调整账号选择'); ready(); selection(); send('done'); return;
    }
    if (stage === 'task-unknown') {
      await diagnose(button(ids[0]), '状态未知'); pending();
      Array.from(document.querySelectorAll('#account-list input')).find(box => Number(box.value) === ids[2]).click();
      Array.from(document.querySelectorAll('#model-list input')).find(box => box.value === 'gpt-6-astra').click();
      get('bps-403-toggle').click();
      const changed = [ids[0], ids[1], ids[2], ids[3]];
      await saveForm(); pending(changed, editedModels);
      for (let attempt = 0; attempt < 2; attempt++) {
        await diagnose(bulk(), '状态未知', changed, editedModels); pending(changed, editedModels);
      }
      await control('complete'); await diagnose(bulk(), '未按探针结果调整账号选择', changed, editedModels);
      ready(); selection(changed, editedModels); send('done'); return;
    }
    if (stage === 'task-identity') {
      await diagnose(button(ids[0]), '不匹配'); pending();
      await control('wrong-target'); await diagnose(bulk(), '不匹配'); pending();
      await control('wrong-owner'); await diagnose(bulk(), '实例已改变'); pending();
      await control('complete'); await diagnose(bulk(), '未按探针结果调整账号选择');
      ready(); selection(); send('done'); return;
    }
    throw new Error('Unexpected task stage: ' + stage);
  } catch (error) { send('failure', { error: error.message }); }
}

export function createDiagnosticTaskHost(accounts, reopen, finish) {
  const clone = value => JSON.parse(JSON.stringify(value));
  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
  const sameSet = (a, b) => Array.isArray(a) && same(a.slice().sort(), b.slice().sort());
  const ids = accounts.map(account => account.id);
  const base = [ids[0], ids[1], ids[3]];
  const baseModels = ['gpt-6-sol', 'gpt-5.6-luna'];
  const models = ['gpt-6-astra', 'gpt-5.6-sol', 'gpt-6-sol', 'gpt-6-luna', 'gpt-5.6-terra', 'gpt-5.6-luna'];
  const scenarios = ['task-success', 'task-prepare-timeout', 'task-commit-timeout', 'task-unknown', 'task-identity', 'task-reopen-running'];
  const completed = [];
  let scenarioIndex = 0;
  let config, owner, tasks, commands, ordinarySaves, starts, explicitSaveAllowed, closedWindow, closedRequests;
  const assert = (ok, message) => { if (!ok) throw new Error(message); };
  function reset() {
    config = { account_ids: base.slice(), auto_select_new_accounts: false, enabled_models: baseModels.slice(),
      timeout_seconds: 123, auth_mode: 'chatgpt', rewrite_tools: false, bps_auto_disable_on_403: false };
    owner = 'browser-task-instance'; tasks = new Map(); commands = []; ordinarySaves = 0; starts = 0;
    explicitSaveAllowed = false; closedWindow = null; closedRequests = 0;
  }
  function complete(task) {
    const targets = task.targets;
    const statuses = targets.length === 1 ? ['degraded'] : ['ok', 'error', 'degraded', 'skipped', 'ok'];
    const check = { request_id: task.task_id, target_account_ids: targets.slice(), completed: true,
      degraded_account_ids: targets.filter((_id, i) => statuses[i] === 'degraded'),
      results: targets.map((id, i) => ({ account_id: id, name: 'foreign-result-name', status: statuses[i],
        ...(statuses[i] === 'ok' || statuses[i] === 'degraded' ? { answer: statuses[i] === 'ok' ? '苹果17' : '苹果16' } : { error: 'fixed local failure' }) })) };
    task.state = 'completed';
    task.result = { success: true, message: 'local task fixture', status_json: JSON.stringify({ degradation_check: check }) };
  }
  function taskInfo() {
    return { protocol: 'config-job-v1', owner_id: owner, available: true, tasks: [...tasks.values()].map(task => ({
      task_id: task.task_id, state: task.state, receipt: task.state === 'prepared' ? task.receipt : undefined, result: task.result })) };
  }
  function recordDone(stage) {
    const expectedTasks = stage === 'task-success-reopen' ? 2 : 1;
    assert(commands.length === (stage === 'task-prepare-timeout' ? 1 : expectedTasks * 2), 'Recovery repeated a prepare or commit');
    assert(starts === (stage === 'task-prepare-timeout' ? 0 : expectedTasks), 'Recovery started an unexpected diagnostic');
    assert(tasks.size === expectedTasks, 'Task identity was lost across queries');
    assert(!Object.hasOwn(config, 'diagnostic_task'), 'A task command leaked into an explicit save');
    assert(ordinarySaves === (['task-success-reopen', 'task-unknown', 'task-reopen-finished'].includes(stage) ? 1 : 0), 'Diagnostic operation unexpectedly saved the form');
    completed.push({ stage, prepareCount: tasks.size, commitCount: starts, ordinarySaves });
    scenarioIndex++;
    if (scenarioIndex === scenarios.length) {
      assert(closedRequests === 0, 'Closed configuration page continued to dispatch requests');
      finish(true, { pluginOnlyTaskScenarios: completed, pluginTaskSingleAndBulkRestored: true, taskSnapshotIsolated: true,
        prepareTimeoutNeverCommitted: true, commitTimeoutRecoveredByQuery: true, unknownOutcomeQueryOnly: true,
        taskAndOwnerMismatchRejected: true, taskReopenNeverReplayed: true, taskSelectorsAndBusyLocks: true });
    } else { reset(); reopen(scenarios[scenarioIndex]); }
  }
  return {
    start() { reset(); reopen(scenarios[0]); },
    observeDetached(event) {
      if (closedWindow && event.source === closedWindow && event.data?.source === 'sub2api-plugin-ui' && event.data.request_id) closedRequests++;
    },
    handle(data, responseWindow, token, stage) {
      const respond = payload => responseWindow.postMessage({ source: 'sub2api-plugin-host', bridge_token: token,
        type: data.type + '.result', request_id: data.request_id, ...payload }, '*');
      try {
        if (data.source === 'bps-ui-browser-test') {
          if (data.type === 'failure') throw new Error(data.error);
          if (data.type === 'fixture-control') {
            const task = [...tasks.values()].at(-1);
            if (data.action === 'before-explicit-save') {
              assert(commands.length === 4 && ordinarySaves === 0 && same(config.account_ids, base) && same(config.enabled_models, baseModels), 'Tasks persisted unsaved route or model edits');
              explicitSaveAllowed = true;
            } else if (data.action === 'expire') task.state = 'expired';
            else if (data.action === 'complete') { owner = 'browser-task-instance'; complete(task); }
            else if (data.action === 'wrong-target') {
              complete(task); const details = JSON.parse(task.result.status_json); details.degradation_check.target_account_ids = [ids[1]]; task.result.status_json = JSON.stringify(details);
            } else if (data.action === 'wrong-owner') owner = 'replacement-instance';
            else if (data.action === 'close-while-running') {
              const waitForCommit = () => {
                const current = [...tasks.values()].at(-1);
                if (!current || current.state !== 'running') return setTimeout(waitForCommit, 10);
                closedWindow = responseWindow; reopen('task-reopen-busy');
              };
              waitForCommit(); return;
            } else if (data.action === 'finish-and-reopen') {
              assert(commands.length === 2 && starts === 1 && ordinarySaves === 0, 'Reopening a running task caused a replay');
              complete(task);
              config.diagnostic_task = { protocol: 'config-job-v1', action: 'commit', task_id: task.task_id, owner_id: owner, receipt: task.receipt };
              reopen('task-reopen-finished'); return;
            } else throw new Error('Unknown task fixture action: ' + data.action);
            responseWindow.postMessage({ source: 'bps-task-fixture', bridge_token: token, request_id: data.request_id }, '*');
            return;
          }
          if (data.type === 'done') {
            if (stage === 'task-success') {
              assert(ordinarySaves === 1 && sameSet(config.account_ids, [ids[1], ids[2], ids[3]]), 'Explicit bulk selection save did not match');
              reopen('task-success-reopen'); return;
            }
            recordDone(stage);
          }
          return;
        }
        if (data.source !== 'sub2api-plugin-ui' || ['ui.resize', 'sub2api.plugin.ready', 'ui.notify'].includes(data.type)) return;
        assert(typeof data.request_id === 'string' && data.request_id, 'Task bridge request has no request ID');
        // This host deliberately never advertises config.testScoped.
        if (data.type === 'config.load') return respond({ ok: true, config: clone(config) });
        if (data.type === 'plugin.status') return respond({ ok: true, result: { healthy: true, status_json: JSON.stringify({
          accounts, available_models: models, enabled_models: config.enabled_models, diagnostic_tasks: taskInfo() }) } });
        assert(data.type !== 'config.test' && data.type !== 'config.testScoped', 'Unpatched task mode invoked a host test endpoint');
        assert(data.type === 'config.save', 'Unexpected task bridge request: ' + data.type);
        const command = data.config?.diagnostic_task;
        if (!command) {
          assert(stage !== 'task-success' || explicitSaveAllowed, 'Task auto-saved before an explicit click');
          assert(!Object.hasOwn(data.config, 'degradation_check') && !Object.hasOwn(data.config, 'degradation_check_account_id') && !Object.hasOwn(data.config, 'degradation_check_account_ids'), 'Ordinary save retained diagnostic targets');
          assert(!Object.hasOwn(data.config, 'bps_device_convergence'), 'Ordinary save includes removed device convergence setting');
          assert(data.config.timeout_seconds === 123 && data.config.auth_mode === 'chatgpt' && data.config.rewrite_tools === false, 'Task save changed unrelated settings');
          ordinarySaves++; config = clone(data.config); return respond({ ok: true, config: clone(config) });
        }
        assert(same(Object.keys(data.config), ['diagnostic_task']), 'Diagnostic outer config included routing fields');
        assert(command.protocol === 'config-job-v1' && command.owner_id === owner, 'Diagnostic changed protocol or owner');
        commands.push(clone(command));
        if (command.action === 'prepare') {
          assert(!tasks.has(command.task_id), 'Prepare was retried');
          assert(![...tasks.values()].some(task => ['prepared', 'running', 'unknown'].includes(task.state)), 'Concurrent task escaped the instance lock');
          const snapshot = command.config;
          const targets = snapshot.degradation_check_account_id ? [snapshot.degradation_check_account_id] : snapshot.degradation_check_account_ids;
          assert(snapshot.degradation_check === true && sameSet(snapshot.account_ids, base), 'Task changed account snapshot');
          assert(sameSet(snapshot.enabled_models, stage === 'task-success' ? ['gpt-6-astra', ...baseModels] : baseModels) && snapshot.bps_auto_disable_on_403 === (stage === 'task-success'), 'Task changed model or parameter snapshot');
          assert(!Object.hasOwn(snapshot, 'bps_device_convergence'), 'Task snapshot includes removed device convergence setting');
          assert(snapshot.timeout_seconds === 123 && snapshot.auth_mode === 'chatgpt' && snapshot.rewrite_tools === false, 'Task discarded unrelated snapshot parameters');
          assert(same(targets, stage === 'task-success' ? (tasks.size === 0 ? [ids[1]] : ids) : [ids[0]]), 'Task did not freeze explicit target IDs');
          tasks.set(command.task_id, { task_id: command.task_id, state: 'prepared', receipt: 'receipt-' + command.task_id, targets: clone(targets), config: clone(snapshot) });
          if (stage === 'task-prepare-timeout') return;
          return respond({ ok: true, config: clone(config) });
        }
        assert(command.action === 'commit', 'Unexpected diagnostic action');
        const task = tasks.get(command.task_id);
        assert(task && task.state === 'prepared' && task.receipt === command.receipt && command.config === undefined, 'Commit was repeated or changed its snapshot');
        starts++;
        if (stage === 'task-unknown') { task.state = 'unknown'; return respond({ ok: false, error: 'local lost commit acknowledgement' }); }
        if (stage === 'task-reopen-running') task.state = 'running'; else complete(task);
        if (stage === 'task-identity') { const details = JSON.parse(task.result.status_json); details.degradation_check.request_id = 'unrelated-task'; task.result.status_json = JSON.stringify(details); }
        if (stage === 'task-commit-timeout') return;
        respond({ ok: true, config: clone(config) });
      } catch (error) { finish(false, { error: stage + ': ' + error.message }); }
    },
  };
}

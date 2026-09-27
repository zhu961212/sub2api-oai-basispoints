"use strict";

// Fast regressions for the sandboxed configuration page. No browser, server,
// network access, third-party packages, or real timeout waits are required.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const root = path.resolve(__dirname, "..");
const appSource = fs.readFileSync(path.join(root, "ui/assets/app.js"), "utf8");
const bridgeSource = fs.readFileSync(path.join(root, "ui/assets/bridge-v1.js"), "utf8");
const htmlSource = fs.readFileSync(path.join(root, "ui/index.html"), "utf8");
const modelIDs = ["gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"];
const modelCatalogIDs = ["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna"];
const defaultModels = ["gpt-6-astra", "gpt-5.6-sol"];
const defaultAutoDegradation = { auto_degradation_enabled: false, auto_degradation_interval_minutes: 30, auto_degradation_manual_revision: 0, degradation_check_model: "gpt-5.4", native_timezone_by_ip: false };
const clone = (value) => JSON.parse(JSON.stringify(value));
const deferred = () => {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
};
const flush = async () => {
  await new Promise(setImmediate);
  await new Promise(setImmediate);
};

class Element {
  constructor(tagName, id = "") {
    this.tagName = tagName.toUpperCase();
    this.id = id;
    this.children = [];
    this.parentNode = null;
    this.listeners = new Map();
    this.attributes = new Map();
    this.disabled = false;
    this.hidden = false;
    this.checked = false;
    this.value = "";
    this.className = "";
    this.scrollHeight = 200;
    this._textContent = "";
  }
  set textContent(value) {
    this._textContent = String(value);
    this.children = [];
  }
  get textContent() {
    return this._textContent;
  }
  appendChild(child) {
    child.parentNode = this;
    this.children.push(child);
    return child;
  }
  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }
  getAttribute(name) {
    return this.attributes.has(name) ? this.attributes.get(name) : null;
  }
  addEventListener(type, listener) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(listener);
  }
  removeEventListener(type, listener) {
    this.listeners.set(type, (this.listeners.get(type) || []).filter((item) => item !== listener));
  }
  emit(type) {
    const event = { type, target: this, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
    for (const listener of this.listeners.get(type) || []) listener(event);
    return event;
  }
  isDisabled() {
    for (let node = this; node; node = node.parentNode) {
      if (node.disabled && (node === this || node.tagName === "FIELDSET")) return true;
    }
    return false;
  }
  click() {
    if (!this.isDisabled() && !this.hidden) return this.emit("click");
  }
  userCheck(checked) {
    if (this.isDisabled()) return;
    this.checked = checked;
    this.emit("change");
  }
  userInput(value) {
    if (this.isDisabled()) return;
    this.value = value;
    this.emit("input");
  }
}

function descendants(element) {
  return element.children.flatMap((child) => [child, ...descendants(child)]);
}

function hasClass(element, className) {
  return element.className.split(/\s+/).includes(className);
}

function createPage(options = {}) {
  const store = options.store || { config: { account_ids: [] } };
  const ids = {};
  const tags = {
    "config-form": "form",
    "account-fields": "fieldset",
    "account-list": "div",
    "account-hint": "span",
    "model-fields": "fieldset",
    "model-list": "div",
    "model-hint": "span",
    "state-chip": "span",
    "form-hint": "p",
    "version-line": "footer",
    "save-button": "button",
    "select-all-button": "button",
    "degradation-check-button": "button",
    "bps-403-toggle": "button",
    "auto-degradation-fields": "fieldset",
    "auto-degradation-toggle": "button",
    "auto-degradation-interval": "input",
    "degradation-check-model": "input",
    "auto-degradation-status": "span",
    "native-environment-fields": "fieldset",
    "native-timezone-toggle": "button",
    "degradation-result": "div",
    "account-check-hint": "span",
    "diagnostic-task-status": "span",
    "retry-button": "button",
  };
  for (const [id, tagName] of Object.entries(tags)) {
    const element = ids[id] = new Element(tagName, id);
    const openingTag = htmlSource.match(new RegExp("<[^>]*\\bid=[\"']" + id + "[\"'][^>]*>"));
    assert.ok(openingTag, "Configuration HTML must include #" + id);
    element.disabled = /\sdisabled(?:\s|=|>)/.test(openingTag[0]);
    element.hidden = /\shidden(?:\s|=|>)/.test(openingTag[0]);
  }
  ids["config-form"].appendChild(ids["account-fields"]);
  ids["config-form"].appendChild(ids["model-fields"]);
  ids["config-form"].appendChild(ids["auto-degradation-fields"]);
  ids["config-form"].appendChild(ids["native-environment-fields"]);
  ids["native-environment-fields"].appendChild(ids["native-timezone-toggle"]);
  for (const id of ["auto-degradation-toggle", "auto-degradation-interval", "degradation-check-model", "auto-degradation-status"]) {
    ids["auto-degradation-fields"].appendChild(ids[id]);
  }
  ids["model-fields"].appendChild(ids["model-list"]);
  ids["model-fields"].appendChild(ids["model-hint"]);
  ids["account-fields"].appendChild(ids["select-all-button"]);
  ids["account-fields"].appendChild(ids["degradation-check-button"]);
  ids["account-fields"].appendChild(ids["bps-403-toggle"]);
  ids["account-fields"].appendChild(ids["account-list"]);
  ids["account-fields"].appendChild(ids["account-hint"]);
  ids["account-fields"].appendChild(ids["degradation-result"]);
  ids["config-form"].appendChild(ids["save-button"]);
  ids["config-form"].appendChild(ids["retry-button"]);
  const calls = { load: 0, save: [], test: 0, status: 0, ready: 0, elements: 0, resize: [], intervals: 0, clearedIntervals: 0, disposed: 0, observerDisconnected: 0 };
  const status = options.status || {
    healthy: true,
    message: "ready",
    status_json: JSON.stringify({ accounts: options.accounts || [
      { id: 1, name: "First", schedulable: true },
      { id: 2, name: "Second", schedulable: true },
      { id: 3, name: "Third", schedulable: true },
    ] }),
  };
  const bridge = options.bridge || {
    hasToken: true,
    supportsScopedTest() { return options.scoped === true; },
    testScoped(config) {
      calls.scoped = calls.scoped || [];
      calls.scoped.push(clone(config));
      return options.testScoped ? options.testScoped(clone(config)) : Promise.resolve(scopedResult(config));
    },
    ready() { calls.ready++; },
    resize(height) { calls.resize.push(height); },
    dispose() { calls.disposed++; },
    status() { calls.status++; return options.getStatus ? options.getStatus(calls.status, store) : Promise.resolve(clone(status)); },
    loadConfig() {
      calls.load++;
      return options.load ? options.load(calls.load, store) : Promise.resolve(clone(store.config));
    },
    saveConfig(config) {
      calls.save.push(clone(config));
      if (options.save) return options.save(clone(config), store);
      store.config = clone(config);
      return Promise.resolve(clone(store.config));
    },
    testConfig() {
      calls.test++;
      if (options.test) return options.test(calls.test, store);
      return Promise.resolve({ status_json: JSON.stringify({
        degradation_check: { completed: true, degraded_account_ids: [], results: [] },
      }) });
    },
  };
  const windowListeners = new Map();
  let pollStatus;
  const window = {
    Sub2APIPluginBridge: { create: () => bridge },
    requestAnimationFrame(callback) { callback(); },
    addEventListener(type, listener) { windowListeners.set(type, listener); },
    setInterval(callback) { calls.intervals++; pollStatus = callback; return 1; },
    clearInterval() { calls.clearedIntervals++; pollStatus = null; },
    ResizeObserver: class {
      observe() {}
      disconnect() { calls.observerDisconnected++; }
    },
  };
  const document = {
    getElementById: (id) => ids[id] || null,
    createElement: (tagName) => { calls.elements++; return new Element(tagName); },
    body: new Element("body"),
    documentElement: new Element("html"),
    visibilityState: "visible",
  };
  vm.runInNewContext(appSource, { window, document, console }, { filename: "ui/assets/app.js" });
  const boxes = () => descendants(ids["account-list"]).filter((node) => node.tagName === "INPUT");
  const modelBoxes = () => descendants(ids["model-list"]).filter((node) => node.tagName === "INPUT");
  const accountRow = (accountID) => {
    const row = ids["account-list"].children.find((element) => descendants(element).some((node) =>
      node.tagName === "INPUT" && Number(node.value) === accountID));
    assert.ok(row, "Account #" + accountID + " must have a row");
    const nodes = descendants(row);
    return {
      row,
      checkbox: nodes.find((node) => node.tagName === "INPUT"),
      name: nodes.find((node) => hasClass(node, "account-name")),
      availability: nodes.find((node) => hasClass(node, "account-availability")),
      result: nodes.find((node) => hasClass(node, "account-check-result")),
      button: nodes.find((node) => hasClass(node, "account-check-button")),
    };
  };
  return {
    ids, calls, store,
    accountRow,
    selected: () => boxes().filter((box) => box.checked).map((box) => Number(box.value)).sort((a, b) => a - b),
    selectedModels: () => modelBoxes().filter((box) => box.checked).map((box) => box.value),
    modelOptions: () => modelBoxes().map((box) => box.value),
    checkModel(model, checked) {
      const box = modelBoxes().find((item) => item.value === model);
      assert.ok(box, "Model " + model + " must be rendered");
      box.userCheck(checked);
    },
    check(accountID, checked) {
      const box = boxes().find((item) => Number(item.value) === accountID);
      assert.ok(box, "Account #" + accountID + " must be rendered");
      box.userCheck(checked);
    },
    save: () => ids["save-button"].click(),
    selectAll: () => ids["select-all-button"].click(),
    degradationCheck: () => ids["degradation-check-button"].click(),
    toggle403: () => ids["bps-403-toggle"].click(),
    toggleAutoDegradation: () => ids["auto-degradation-toggle"].click(),
    toggleNativeTimezone: () => ids["native-timezone-toggle"].click(),
    checkAccount: (accountID) => {
      const button = accountRow(accountID).button;
      assert.ok(button, "Account #" + accountID + " must have its own diagnostic button");
      return button.click();
    },
    pollStatus: () => { if (pollStatus) pollStatus(); },
    unload: () => windowListeners.get("beforeunload")(),
    retry: () => ids["retry-button"].click(),
  };
}

test("request timezone toggle defaults off and saves its legacy key independently of automatic diagnostics", async () => {
  const page = createPage({ scoped: true, store: { config: { account_ids: [1], auto_degradation_enabled: true, auto_degradation_manual_revision: 12 } } });
  await flush();
  assert.equal(page.ids["native-timezone-toggle"].getAttribute("aria-pressed"), "false");
  assert.equal(page.ids["native-timezone-toggle"].textContent, "请求时区跟随出口 IP：已关闭");
  assert.ok(htmlSource.includes('<legend class="name">请求环境</legend>'));
  assert.match(htmlSource, /此开关只控制原生 Codex 和 BPS 业务请求的时区，独立于“自动检测与切换”/);
  assert.match(htmlSource, /降智检测始终只请求原生 Codex，不检测 BPS/);
  assert.match(htmlSource, /ipapi.co.*6 小时.*5 分钟/);
  assert.match(htmlSource, /关闭时不查询 IP.*失败保留原请求/);
  assert.match(htmlSource, /不保证改善降智/);
  page.toggleNativeTimezone();
  assert.match(page.ids["native-timezone-toggle"].textContent, /已开启.*待保存/);
  assert.equal(page.calls.save.length, 0);
  assert.equal(page.calls.test, 0);
  page.pollStatus();
  await flush();
  assert.equal(page.ids["native-timezone-toggle"].getAttribute("aria-pressed"), "true");
  page.checkAccount(1);
  await flush();
  assert.equal(page.calls.scoped[0].native_timezone_by_ip, true, "The manual diagnostic uses the current explicit form choice");
  assert.equal(page.calls.save.length, 0);
  page.save();
  await flush();
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  assert.equal(page.store.config.native_timezone_by_ip, true);
  assert.equal(page.store.config.auto_degradation_enabled, true);
  assert.equal(page.store.config.auto_degradation_manual_revision, 12);
  assert.deepEqual(page.store.config.account_ids, [1]);
  const reopened = createPage({ store: page.store });
  await flush();
  assert.equal(reopened.ids["native-timezone-toggle"].getAttribute("aria-pressed"), "true");
  reopened.toggleNativeTimezone();
  reopened.save();
  await flush();
  assert.equal(reopened.store.config.native_timezone_by_ip, false);
  assert.equal(reopened.store.config.auto_degradation_enabled, true);
  assert.equal(reopened.store.config.auto_degradation_manual_revision, 12);
});

test("request timezone setting must survive save acknowledgement and verification", async t => {
  for (const phase of ["save", "load"]) {
    await t.test(phase, async () => {
      const page = createPage({
        save: (config, store) => { store.config = clone(config); return Promise.resolve(phase === "save" ? { ...config, native_timezone_by_ip: false } : config); },
        load: (count, store) => Promise.resolve(count > 1 && phase === "load" ? { ...store.config, native_timezone_by_ip: false } : clone(store.config)),
      });
      await flush();
      page.toggleNativeTimezone();
      page.save();
      await flush();
      assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
      assert.match(page.ids["form-hint"].textContent, /请求时区开关.*不一致/);
      assert.equal(page.ids["native-timezone-toggle"].getAttribute("aria-pressed"), "true");
      assert.equal(page.ids["native-timezone-toggle"].disabled, false);
    });
  }
});

test("request timezone toggle stays locked while loading saving and verifying", async () => {
  const initial = deferred();
  const saving = deferred();
  const verification = deferred();
  const page = createPage({ load: number => number === 1 ? initial.promise : verification.promise, save: () => saving.promise });
  await flush();
  page.ids["native-timezone-toggle"].emit("click");
  assert.equal(page.ids["native-timezone-toggle"].getAttribute("aria-pressed"), "false");
  initial.resolve({ account_ids: [1], native_timezone_by_ip: false });
  await flush();
  page.toggleNativeTimezone();
  page.save();
  assert.equal(page.ids["native-environment-fields"].disabled, true);
  page.ids["native-timezone-toggle"].emit("click");
  assert.equal(page.ids["native-timezone-toggle"].getAttribute("aria-pressed"), "true");
  saving.resolve(clone(page.calls.save[0]));
  await flush();
  assert.equal(page.ids["native-timezone-toggle"].disabled, true);
  verification.resolve(clone(page.calls.save[0]));
  await flush();
  assert.equal(page.ids["native-timezone-toggle"].disabled, false);
  assert.equal(page.ids["native-environment-fields"].disabled, false);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("automatic native diagnostics default off and settings survive saving and reopening", async () => {
  const page = createPage({ scoped: true });
  await flush();
  assert.equal(page.ids["auto-degradation-toggle"].getAttribute("aria-pressed"), "false");
  assert.equal(page.ids["auto-degradation-interval"].value, "30");
  assert.equal(page.ids["degradation-check-model"].value, "gpt-5.4");
  page.toggleAutoDegradation();
  assert.match(page.ids["auto-degradation-toggle"].textContent, /已开启.*待保存/);
  assert.equal(page.calls.save.length, 0);
  assert.equal(page.accountRow(1).checkbox.disabled, true);
  page.ids["auto-degradation-interval"].userInput("45");
  page.ids["degradation-check-model"].userInput("gpt-5.4-mini");
  page.save();
  await flush();
  assert.equal(page.store.config.auto_degradation_enabled, true);
  assert.equal(page.store.config.auto_degradation_interval_minutes, 45);
  assert.equal(page.store.config.degradation_check_model, "gpt-5.4-mini");
  assert.equal(page.calls.test, 0);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  const reopened = createPage({ store: page.store });
  await flush();
  assert.equal(reopened.ids["auto-degradation-toggle"].getAttribute("aria-pressed"), "true");
  assert.equal(reopened.ids["auto-degradation-interval"].value, "45");
  assert.equal(reopened.ids["degradation-check-model"].value, "gpt-5.4-mini");
});

test("automatic diagnostic inputs reject invalid values without writes or requests", async t => {
  for (const [field, value] of [
    ["auto-degradation-interval", ""], ["auto-degradation-interval", "4"],
    ["auto-degradation-interval", "1441"], ["auto-degradation-interval", "5.5"],
    ["degradation-check-model", ""], ["degradation-check-model", "gpt 5.4"],
    ["degradation-check-model", "gpt" + String.fromCharCode(0) + "5.4"],
    ["degradation-check-model", "x".repeat(129)],
  ]) {
    await t.test(field + " / " + JSON.stringify(value), async () => {
      const page = createPage({ scoped: true });
      await flush();
      page.ids[field].userInput(value);
      page.save();
      page.degradationCheck();
      await flush();
      assert.equal(page.calls.save.length, 0);
      assert.equal((page.calls.scoped || []).length, 0);
      assert.match(page.ids["form-hint"].textContent, /间隔|原生检测模型/);
    });
  }
});

test("automatic status drives displayed routes while saves preserve the routing baseline", async () => {
  const baseline = { account_ids: [1, 2], excluded_account_ids: [3], auto_select_new_accounts: true };
  const store = { config: { ...baseline, auto_degradation_enabled: true } };
  const runtime = { enabled: true, running: false, interval_minutes: 30,
    last_run_at: "2026-09-27T12:00:00Z", next_run_at: "2026-09-27T12:05:00Z", accounts: [
      { account_id: 1, status: "ok", bps_enabled: false, managed: true },
      { account_id: 2, status: "degraded", bps_enabled: true, managed: true },
      { account_id: 3, status: "error", bps_enabled: false, managed: false },
    ] };
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [1, 2, 3].map(id => ({ id, schedulable: true })), auto_degradation: runtime }) };
  const page = createPage({ store, status, scoped: true });
  await flush();
  assert.deepEqual(page.selected(), [2]);
  assert.match(page.accountRow(1).availability.textContent, /原生 Codex.*自动管理/);
  assert.match(page.accountRow(2).availability.textContent, /BPS 已启用.*自动管理/);
  assert.match(page.ids["auto-degradation-status"].textContent, /上次.*下次/);
  page.check(1, true);
  page.selectAll();
  page.accountRow(1).checkbox.checked = true;
  page.accountRow(1).checkbox.emit("change");
  assert.deepEqual(page.selected(), [2]);
  page.checkModel("gpt-6-sol", true);
  page.save();
  await flush();
  for (const key of Object.keys(baseline)) assert.deepEqual(store.config[key], baseline[key]);
  assert.deepEqual(page.selected(), [2]);
  page.toggleAutoDegradation();
  assert.equal(page.accountRow(1).checkbox.disabled, true, "Closing is pending until the save is confirmed");
  runtime.enabled = false;
  status.status_json = JSON.stringify({ accounts: [1, 2, 3].map(id => ({ id, schedulable: true })), auto_degradation: runtime });
  page.save();
  await flush();
  assert.equal(store.config.auto_degradation_enabled, false);
  for (const key of Object.keys(baseline)) assert.deepEqual(store.config[key], baseline[key]);
  assert.deepEqual(page.selected(), [2]);
  assert.equal(page.accountRow(1).checkbox.disabled, false);
  page.check(1, true);
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selected(), [1, 2], "Status must retain unsaved manual changes after closing automation");
  page.save();
  await flush();
  assert.deepEqual(store.config.account_ids.slice().sort(), [1, 2]);
  assert.deepEqual(store.config.excluded_account_ids, [3]);
});

test("automatic bulk checks only display results and BPS blocks still permit native checks", async () => {
  const details = JSON.parse(bpsStatus([[2, bpsBlockA]]).status_json);
  details.auto_degradation = { enabled: true, accounts: [
    { account_id: 1, status: "ok", bps_enabled: false, managed: true },
    { account_id: 2, status: "degraded", bps_enabled: false, managed: true },
    { account_id: 3, status: "error", bps_enabled: true, managed: false },
  ] };
  const status = { healthy: true, status_json: JSON.stringify(details) };
  const page = createPage({ scoped: true, status, store: { config: { account_ids: [1, 2, 3], auto_degradation_enabled: true } },
    testScoped: config => Promise.resolve(scopedResult(config, { 1: { status: "degraded", answer: "苹果16" } })) });
  await flush();
  assert.deepEqual(page.selected(), [3]);
  assert.equal(page.accountRow(2).button.disabled, false);
  page.degradationCheck();
  await flush();
  assert.deepEqual(page.selected(), [3]);
  assert.equal(page.calls.save.length, 0);
  assert.equal(page.calls.scoped[0].degradation_check_model, "gpt-5.4");
  assert.match(page.ids["form-hint"].textContent, /未按探针结果调整/);
});

test("automatic state shows confirmation and errors without overriding effective routes", async () => {
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1, schedulable: true }], auto_degradation: {
    enabled: true, error: "native unavailable", accounts: [{ account_id: 1, status: "error", bps_enabled: true, managed: true, pending_status: "ok", consecutive: 1 }],
  } }) };
  const page = createPage({ status, store: { config: { auto_degradation_enabled: true, account_ids: [] } } });
  await flush();
  assert.deepEqual(page.selected(), [1]);
  assert.ok(page.accountRow(1).result.textContent.includes("检测失败 · 待复测 1/2"));
  assert.match(page.ids["auto-degradation-status"].textContent, /检测异常，保留路由.*native unavailable/);
});

test("unready automatic state cannot overwrite selections or allow stale account edits", async () => {
  const details = { accounts: [{ id: 1, schedulable: true }, { id: 2, schedulable: true }], auto_degradation: {
    enabled: false, ready: false, accounts: [{ account_id: 1, bps_enabled: false, managed: true }],
  } };
  const status = { healthy: true, status_json: JSON.stringify(details) };
  const page = createPage({ status, store: { config: { account_ids: [1] } } });
  await flush();
  assert.deepEqual(page.selected(), [1]);
  assert.equal(page.accountRow(1).checkbox.disabled, true);
  assert.equal(page.accountRow(1).checkbox.indeterminate, true);
  assert.match(page.ids["auto-degradation-status"].textContent, /正在加载/);
  page.save();
  await flush();
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.equal(page.store.config.auto_select_new_accounts, undefined);
  details.auto_degradation.ready = true;
  status.status_json = JSON.stringify(details);
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selected(), []);
  assert.equal(page.accountRow(1).checkbox.disabled, false);
  assert.equal(page.accountRow(1).checkbox.indeterminate, false);
});

test("canceling an unsaved automatic toggle preserves previous manual edits", async () => {
  const page = createPage({ store: { config: { account_ids: [1] } } });
  await flush();
  page.check(2, true);
  page.toggleAutoDegradation();
  page.toggleAutoDegradation();
  assert.deepEqual(page.selected(), [1, 2]);
  page.save();
  await flush();
  assert.deepEqual(page.store.config.account_ids, [1, 2]);
  assert.equal(page.store.config.auto_degradation_enabled, false);
});

test("automatic settings must match save acknowledgement and confirmed read-back", async t => {
  for (const phase of ["save", "load"]) {
    await t.test(phase, async () => {
      const page = createPage({
        save: (config, store) => { store.config = clone(config); return Promise.resolve(phase === "save" ? { ...config, auto_degradation_enabled: false } : config); },
        load: (count, store) => Promise.resolve(count > 1 && phase === "load" ? { ...store.config, auto_degradation_interval_minutes: 60 } : clone(store.config)),
      });
      await flush();
      page.toggleAutoDegradation();
      page.save();
      await flush();
      assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
      assert.match(page.ids["form-hint"].textContent, /自动检测.*不一致/);
      assert.equal(page.ids["auto-degradation-toggle"].getAttribute("aria-pressed"), "true");
    });
  }
});

test("manual revision invalidates automatic routes when an explicit edit returns to the original baseline", async () => {
  const baseline = { account_ids: [1], excluded_account_ids: [7], auto_select_new_accounts: true };
  const store = { config: { ...baseline, auto_degradation_enabled: true, auto_degradation_manual_revision: 11 } };
  const page = createPage({ store, scoped: true, getStatus() {
    const managed = store.config.auto_degradation_manual_revision === 11;
    return Promise.resolve({ healthy: true, status_json: JSON.stringify({
      accounts: [1, 7].map(id => ({ id, schedulable: true })),
      auto_degradation: { ready: true, enabled: store.config.auto_degradation_enabled === true, accounts: [
        { account_id: 1, status: "pending", bps_enabled: true, managed: false },
        { account_id: 7, status: "degraded", bps_enabled: managed, managed },
      ] },
    }) });
  } });
  await flush();
  assert.deepEqual(page.selected(), [1, 7]);
  page.checkModel("gpt-6-sol", true);
  page.save();
  await flush();
  assert.equal(store.config.auto_degradation_manual_revision, 11, "Ordinary automatic saves preserve revision");
  page.toggleAutoDegradation();
  page.save();
  await flush();
  assert.equal(store.config.auto_degradation_manual_revision, 11, "Closing automation preserves the last automatic route");
  assert.deepEqual(page.selected(), [1, 7]);
  page.check(7, false);
  page.save();
  await flush();
  for (const key of Object.keys(baseline)) assert.deepEqual(store.config[key], baseline[key]);
  assert.equal(store.config.auto_degradation_manual_revision, 12, "Explicit intent changes revision even when IDs equal the original baseline");
  assert.deepEqual(page.selected(), [1]);
  page.save();
  await flush();
  assert.equal(store.config.auto_degradation_manual_revision, 12, "A save without account edits cannot change revision");
  page.check(7, true);
  page.check(7, false);
  page.checkAccount(1);
  await flush();
  assert.equal(page.calls.scoped.at(-1).auto_degradation_manual_revision, 12, "A manual probe does not persist route intent");
  page.save();
  await flush();
  assert.equal(store.config.auto_degradation_manual_revision, 13, "Edit intent matters even when the net form selection is unchanged");
});

test("manual route revision reaches its safe integer limit and refuses further edits", async () => {
  const page = createPage({ store: { config: { account_ids: [1], auto_degradation_manual_revision: Number.MAX_SAFE_INTEGER - 1 } } });
  await flush();
  page.check(2, true);
  page.save();
  await flush();
  assert.equal(page.store.config.auto_degradation_manual_revision, Number.MAX_SAFE_INTEGER);
  page.save();
  await flush();
  assert.equal(page.calls.save.length, 2, "Unedited saves remain possible at the limit");
  page.check(2, false);
  page.save();
  await flush();
  assert.equal(page.calls.save.length, 2);
  assert.equal(page.store.config.auto_degradation_manual_revision, Number.MAX_SAFE_INTEGER);
  assert.match(page.ids["form-hint"].textContent, /手动账号选择版本已达到上限/);
  assert.deepEqual(page.selected(), [1], "The rejected edit remains visible for recovery");
});

test("configuration actions work without sandboxed form submission", () => {
  for (const id of ["save-button", "retry-button", "select-all-button", "bps-403-toggle"]) {
    const tag = htmlSource.match(new RegExp("<button\\b[^>]*\\bid=[\"']" + id + "[\"'][^>]*>"));
    assert.ok(tag, id + " exists");
    assert.match(tag[0], /\btype=["']button["']/);
  }
});

test("six model choices default to Astra and 5.6 Sol", async (t) => {
  for (const config of [
    { account_ids: [] },
    { account_ids: [], enabled_models: null },
  ]) {
    await t.test(JSON.stringify(config), async () => {
      const page = createPage({ store: { config } });
      assert.equal(page.ids["model-fields"].disabled, true);
      await flush();
      assert.deepEqual(page.modelOptions(), modelIDs);
      assert.deepEqual(page.selectedModels(), defaultModels);
      assert.equal(page.ids["model-fields"].disabled, false);
      assert.match(page.ids["model-hint"].textContent, /未选模型原样透传/);
    });
  }
});

test("every model subset saves and survives reopening, including none and all six", async (t) => {
  for (let mask = 0; mask < 1 << modelIDs.length; mask++) {
    const expected = modelIDs.filter((_, index) => mask & 1 << index);
    await t.test("subset " + mask, async () => {
      const store = { config: { account_ids: [2] } };
      const page = createPage({ store });
      await flush();
      for (const model of modelIDs) page.checkModel(model, expected.includes(model));
      page.save();
      await flush();
      assert.deepEqual(store.config.enabled_models, modelCatalogIDs.filter((model) => expected.includes(model)));
      assert.deepEqual(store.config.account_ids, [2]);
      assert.match(page.ids["form-hint"].textContent, /已保存/);
      const reopened = createPage({ store });
      await flush();
      assert.deepEqual(reopened.selectedModels(), expected);
      assert.deepEqual(reopened.selected(), [2]);
      if (!mask) assert.match(reopened.ids["model-hint"].textContent, /所有模型原样透传，不走 Basis Points/);
    });
  }
});

test("model loading trims IDs and deduplicates in catalog order before saving", async () => {
  const store = { config: { account_ids: [], enabled_models: [" gpt-5.6-luna ", "\tgpt-6-sol\n", "gpt-5.6-luna", null, 42] } };
  const page = createPage({ store });
  await flush();
  assert.deepEqual(page.selectedModels(), ["gpt-6-sol", "gpt-5.6-luna"]);
  page.save();
  await flush();
  assert.deepEqual(store.config.enabled_models, ["gpt-6-sol", "gpt-5.6-luna"]);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("model controls stay locked while loading, saving, and verifying", async () => {
  const initial = deferred();
  const saving = deferred();
  const verification = deferred();
  const page = createPage({
    load: (number) => number === 1 ? initial.promise : verification.promise,
    save: () => saving.promise,
  });
  await flush();
  page.checkModel("gpt-6-luna", true);
  assert.deepEqual(page.selectedModels(), defaultModels);
  initial.resolve({ account_ids: [], enabled_models: ["gpt-6-sol"] });
  await flush();
  page.checkModel("gpt-6-luna", true);
  page.save();
  assert.equal(page.ids["model-fields"].disabled, true);
  page.checkModel("gpt-6-astra", true);
  saving.resolve(clone(page.calls.save[0]));
  await flush();
  assert.equal(page.ids["model-fields"].disabled, true);
  page.checkModel("gpt-6-sol", false);
  assert.deepEqual(page.selectedModels(), ["gpt-6-sol", "gpt-6-luna"]);
  verification.resolve({ ...clone(page.calls.save[0]), enabled_models: ["gpt-6-luna", "gpt-6-sol"] });
  await flush();
  assert.equal(page.ids["model-fields"].disabled, false);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("rejected or inconsistent model saves retain local choices for retry", async (t) => {
  for (const failure of ["rejected", "acknowledgement", "read-back", "verification failed", "empty normalized to null"]) {
    await t.test(failure, async () => {
      const store = { config: { account_ids: [2], enabled_models: defaultModels } };
      let attempt = 0;
      const page = createPage({
        store,
        save(config, saved) {
          attempt++;
          if (attempt > 1) {
            saved.config = clone(config);
            return Promise.resolve(clone(saved.config));
          }
          if (failure === "rejected") return Promise.reject(new Error("save unavailable"));
          if (failure === "acknowledgement") return Promise.resolve(clone(saved.config));
          if (failure === "empty normalized to null") return Promise.resolve({ ...config, enabled_models: null });
          return Promise.resolve(config);
        },
        load(number, saved) {
          return number === 2 && failure === "verification failed"
            ? Promise.reject(new Error("verify unavailable"))
            : Promise.resolve(clone(saved.config));
        },
      });
      await flush();
      const expected = failure === "empty normalized to null" ? [] : ["gpt-6-luna"];
      for (const model of modelIDs) page.checkModel(model, expected.includes(model));
      page.save();
      await flush();
      assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
      assert.match(page.ids["form-hint"].textContent, /失败|不一致|未确认/);
      assert.deepEqual(page.selectedModels(), expected);
      assert.deepEqual(page.selected(), [2]);
      assert.equal(page.ids["model-fields"].disabled, false);
      page.save();
      await flush();
      assert.match(page.ids["form-hint"].textContent, /已保存/);
      assert.deepEqual(store.config.enabled_models, expected);
    });
  }
});

test("status polling shows saved model state without overwriting unsaved model or account choices", async () => {
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }], enabled_models: defaultModels }) };
  const page = createPage({ status });
  await flush();
  page.checkModel("gpt-6-luna", true);
  page.check(1, true);
  status.status_json = JSON.stringify({ accounts: [{ id: 1 }], enabled_models: [] });
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selectedModels(), ["gpt-6-astra", "gpt-6-luna", "gpt-5.6-sol"]);
  assert.deepEqual(page.selected(), [1]);
  assert.match(page.ids["version-line"].textContent, /未启用模型（全部透传）/);
});

test("select all preserves hidden saved IDs and adds every displayed account only once", async () => {
  const store = { config: { account_ids: [99, 2], timeout_seconds: 123 } };
  const page = createPage({ store, accounts: [
    { id: 1, name: "First", schedulable: true },
    { id: 2, name: "Second", schedulable: true },
    { id: 3, name: "Paused", schedulable: false, status: "paused" },
    { id: 2, name: "Duplicate", schedulable: true },
    null, { id: 0 }, { id: -1 }, { id: 1.5 }, { id: "4" }, { id: Number.MAX_SAFE_INTEGER + 1 },
  ] });
  assert.equal(page.ids["select-all-button"].disabled, true);
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, false);
  page.selectAll();
  assert.deepEqual(page.selected(), [1, 2, 3], "Each account must have exactly one checkbox");
  assert.equal(page.ids["select-all-button"].disabled, false);
  assert.equal(page.ids["select-all-button"].textContent, "已全选列表账号");
  assert.equal(page.calls.save.length, 0, "Selecting accounts must still require Save");
  assert.match(page.ids["account-hint"].textContent, /已勾选 4 个账号/);
  page.check(2, false);
  assert.deepEqual(page.selected(), [1, 3], "Unchecking an account must leave no duplicate checked row");
  page.check(1, false);
  assert.equal(page.ids["select-all-button"].disabled, false);
  page.selectAll();
  page.selectAll();
  page.save();
  await flush();
  assert.deepEqual(store.config.account_ids.slice().sort((a, b) => a - b), [1, 2, 3, 99]);
  assert.equal(store.config.timeout_seconds, 123);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("select all is disabled when the list is empty and preserves hidden saved IDs", async () => {
  const page = createPage({ accounts: [], store: { config: { account_ids: [99] } } });
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, true);
  page.selectAll();
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[0].account_ids, [99]);
});

test("account rows show complete names with secondary IDs and separate diagnostic controls", async () => {
  const names = ["shared-first@example.com", "shared-second@example.com", "短名", "😀一二三四五六七", "同名账号", "同名账号", "完整超长账号名称-".repeat(20), "<img src=x onerror=alert(1)>"];
  const page = createPage({ accounts: names.map((name, index) => ({ id: index + 1, name, schedulable: true })) });
  await flush();
  for (let index = 0; index < names.length; index++) {
    const { row, name, availability, result, button, checkbox } = page.accountRow(index + 1);
    assert.ok(hasClass(row, "account-row"));
    assert.equal(name.textContent, names[index]);
    assert.equal(name.children.length, 0, "Account names are plain text, never parsed markup");
    assert.equal(name.title, "账号 ID：" + (index + 1));
    assert.equal(button.getAttribute("aria-label"), "检测账号 ID：" + (index + 1));
    assert.ok(availability);
    assert.equal(availability.textContent, "#" + (index + 1) + " · 可用");
    assert.ok(result);
    assert.equal(button.textContent, "降智检测");
    assert.equal(button.type, "button");
    assert.equal(button.value, String(index + 1));
    assert.equal(checkbox.parentNode.tagName, "LABEL");
    assert.ok(!descendants(checkbox.parentNode).includes(button), "Diagnostic clicks cannot toggle the checkbox label");
  }
  assert.notEqual(page.accountRow(1).name.textContent, page.accountRow(2).name.textContent);
  assert.equal(page.accountRow(5).name.textContent, page.accountRow(6).name.textContent);
  assert.notEqual(page.accountRow(5).availability.textContent, page.accountRow(6).availability.textContent);
});

test("account rows never truncate long IDs when names are missing", async () => {
  const page = createPage({ accounts: [{ id: 123456789, schedulable: true }] });
  await flush();
  const { name, button } = page.accountRow(123456789);
  assert.equal(name.textContent, "123456789");
  assert.match(name.title, /123456789/);
  assert.equal(button.value, "123456789");
});

test("empty or non-string account names fall back to the full account ID", async (t) => {
  for (const value of [undefined, null, "", "   ", String.fromCharCode(9, 10), 123, {}, []]) {
    await t.test(JSON.stringify(value) || "undefined", async () => {
      const page = createPage({ accounts: [{ id: 1234567890123456, name: value, schedulable: true }] });
      await flush();
      const row = page.accountRow(1234567890123456);
      assert.equal(row.name.textContent, "1234567890123456");
      assert.equal(row.availability.textContent, "#1234567890123456 · 可用");
      assert.equal(row.button.value, "1234567890123456");
    });
  }
});


test("diagnostic names use safe text and directory renames refresh cached rows and reports", async () => {
  const id = 1234567890123456;
  const name = "<svg onload=alert(1)>😀完整账号名称";
  const check = { completed: true, degraded_account_ids: [], results: [
    { account_id: id, name: "outdated-name", status: "ok", answer: "苹果17" },
    { account_id: 99, name: "<img src=x onerror=alert(1)>", status: "error", error: "HTTP 429" },
    { account_id: 1234567999999999, status: "skipped", error: "unavailable" },
  ] };
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id, name, schedulable: true }], degradation_check: check }) };
  const page = createPage({ status });
  await flush();
  const report = page.ids["degradation-result"];
  assert.ok(report.children[1].textContent.startsWith(name + "（#" + id + "）"));
  assert.ok(report.children[2].textContent.startsWith("<img src=x onerror=alert(1)>（#99）"));
  assert.ok(report.children[3].textContent.startsWith("#1234567999999999 · "));
  for (const line of report.children) assert.equal(line.children.length, 0);
  const oldRow = page.accountRow(id).row;
  status.status_json = JSON.stringify({ accounts: [{ id, name: "重命名后的完整账号😀", schedulable: true }] });
  page.pollStatus();
  await flush();
  assert.equal(page.accountRow(id).name.textContent, "重命名后的完整账号😀");
  assert.notEqual(page.accountRow(id).row, oldRow);
  assert.ok(report.children[1].textContent.startsWith("重命名后的完整账号😀（#" + id + "）"));
  assert.match(page.accountRow(id).result.textContent, /符合检测规则/);
  const stableRow = page.accountRow(id).row;
  const stableReport = report.children[1];
  const created = page.calls.elements;
  page.pollStatus();
  await flush();
  assert.equal(page.calls.elements, created);
  assert.equal(page.accountRow(id).row, stableRow);
  assert.equal(report.children[1], stableReport);
});

test("403 auto-disable defaults on and both settings survive saving and reopening", async () => {
  const store = { config: { account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2] } };
  let page = createPage({ store });
  assert.equal(page.ids["bps-403-toggle"].disabled, true);
  await flush();
  assert.equal(page.ids["bps-403-toggle"].getAttribute("aria-pressed"), "true");
  for (const expected of [false, true]) {
    page.toggle403();
    assert.equal(page.ids["bps-403-toggle"].getAttribute("aria-pressed"), String(expected));
    assert.match(page.ids["bps-403-toggle"].textContent, /待保存/);
    assert.equal(page.calls.save.length, 0, "Toggling requires an explicit save");
    page.pollStatus();
    await flush();
    assert.equal(page.ids["bps-403-toggle"].getAttribute("aria-pressed"), String(expected));
    page.save();
    assert.equal(page.ids["bps-403-toggle"].disabled, true);
    page.toggle403();
    await flush();
    assert.equal(store.config.bps_auto_disable_on_403, expected);
    assert.deepEqual(page.selected(), [1, 3]);
    assert.deepEqual(store.config.excluded_account_ids, [2]);
    assert.match(page.ids["form-hint"].textContent, /已保存/);
    assert.doesNotMatch(page.ids["bps-403-toggle"].textContent, /待保存/);
    page = createPage({ store });
    await flush();
    assert.equal(page.ids["bps-403-toggle"].getAttribute("aria-pressed"), String(expected));
  }
});

test("403 auto-disable rejects a dropped setting in save acknowledgement or reload", async (t) => {
  for (const phase of ["save", "reload"]) {
    await t.test(phase, async () => {
      const page = createPage({
        save(config, store) {
          store.config = clone(config);
          const reply = clone(config);
          if (phase === "save") delete reply.bps_auto_disable_on_403;
          return Promise.resolve(reply);
        },
        load(number, store) {
          const reply = clone(store.config);
          if (number > 1 && phase === "reload") delete reply.bps_auto_disable_on_403;
          return Promise.resolve(reply);
        },
      });
      await flush();
      page.toggle403();
      page.save();
      await flush();
      assert.match(page.ids["form-hint"].textContent, /403 自动停用开关与提交内容不一致/);
      assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
      assert.equal(page.ids["bps-403-toggle"].disabled, false);
    });
  }
});

test("configuration UI no longer exposes device convergence", async () => {
  assert.doesNotMatch(htmlSource, /bps-device-toggle|bps-device-hint/);
  assert.doesNotMatch(appSource, /bps_device_convergence/);
  const page = createPage();
  await flush();
  page.save();
  await flush();
  assert.equal(Object.hasOwn(page.calls.save[0], "bps_device_convergence"), false);
  assert.equal(Object.hasOwn(page.store.config, "bps_device_convergence"), false);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("ordinary saves discard a stale single-account diagnostic selector", async () => {
  const page = createPage({ store: { config: { account_ids: [1], degradation_check: true, degradation_check_account_id: 2 } } });
  await flush();
  page.save();
  await flush();
  assert.equal(page.calls.test, 0);
  assert.equal(page.store.config.degradation_check, false);
  assert.ok(!page.store.config.degradation_check_account_id);
});







test("an invalid-only account list shows the empty state and retains saved selections", async () => {
  const page = createPage({
    accounts: [null, { id: 0 }, { id: -1 }, { id: 1.5 }, { id: "4" }],
    store: { config: { account_ids: [99] } },
  });
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, true);
  assert.deepEqual(page.selected(), []);
  assert.match(page.ids["account-list"].children[0]?.textContent || "", /暂未获取账号列表/);
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[0].account_ids, [99]);
});

test("a rejected save preserves selection and unlocks select all for retry", async () => {
  const pending = deferred();
  let attempts = 0;
  const page = createPage({
    store: { config: { account_ids: [99] } },
    save(config, saved) {
      if (++attempts === 1) return pending.promise;
      saved.config = clone(config);
      return Promise.resolve(clone(saved.config));
    },
  });
  await flush();
  page.check(2, true);
  page.save();
  assert.equal(page.ids["select-all-button"].disabled, true);
  pending.reject(new Error("save unavailable"));
  await flush();
  assert.match(page.ids["form-hint"].textContent, /保存失败.*save unavailable/);
  assert.deepEqual(page.selected(), [2]);
  assert.equal(page.ids["select-all-button"].disabled, false);
  assert.equal(page.ids["save-button"].disabled, false);
  page.selectAll();
  page.save();
  await flush();
  assert.deepEqual(page.store.config.account_ids.slice().sort((a, b) => a - b), [1, 2, 3, 99]);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("select all respects configuration load and save read-back locks", async () => {
  const initial = deferred();
  const verification = deferred();
  const page = createPage({ load: (number) => number === 1 ? initial.promise : verification.promise });
  await flush();
  page.ids["select-all-button"].emit("click");
  assert.deepEqual(page.selected(), []);
  initial.resolve({ account_ids: [2] });
  await flush();
  page.selectAll();
  page.check(1, false);
  page.save();
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, true);
  page.ids["select-all-button"].emit("click");
  assert.deepEqual(page.selected(), [2, 3]);
  verification.resolve(clone(page.calls.save[0]));
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, false);
  assert.deepEqual(page.calls.save[0].account_ids, [2, 3]);
});

test("status refresh automatically selects new accounts without losing existing selections", async () => {
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }] }) };
  const page = createPage({ status });
  await flush();
  page.selectAll();
  assert.equal(page.ids["select-all-button"].disabled, false);
  status.status_json = JSON.stringify({ accounts: [{ id: 2 }] });
  page.pollStatus();
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, false);
  assert.deepEqual(page.selected(), [2]);
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[0].account_ids, [1, 2]);
});

test("selected accounts save on click, persist on read-back, and survive reopening", async () => {
  const store = { config: { account_ids: [2], timeout_seconds: 123, rewrite_tools: false } };
  const page = createPage({ store });
  assert.equal(page.ids["save-button"].disabled, true);
  assert.equal(page.ids["account-fields"].disabled, true);
  await flush();
  assert.deepEqual(page.selected(), [2]);
  page.check(1, true);
  page.save();
  assert.equal(page.ids["save-button"].disabled, true);
  assert.equal(page.ids["account-fields"].disabled, true);
  page.check(3, true);
  page.save();
  await flush();
  assert.equal(page.calls.save.length, 1);
  assert.equal(page.calls.load, 2, "Saving must confirm the persisted host configuration");
  const submitted = clone(page.calls.save[0]);
  submitted.account_ids.sort((a, b) => a - b);
  assert.deepEqual(submitted, { ...defaultAutoDegradation, auto_degradation_manual_revision: 1, account_ids: [1, 2], auto_select_new_accounts: true, excluded_account_ids: [3], enabled_models: defaultModels, timeout_seconds: 123, rewrite_tools: false, bps_auto_disable_on_403: true });
  assert.deepEqual(page.selected(), [1, 2]);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  assert.equal(page.ids["account-fields"].disabled, false);
  const reopened = createPage({ store });
  await flush();
  assert.deepEqual(reopened.selected(), [1, 2]);
});

test("saving an empty selection persists an explicit account_ids array", async () => {
  const store = { config: { account_ids: [1, 2] } };
  const page = createPage({ store });
  await flush();
  page.check(1, false);
  page.check(2, false);
  page.save();
  await flush();
  assert.deepEqual(store.config.account_ids, []);
  assert.deepEqual(page.calls.save[0].account_ids, []);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  const reopened = createPage({ store });
  await flush();
  assert.deepEqual(reopened.selected(), []);
});

test("a host-normalized null account_ids confirms an empty selection and survives reopening", async () => {
  const store = { config: { account_ids: [1, 2] } };
  const page = createPage({
    store,
    save(config, saved) {
      saved.config = Object.assign({}, config, { account_ids: null });
      return Promise.resolve(clone(saved.config));
    },
  });
  await flush();
  page.check(1, false);
  page.check(2, false);
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[0].account_ids, []);
  assert.equal(store.config.account_ids, null);
  assert.equal(page.calls.load, 2, "Saving must read back the host-normalized null value");
  assert.deepEqual(page.selected(), []);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  const reopened = createPage({ store });
  await flush();
  assert.deepEqual(reopened.selected(), []);
  assert.equal(reopened.ids["save-button"].disabled, false);
});

test("a delayed initial load cannot overwrite an allowed user edit", async () => {
  const initial = deferred();
  const page = createPage({ load: () => initial.promise });
  await flush();
  assert.equal(page.ids["account-fields"].disabled, true);
  assert.equal(page.ids["save-button"].disabled, true);
  page.check(1, true);
  page.save();
  assert.deepEqual(page.selected(), []);
  assert.equal(page.calls.save.length, 0);
  initial.resolve({ account_ids: [2] });
  await flush();
  assert.deepEqual(page.selected(), [2]);
  assert.equal(page.ids["account-fields"].disabled, false);
  page.check(1, true);
  await flush();
  assert.deepEqual(page.selected(), [1, 2]);
});

test("failed initial reads keep controls locked until a successful retry", async () => {
  const retry = deferred();
  const page = createPage({ load: (number) => number === 1 ? Promise.reject(new Error("read unavailable")) : retry.promise });
  await flush();
  assert.equal(page.ids["account-fields"].disabled, true);
  assert.equal(page.ids["save-button"].disabled, true);
  assert.equal(page.ids["retry-button"].hidden, false);
  assert.match(page.ids["form-hint"].textContent, /读取配置失败/);
  assert.doesNotMatch(page.ids["form-hint"].textContent, /已填入默认值/);
  page.check(1, true);
  page.save();
  assert.equal(page.calls.save.length, 0);
  page.retry();
  assert.equal(page.calls.load, 2);
  assert.equal(page.ids["account-fields"].disabled, true);
  retry.resolve({ account_ids: [3] });
  await flush();
  assert.deepEqual(page.selected(), [3]);
  assert.equal(page.ids["account-fields"].disabled, false);
  assert.equal(page.ids["save-button"].disabled, false);
  assert.equal(page.ids["retry-button"].hidden, true);
});

test("the page stays locked until save read-back completes", async () => {
  const verification = deferred();
  const page = createPage({ load: (number) => number === 1 ? Promise.resolve({ account_ids: [1] }) : verification.promise });
  await flush();
  page.check(2, true);
  page.save();
  await flush();
  assert.equal(page.calls.load, 2);
  assert.equal(page.ids["save-button"].disabled, true);
  assert.equal(page.ids["account-fields"].disabled, true);
  assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
  verification.resolve({ ...clone(page.calls.save[0]), account_ids: [2, 1] });
  await flush();
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  assert.equal(page.ids["save-button"].disabled, false);
});

test("a save acknowledgement with unchanged persisted accounts never reports success", async () => {
  const page = createPage({
    store: { config: { account_ids: [1] } },
    save: (config) => Promise.resolve(config),
  });
  await flush();
  page.check(2, true);
  page.save();
  await flush();
  assert.equal(page.calls.load, 2);
  assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
  assert.match(page.ids["form-hint"].textContent, /失败|不一致|未保存/);
  assert.deepEqual(page.store.config.account_ids, [1]);
});

test("a failed verification read never turns a save acknowledgement into success", async () => {
  const page = createPage({ load: (number) => number === 1 ? Promise.resolve({ account_ids: [] }) : Promise.reject(new Error("verify unavailable")) });
  await flush();
  page.check(2, true);
  page.save();
  await flush();
  assert.equal(page.calls.load, 2);
  assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
  assert.match(page.ids["form-hint"].textContent, /verify unavailable/);
});

test("automatic images need no configuration controls", async () => {
  assert.match(htmlSource, /支持直接发送图片和截图，无需额外配置/);
  assert.doesNotMatch(htmlSource, /image-relay|图片中转|公网 HTTPS|反向代理|监听地址|存储目录/);
  const inputs = [...htmlSource.matchAll(/<input[^>]+id="([^"]+)"/g)].map(match => match[1]);
  assert.deepEqual(inputs, ["auto-degradation-interval", "degradation-check-model"]);
  assert.doesNotMatch(htmlSource, /<select |<details /);
  const page = createPage();
  await flush();
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[0], { ...defaultAutoDegradation, account_ids: [1, 2, 3], auto_select_new_accounts: true, excluded_account_ids: [], enabled_models: defaultModels, bps_auto_disable_on_403: true });
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("degradation check control is wired through the existing bridge", () => {
  assert.match(htmlSource, /id="degradation-check-button"/);
  assert.match(htmlSource, /id="degradation-result"/);
  assert.match(appSource, /handleDegradationCheck/);
  assert.match(appSource, /degradation_check/);
});

test("opening configuration does not save before loading or mutate stored settings", async () => {
  const initial = deferred();
  const store = { config: { account_ids: [2], enabled_models: ["gpt-6-sol"] } };
  const original = clone(store.config);
  const page = createPage({ store, load: () => initial.promise });
  page.save();
  assert.equal(page.calls.save.length, 0);
  initial.resolve(clone(store.config));
  await flush();
  assert.deepEqual(store.config, original);
  assert.equal(page.calls.save.length, 0);
  assert.deepEqual(page.selected(), [2]);
});

test("a rejected save preserves account edits for retry", async () => {
  let first = true;
  const page = createPage({ store: { config: { account_ids: [] } }, save(config, store) {
    if (first) { first = false; return Promise.reject(new Error("write unavailable")); }
    store.config = config;
    return Promise.resolve(clone(config));
  } });
  await flush();
  page.check(1, true);
  page.check(2, false);
  page.check(3, false);
  page.save();
  await flush();
  assert.match(page.ids["form-hint"].textContent, /保存失败.*unavailable/);
  assert.deepEqual(page.selected(), [1]);
  assert.equal(page.ids["account-fields"].disabled, false);
  page.save();
  await flush();
  assert.equal(page.calls.save.length, 2);
  assert.deepEqual(page.store.config, { ...defaultAutoDegradation, auto_degradation_manual_revision: 1, account_ids: [1], auto_select_new_accounts: true, excluded_account_ids: [2, 3], enabled_models: defaultModels, bps_auto_disable_on_403: true });
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("legacy unrestricted accounts display all selected and persist automatic routing on first save", async () => {
  const page = createPage();
  await flush();
  assert.deepEqual(page.selected(), [1, 2, 3]);
  assert.match(page.ids["account-hint"].textContent, /保存一次/);
  assert.equal(page.calls.save.length, 0);
  page.save();
  await flush();
  assert.equal(page.store.config.auto_select_new_accounts, true);
  assert.deepEqual(page.store.config.excluded_account_ids, []);
  assert.match(page.ids["account-hint"].textContent, /无需再次打开配置页/);
  const reopened = createPage({ store: page.store, accounts: [{ id: 1 }, { id: 4 }] });
  await flush();
  assert.deepEqual(reopened.selected(), [1, 4]);
});

test("legacy whitelist migration preserves unchecked and hidden excluded accounts", async () => {
  const page = createPage({ store: { config: { account_ids: [2, 99], excluded_account_ids: [88] } } });
  await flush();
  assert.deepEqual(page.selected(), [2]);
  page.save();
  await flush();
  assert.equal(page.store.config.auto_select_new_accounts, true);
  assert.deepEqual(page.store.config.excluded_account_ids.sort((a, b) => a - b), [1, 3, 88]);
  assert.deepEqual(page.store.config.account_ids, [2, 99]);
  const reopened = createPage({ store: page.store, accounts: [{ id: 1 }, { id: 2 }, { id: 4 }, { id: 88 }, { id: 99 }] });
  await flush();
  assert.deepEqual(reopened.selected(), [2, 4, 99]);
});

test("legacy whitelist stays unchanged until a valid account directory arrives", async (t) => {
  for (const status of [
    { healthy: true, status_json: "not-json" },
    { healthy: true, status_json: JSON.stringify({}) },
    { healthy: true, status_json: JSON.stringify({ accounts: [] }) },
    { healthy: true, status_json: JSON.stringify({ accounts: [null, { id: 0 }] }) },
  ]) {
    await t.test(status.status_json, async () => {
      const page = createPage({ status, store: { config: { account_ids: [2], excluded_account_ids: [88] } } });
      await flush();
      assert.match(page.ids["account-hint"].textContent, /保留旧白名单/);
      page.save();
      await flush();
      assert.notEqual(page.store.config.auto_select_new_accounts, true);
      assert.deepEqual(page.store.config.account_ids, [2]);
      status.status_json = JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }] });
      page.pollStatus();
      await flush();
      page.save();
      await flush();
      assert.equal(page.store.config.auto_select_new_accounts, true);
      assert.deepEqual(page.store.config.excluded_account_ids, [88, 1]);
    });
  }
});

test("automatic mode honors exclusions over snapshots and retains unsaved deselection across polling", async () => {
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }, { id: 3 }], account_ids: [1, 2], auto_select_new_accounts: true }) };
  const page = createPage({ status, store: { config: { auto_select_new_accounts: true, account_ids: [1, 2], excluded_account_ids: [2, 88] } } });
  await flush();
  assert.deepEqual(page.selected(), [1, 3]);
  assert.match(page.ids["version-line"].textContent, /新增账号自动使用 BPS/);
  assert.doesNotMatch(page.ids["version-line"].textContent, /固定/);
  page.check(1, false);
  status.status_json = JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }, { id: 3 }, { id: 4 }, { id: 88 }] });
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selected(), [3, 4]);
  page.save();
  await flush();
  assert.deepEqual(page.store.config.excluded_account_ids, [2, 88, 1]);
});

test("deselecting all known accounts excludes them while a later new account remains automatic", async () => {
  const page = createPage();
  await flush();
  [1, 2, 3].forEach((id) => page.check(id, false));
  page.save();
  await flush();
  assert.deepEqual(page.store.config.account_ids, []);
  assert.deepEqual(page.store.config.excluded_account_ids, [1, 2, 3]);
  const reopened = createPage({ store: page.store, accounts: [{ id: 1 }, { id: 2 }, { id: 3 }, { id: 4 }] });
  await flush();
  assert.deepEqual(reopened.selected(), [4]);
});

test("select all clears visible exclusions but preserves hidden exclusions", async () => {
  const page = createPage({ store: { config: { auto_select_new_accounts: true, excluded_account_ids: [1, 2, 88] } } });
  await flush();
  assert.deepEqual(page.selected(), [3]);
  page.selectAll();
  page.save();
  await flush();
  assert.deepEqual(page.store.config.excluded_account_ids, [88]);
  assert.deepEqual(page.selected(), [1, 2, 3]);
});

test("late initial configuration respects existing exclusions after status arrives first", async () => {
  const initial = deferred();
  const page = createPage({ load: () => initial.promise });
  await flush();
  initial.resolve({ auto_select_new_accounts: true, account_ids: [1], excluded_account_ids: [2] });
  await flush();
  assert.deepEqual(page.selected(), [1, 3]);
});

test("status polls share an outstanding request while preserving unsaved exclusions", async () => {
  const older = deferred();
  const newer = deferred();
  const page = createPage({ getStatus: (number) => number === 1
    ? Promise.resolve({ healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }] }) })
    : number === 2 ? older.promise : newer.promise });
  await flush();
  page.check(1, false);
  page.pollStatus();
  page.pollStatus();
  assert.equal(page.calls.status, 2, "A pending poll must not create another host request");
  older.resolve({ healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }] }) });
  await flush();
  assert.deepEqual(page.selected(), [2]);
  page.pollStatus();
  assert.equal(page.calls.status, 3, "The next completed interval must fetch fresh accounts");
  newer.resolve({ healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }, { id: 3 }] }) });
  await flush();
  assert.deepEqual(page.selected(), [2, 3]);
});

test("a verified save obtains a newer status without waiting for an older poll", async () => {
  const older = deferred();
  const newer = deferred();
  const page = createPage({ getStatus: (number) => number === 1
    ? Promise.resolve({ healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }] }) })
    : number === 2 ? older.promise : newer.promise });
  await flush();
  page.check(1, false);
  page.pollStatus();
  page.save();
  await flush();
  assert.equal(page.calls.status, 3, "The save must not reuse a stale poll");
  newer.resolve({ healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }] }) });
  await flush();
  older.resolve({ healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }] }) });
  await flush();
  assert.deepEqual(page.selected(), [2]);
});

test("unchanged status polls create zero account nodes or resize messages for a large directory", async () => {
  const accounts = Array.from({ length: 1000 }, (_, index) => ({ id: index + 1, name: "Account " + (index + 1), schedulable: true }));
  const page = createPage({ accounts });
  await flush();
  page.check(1, false);
  const rows = [1, 500, 1000].map((accountID) => page.accountRow(accountID).row);
  const created = page.calls.elements;
  const resized = page.calls.resize.length;
  for (let poll = 0; poll < 5; poll++) { page.pollStatus(); await flush(); }
  assert.equal(page.calls.elements, created, "Unchanged polls must reuse every existing account row");
  assert.equal(page.calls.resize.length, resized, "Unchanged content height must not notify the host again");
  [1, 500, 1000].forEach((accountID, index) => assert.equal(page.accountRow(accountID).row, rows[index]));
  assert.equal(page.accountRow(1).checkbox.checked, false);
  assert.equal(page.selected().length, 999);
});

test("unchanged diagnostic summaries reuse their DOM and account results", async () => {
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1, schedulable: true }], degradation_check: {
    completed: true, degraded_account_ids: [1], results: [{ account_id: 1, status: "degraded", answer: "苹果16" }],
  } }) };
  const page = createPage({ status });
  await flush();
  const summary = page.ids["degradation-result"].children[0];
  const created = page.calls.elements;
  page.pollStatus();
  await flush();
  assert.equal(page.calls.elements, created);
  assert.equal(page.ids["degradation-result"].children[0], summary);
  assert.match(page.accountRow(1).result.textContent, /疑似降智/);
});

test("closing during initial status loading does not start a late polling timer", async () => {
  const pending = deferred();
  const page = createPage({ getStatus: () => pending.promise });
  await flush();
  page.unload();
  pending.resolve({ healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }] }) });
  await flush();
  assert.equal(page.calls.intervals, 0);
  assert.equal(page.calls.disposed, 1);
  assert.equal(page.calls.observerDisconnected, 1);
  assert.equal(page.calls.status, 1);
});

test("closing an initialized page releases its polling timer and resize observer", async () => {
  const page = createPage();
  await flush();
  page.unload();
  page.pollStatus();
  assert.equal(page.calls.intervals, 1);
  assert.equal(page.calls.clearedIntervals, 1);
  assert.equal(page.calls.observerDisconnected, 1);
  assert.equal(page.calls.disposed, 1);
  assert.equal(page.calls.status, 1);
});

test("accounts added during a migrating save stay selected through failure and retry", async () => {
  const pending = deferred();
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }] }) };
  let saves = 0;
  const page = createPage({ status, store: { config: { account_ids: [1] } }, save(config, store) {
    if (++saves === 1) return pending.promise;
    store.config = clone(config);
    return Promise.resolve(clone(config));
  } });
  await flush();
  page.save();
  status.status_json = JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }, { id: 3 }] });
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selected(), [1, 3]);
  pending.reject(new Error("save unavailable"));
  await flush();
  page.save();
  await flush();
  assert.deepEqual(page.store.config.excluded_account_ids, [2]);
  assert.deepEqual(page.selected(), [1, 3]);
});

test("accounts added during save verification stay automatically selected after persisted snapshot applies", async () => {
  const verification = deferred();
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }] }) };
  const page = createPage({ status, store: { config: { account_ids: [1] } }, load: (number, store) =>
    number === 1 ? Promise.resolve(clone(store.config)) : verification.promise });
  await flush();
  page.save();
  await flush();
  status.status_json = JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }, { id: 3 }] });
  page.pollStatus();
  await flush();
  verification.resolve(clone(page.calls.save[0]));
  await flush();
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.deepEqual(page.store.config.excluded_account_ids, [2]);
  assert.deepEqual(page.selected(), [1, 3]);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("save acknowledgement and read-back must retain automatic mode and exclusions", async (t) => {
  for (const stage of ["acknowledgement", "read-back"]) {
    for (const field of ["auto_select_new_accounts", "excluded_account_ids"]) {
      await t.test(stage + " " + field, async () => {
        const page = createPage({ store: { config: { account_ids: [1] } }, save(config, store) {
          const changed = clone(config);
          delete changed[field];
          store.config = stage === "read-back" ? changed : clone(config);
          return Promise.resolve(stage === "acknowledgement" ? changed : clone(config));
        } });
        await flush();
        page.save();
        await flush();
        assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
        assert.match(page.ids["form-hint"].textContent, /不一致/);
        assert.deepEqual(page.selected(), [1]);
      });
    }
  }
});

test("host-normalized null exclusions are accepted when the submitted exclusion set is empty", async () => {
  const page = createPage({ save(config, store) {
    store.config = { ...config, excluded_account_ids: null };
    return Promise.resolve(clone(store.config));
  } });
  await flush();
  page.save();
  await flush();
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  assert.deepEqual(page.selected(), [1, 2, 3]);
});




const bpsBlockA = "a".repeat(32);
const bpsBlockB = "b".repeat(32);
function bpsStatus(blocks = [], accounts = [{ id: 1, schedulable: true }, { id: 2, schedulable: true }, { id: 3, schedulable: true }]) {
  return { healthy: true, status_json: JSON.stringify({
    accounts, bps_disabled_account_ids: blocks.map(([accountID]) => accountID),
    bps_disabled_accounts: blocks.map(([accountID, blockID]) => ({ account_id: accountID, block_id: blockID })),
  }) };
}

test("BPS persistence warning is visible as text and clears after status recovery", async () => {
  const status = bpsStatus();
  const details = JSON.parse(status.status_json);
  const warning = "write failed <storage unavailable>";
  details.bps_account_persistence_error = warning;
  status.status_json = JSON.stringify(details);
  const page = createPage({ status });
  await flush();
  assert.ok(page.ids["version-line"].textContent.includes("BPS 账号状态存储警告：" + warning));
  assert.equal(page.ids["version-line"].children.length, 0);
  assert.equal(page.ids["state-chip"].textContent, "运行中");
  assert.equal(page.calls.save.length, 0);

  Object.assign(status, bpsStatus());
  page.pollStatus();
  await flush();
  assert.ok(!page.ids["version-line"].textContent.includes("BPS 账号状态存储警告"));
  assert.ok(!page.ids["version-line"].textContent.includes(warning));
  assert.equal(page.ids["state-chip"].textContent, "运行中");
  assert.equal(page.calls.save.length, 0);
});

test("one reported BPS 403 deselects only that account without saving during polls", async () => {
  const status = bpsStatus();
  const page = createPage({ status });
  await flush();
  page.check(3, false);
  Object.assign(status, bpsStatus([[2, bpsBlockA]]));
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selected(), [1]);
  assert.equal(page.calls.save.length, 0);
  assert.match(page.accountRow(2).availability.textContent, /BPS.*403/);
  assert.match(page.ids["account-hint"].textContent, /一次 BPS HTTP 403/);
  assert.doesNotMatch(page.ids["account-hint"].textContent, /basispoints_upstream_error/);
  const row = page.accountRow(2).row;
  const elements = page.calls.elements;
  page.pollStatus();
  await flush();
  assert.equal(page.accountRow(2).row, row);
  assert.equal(page.calls.elements, elements);
  page.save();
  await flush();
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.deepEqual(page.store.config.excluded_account_ids.sort(), [2, 3]);
  assert.ok(!Object.hasOwn(page.store.config, "bps_reenabled_accounts"));
});

test("a late config load cannot reselect an account already disabled in status", async () => {
  const initial = deferred();
  const page = createPage({ status: bpsStatus([[1, bpsBlockA]]), load: () => initial.promise });
  await flush();
  initial.resolve({ account_ids: [1, 2], auto_select_new_accounts: true, excluded_account_ids: [3] });
  await flush();
  assert.deepEqual(page.selected(), [2]);
  assert.match(page.accountRow(1).availability.textContent, /403/);
  assert.equal(page.calls.save.length, 0);
});

test("same-version 403 polls preserve an explicit restore until ordinary save confirms it", async () => {
  const status = bpsStatus([[2, bpsBlockA]]);
  const page = createPage({ status, save(config, store) {
    store.config = clone(config);
    if (config.bps_reenabled_accounts && config.bps_reenabled_accounts[2] === bpsBlockA) Object.assign(status, bpsStatus());
    return Promise.resolve(clone(config));
  } });
  await flush();
  assert.deepEqual(page.selected(), [1, 3]);
  page.check(2, true);
  assert.match(page.accountRow(2).availability.textContent, /待恢复/);
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selected(), [1, 2, 3]);
  assert.equal(page.calls.save.length, 0);
  page.save();
  await flush();
  assert.deepEqual(page.store.config.bps_reenabled_accounts, { 2: bpsBlockA });
  assert.deepEqual(page.store.config.excluded_account_ids, []);
  assert.deepEqual(page.selected(), [1, 2, 3]);
  assert.equal(page.accountRow(2).availability.textContent, "#2 · 可用");
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[1].bps_reenabled_accounts, { 2: bpsBlockA }, "Confirmed acknowledgement remains persisted");
});

test("a new 403 version cancels an unsaved restore and keeps old acknowledgements ineffective", async () => {
  const status = bpsStatus([[2, bpsBlockA]]);
  const page = createPage({ status, store: { config: { account_ids: [1, 2, 3], auto_select_new_accounts: true, bps_reenabled_accounts: { 88: bpsBlockA } } } });
  await flush();
  page.check(2, true);
  Object.assign(status, bpsStatus([[2, bpsBlockB]]));
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selected(), [1, 3]);
  assert.doesNotMatch(page.accountRow(2).availability.textContent, /待恢复/);
  page.save();
  await flush();
  assert.deepEqual(page.store.config.bps_reenabled_accounts, { 88: bpsBlockA });
  assert.deepEqual(page.store.config.excluded_account_ids, [2]);
  page.check(2, true);
  page.save();
  await flush();
  assert.deepEqual(page.store.config.bps_reenabled_accounts, { 2: bpsBlockB, 88: bpsBlockA });
});

test("403 status arriving during save acknowledgement or verification remains excluded", async (t) => {
  for (const stage of ["acknowledgement", "verification"]) {
    await t.test(stage, async () => {
      const pending = deferred();
      const status = bpsStatus();
      const page = createPage({ status, save(config, store) {
        store.config = clone(config);
        return stage === "acknowledgement" ? pending.promise : Promise.resolve(clone(config));
      }, load(number, store) {
        return number > 1 && stage === "verification" ? pending.promise : Promise.resolve(clone(store.config));
      } });
      await flush();
      page.save();
      await flush();
      Object.assign(status, bpsStatus([[1, bpsBlockA]]));
      page.pollStatus();
      await flush();
      assert.deepEqual(page.selected(), [2, 3]);
      pending.resolve(clone(page.store.config));
      await flush();
      assert.deepEqual(page.selected(), [2, 3]);
      assert.equal(page.calls.save.length, 1, "Status must not start a second config write");
      assert.match(page.accountRow(1).availability.textContent, /403/);
    });
  }
});

test("a repeated 403 during restore saving invalidates the submitted acknowledgement", async () => {
  const pending = deferred();
  const status = bpsStatus([[2, bpsBlockA]]);
  const page = createPage({ status, save(config, store) { store.config = clone(config); return pending.promise; } });
  await flush();
  page.check(2, true);
  page.save();
  await flush();
  Object.assign(status, bpsStatus([[2, bpsBlockB]]));
  page.pollStatus();
  await flush();
  pending.resolve(clone(page.store.config));
  await flush();
  assert.deepEqual(page.selected(), [1, 3]);
  assert.deepEqual(page.store.config.bps_reenabled_accounts, { 2: bpsBlockA });
  assert.match(page.accountRow(2).availability.textContent, /403/);
  assert.doesNotMatch(page.accountRow(2).availability.textContent, /待恢复/);
});

test("restoration requires acknowledgement persistence at save and read-back", async (t) => {
  for (const stage of ["acknowledgement", "verification"]) {
    await t.test(stage, async () => {
      const page = createPage({ status: bpsStatus([[2, bpsBlockA]]), save(config, store) {
        const dropped = clone(config);
        delete dropped.bps_reenabled_accounts;
        store.config = stage === "verification" ? dropped : clone(config);
        return Promise.resolve(stage === "acknowledgement" ? dropped : clone(config));
      } });
      await flush();
      page.check(2, true);
      page.save();
      await flush();
      assert.match(page.ids["form-hint"].textContent, /失败|未确认/);
      assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
    });
  }
});




test("unchecking a pending restore discards its unsaved acknowledgement", async () => {
  const page = createPage({ status: bpsStatus([[2, bpsBlockA]]) });
  await flush();
  page.check(2, true);
  page.check(2, false);
  page.save();
  await flush();
  assert.deepEqual(page.selected(), [1, 3]);
  assert.deepEqual(page.store.config.excluded_account_ids, [2]);
  assert.ok(!Object.hasOwn(page.store.config, "bps_reenabled_accounts"));
  assert.doesNotMatch(page.accountRow(2).availability.textContent, /待恢复/);
});

test("a disabled sole legacy account keeps its original boundary without a directory", async () => {
  const page = createPage({ status: bpsStatus([[7, bpsBlockA]], []), store: { config: { account_ids: [7] } } });
  await flush();
  page.save();
  await flush();
  assert.deepEqual(page.store.config.account_ids, [7], "An empty legacy list would incorrectly allow all accounts");
  assert.deepEqual(page.store.config.excluded_account_ids, [7]);
  assert.notEqual(page.store.config.auto_select_new_accounts, true);
  assert.ok(!Object.hasOwn(page.store.config, "bps_reenabled_accounts"));
});

test("a disabled sole legacy account migrates to explicit exclusions after reading a directory", async () => {
  const page = createPage({ status: bpsStatus([[1, bpsBlockA]]), store: { config: { account_ids: [1] } } });
  await flush();
  assert.deepEqual(page.selected(), []);
  page.save();
  await flush();
  assert.equal(page.store.config.auto_select_new_accounts, true);
  assert.deepEqual(page.store.config.account_ids, []);
  assert.deepEqual(page.store.config.excluded_account_ids.sort(), [1, 2, 3]);
});

test("missing block tokens cannot implicitly restore a 403-disabled account", async () => {
  const page = createPage({ status: { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }], bps_disabled_account_ids: [1] }) } });
  await flush();
  assert.deepEqual(page.selected(), []);
  assert.equal(page.accountRow(1).checkbox.isDisabled(), true);
  page.selectAll();
  page.save();
  await flush();
  assert.deepEqual(page.store.config.excluded_account_ids, [1]);
  assert.ok(!Object.hasOwn(page.store.config, "bps_reenabled_accounts"));
});

test("select all records explicit versioned restores only when the user saves", async () => {
  const page = createPage({ status: bpsStatus([[1, bpsBlockA], [2, bpsBlockB]]) });
  await flush();
  page.selectAll();
  assert.deepEqual(page.selected(), [1, 2, 3]);
  assert.equal(page.calls.save.length, 0);
  page.save();
  await flush();
  assert.deepEqual(page.store.config.bps_reenabled_accounts, { 1: bpsBlockA, 2: bpsBlockB });
});

test("ordinary saves never confirm retained diagnostic command markers", async (t) => {
  for (const phase of ["acknowledgement", "read-back"]) {
    for (const marker of [{ degradation_check: true }, { degradation_check_account_id: 2 }, { degradation_check_account_ids: [2] }, { degradation_check_account_ids: [] }]) {
      await t.test(phase + " / " + Object.keys(marker)[0], async () => {
        const page = createPage({
          save: (config, store) => {
            store.config = clone(config);
            return Promise.resolve(Object.assign(clone(config), phase === "acknowledgement" ? marker : {}));
          },
          load: (count, store) => Promise.resolve(Object.assign(clone(store.config), count > 1 && phase === "read-back" ? marker : {})),
        });
        await flush();
        page.save();
        await flush();
        assert.match(page.ids["form-hint"].textContent, /检测标记/);
        assert.match(page.ids["form-hint"].className, /hint-error/);
        assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存，并已重新读取确认/);
        assert.equal(page.ids["save-button"].disabled, false);
      });
    }
  }
});


function scopedResult(config, verdicts = {}) {
  const ids = config.degradation_check_account_id ? [config.degradation_check_account_id] : config.degradation_check_account_ids;
  const results = ids.map(account_id => ({ account_id, status: "ok", answer: "苹果17", ...verdicts[account_id] }));
  return { success: true, status_json: JSON.stringify({ degradation_check: {
    request_id: "mock-request", target_account_ids: ids, completed: true, results,
    degraded_account_ids: results.filter(row => row.status === "degraded").map(row => row.account_id),
  } }) };
}

test("scoped single-account checks use page snapshots without saving shared configuration", async () => {
  const store = { config: { account_ids: [1], enabled_models: ["gpt-6-astra"] } };
  const first = createPage({ store, scoped: true });
  const second = createPage({ store, scoped: true });
  await flush();
  first.checkModel("gpt-6-sol", true);
  first.checkAccount(1);
  second.checkAccount(2);
  await flush();
  assert.equal(first.calls.scoped[0].degradation_check_account_id, 1);
  assert.equal(second.calls.scoped[0].degradation_check_account_id, 2);
  assert.ok(first.calls.scoped[0].enabled_models.includes("gpt-6-sol"));
  assert.ok(!second.calls.scoped[0].enabled_models.includes("gpt-6-sol"));
  assert.deepEqual(store.config, { account_ids: [1], enabled_models: ["gpt-6-astra"] });
  for (const page of [first, second]) {
    assert.equal(page.calls.save.length, 0);
    assert.equal(page.calls.test, 0);
    assert.deepEqual(page.selected(), [1]);
  }
  assert.match(first.accountRow(1).result.textContent, /符合检测规则/);
  assert.equal(first.accountRow(2).result.textContent, "未检测");
  assert.match(second.accountRow(2).result.textContent, /符合检测规则/);
});

test("bulk snapshot selects confirmed degraded accounts locally and preserves failed or hidden selections", async () => {
  const page = createPage({ scoped: true, store: { config: { account_ids: [1, 3, 99] } },
    testScoped: config => Promise.resolve(scopedResult(config, {
      2: { status: "degraded", answer: "苹果16" },
      3: { status: "error", answer: "", error: "HTTP 429" },
    })) });
  await flush();
  page.degradationCheck();
  await flush();
  assert.deepEqual(page.calls.scoped[0].degradation_check_account_ids, [1, 2, 3]);
  assert.deepEqual(page.selected(), [2, 3]);
  assert.equal(page.calls.save.length, 0);
  assert.match(page.ids["form-hint"].textContent, /保存后生效/);
  page.save();
  await flush();
  assert.deepEqual([...page.store.config.account_ids].sort((a,b) => a-b), [2, 3, 99]);
  assert.ok(!page.store.config.degradation_check);
  assert.ok(!page.store.config.degradation_check_account_id);
  assert.ok(!page.store.config.degradation_check_account_ids);
});

test("diagnostic controls explain probe scope, request cost, and both bulk selection changes", async () => {
  const page = createPage({ scoped: true });
  await flush();
  const hint = page.ids["account-check-hint"].textContent;
  assert.match(hint, /原生 Codex.*消耗额度/);
  assert.match(hint, /本次探针/);
  assert.match(hint, /取消.*符合规则/);
  assert.match(hint, /原生错误不会触发 BPS 停用/);
  assert.match(page.ids["degradation-check-button"].title, /原生 Codex.*自动模式仅展示结果/);
  assert.match(page.accountRow(1).button.title, /探针结果/);
  assert.match(htmlSource, /PDF.*Word/);
  assert.match(htmlSource, /客户端.*工具/);
});

test("diagnostic 403 protection reports deselection without claiming unchanged choices", async t => {
  for (const single of [true, false]) {
    await t.test(single ? "single" : "bulk", async () => {
      const status = bpsStatus([]);
      const page = createPage({ scoped: true, status, testScoped: config => {
        const result = scopedResult(config, { 1: { status: "error", answer: "", error: "HTTP 403" } });
        const blocked = JSON.parse(bpsStatus([[1, bpsBlockA]]).status_json);
        const details = { ...blocked, ...JSON.parse(result.status_json) };
        status.status_json = JSON.stringify(blocked);
        return Promise.resolve({ ...result, status_json: JSON.stringify(details) });
      } });
      await flush();
      page.toggle403(); // The local edit is deliberately not yet saved.
      if (single) page.checkAccount(1);
      else page.degradationCheck();
      await flush();
      assert.deepEqual(page.selected(), [2, 3]);
      assert.equal(page.calls.save.length, 0);
      assert.equal(page.calls.scoped[0].bps_auto_disable_on_403, false);
      assert.match(page.ids["form-hint"].textContent, /403.*自动停用.*1.*取消勾选/);
      assert.doesNotMatch(page.ids["form-hint"].textContent, /账号选择未改变|保留原账号选择/);
      const summary = page.ids["degradation-result"].children[0].textContent;
      assert.doesNotMatch(summary, /保留原账号选择/);
    });
  }
});

test("partial diagnostics label answered results incomplete and preserve choices", async () => {
  const page = createPage({ scoped: true, store: { config: { account_ids: [1] } }, testScoped: config => {
    const result = scopedResult(config, { 2: { status: "degraded", answer: "苹果16" } });
    const details = JSON.parse(result.status_json);
    details.degradation_check.completed = false;
    result.status_json = JSON.stringify(details);
    return Promise.resolve(result);
  } });
  await flush();
  page.degradationCheck();
  await flush();
  assert.deepEqual(page.selected(), [1]);
  assert.equal(page.calls.save.length, 0);
  const summary = page.ids["degradation-result"].children[0].textContent;
  assert.match(summary, /检测未全部完成/);
  assert.doesNotMatch(summary, /^检测完成/);
  assert.match(summary, /本次探针/);
});

test("diagnostic errors and mismatched targets preserve choices and unlock controls", async t => {
  for (const mode of ["network", "wrong-target", "duplicate", "empty", "no-degraded", "incomplete"]) {
    await t.test(mode, async () => {
      const page = createPage({ scoped: true, store: { config: { account_ids: [2] } }, testScoped: config => {
        if (mode === "network") return Promise.reject(new Error("timeout"));
        const result = scopedResult(config);
        const details = JSON.parse(result.status_json);
        if (mode === "wrong-target") details.degradation_check.target_account_ids = [99];
        if (mode === "duplicate") details.degradation_check.results[1] = details.degradation_check.results[0];
        if (mode === "empty") details.degradation_check.results = [];
        if (mode === "incomplete") details.degradation_check.completed = false;
        result.status_json = JSON.stringify(details);
        return Promise.resolve(result);
      } });
      await flush();
      page.degradationCheck();
      await flush();
      assert.deepEqual(page.selected(), [2]);
      assert.equal(page.calls.save.length, 0);
      assert.equal(page.ids["save-button"].disabled, false);
      assert.equal(page.ids["degradation-check-button"].disabled, false);
      if (!["no-degraded", "incomplete"].includes(mode)) assert.match(page.ids["form-hint"].textContent, /检测失败/);
    });
  }
});

test("active diagnostics lock edits and dispatch once even when events are forced", async () => {
  const pending = deferred();
  const page = createPage({ scoped: true, testScoped: () => pending.promise });
  await flush();
  page.checkAccount(1);
  assert.equal(page.ids["save-button"].disabled, true);
  assert.equal(page.ids["select-all-button"].disabled, true);
  assert.equal(page.accountRow(1).button.textContent, "检测中…");
  page.accountRow(2).button.emit("click");
  page.ids["degradation-check-button"].emit("click");
  page.ids["save-button"].emit("click");
  page.ids["select-all-button"].emit("click");
  assert.equal(page.calls.scoped.length, 1);
  assert.equal(page.calls.save.length, 0);
  pending.resolve(scopedResult(page.calls.scoped[0]));
  await flush();
  assert.equal(page.ids["save-button"].disabled, false);
});

test("scoped diagnostics do not acknowledge unsaved 403 restores", async () => {
  const page = createPage({ scoped: true, status: bpsStatus([[2, bpsBlockA]]),
    store: { config: { account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2] } } });
  await flush();
  page.check(2, true);
  assert.equal(page.accountRow(2).button.disabled, false);
  page.checkAccount(1);
  await flush();
  assert.ok(!page.calls.scoped[0].bps_reenabled_accounts || !page.calls.scoped[0].bps_reenabled_accounts[2]);
  assert.ok(page.selected().includes(2));
  assert.match(page.accountRow(2).availability.textContent, /待恢复/);
  assert.equal(page.calls.save.length, 0);
});

function createBridgeHarness(options = {}) {
  const listeners = new Set();
  const sent = [];
  const timers = new Map();
  let nextID = 0;
  function deliver(request, overrides = {}, source = parent) {
    const data = Object.assign({ source: "sub2api-plugin-host", bridge_token: "trusted-token", request_id: request.request_id, ok: true }, overrides);
    for (const listener of listeners) listener({ source, data });
  }
  const parent = { postMessage(data, targetOrigin) {
    sent.push({ data: clone(data), targetOrigin });
    if (data.request_id && options.onRequest) {
      const response = options.onRequest(clone(data));
      if (response !== undefined) Promise.resolve(response).then(value => deliver(data, value));
    }
  } };
  const window = {
    parent,
    location: { hash: options.hash === undefined ? "#bridge_token=trusted-token" : options.hash },
    crypto: { randomUUID: () => (options.idPrefix || "request-") + (++nextID) },
    addEventListener(type, callback) { assert.equal(type, "message"); listeners.add(callback); },
    removeEventListener(type, callback) { assert.equal(type, "message"); listeners.delete(callback); },
    setTimeout(callback, delay) { const id = ++nextID; timers.set(id, { callback, delay }); return id; },
    clearTimeout(id) { timers.delete(id); },
  };
  vm.runInNewContext(bridgeSource, { window, console }, { filename: "ui/assets/bridge-v1.js" });
  const bridge = window.Sub2APIPluginBridge.create(options.settings || {});
  return {
    bridge, sent, timers, parent,
    reply(overrides = {}, source = parent) {
      assert.ok(sent.length, "A pending bridge request must have been posted");
      const request = sent[sent.length - 1].data;
      deliver(request, overrides, source);
    },
    expire() {
      for (const [id, timer] of [...timers]) {
        timers.delete(id);
        timer.callback();
      }
    },
  };
}

function createTaskServer(options = {}) {
  const server = {
    store: options.store || { config: { account_ids: [1] } }, ownerID: "instance-a",
    tasks: new Map(), commands: [], diagnostics: 0, ordinarySaves: [], legacyTests: 0,
    prepareMode: options.prepareMode, commitMode: options.commitMode, statusError: false,
    verdicts: options.verdicts || {}, statusDetails: options.statusDetails || {},
    complete(task) {
      const result = scopedResult(task.config, server.verdicts);
      const details = JSON.parse(result.status_json);
      details.degradation_check.request_id = task.task_id;
      result.status_json = JSON.stringify(details);
      task.state = "completed";
      task.result = result;
    },
    handle(request) {
      if (request.type === "config.load") return { config: clone(server.store.config) };
      if (request.type === "plugin.status") {
        if (server.statusError) return { ok: false, error: "status unavailable" };
        const busy = [...server.tasks.values()].some(task => ["prepared", "queued", "running", "unknown"].includes(task.state));
        return { result: { healthy: true, status_json: JSON.stringify({
          accounts: [1, 2, 3].map(id => ({ id, name: "Account " + id, schedulable: true })),
          ...server.statusDetails,
          diagnostic_tasks: { protocol: "config-job-v1", owner_id: server.ownerID, available: true,
            error: busy ? "另一个检测任务正在执行或等待确认" : undefined,
            tasks: [...server.tasks.values()].map(task => ({ task_id: task.task_id, state: task.state,
              receipt: task.state === "prepared" && server.prepareMode !== "missing-receipt" ? task.receipt : undefined,
              result: task.result, error: task.error })) },
        }) } };
      }
      if (request.type === "config.test" || request.type === "config.testScoped") {
        server.legacyTests++;
        return { ok: false, error: "unexpected host test" };
      }
      if (request.type !== "config.save") return {};
      const command = request.config.diagnostic_task;
      if (!command) {
        server.ordinarySaves.push(clone(request.config));
        server.store.config = clone(request.config);
        return { config: clone(server.store.config) };
      }
      assert.deepEqual(Object.keys(request.config), ["diagnostic_task"]);
      server.commands.push(clone(command));
      if (command.owner_id !== server.ownerID) return { ok: false, error: "owner mismatch" };
      if (command.action === "prepare") {
        assert.equal(server.tasks.has(command.task_id), false, "prepare must never be retried");
        const task = { task_id: command.task_id, state: "prepared", receipt: "receipt-" + command.task_id, config: clone(command.config) };
        if (server.prepareMode !== "unknown") server.tasks.set(task.task_id, task);
        if (server.prepareMode === "reject" || server.prepareMode === "unknown") return { ok: false, error: "prepare acknowledgement lost" };
        if (server.prepareMode === "invalid-ack") return { config: null };
        if (server.prepareMode === "timeout") return undefined;
        return { config: clone(server.store.config) };
      }
      assert.equal(command.action, "commit");
      const task = server.tasks.get(command.task_id);
      assert.ok(task);
      assert.equal(command.receipt, task.receipt);
      assert.equal(command.config, undefined);
      if (server.commitMode === "prepared") return { ok: false, error: "commit was not confirmed" };
      server.diagnostics++;
      if (server.commitMode === "running" || server.commitMode === "unknown") task.state = server.commitMode;
      else server.complete(task);
      if (server.commitMode === "timeout-completed") return undefined;
      if (server.commitMode === "error-completed" || server.commitMode === "unknown") return { ok: false, error: "commit acknowledgement lost" };
      return { config: clone(server.store.config) };
    },
  };
  return server;
}

function taskHost(server, options = {}) {
  return createBridgeHarness({ onRequest: request => server.handle(request), ...options });
}

test("unpatched hosts run a single diagnostic through command-only saves without replacing the form", async () => {
  const server = createTaskServer();
  const before = clone(server.store.config);
  const host = taskHost(server);
  const page = createPage({ bridge: host.bridge });
  await flush();
  assert.equal(host.bridge.supportsScopedTest(), false);
  assert.equal(page.accountRow(2).button.disabled, false);
  assert.match(page.ids["account-check-hint"].textContent, /无需修改宿主/);
  page.check(2, true);
  page.checkModel("gpt-6-sol", true);
  page.checkAccount(2);
  await flush();
  assert.deepEqual(server.commands.map(command => command.action), ["prepare", "commit"]);
  assert.equal(server.commands[0].config.degradation_check_account_id, 2);
  assert.ok(server.commands[0].config.enabled_models.includes("gpt-6-sol"));
  assert.equal(Object.hasOwn(server.commands[0].config, "bps_device_convergence"), false);
  assert.deepEqual(server.store.config, before);
  assert.deepEqual(page.selected(), [1, 2]);
  assert.ok(page.selectedModels().includes("gpt-6-sol"));
  assert.match(page.accountRow(2).result.textContent, /符合检测规则/);
  assert.equal(server.diagnostics, 1);
  assert.equal(server.legacyTests, 0);
  assert.equal(host.bridge.hasPendingDiagnosticTask(), false);
  assert.equal(page.ids["diagnostic-task-status"].hidden, false);
  assert.ok(page.ids["diagnostic-task-status"].textContent.includes(server.commands[0].task_id));
  assert.match(page.ids["diagnostic-task-status"].textContent, /已完成/);
});

test("unpatched hosts select completed bulk results locally and require an ordinary save", async () => {
  const server = createTaskServer({ store: { config: { account_ids: [1, 3, 99] } }, verdicts: {
    2: { status: "degraded", answer: "苹果16" }, 3: { status: "error", answer: "", error: "HTTP 429" },
  } });
  const page = createPage({ bridge: taskHost(server).bridge });
  await flush();
  page.degradationCheck();
  await flush();
  assert.deepEqual(server.commands[0].config.degradation_check_account_ids, [1, 2, 3]);
  assert.deepEqual(page.selected(), [2, 3]);
  assert.deepEqual(server.store.config.account_ids, [1, 3, 99]);
  assert.match(page.ids["form-hint"].textContent, /保存后生效/);
  page.save();
  await flush();
  assert.deepEqual(server.store.config.account_ids.sort((a, b) => a - b), [2, 3, 99]);
  assert.equal(server.ordinarySaves.length, 1);
  assert.equal(server.store.config.diagnostic_task, undefined);
  assert.equal(server.legacyTests, 0);
});

test("compatible detection preserves unsaved 403 restores and model and routing edits", async () => {
  const server = createTaskServer({ statusDetails: JSON.parse(bpsStatus([[2, bpsBlockA]]).status_json),
    store: { config: { account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2] } } });
  const page = createPage({ bridge: taskHost(server).bridge });
  await flush();
  page.check(2, true);
  page.check(3, false);
  page.toggle403();
  page.checkAccount(1);
  await flush();
  assert.deepEqual(page.selected(), [1, 2]);
  assert.ok(!server.commands[0].config.bps_reenabled_accounts || !server.commands[0].config.bps_reenabled_accounts[2]);
  assert.match(page.accountRow(2).availability.textContent, /待恢复/);
  assert.equal(page.ids["bps-403-toggle"].getAttribute("aria-pressed"), "false");
  assert.equal(server.store.config.bps_reenabled_accounts, undefined);
  assert.deepEqual(server.store.config.account_ids, [1, 3]);
});

test("two compatible pages respect a busy instance and never dispatch another task", async () => {
  const server = createTaskServer({ commitMode: "running" });
  const firstHost = taskHost(server, { idPrefix: "first-" });
  const first = createPage({ bridge: firstHost.bridge });
  await flush();
  first.checkAccount(1);
  await flush();
  const secondHost = taskHost(server, { idPrefix: "second-" });
  const second = createPage({ bridge: secondHost.bridge });
  await flush();
  assert.equal(second.accountRow(2).button.disabled, true);
  assert.match(second.ids["account-check-hint"].textContent, /另一个检测任务/);
  second.accountRow(2).button.emit("click");
  await flush();
  assert.equal(server.commands.length, 2);
  assert.equal(server.diagnostics, 1);
  server.complete([...server.tasks.values()][0]);
  firstHost.expire();
  await flush();
  assert.equal(firstHost.bridge.hasPendingDiagnosticTask(), false);
  second.pollStatus();
  await flush();
  assert.equal(second.accountRow(2).button.disabled, false);
  assert.equal(server.legacyTests, 0);
});

test("lost or invalid prepare acknowledgements never auto-commit and retain a query-only task", async t => {
  for (const prepareMode of ["reject", "invalid-ack", "unknown", "timeout"]) {
    await t.test(prepareMode, async () => {
      const server = createTaskServer({ prepareMode });
      const host = taskHost(server);
      const page = createPage({ bridge: host.bridge });
      await flush();
      page.checkAccount(1);
      await flush();
      if (prepareMode === "timeout") { host.expire(); await flush(); }
      assert.equal(server.commands.length, 1);
      assert.equal(server.diagnostics, 0);
      assert.equal(host.bridge.hasPendingDiagnosticTask(), true);
      assert.equal(page.accountRow(2).button.disabled, true);
      assert.equal(page.ids["degradation-check-button"].textContent, "继续查询上次检测");
      assert.match(page.ids["form-hint"].textContent, /继续查询/);
      page.degradationCheck();
      await flush();
      assert.equal(server.commands.length, 1);
      assert.equal(server.diagnostics, 0);
      assert.equal(host.bridge.hasPendingDiagnosticTask(), true);
      if (prepareMode !== "unknown") {
        [...server.tasks.values()][0].state = "expired";
        page.degradationCheck();
        await flush();
        assert.equal(host.bridge.hasPendingDiagnosticTask(), false);
        assert.equal(page.ids["degradation-check-button"].textContent, "一键检测原生 Codex");
      }
    });
  }
});

test("missing preparation receipts require a later query before a single commit", async () => {
  const server = createTaskServer({ prepareMode: "missing-receipt" });
  const host = taskHost(server);
  const page = createPage({ bridge: host.bridge });
  await flush();
  page.checkAccount(1);
  await flush();
  assert.equal(server.commands.length, 1);
  assert.equal(server.diagnostics, 0);
  assert.match(page.ids["form-hint"].textContent, /准备回执/);
  server.prepareMode = undefined;
  page.degradationCheck();
  await flush();
  assert.equal(server.commands.length, 2);
  assert.equal(server.diagnostics, 1);
  assert.equal(host.bridge.hasPendingDiagnosticTask(), false);
});

test("lost commit acknowledgements recover completed results without resubmitting", async t => {
  for (const commitMode of ["error-completed", "timeout-completed"]) {
    await t.test(commitMode, async () => {
      const server = createTaskServer({ commitMode });
      const host = taskHost(server);
      const page = createPage({ bridge: host.bridge });
      await flush();
      page.checkAccount(2);
      await flush();
      if (commitMode === "timeout-completed") { host.expire(); await flush(); }
      assert.equal(server.commands.length, 2);
      assert.equal(server.diagnostics, 1);
      assert.equal(host.bridge.hasPendingDiagnosticTask(), false);
      assert.match(page.accountRow(2).result.textContent, /符合检测规则/);
      assert.deepEqual(page.selected(), [1]);
    });
  }
});

test("unknown commit outcomes survive passive polling and ordinary saves and never retry writes", async t => {
  for (const commitMode of ["unknown", "prepared"]) {
    await t.test(commitMode, async () => {
      const server = createTaskServer({ commitMode });
      const host = taskHost(server);
      const page = createPage({ bridge: host.bridge });
      await flush();
      page.checkAccount(1);
      await flush();
      assert.equal(host.bridge.hasPendingDiagnosticTask(), true);
      page.check(2, true);
      page.checkModel("gpt-6-sol", true);
      page.pollStatus();
      page.save();
      await flush();
      assert.equal(host.bridge.hasPendingDiagnosticTask(), true);
      assert.deepEqual(server.store.config.account_ids, [1, 2]);
      assert.ok(server.store.config.enabled_models.includes("gpt-6-sol"));
      assert.equal(page.ids["degradation-check-button"].textContent, "继续查询上次检测");
      page.degradationCheck();
      await flush();
      assert.equal(server.commands.length, 2);
      assert.equal(host.bridge.hasPendingDiagnosticTask(), true);
      assert.equal(page.accountRow(2).button.disabled, true);
      const task = [...server.tasks.values()][0];
      if (commitMode === "unknown") server.complete(task);
      else task.state = "expired";
      page.degradationCheck();
      await flush();
      assert.equal(host.bridge.hasPendingDiagnosticTask(), false);
      assert.equal(server.commands.length, 2);
      assert.deepEqual(page.selected(), [1, 2]);
    });
  }
});

test("owner changes and invalid results stay tied to the original task and target snapshot", async t => {
  for (const failure of ["owner", "request-id", "targets", "duplicate-results", "missing-results"]) {
    await t.test(failure, async () => {
      const server = createTaskServer({ commitMode: "running" });
      const host = taskHost(server);
      const page = createPage({ bridge: host.bridge });
      await flush();
      page.degradationCheck();
      await flush();
      const task = [...server.tasks.values()][0];
      server.complete(task);
      if (failure === "owner") server.ownerID = "instance-b";
      else {
        const details = JSON.parse(task.result.status_json);
        if (failure === "request-id") details.degradation_check.request_id = "another-task";
        if (failure === "targets") details.degradation_check.target_account_ids = [1, 2, 99];
        if (failure === "duplicate-results") details.degradation_check.results[1] = details.degradation_check.results[0];
        if (failure === "missing-results") details.degradation_check.results = [];
        task.result.status_json = JSON.stringify(details);
      }
      host.expire();
      await flush();
      assert.deepEqual(page.selected(), [1]);
      assert.equal(host.bridge.hasPendingDiagnosticTask(), true);
      assert.match(page.ids["form-hint"].textContent, /实例已改变|不匹配/);
      page.degradationCheck();
      await flush();
      assert.equal(server.commands.length, 2);
      assert.equal(server.diagnostics, 1);
      server.ownerID = "instance-a";
      server.complete(task);
      page.degradationCheck();
      await flush();
      assert.equal(host.bridge.hasPendingDiagnosticTask(), false);
    });
  }
});

test("poll exhaustion retains a resumable task and unloading cancels all pending polls", async () => {
  const server = createTaskServer({ commitMode: "running" });
  const host = taskHost(server, { settings: { diagnosticMaxPolls: 1 } });
  const page = createPage({ bridge: host.bridge });
  await flush();
  page.checkAccount(1);
  await flush();
  host.expire();
  await flush();
  assert.equal(host.timers.size, 0);
  assert.equal(host.bridge.hasPendingDiagnosticTask(), true);
  assert.match(page.ids["form-hint"].textContent, /轮询已结束/);
  page.degradationCheck();
  await flush();
  assert.equal(host.timers.size, 1);
  page.unload();
  await flush();
  assert.equal(host.timers.size, 0);
  const requests = host.sent.length;
  host.expire();
  await flush();
  assert.equal(host.sent.length, requests);
  assert.equal(server.commands.length, 2);
});

test("owner replacement after preparation cannot commit on another runtime", async () => {
  const server = createTaskServer();
  const host = createBridgeHarness({ onRequest(request) {
    const response = server.handle(request);
    if (request.config && request.config.diagnostic_task && request.config.diagnostic_task.action === "prepare") {
      server.ownerID = "replacement-instance";
    }
    return response;
  } });
  const page = createPage({ bridge: host.bridge });
  await flush();
  page.checkAccount(1);
  await flush();
  assert.equal(server.commands.length, 1);
  assert.equal(server.diagnostics, 0);
  assert.equal(host.bridge.hasPendingDiagnosticTask(), true);
  assert.match(page.ids["form-hint"].textContent, /实例已改变/);
  page.degradationCheck();
  await flush();
  assert.equal(server.commands.length, 1);
});

test("status failures before prepare write nothing and failures after commit retain the same task", async t => {
  for (const phase of ["preflight", "after-commit"]) {
    await t.test(phase, async () => {
      const server = createTaskServer();
      const host = createBridgeHarness({ onRequest(request) {
        const response = server.handle(request);
        if (phase === "after-commit" && request.config && request.config.diagnostic_task && request.config.diagnostic_task.action === "commit") {
          server.statusError = true;
        }
        return response;
      } });
      const page = createPage({ bridge: host.bridge });
      await flush();
      if (phase === "preflight") server.statusError = true;
      page.checkAccount(1);
      await flush();
      assert.equal(server.commands.length, phase === "preflight" ? 0 : 2);
      assert.equal(host.bridge.hasPendingDiagnosticTask(), phase !== "preflight");
      server.statusError = false;
      if (phase === "after-commit") {
        page.degradationCheck();
        await flush();
        assert.equal(server.commands.length, 2);
        assert.equal(server.diagnostics, 1);
        assert.equal(host.bridge.hasPendingDiagnosticTask(), false);
      }
    });
  }
});

test("loading stale diagnostic commands never replays them and an ordinary save strips them", async () => {
  const server = createTaskServer({ store: { config: { account_ids: [1], diagnostic_task: {
    protocol: "config-job-v1", action: "commit", owner_id: "stale-owner", task_id: "stale-task", receipt: "stale-receipt",
  } } } });
  const page = createPage({ bridge: taskHost(server).bridge });
  await flush();
  assert.equal(server.commands.length, 0);
  assert.equal(server.diagnostics, 0);
  page.save();
  await flush();
  assert.equal(server.commands.length, 0);
  assert.equal(server.store.config.diagnostic_task, undefined);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("hosts without either scoped support or plugin task capability never dispatch account tests", async (t) => {
  for (const markers of [{}, { degradation_check: true, degradation_check_account_id: 1 }]) {
    await t.test(JSON.stringify(markers), async () => {
      const store = { config: { account_ids: [1, 2], auto_select_new_accounts: true,
        bps_auto_disable_on_403: false, ...markers } };
      const before = clone(store.config);
      const dispatched = [];
      const options = { store, test: (_n, shared) => {
        dispatched.push(shared.config.degradation_check_account_id);
        return Promise.reject(new Error("must not dispatch"));
      } };
      const first = createPage(options);
      const second = createPage(options);
      await flush();
      for (const [page, target] of [[first, 1], [second, 2]]) {
        assert.equal(page.accountRow(target).button.disabled, true);
        assert.match(page.accountRow(target).button.title, /原子绑定检测账号/);
        assert.equal(page.ids["degradation-check-button"].disabled, true);
        page.checkAccount(target);
        page.degradationCheck();
        // Directly emitted events also fail closed; disabled buttons alone
        // cannot protect stale pages or programmatically dispatched events.
        page.accountRow(target).button.emit("click");
        page.ids["degradation-check-button"].emit("click");
      }
      await flush();
      assert.deepEqual(dispatched, []);
      assert.deepEqual(store.config, before);
      for (const page of [first, second]) {
        assert.equal(page.calls.save.length, 0);
        assert.equal(page.calls.test, 0);
        assert.match(page.ids["form-hint"].textContent, /账号检测已暂停/);
        assert.equal(page.ids["save-button"].disabled, false);
        assert.deepEqual(page.selected(), [1, 2, 3]);
      }
    });
  }
});

test("blocked diagnostics preserve unsaved route and policy edits and explicit restores", async () => {
  const page = createPage({ status: bpsStatus([[2, bpsBlockA]]), store: { config: {
    account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2],
    bps_reenabled_accounts: { 88: bpsBlockB },
  } } });
  await flush();
  const before = clone(page.store.config);
  page.check(2, true);
  page.check(3, false);
  page.toggle403();
  page.accountRow(1).button.emit("click");
  await flush();
  assert.deepEqual(page.store.config, before);
  assert.equal(page.calls.save.length, 0);
  assert.equal(page.calls.test, 0);
  assert.deepEqual(page.selected(), [1, 2]);
  assert.match(page.accountRow(2).availability.textContent, /待恢复/);
  assert.equal(page.ids["bps-403-toggle"].getAttribute("aria-pressed"), "false");
  page.save();
  await flush();
  assert.equal(page.calls.test, 0);
  assert.deepEqual(page.store.config.account_ids, [1, 2]);
  assert.deepEqual(page.store.config.bps_reenabled_accounts, { 2: bpsBlockA, 88: bpsBlockB });
});

test("passive diagnostic verdicts stay tied to numeric account IDs after reordering", async (t) => {
  for (const fixture of [
    { status: "ok", answer: "苹果17", label: /符合检测规则/ },
    { status: "degraded", answer: "苹果16", label: /疑似降智/ },
    { status: "error", error: "HTTP 403", label: /检测失败/ },
    { status: "error", error: "HTTP 429", label: /检测失败/ },
    { status: "skipped", error: "unavailable", label: /跳过/ },
    { status: "ok", answer: "", label: /检测失败/ },
  ]) {
    await t.test(fixture.status + (fixture.error || fixture.answer), async () => {
      const accounts = [{ id: 1, name: "相同账号名", schedulable: true }, { id: 2, name: "相同账号名", schedulable: true }];
      const { label, ...result } = fixture;
      const status = { healthy: true, status_json: JSON.stringify({ accounts, degradation_check: {
        completed: true, results: [{ account_id: 2, ...result }], degraded_account_ids: [],
      } }) };
      const page = createPage({ status });
      await flush();
      assert.match(page.accountRow(2).result.textContent, label);
      assert.equal(page.accountRow(1).result.textContent, "未检测");
      status.status_json = JSON.stringify({ accounts: [{ ...accounts[1], name: "更名账号" }, accounts[0]] });
      page.pollStatus();
      await flush();
      assert.match(page.accountRow(2).result.textContent, label);
      assert.equal(page.accountRow(2).name.textContent, "更名账号");
      assert.equal(page.accountRow(1).result.textContent, "未检测");
      assert.equal(page.calls.test, 0);
      assert.equal(page.calls.save.length, 0);
    });
  }
});

test("scoped bridge requires advertised support and correlates the diagnostic request ID", async () => {
  const host = createBridgeHarness();
  await assert.rejects(host.bridge.testScoped({ degradation_check: true }), /升级宿主/);
  assert.equal(host.sent.length, 0);
  const load = host.bridge.loadConfig();
  host.reply({ config: {}, capabilities: ["config.testScoped"] });
  await load;
  assert.equal(host.bridge.supportsScopedTest(), true);
  const config = { degradation_check: true, degradation_check_account_id: 2 };
  const pending = host.bridge.testScoped(config);
  const request = host.sent.at(-1).data;
  assert.equal(request.type, "config.testScoped");
  assert.deepEqual(request.config, config);
  assert.equal([...host.timers.values()][0].delay, 150000);
  const result = scopedResult(config);
  const details = JSON.parse(result.status_json);
  details.degradation_check.request_id = request.request_id;
  result.status_json = JSON.stringify(details);
  host.reply({ result });
  assert.deepEqual(clone(await pending), result);
  const wrong = host.bridge.testScoped(config);
  host.reply({ result });
  await assert.rejects(wrong, /标识不匹配/);
  const reload = host.bridge.loadConfig();
  host.reply({ config: {} });
  await reload;
  assert.equal(host.bridge.supportsScopedTest(), false);
});

test("Bridge v1 keeps account tasks capability-gated while retaining ordinary testing", async () => {
  const host = createBridgeHarness();
  assert.match(host.bridge.accountCheckUnavailableReason, /原子绑定检测账号/);
  assert.equal(host.bridge.supportsScopedTest(), false);
  assert.match(htmlSource, /id="account-check-hint"/);
  const pending = host.bridge.testConfig();
  assert.equal(host.sent.length, 1);
  assert.equal(host.sent[0].data.type, "config.test");
  assert.equal(Object.hasOwn(host.sent[0].data, "config"), false);
  assert.equal(Object.hasOwn(host.sent[0].data, "account_id"), false);
  host.reply({ result: { success: true, message: "endpoint reachable" } });
  assert.equal((await pending).message, "endpoint reachable");
});

test("bridge accepts confirmed config objects and clears the request timeout", async () => {
  const host = createBridgeHarness();
  const loaded = host.bridge.loadConfig();
  assert.equal(host.sent[0].data.type, "config.load");
  assert.equal(host.sent[0].targetOrigin, "*");
  host.reply({ config: { account_ids: [2] } });
  assert.deepEqual(clone(await loaded), { account_ids: [2] });
  assert.equal(host.timers.size, 0);
  const saved = host.bridge.saveConfig({ account_ids: [1, 2] });
  assert.equal(host.sent[1].data.type, "config.save");
  assert.deepEqual(host.sent[1].data.config, { account_ids: [1, 2] });
  host.reply({ config: { account_ids: [1, 2], timeout_seconds: 300 } });
  assert.deepEqual(clone(await saved), { account_ids: [1, 2], timeout_seconds: 300 });
  assert.equal(host.timers.size, 0);
});

test("bridge rejects missing or invalid config rather than fabricating success", async (t) => {
  for (const method of ["loadConfig", "saveConfig"]) {
    for (const [name, config] of [["missing", undefined], ["null", null], ["array", []], ["string", "bad"], ["number", 7]]) {
      await t.test(method + " / " + name, async () => {
        const host = createBridgeHarness();
        const result = host.bridge[method]({ account_ids: [3] });
        const rejected = assert.rejects(result, /配置|config/i);
        host.reply(config === undefined ? {} : { config });
        await rejected;
        assert.equal(host.timers.size, 0);
      });
    }
  }
});

test("bridge requires an explicit ok:true acknowledgement", async (t) => {
  for (const ok of [undefined, false, 0, "true"]) {
    await t.test(String(ok), async () => {
      const host = createBridgeHarness();
      const result = host.bridge.saveConfig({ account_ids: [3] });
      const rejected = assert.rejects(result);
      host.reply({ ok, config: { account_ids: [3] } });
      await rejected;
      assert.equal(host.timers.size, 0);
    });
  }
});

test("bridge ignores wrong tokens, window sources, and request identities", async () => {
  const host = createBridgeHarness();
  const result = host.bridge.loadConfig();
  let settled = false;
  result.then(() => { settled = true; }, () => { settled = true; });
  host.reply({ bridge_token: "wrong-token", config: { account_ids: [1] } });
  host.reply({ config: { account_ids: [1] } }, {});
  host.reply({ source: "other-source", config: { account_ids: [1] } });
  host.reply({ request_id: "another-request", config: { account_ids: [1] } });
  await flush();
  assert.equal(settled, false);
  assert.equal(host.timers.size, 1);
  host.reply({ config: { account_ids: [2] } });
  assert.deepEqual(clone(await result), { account_ids: [2] });
  assert.equal(host.timers.size, 0);
});

test("requests without a bridge token fail immediately without posting", async () => {
  const host = createBridgeHarness({ hash: "" });
  assert.equal(host.bridge.hasToken, false);
  for (const method of ["loadConfig", "saveConfig"]) {
    let settled = false;
    const outcome = host.bridge[method]({ account_ids: [] }).then(
      (value) => { settled = true; return { value }; },
      (error) => { settled = true; return { error }; }
    );
    await flush();
    const settledImmediately = settled;
    const timerCount = host.timers.size;
    // Cleanup also makes this test fail quickly against older clients that
    // scheduled a timeout instead of rejecting missing tokens immediately.
    host.expire();
    const result = await outcome;
    assert.equal(settledImmediately, true);
    assert.ok(result.error);
    assert.match(result.error.message, /token|令牌|宿主|重新打开/i);
    assert.equal(host.sent.length, 0);
    assert.equal(timerCount, 0);
  }
});

test("bridge timeouts reject instead of leaving a save unresolved", async () => {
  const host = createBridgeHarness();
  const result = host.bridge.saveConfig({ account_ids: [1] });
  const rejected = assert.rejects(result, /超时/);
  host.expire();
  await rejected;
  assert.equal(host.timers.size, 0);
});

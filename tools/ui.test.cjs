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
    "bps-device-toggle": "button",
    "degradation-result": "div",
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
  ids["model-fields"].appendChild(ids["model-list"]);
  ids["model-fields"].appendChild(ids["model-hint"]);
  ids["account-fields"].appendChild(ids["select-all-button"]);
  ids["account-fields"].appendChild(ids["degradation-check-button"]);
  ids["account-fields"].appendChild(ids["bps-403-toggle"]);
  ids["account-fields"].appendChild(ids["bps-device-toggle"]);
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
  const bridge = {
    hasToken: true,
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
    toggleDevice: () => ids["bps-device-toggle"].click(),
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

test("configuration actions work without sandboxed form submission", () => {
  for (const id of ["save-button", "retry-button", "select-all-button", "bps-403-toggle", "bps-device-toggle"]) {
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
  assert.equal(page.ids["select-all-button"].disabled, true);
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

test("duplicate names keep single-account 403 and 429 verdicts attached to numeric IDs", async () => {
  const sharedName = "同名账号😀-完整名称-不应截断";
  const fixtures = [
    { id: 1234567890123456, error: "HTTP 403" },
    { id: 2234567890123456, error: "HTTP 429" },
  ];
  const page = createPage({
    accounts: fixtures.map((fixture) => ({ id: fixture.id, name: sharedName, schedulable: true })),
    test: (_number, store) => {
      const current = fixtures.find((fixture) => fixture.id === store.config.degradation_check_account_id);
      return Promise.resolve({ status_json: JSON.stringify({ degradation_check: { completed: true, degraded_account_ids: [],
        results: [{ account_id: current.id, name: "wrong-name-from-stale-result", status: "error", error: current.error }],
      } }) });
    },
  });
  await flush();
  for (const [index, fixture] of fixtures.entries()) {
    page.checkAccount(fixture.id);
    await flush();
    assert.equal(page.calls.save[index * 2].degradation_check_account_id, fixture.id);
    assert.match(page.accountRow(fixture.id).result.textContent, /检测失败/);
    assert.equal(page.accountRow(fixture.id).result.title, fixture.error);
    if (index === 0) assert.equal(page.accountRow(fixtures[1].id).result.textContent, "未检测");
    const line = page.ids["degradation-result"].children[1];
    assert.ok(line.textContent.startsWith(sharedName + "（#" + fixture.id + "） · "));
    assert.ok(line.textContent.endsWith(fixture.error));
    assert.doesNotMatch(line.textContent, /wrong-name/);
    assert.deepEqual(page.selected(), fixtures.map((entry) => entry.id));
  }
  assert.equal(page.accountRow(fixtures[0].id).result.title, "HTTP 403");
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

test("device convergence is opt-in and requires the strict true configuration value", async (t) => {
  for (const value of [undefined, null, false, 0, 1, "true", true]) {
    await t.test(String(value), async () => {
      const page = createPage({ store: { config: { account_ids: [], bps_device_convergence: value } } });
      assert.equal(page.ids["bps-device-toggle"].disabled, true);
      page.toggleDevice();
      await flush();
      assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), String(value === true));
      assert.equal(page.ids["bps-device-toggle"].textContent, "设备收敛：" + (value === true ? "已开启" : "已关闭"));
      assert.equal(page.calls.save.length, 0);
    });
  }
});

test("device convergence saves on and off across reopen without polling over unsaved changes", async () => {
  const store = { config: { account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2], bps_auto_disable_on_403: false } };
  const options = { store, getStatus: () => Promise.resolve({ healthy: true, status_json: JSON.stringify({
    accounts: [1, 2, 3].map((id) => ({ id, schedulable: true })), bps_device_convergence: true,
  }) }) };
  let page = createPage(options);
  await flush();
  assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), "false");
  for (const expected of [true, false]) {
    page.toggleDevice();
    assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), String(expected));
    assert.match(page.ids["bps-device-toggle"].textContent, /待保存/);
    assert.equal(page.calls.save.length, 0);
    page.pollStatus();
    await flush();
    assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), String(expected));
    assert.match(page.ids["bps-device-toggle"].textContent, /待保存/);
    page.save();
    assert.equal(page.ids["bps-device-toggle"].disabled, true);
    page.ids["bps-device-toggle"].emit("click");
    await flush();
    assert.equal(store.config.bps_device_convergence, expected);
    assert.equal(store.config.bps_auto_disable_on_403, false);
    assert.deepEqual(page.selected(), [1, 3]);
    assert.deepEqual(store.config.excluded_account_ids, [2]);
    assert.match(page.ids["form-hint"].textContent, /已保存/);
    assert.doesNotMatch(page.ids["bps-device-toggle"].textContent, /待保存/);
    assert.equal(page.ids["bps-device-toggle"].disabled, false);
    page = createPage(options);
    await flush();
    assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), String(expected));
  }
});

test("device convergence rejects unsupported save acknowledgements and read-back values", async (t) => {
  for (const phase of ["save", "reload"]) {
    for (const value of [undefined, false, "true", 1]) {
      await t.test(phase + "/" + String(value), async () => {
        const tamper = (reply) => {
          if (value === undefined) delete reply.bps_device_convergence;
          else reply.bps_device_convergence = value;
          return reply;
        };
        const page = createPage({
          save(config, store) { store.config = clone(config); return Promise.resolve(phase === "save" ? tamper(clone(config)) : clone(config)); },
          load(number, store) { const reply = clone(store.config); return Promise.resolve(number > 1 && phase === "reload" ? tamper(reply) : reply); },
        });
        await flush();
        page.toggleDevice();
        page.save();
        await flush();
        assert.match(page.ids["form-hint"].textContent, /设备收敛开关与提交内容不一致/);
        assert.doesNotMatch(page.ids["form-hint"].textContent, /已保存/);
        assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), "true");
        assert.match(page.ids["bps-device-toggle"].textContent, /待保存/);
        assert.equal(page.ids["bps-device-toggle"].disabled, false);
      });
    }
  }
});

test("disabled device convergence accepts an omitted false value but rejects a true reply", async (t) => {
  for (const phase of ["save", "reload"]) {
    for (const unexpectedTrue of [false, true]) {
      await t.test(phase + "/unexpected_true=" + unexpectedTrue, async () => {
        const page = createPage({
          store: { config: { account_ids: [], bps_device_convergence: true } },
          save(config, store) {
            store.config = clone(config);
            delete store.config.bps_device_convergence;
            const reply = clone(store.config);
            if (phase === "save" && unexpectedTrue) reply.bps_device_convergence = true;
            return Promise.resolve(reply);
          },
          load(number, store) {
            const reply = clone(store.config);
            if (number > 1 && phase === "reload" && unexpectedTrue) reply.bps_device_convergence = true;
            return Promise.resolve(reply);
          },
        });
        await flush();
        page.toggleDevice();
        page.save();
        await flush();
        assert.equal(page.calls.save[0].bps_device_convergence, false);
        assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), "false");
        if (unexpectedTrue) {
          assert.match(page.ids["form-hint"].textContent, /设备收敛开关与提交内容不一致/);
        } else {
          assert.match(page.ids["form-hint"].textContent, /已保存/);
          assert.doesNotMatch(page.ids["bps-device-toggle"].textContent, /待保存/);
          const reopened = createPage({ store: page.store });
          await flush();
          assert.equal(reopened.ids["bps-device-toggle"].getAttribute("aria-pressed"), "false");
        }
      });
    }
  }
});

test("failed device convergence saves preserve the user's pending setting and unlock controls", async (t) => {
  for (const phase of ["save", "reload"]) {
    await t.test(phase, async () => {
      const page = createPage({
        save(config, store) {
          if (phase === "save") return Promise.reject(new Error("save unavailable"));
          store.config = clone(config);
          return Promise.resolve(clone(config));
        },
        load(number, store) { return number > 1 && phase === "reload" ? Promise.reject(new Error("reload unavailable")) : Promise.resolve(clone(store.config)); },
      });
      await flush();
      page.toggleDevice();
      page.save();
      await flush();
      assert.match(page.ids["form-hint"].textContent, /失败|未确认/);
      assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), "true");
      assert.match(page.ids["bps-device-toggle"].textContent, /待保存/);
      assert.equal(page.ids["bps-device-toggle"].disabled, false);
    });
  }
});

test("all diagnostic saves and failure cleanup preserve the selected device convergence policy", async (t) => {
  for (const targeted of [false, true]) {
    for (const enabled of [false, true]) {
      for (const failed of [false, true]) {
        await t.test("targeted=" + targeted + "/enabled=" + enabled + "/failed=" + failed, async () => {
          const page = createPage({
            store: { config: { account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2], bps_device_convergence: !enabled } },
            test: () => failed ? Promise.reject(new Error("diagnostic unavailable")) : Promise.resolve({ status_json: JSON.stringify({
              degradation_check: { completed: true, results: [{ account_id: 2, status: "ok", answer: "苹果17" }], degraded_account_ids: [] },
            }) }),
          });
          await flush();
          page.toggleDevice();
          if (targeted) page.checkAccount(2);
          else page.degradationCheck();
          assert.equal(page.ids["bps-device-toggle"].disabled, true);
          page.ids["bps-device-toggle"].emit("click");
          await flush();
          assert.equal(page.calls.test, 1);
          assert.equal(page.calls.save.length, 2);
          for (const saved of page.calls.save) assert.equal(saved.bps_device_convergence, enabled);
          assert.equal(page.store.config.bps_device_convergence, enabled);
          assert.equal(page.store.config.degradation_check, false);
          assert.ok(!page.store.config.degradation_check_account_id);
          assert.equal(page.ids["bps-device-toggle"].getAttribute("aria-pressed"), String(enabled));
          assert.equal(page.ids["bps-device-toggle"].disabled, false);
          assert.deepEqual(page.selected(), [1, 3]);
          if (failed) assert.match(page.ids["form-hint"].textContent, /降智检测失败/);
          else assert.doesNotMatch(page.ids["bps-device-toggle"].textContent, /待保存/);
        });
      }
    }
  }
});

test("diagnostics reject a lost enabled device setting at trigger, cleanup, or read-back", async (t) => {
  for (const phase of ["trigger", "cleanup", "reload"]) {
    await t.test(phase, async () => {
      const page = createPage({
        store: { config: { account_ids: [], bps_device_convergence: true } },
        save(config, store) {
          store.config = clone(config);
          const reply = clone(config);
          if ((phase === "trigger" && config.degradation_check) || (phase === "cleanup" && !config.degradation_check)) delete reply.bps_device_convergence;
          return Promise.resolve(reply);
        },
        load(number, store) {
          const reply = clone(store.config);
          if (number > 1 && phase === "reload") delete reply.bps_device_convergence;
          return Promise.resolve(reply);
        },
        test: () => Promise.resolve({ status_json: JSON.stringify({ degradation_check: { completed: true, results: [{ account_id: 2, status: "ok", answer: "苹果17" }], degraded_account_ids: [] } }) }),
      });
      await flush();
      page.checkAccount(2);
      await flush();
      assert.equal(page.calls.test, phase === "trigger" ? 0 : 1);
      assert.match(page.ids["form-hint"].textContent, /设备收敛开关与提交内容不一致/);
      assert.equal(page.store.config.degradation_check, false);
      for (const saved of page.calls.save) assert.equal(saved.bps_device_convergence, true);
      assert.equal(page.ids["bps-device-toggle"].disabled, false);
    });
  }
});

test("single-account checks keep every result tied to its ID across repeated checks", async () => {
  const fixtures = [
    { account_id: 1, status: "ok", answer: "苹果17", label: /符合检测规则/ },
    { account_id: 2, status: "degraded", answer: "苹果16", label: /疑似降智/ },
    { account_id: 3, status: "error", error: "HTTP 429", label: /检测失败/ },
  ];
  const probed = [];
  const page = createPage({
    store: { config: { account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2], bps_auto_disable_on_403: false } },
    accounts: fixtures.map((fixture) => ({ id: fixture.account_id, name: "identical-name", schedulable: true })),
    test(_number, store) {
      const target = store.config.degradation_check_account_id;
      probed.push(target);
      const { label, ...result } = fixtures.find((fixture) => fixture.account_id === target);
      return Promise.resolve({ status_json: JSON.stringify({ degradation_check: {
        completed: true, results: [result], degraded_account_ids: result.status === "degraded" ? [target] : [],
      } }) });
    },
  });
  await flush();
  const checked = new Set();
  for (const target of [3, 1, 2, 1]) {
    page.checkAccount(target);
    assert.equal(page.ids["bps-403-toggle"].disabled, true);
    page.toggle403();
    await flush();
    checked.add(target);
    for (const fixture of fixtures) {
      assert.match(page.accountRow(fixture.account_id).result.textContent, checked.has(fixture.account_id) ? fixture.label : /未检测/);
    }
    assert.deepEqual(page.selected(), [1, 3]);
    assert.equal(page.store.config.bps_auto_disable_on_403, false);
    assert.equal(page.store.config.degradation_check, false);
    assert.ok(!page.store.config.degradation_check_account_id);
  }
  assert.deepEqual(probed, [3, 1, 2, 1]);
});

test("diagnostics do not start when the host loses the 403 switch setting", async () => {
  const page = createPage({
    store: { config: { account_ids: [], bps_auto_disable_on_403: false } },
    save(config, store) {
      store.config = clone(config);
      const reply = clone(config);
      if (config.degradation_check) delete reply.bps_auto_disable_on_403;
      return Promise.resolve(reply);
    },
  });
  await flush();
  page.checkAccount(2);
  await flush();
  assert.equal(page.calls.test, 0);
  assert.equal(page.store.config.degradation_check, false);
  assert.equal(page.store.config.bps_auto_disable_on_403, false);
  assert.match(page.ids["form-hint"].textContent, /403 自动停用开关与提交内容不一致/);
});

test("single-account diagnostics use the real ID and preserve selection models and exclusions", async () => {
  const config = {
    account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2, 88],
    enabled_models: ["gpt-6-astra"], timeout_seconds: 123,
    bps_auto_disable_on_403: false,
    bps_device_convergence: true,
  };
  const page = createPage({
    store: { config: clone(config) },
    accounts: [
      { id: 1, name: "shared-first@example.com", schedulable: true },
      { id: 2, name: "shared-second@example.com", schedulable: true },
      { id: 3, name: "Third", schedulable: true },
    ],
    test: (_number, store) => {
      assert.equal(store.config.degradation_check, true);
      assert.equal(store.config.degradation_check_account_id, 2);
      return Promise.resolve({ status_json: JSON.stringify({ degradation_check: {
        completed: true, degraded_account_ids: [2],
        results: [{ account_id: 2, status: "degraded", answer: "苹果16" }],
      } }) });
    },
  });
  await flush();
  page.checkAccount(2);
  await flush();
  assert.equal(page.calls.test, 1);
  assert.equal(page.calls.save.length, 2);
  assert.equal(page.calls.save[0].degradation_check_account_id, 2);
  assert.equal(page.calls.save[0].degradation_check, true);
  assert.equal(page.store.config.degradation_check, false);
  assert.ok(!page.store.config.degradation_check_account_id);
  for (const saved of page.calls.save) {
    for (const key of Object.keys(config)) assert.deepEqual(saved[key], config[key], key + " must survive diagnostics");
  }
  assert.deepEqual(page.selected(), [1, 3]);
  assert.deepEqual(page.selectedModels(), ["gpt-6-astra"]);
  assert.match(page.accountRow(2).result.textContent, /疑似降智/);
  assert.doesNotMatch(page.ids["form-hint"].textContent, /已自动选择/);
});

test("single-account diagnostics render each outcome without treating failures as degradation", async (t) => {
  for (const fixture of [
    { status: "ok", answer: "苹果17", label: /符合检测规则|正常/ },
    { status: "degraded", answer: "苹果16", label: /疑似降智/ },
    { status: "error", error: "HTTP 429", label: /检测失败/ },
    { status: "skipped", error: "account is not schedulable", label: /跳过/ },
  ]) {
    await t.test(fixture.status, async () => {
      const result = { account_id: 2, status: fixture.status };
      if (fixture.answer) result.answer = fixture.answer;
      if (fixture.error) result.error = fixture.error;
      const page = createPage({
        store: { config: { account_ids: [1], auto_select_new_accounts: true, excluded_account_ids: [2, 3] } },
        test: () => Promise.resolve({ status_json: JSON.stringify({ degradation_check: {
          completed: true, degraded_account_ids: fixture.status === "degraded" ? [2] : [], results: [result],
        } }) }),
      });
      await flush();
      page.checkAccount(2);
      await flush();
      assert.deepEqual(page.selected(), [1]);
      assert.deepEqual(page.store.config.excluded_account_ids, [2, 3]);
      assert.match(page.accountRow(2).result.textContent, fixture.label);
      assert.equal(page.store.config.degradation_check, false);
      assert.ok(!page.store.config.degradation_check_account_id);
      assert.equal(page.accountRow(2).button.isDisabled(), false);
    });
  }
});

test("single-account diagnostics clear both transient fields when requests fail", async () => {
  const page = createPage({
    store: { config: { account_ids: [1], auto_select_new_accounts: true, excluded_account_ids: [2, 3] } },
    test: () => Promise.reject(new Error("test timed out")),
  });
  await flush();
  page.checkAccount(2);
  await flush();
  assert.equal(page.calls.save.length, 2);
  assert.equal(page.calls.save[0].degradation_check_account_id, 2);
  assert.equal(page.calls.save[1].degradation_check, false);
  assert.ok(!page.calls.save[1].degradation_check_account_id);
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.deepEqual(page.store.config.excluded_account_ids, [2, 3]);
  assert.deepEqual(page.selected(), [1]);
  assert.match(page.accountRow(2).result.textContent, /检测失败/);
  assert.match(page.ids["form-hint"].textContent, /降智检测失败/);
  assert.equal(page.accountRow(2).button.isDisabled(), false);
});

test("single-account diagnostics reject malformed and unrelated results", async (t) => {
  const badResults = [
    { title: "invalid JSON", result: { status_json: "{" } },
    { title: "missing report", result: {} },
    { title: "incomplete report", check: { state: "running", results: [] } },
    { title: "missing target", check: { completed: true, results: [] } },
    { title: "another account", check: { completed: true, results: [{ account_id: 1, status: "degraded", answer: "苹果16" }] } },
    { title: "empty normal answer", check: { completed: true, results: [{ account_id: 2, status: "ok", answer: "   " }] } },
    { title: "empty degraded answer", check: { completed: true, results: [{ account_id: 2, status: "degraded", answer: "" }] } },
    { title: "conflicting target verdicts", check: { completed: true, results: [
      { account_id: 2, status: "degraded", answer: "苹果16" }, { account_id: 2, status: "error", error: "HTTP 429" },
    ] } },
  ];
  for (const fixture of badResults) {
    await t.test(fixture.title, async () => {
      const page = createPage({
        store: { config: { account_ids: [1], auto_select_new_accounts: true, excluded_account_ids: [2, 3] } },
        test: () => Promise.resolve(fixture.result || { status_json: JSON.stringify({ degradation_check: fixture.check }) }),
      });
      await flush();
      page.checkAccount(2);
      await flush();
      assert.deepEqual(page.selected(), [1]);
      assert.deepEqual(page.store.config.excluded_account_ids, [2, 3]);
      assert.equal(page.store.config.degradation_check, false);
      assert.ok(!page.store.config.degradation_check_account_id);
      assert.match(page.accountRow(2).result.textContent, /检测失败/);
      assert.doesNotMatch(page.accountRow(1).result.textContent, /疑似降智/);
      assert.match(page.ids["form-hint"].textContent, /检测失败/);
    });
  }
});

test("single-account diagnostics reject dropped or changed target acknowledgement before probing", async (t) => {
  for (const target of [undefined, 0, 1]) {
    await t.test(String(target), async () => {
      const page = createPage({ save(config, store) {
        store.config = clone(config);
        const reply = clone(config);
        if (config.degradation_check) {
          if (target === undefined) delete reply.degradation_check_account_id;
          else reply.degradation_check_account_id = target;
        }
        return Promise.resolve(reply);
      } });
      await flush();
      page.checkAccount(2);
      await flush();
      assert.equal(page.calls.test, 0, "A lost target must never run a bulk check");
      assert.equal(page.store.config.degradation_check, false);
      assert.ok(!page.store.config.degradation_check_account_id);
      assert.match(page.accountRow(2).result.textContent, /检测失败/);
      assert.equal(page.accountRow(2).button.isDisabled(), false);
    });
  }
});

test("account diagnostics disable unavailable accounts and prevent overlapping checks", async () => {
  const pending = deferred();
  const page = createPage({
    accounts: [
      { id: 1, name: "First", schedulable: true },
      { id: 2, name: "Second", schedulable: true },
      { id: 3, name: "Paused", schedulable: false, status: "paused" },
    ],
    test: () => pending.promise,
  });
  await flush();
  assert.equal(page.accountRow(3).button.isDisabled(), true);
  page.checkAccount(3);
  assert.equal(page.calls.save.length, 0);
  const selected = page.selected();
  page.checkAccount(1);
  await flush();
  assert.equal(page.calls.test, 1);
  assert.match(page.accountRow(1).result.textContent, /检测中|正在检测/);
  assert.equal(page.accountRow(2).button.isDisabled(), true);
  page.checkAccount(2);
  page.checkAccount(1);
  page.degradationCheck();
  page.check(2, false);
  page.save();
  page.selectAll();
  assert.equal(page.calls.save.length, 1);
  assert.equal(page.calls.test, 1);
  assert.deepEqual(page.selected(), selected);
  pending.resolve({ status_json: JSON.stringify({ degradation_check: {
    completed: true, results: [{ account_id: 1, status: "ok", answer: "苹果17" }], degraded_account_ids: [],
  } }) });
  await flush();
  assert.equal(page.accountRow(1).button.isDisabled(), false);
  assert.equal(page.accountRow(2).button.isDisabled(), false);
  assert.equal(page.accountRow(3).button.isDisabled(), true);
});

test("per-account verdicts remain attached to real IDs after status polling and row reorder", async () => {
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [
    { id: 1, name: "shared-one", schedulable: true }, { id: 2, name: "shared-two", schedulable: true },
  ] }) };
  const page = createPage({ status, test: (_number, store) => {
    const accountID = store.config.degradation_check_account_id;
    return Promise.resolve({ status_json: JSON.stringify({ degradation_check: {
      completed: true, degraded_account_ids: accountID === 1 ? [1] : [],
      results: [{ account_id: accountID, status: accountID === 1 ? "degraded" : "ok", answer: accountID === 1 ? "苹果16" : "苹果17" }],
    } }) });
  } });
  await flush();
  page.checkAccount(1);
  await flush();
  page.checkAccount(2);
  await flush();
  status.status_json = JSON.stringify({ accounts: [
    { id: 2, name: "renamed-two", schedulable: true },
    { id: 3, name: "new-account", schedulable: true },
    { id: 1, name: "renamed-one", schedulable: true },
  ] });
  page.pollStatus();
  await flush();
  assert.match(page.accountRow(1).result.textContent, /疑似降智/);
  assert.match(page.accountRow(2).result.textContent, /符合检测规则|正常/);
  assert.doesNotMatch(page.accountRow(3).result.textContent, /疑似降智|符合检测规则|正常/);
  assert.equal(page.accountRow(1).name.textContent, "renamed-one");
  assert.equal(page.accountRow(2).name.title, "账号 ID：2");
  assert.deepEqual(page.selected(), [1, 2, 3]);
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

test("degradation check selects returned degraded accounts and clears its trigger", async () => {
  const store = { config: { account_ids: [1], enabled_models: defaultModels, degradation_check: false } };
  const page = createPage({
    store,
    test: () => Promise.resolve({ status_json: JSON.stringify({
      degradation_check: {
        completed: true,
        expected: "苹果17",
        degraded_account_ids: [2],
        results: [
          { account_id: 1, status: "ok", answer: "苹果17" },
          { account_id: 2, status: "degraded", answer: "苹果16" },
          { account_id: 3, status: "skipped", error: "account is not schedulable" },
        ],
      },
    }) }),
  });
  await flush();
  page.degradationCheck();
  await flush();
  assert.equal(page.calls.test, 1);
  assert.deepEqual(page.calls.save[0].degradation_check, true);
  assert.deepEqual(page.calls.save[1].account_ids, [2]);
  assert.equal(page.calls.save[1].degradation_check, false);
  assert.deepEqual(page.store.config.account_ids, [2]);
  assert.equal(page.store.config.degradation_check, false);
  assert.deepEqual(page.selected(), [2]);
  assert.match(page.ids["form-hint"].textContent, /已自动选择 1 个降智账号/);
  assert.match(page.ids["degradation-result"].children.map((item) => item.textContent).join(" "), /Second（#2） · 疑似降智/);
});

test("degradation check keeps the existing account scope when none are degraded", async () => {
  const store = { config: { account_ids: [1], enabled_models: defaultModels, degradation_check: false } };
  const page = createPage({
    store,
    test: () => Promise.resolve({ status_json: JSON.stringify({
      degradation_check: { completed: true, degraded_account_ids: [], results: [] },
    }) }),
  });
  await flush();
  page.degradationCheck();
  await flush();
  assert.deepEqual(page.calls.save[1].account_ids, [1]);
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.match(page.ids["form-hint"].textContent, /保留原账号选择/);
});

test("degradation check excludes failed skipped empty and conflicting verdicts", async () => {
  const store = { config: { account_ids: [1], enabled_models: defaultModels, degradation_check: false } };
  const page = createPage({
    store,
    test: () => Promise.resolve({ status_json: JSON.stringify({
      degradation_check: {
        completed: true,
        degraded_account_ids: [2, 3, 4, 5, 6, 7, 8, 99],
        results: [
          { account_id: 2, status: "error", error: "HTTP 429" },
          { account_id: 3, status: "error", error: "HTTP 401" },
          { account_id: 4, status: "error", error: "HTTP 403" },
          { account_id: 5, status: "error", error: "deadline exceeded" },
          { account_id: 6, status: "skipped" },
          { account_id: 7, status: "degraded", answer: "   " },
          { account_id: 8, status: "degraded", answer: "苹果16" },
          { account_id: 8, status: "error", error: "HTTP 429" },
        ],
      },
    }) }),
  });
  await flush();
  page.degradationCheck();
  await flush();
  assert.deepEqual(page.calls.save[1].account_ids, [1]);
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.deepEqual(page.selected(), [1]);
  assert.equal(page.store.config.degradation_check, false);
});

test("all failed degradation probes report no valid answer and preserve scope", async () => {
  const store = { config: { account_ids: [1], enabled_models: defaultModels } };
  const page = createPage({
    store,
    test: () => Promise.resolve({ status_json: JSON.stringify({
      degradation_check: {
        completed: true, degraded_account_ids: [],
        results: [{ account_id: 1, status: "error", error: "HTTP 429" }],
      },
    }) }),
  });
  await flush();
  page.degradationCheck();
  await flush();
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.match(page.ids["form-hint"].textContent, /未获得有效回答/);
  assert.match(page.ids["form-hint"].className, /hint-error/);
  assert.match(page.ids["degradation-result"].children.map((item) => item.textContent).join(" "), /未获得有效回答/);
});

test("a failed degradation result save restores the original scope", async () => {
  let saves = 0;
  const store = { config: { account_ids: [1], enabled_models: defaultModels } };
  const page = createPage({
    store,
    save: (config, target) => {
      saves++;
      if (saves === 2) return Promise.reject(new Error("save unavailable"));
      target.config = clone(config);
      return Promise.resolve(clone(target.config));
    },
    test: () => Promise.resolve({ status_json: JSON.stringify({
      degradation_check: {
        completed: true, degraded_account_ids: [2],
        results: [{ account_id: 2, status: "degraded", answer: "苹果16" }],
      },
    }) }),
  });
  await flush();
  page.degradationCheck();
  await flush();
  assert.equal(page.calls.save.length, 3);
  assert.deepEqual(page.calls.save[1].account_ids, [2]);
  assert.deepEqual(page.calls.save[2].account_ids, [1]);
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.deepEqual(page.selected(), [1]);
  assert.equal(page.store.config.degradation_check, false);
  assert.match(page.ids["form-hint"].textContent, /降智检测失败/);
  assert.equal(page.ids["save-button"].disabled, false);
});

test("a rejected degradation test clears its trigger without changing account scope", async () => {
  const store = { config: { account_ids: [1], enabled_models: defaultModels } };
  const page = createPage({ store, test: () => Promise.reject(new Error("test timed out")) });
  await flush();
  page.degradationCheck();
  await flush();
  assert.deepEqual(page.store.config.account_ids, [1]);
  assert.deepEqual(page.selected(), [1]);
  assert.equal(page.store.config.degradation_check, false);
  assert.match(page.ids["form-hint"].textContent, /降智检测失败/);
  assert.equal(page.ids["save-button"].disabled, false);
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
  assert.equal(page.ids["select-all-button"].disabled, true);
  status.status_json = JSON.stringify({ accounts: [{ id: 2 }] });
  page.pollStatus();
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, true);
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
  assert.deepEqual(submitted, { account_ids: [1, 2], auto_select_new_accounts: true, excluded_account_ids: [3], enabled_models: defaultModels, timeout_seconds: 123, rewrite_tools: false, bps_auto_disable_on_403: true, bps_device_convergence: false });
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
  assert.doesNotMatch(htmlSource, /<input |<select |<details /);
  const page = createPage();
  await flush();
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[0], { account_ids: [1, 2, 3], auto_select_new_accounts: true, excluded_account_ids: [], enabled_models: defaultModels, bps_auto_disable_on_403: true, bps_device_convergence: false });
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
  assert.deepEqual(page.store.config, { account_ids: [1], auto_select_new_accounts: true, excluded_account_ids: [2, 3], enabled_models: defaultModels, bps_auto_disable_on_403: true, bps_device_convergence: false });
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

test("degradation selection updates existing exclusions while concurrent and future new accounts stay automatic", async () => {
  const pending = deferred();
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }, { id: 3 }] }) };
  const page = createPage({ status, store: { config: { auto_select_new_accounts: true, account_ids: [1], excluded_account_ids: [3, 88] } }, test: () => pending.promise });
  await flush();
  page.degradationCheck();
  await flush();
  status.status_json = JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }, { id: 3 }, { id: 4 }] });
  page.pollStatus();
  await flush();
  pending.resolve({ status_json: JSON.stringify({ degradation_check: { completed: true, degraded_account_ids: [2], results: [{ account_id: 2, status: "degraded", answer: "苹果16" }] } }) });
  await flush();
  assert.equal(page.store.config.auto_select_new_accounts, true);
  assert.deepEqual(page.store.config.excluded_account_ids.sort((a, b) => a - b), [1, 3, 88]);
  assert.deepEqual(page.selected(), [2, 4]);
  const reopened = createPage({ store: page.store, accounts: [{ id: 1 }, { id: 2 }, { id: 5 }, { id: 88 }] });
  await flush();
  assert.deepEqual(reopened.selected(), [2, 5]);
});

test("degradation save and read-back reject dropped automatic mode or exclusions", async (t) => {
  for (const stage of ["acknowledgement", "read-back"]) {
    for (const field of ["auto_select_new_accounts", "excluded_account_ids"]) {
      await t.test(stage + " " + field, async () => {
        let saves = 0;
        const page = createPage({ store: { config: { account_ids: [1] } }, save(config, store) {
          saves++;
          const changed = clone(config);
          if (saves === 2) delete changed[field];
          store.config = stage === "read-back" ? changed : clone(config);
          return Promise.resolve(stage === "acknowledgement" ? changed : clone(config));
        }, test: () => Promise.resolve({ status_json: JSON.stringify({ degradation_check: {
          completed: true, degraded_account_ids: [2], results: [{ account_id: 2, status: "degraded", answer: "苹果16" }],
        } }) }) });
        await flush();
        page.degradationCheck();
        await flush();
        assert.match(page.ids["form-hint"].textContent, /降智检测失败/);
        assert.doesNotMatch(page.ids["form-hint"].textContent, /已自动选择/);
        assert.deepEqual(page.selected(), [1]);
      });
    }
  }
});

test("failed degradation cleanup never reselects locally excluded accounts", async () => {
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }] }) };
  let saves = 0;
  const page = createPage({ status, save(config, store) {
    if (++saves > 1) return Promise.reject(new Error("cleanup unavailable"));
    store.config = clone(config);
    return Promise.resolve(clone(config));
  }, test: () => Promise.reject(new Error("test unavailable")) });
  await flush();
  page.check(1, false);
  page.degradationCheck();
  await flush();
  status.status_json = JSON.stringify({ accounts: [{ id: 1 }, { id: 2 }, { id: 3 }] });
  page.pollStatus();
  await flush();
  assert.deepEqual(page.selected(), [2, 3]);
  assert.deepEqual(page.calls.save[1].excluded_account_ids, [1]);
  assert.match(page.ids["form-hint"].textContent, /降智检测失败/);
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

test("diagnostic saves preserve existing acknowledgements but never submit a pending restore", async () => {
  const status = bpsStatus([[2, bpsBlockA]]);
  const page = createPage({ status, store: { config: { account_ids: [1, 3], auto_select_new_accounts: true, excluded_account_ids: [2], bps_reenabled_accounts: { 88: bpsBlockB } } }, test: () => Promise.resolve({
    status_json: JSON.stringify({ bps_disabled_account_ids: [2], bps_disabled_accounts: [{ account_id: 2, block_id: bpsBlockA }],
      degradation_check: { completed: true, results: [{ account_id: 1, status: "ok", answer: "苹果17" }], degraded_account_ids: [] } }),
  }) });
  await flush();
  page.check(2, true);
  page.checkAccount(1);
  await flush();
  assert.equal(page.calls.save.length, 2);
  for (const saved of page.calls.save) {
    assert.deepEqual(saved.bps_reenabled_accounts, { 88: bpsBlockB });
    assert.deepEqual(saved.excluded_account_ids, [2]);
    assert.ok(!saved.account_ids.includes(2));
  }
  assert.deepEqual(page.selected(), [1, 2, 3], "The manual restore remains an unsaved local choice");
  assert.match(page.accountRow(2).availability.textContent, /待恢复/);
});

test("403 state returned by diagnostics is excluded before cleanup is saved", async () => {
  const status = bpsStatus();
  const page = createPage({ status, test: () => {
    Object.assign(status, bpsStatus([[2, bpsBlockA]]));
    const details = JSON.parse(status.status_json);
    details.degradation_check = { completed: true, results: [{ account_id: 2, status: "error", error: "upstream returned HTTP 403" }], degraded_account_ids: [] };
    return Promise.resolve({ status_json: JSON.stringify(details) });
  } });
  await flush();
  page.checkAccount(2);
  await flush();
  assert.equal(page.calls.save.length, 2);
  assert.deepEqual(page.calls.save[1].excluded_account_ids, [2]);
  assert.deepEqual(page.calls.save[1].account_ids, [1, 3]);
  assert.deepEqual(page.selected(), [1, 3]);
  assert.match(page.accountRow(2).availability.textContent, /403/);
  assert.match(page.accountRow(2).result.textContent, /失败/);
});

test("failed diagnostics cleanup merges a concurrent 403 and preserves confirmed acknowledgements", async () => {
  const pending = deferred();
  const status = bpsStatus();
  const page = createPage({ status, store: { config: { account_ids: [1, 2, 3], auto_select_new_accounts: true, bps_reenabled_accounts: { 88: bpsBlockA } } }, test: () => pending.promise });
  await flush();
  page.checkAccount(1);
  await flush();
  Object.assign(status, bpsStatus([[2, bpsBlockB]]));
  page.pollStatus();
  await flush();
  pending.reject(new Error("diagnostic unavailable"));
  await flush();
  assert.equal(page.calls.save.length, 2);
  assert.deepEqual(page.calls.save[1].account_ids, [1, 3]);
  assert.deepEqual(page.calls.save[1].excluded_account_ids, [2]);
  assert.deepEqual(page.calls.save[1].bps_reenabled_accounts, { 88: bpsBlockA });
  assert.deepEqual(page.selected(), [1, 3]);
  const reopened = createPage({ status, store: page.store });
  await flush();
  assert.deepEqual(reopened.selected(), [1, 3]);
  assert.equal(reopened.calls.save.length, 0);
  assert.match(reopened.accountRow(2).availability.textContent, /403/);
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
    for (const marker of [{ degradation_check: true }, { degradation_check_account_id: 2 }]) {
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

test("diagnostic 403 snapshots supersede status polls started before their result", async () => {
  const stalePoll = deferred();
  const finalSave = deferred();
  let statusCalls = 0;
  let saveCalls = 0;
  const page = createPage({
    getStatus: () => ++statusCalls === 2 ? stalePoll.promise : Promise.resolve(bpsStatus()),
    save: (config, store) => {
      store.config = clone(config);
      return ++saveCalls === 2 ? finalSave.promise : Promise.resolve(clone(config));
    },
    test: () => Promise.resolve({ status_json: JSON.stringify({
      bps_disabled_accounts: [{ account_id: 2, block_id: bpsBlockA }],
      degradation_check: { completed: true, degraded_account_ids: [], results: [{ account_id: 2, status: "error", error: "HTTP 403" }] },
    }) }),
  });
  await flush();
  page.pollStatus();
  page.checkAccount(2);
  await flush();
  assert.match(page.accountRow(2).availability.textContent, /403/);
  stalePoll.resolve(bpsStatus());
  await flush();
  assert.match(page.accountRow(2).availability.textContent, /403/, "An earlier status response must not erase the diagnostic block");
  assert.equal(page.accountRow(2).checkbox.checked, false);
  finalSave.resolve(clone(page.store.config));
  await flush();
});

function createBridgeHarness(options = {}) {
  const listeners = new Set();
  const sent = [];
  const timers = new Map();
  let nextID = 0;
  const parent = { postMessage(data, targetOrigin) { sent.push({ data: clone(data), targetOrigin }); } };
  const window = {
    parent,
    location: { hash: options.hash === undefined ? "#bridge_token=trusted-token" : options.hash },
    crypto: { randomUUID: () => "request-" + (++nextID) },
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
      const data = Object.assign({ source: "sub2api-plugin-host", bridge_token: "trusted-token", request_id: request.request_id, ok: true }, overrides);
      for (const listener of listeners) listener({ source, data });
    },
    expire() {
      for (const [id, timer] of [...timers]) {
        timers.delete(id);
        timer.callback();
      }
    },
  };
}

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

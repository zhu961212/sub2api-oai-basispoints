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

function createPage(options = {}) {
  const store = options.store || { config: { account_ids: [] } };
  const ids = {};
  const tags = {
    "config-form": "form",
    "account-fields": "fieldset",
    "account-list": "div",
    "account-hint": "span",
    "state-chip": "span",
    "form-hint": "p",
    "version-line": "footer",
    "save-button": "button",
    "select-all-button": "button",
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
  ids["account-fields"].appendChild(ids["select-all-button"]);
  ids["account-fields"].appendChild(ids["account-list"]);
  ids["account-fields"].appendChild(ids["account-hint"]);
  ids["config-form"].appendChild(ids["save-button"]);
  ids["config-form"].appendChild(ids["retry-button"]);
  const calls = { load: 0, save: [], status: 0, ready: 0 };
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
    resize() {},
    dispose() {},
    status() { calls.status++; return Promise.resolve(clone(status)); },
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
  };
  const windowListeners = new Map();
  let pollStatus;
  const window = {
    Sub2APIPluginBridge: { create: () => bridge },
    requestAnimationFrame(callback) { callback(); },
    addEventListener(type, listener) { windowListeners.set(type, listener); },
    setInterval(callback) { pollStatus = callback; return 1; },
    clearInterval() {},
  };
  const document = {
    getElementById: (id) => ids[id] || null,
    createElement: (tagName) => new Element(tagName),
    body: new Element("body"),
    documentElement: new Element("html"),
    visibilityState: "visible",
  };
  vm.runInNewContext(appSource, { window, document, console }, { filename: "ui/assets/app.js" });
  const boxes = () => ids["account-list"].children.flatMap((label) => label.children).filter((node) => node.tagName === "INPUT");
  return {
    ids, calls, store,
    selected: () => boxes().filter((box) => box.checked).map((box) => Number(box.value)).sort((a, b) => a - b),
    check(accountID, checked) {
      const box = boxes().find((item) => Number(item.value) === accountID);
      assert.ok(box, "Account #" + accountID + " must be rendered");
      box.userCheck(checked);
    },
    save: () => ids["save-button"].click(),
    selectAll: () => ids["select-all-button"].click(),
    pollStatus: () => pollStatus(),
    retry: () => ids["retry-button"].click(),
  };
}

test("configuration actions work without sandboxed form submission", () => {
  for (const id of ["save-button", "retry-button", "select-all-button"]) {
    const tag = htmlSource.match(new RegExp("<button\\b[^>]*\\bid=[\"']" + id + "[\"'][^>]*>"));
    assert.ok(tag, id + " exists");
    assert.match(tag[0], /\btype=["']button["']/);
  }
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
  verification.resolve({ account_ids: [2, 3] });
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, false);
  assert.deepEqual(page.calls.save[0].account_ids, [2, 3]);
});

test("status refresh enables select all for new accounts without losing unsaved selections", async () => {
  const status = { healthy: true, status_json: JSON.stringify({ accounts: [{ id: 1 }] }) };
  const page = createPage({ status });
  await flush();
  page.selectAll();
  assert.equal(page.ids["select-all-button"].disabled, true);
  status.status_json = JSON.stringify({ accounts: [{ id: 2 }] });
  page.pollStatus();
  await flush();
  assert.equal(page.ids["select-all-button"].disabled, false);
  page.selectAll();
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[0].account_ids, [1, 2]);
});

test("selected accounts save on click, persist on read-back, and survive reopening", async () => {
  const store = { config: { account_ids: [2], timeout_seconds: 123, model_map: { original: "preserved" }, obsolete_field: "remove" } };
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
  assert.deepEqual(submitted, { account_ids: [1, 2], timeout_seconds: 123, model_map: { original: "preserved" } });
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
  verification.resolve({ account_ids: [2, 1] });
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

test("legacy account_id migrates only when account_ids is absent", async (t) => {
  for (const [name, config, expected] of [
    ["legacy single account", { account_id: 2 }, [2]],
    ["explicit empty selection wins", { account_id: 2, account_ids: [] }, []],
    ["explicit selected accounts win", { account_id: 2, account_ids: [1] }, [1]],
  ]) {
    await t.test(name, async () => {
      const page = createPage({ store: { config } });
      await flush();
      assert.deepEqual(page.selected(), expected);
      page.save();
      await flush();
      assert.deepEqual(page.calls.save[0].account_ids, expected);
      assert.equal(Object.hasOwn(page.calls.save[0], "account_id"), false);
      assert.match(page.ids["form-hint"].textContent, /已保存/);
    });
  }
});

test("automatic images need no configuration controls", async () => {
  assert.match(htmlSource, /支持直接发送图片和截图，无需额外配置/);
  assert.doesNotMatch(htmlSource, /image-relay|图片中转|公网 HTTPS|反向代理|监听地址|存储目录/);
  assert.doesNotMatch(htmlSource, /<input |<select |<details /);
  const page = createPage();
  await flush();
  page.save();
  await flush();
  assert.deepEqual(page.calls.save[0], { account_ids: [] });
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

const legacyImageConfig = { image_relay_enabled: true, image_relay_public_url: "https://images.example.test",
  image_relay_listen: "0.0.0.0:8788", image_relay_storage_dir: "/srv/bps-images" };

function assertNoImageSettings(config) {
  assert.equal(Object.keys(config).some((key) => key.startsWith("image_relay_")), false);
}

test("saving legacy image settings removes all four fields and preserves account configuration", async () => {
  const preserved = { account_ids: [2], timeout_seconds: 231, auth_mode: "chatgpt", model_map: { keep: "model" }, rewrite_tools: false };
  const store = { config: { ...preserved, ...legacyImageConfig } };
  const page = createPage({ store });
  await flush();
  assert.deepEqual(page.selected(), [2]);
  page.check(1, true);
  page.save();
  await flush();
  assert.deepEqual(store.config, { ...preserved, account_ids: [2, 1] });
  assertNoImageSettings(page.calls.save[0]);
  assert.equal(page.calls.load, 2);
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  const reopened = createPage({ store });
  await flush();
  assert.deepEqual(reopened.selected(), [1, 2]);
  reopened.save();
  await flush();
  assertNoImageSettings(reopened.calls.save[0]);
});

test("disabled and partial legacy image configurations do not block account saves", async (t) => {
  for (const [name, legacy] of [
    ["disabled", { ...legacyImageConfig, image_relay_enabled: false }],
    ["enabled without an address", { image_relay_enabled: true }],
    ["obsolete malformed values", { image_relay_enabled: "yes", image_relay_public_url: null, image_relay_listen: 8788, image_relay_storage_dir: {} }],
  ]) {
    await t.test(name, async () => {
      const page = createPage({ store: { config: { account_ids: [], ...legacy } } });
      await flush();
      page.check(3, true);
      page.save();
      await flush();
      assert.deepEqual(page.calls.save[0], { account_ids: [3] });
      assert.match(page.ids["form-hint"].textContent, /已保存/);
    });
  }
});

test("migration does not save before loading or mutate stored configuration on open", async () => {
  const initial = deferred();
  const store = { config: { account_ids: [2], ...legacyImageConfig } };
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

test("a rejected save preserves account edits and retries without obsolete image fields", async () => {
  let first = true;
  const page = createPage({ store: { config: { account_ids: [], ...legacyImageConfig } }, save(config, store) {
    if (first) { first = false; return Promise.reject(new Error("write unavailable")); }
    store.config = config;
    return Promise.resolve(clone(config));
  } });
  await flush();
  page.check(1, true);
  page.save();
  await flush();
  assert.match(page.ids["form-hint"].textContent, /保存失败.*unavailable/);
  assert.deepEqual(page.selected(), [1]);
  assert.equal(page.ids["account-fields"].disabled, false);
  page.save();
  await flush();
  assert.equal(page.calls.save.length, 2);
  page.calls.save.forEach(assertNoImageSettings);
  assert.deepEqual(page.store.config, { account_ids: [1] });
  assert.match(page.ids["form-hint"].textContent, /已保存/);
});

test("legacy image fields reintroduced by an old host are never submitted again", async () => {
  const page = createPage({ save(config, store) {
    store.config = { ...config, ...legacyImageConfig };
    return Promise.resolve(clone(store.config));
  } });
  await flush();
  page.check(2, true);
  page.save();
  await flush();
  assert.match(page.ids["form-hint"].textContent, /已保存/);
  page.save();
  await flush();
  assert.equal(page.calls.save.length, 2);
  page.calls.save.forEach(assertNoImageSettings);
  assert.deepEqual(page.selected(), [2]);
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

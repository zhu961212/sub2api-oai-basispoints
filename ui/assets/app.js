/* 配置页脚本：配置账号路由。图片和截图直接随对话发送，无需额外配置。
 *
 * 语义：
 *   - 一个都不勾 → 不做账号限制：命中插件灰度的账号都走 Basis Points；
 *   - 勾选若干账号 → 白名单：只有这些账号走 Basis Points，其他账号的请求由插件
 *     原样透传到宿主原本指定的上游（等同于没有启用插件）。
 *
 * 无论是否勾选，**凭据与代理始终来自宿主本次调度的那个账号**：宿主带了出站令牌
 * 就直接用；宿主只负责调度、令牌由插件取时，插件按同一个账号解析身份（连同该账号
 * 自己的代理）。插件不做任何账号替换或轮询，宿主的并发计数、限流与用量归属始终准确。
 *
 * 保存时保留已读取的受支持配置字段，覆盖当前表单字段，并重新读取确认。
 */
(function (global) {
  "use strict";

  var bridgeFactory = global.Sub2APIPluginBridge;
  if (!bridgeFactory) {
    return;
  }

  var STATUS_POLL_MS = 15000;
  // ValidateConfig 严格拒绝未知字段。配置页只展示可编辑项，但仍要保留
  // 其它受支持字段；旧版本或宿主扩展留下的未知字段不能跟着再次提交。
  var CONFIG_KEYS = [
    "responses_url",
    "upstream_model",
    "models",
    "model_map",
    "timeout_seconds",
    "account_ids",
    "max_response_bytes",
    "auth_mode",
    "tools_version_id",
    "rewrite_tools",
    "transform_responses",
  ];

  var bridge = bridgeFactory.create({});
  var loaded = {};
  var selectedIDs = [];
  var accounts = [];
  var pollTimer = null;
  var resizeScheduled = false;
  var saving = false;
  var configReady = false;
  var loading = false;

  function id(name) {
    return document.getElementById(name);
  }

  function scheduleResize() {
    if (resizeScheduled) {
      return;
    }
    resizeScheduled = true;
    global.requestAnimationFrame(function () {
      resizeScheduled = false;
      bridge.resize(
        Math.max(document.body.scrollHeight, document.documentElement.scrollHeight)
      );
    });
  }

  function setChip(text, tone) {
    var chip = id("state-chip");
    chip.textContent = text;
    chip.className = "chip chip-" + tone;
  }

  function setHint(message, tone) {
    var hint = id("form-hint");
    hint.textContent = message || "";
    hint.className = "hint" + (tone ? " hint-" + tone : "");
  }

  function updateControls() {
    var locked = !configReady || saving || loading;
    id("save-button").disabled = locked;
    id("account-fields").disabled = locked;
    id("retry-button").hidden = configReady || loading;
    id("retry-button").disabled = loading || saving;
    updateAccountActions();
  }

  function statusLabel(account) {
    var parts = ["#" + account.id];
    if (account.name) {
      parts.push(account.name);
    }
    parts.push(account.schedulable ? "可用" : (account.status || "暂停"));
    return parts.join(" · ");
  }

  function isSelected(accountID) {
    return selectedIDs.indexOf(accountID) >= 0;
  }

  function isSelectableAccount(account) {
    return account && Number.isSafeInteger(account.id) && account.id > 0;
  }

  function updateAccountActions() {
    var hasUnselectedAccount = accounts.some(function (account) {
      return isSelectableAccount(account) && !isSelected(account.id);
    });
    id("select-all-button").disabled = !configReady || saving || loading || !hasUnselectedAccount;
  }

  function selectAllAccounts() {
    if (!configReady || saving || loading) {
      return;
    }
    // 只补选当前列表中的账号；保留已保存但暂未出现在状态列表里的账号。
    accounts.forEach(function (account) {
      if (isSelectableAccount(account) && !isSelected(account.id)) {
        selectedIDs.push(account.id);
      }
    });
    renderAccountList();
    renderAccountHint();
    scheduleResize();
  }

  function renderAccountList() {
    var list = id("account-list");
    list.textContent = "";
    updateAccountActions();

    if (!accounts.length) {
      var empty = document.createElement("p");
      empty.className = "muted";
      empty.textContent = "暂未获取账号列表；已保存的账号选择会保留。启用插件后可重新打开查看列表。";
      list.appendChild(empty);
      return;
    }

    accounts.forEach(function (account) {
      if (!isSelectableAccount(account)) {
        return;
      }
      var label = document.createElement("label");
      label.className = "check";
      var box = document.createElement("input");
      box.type = "checkbox";
      box.value = String(account.id);
      box.checked = isSelected(account.id);
      box.addEventListener("change", function () {
        if (!configReady || saving || loading) {
          box.checked = isSelected(account.id);
          return;
        }
        var value = Number.parseInt(box.value, 10);
        var at = selectedIDs.indexOf(value);
        if (box.checked && at < 0) {
          selectedIDs.push(value);
        }
        if (!box.checked && at >= 0) {
          selectedIDs.splice(at, 1);
        }
        renderAccountHint();
      });
      var text = document.createElement("span");
      text.textContent = statusLabel(account);
      label.appendChild(box);
      label.appendChild(text);
      list.appendChild(label);
    });
  }

  function renderAccountHint() {
    updateAccountActions();
    var hint = id("account-hint");
    var count = selectedIDs.length;
    var routing = "只有状态行列出的模型走 Basis Points，其他模型（含宿主的账号连通性测试）原样透传。";
    if (count === 0) {
      hint.textContent = "未勾选 = 不做账号限制：命中插件灰度的账号都走 Basis Points。" + routing;
      return;
    }
    hint.textContent =
      "已勾选 " + count + " 个账号 = 只有这些账号走 Basis Points；其他账号的请求原样透传给宿主的上游。" + routing;
  }

  function readForm() {
    var config = {};
    CONFIG_KEYS.forEach(function (key) {
      // null/undefined 代表缺省值；省略它们可避免旧配置中的空字段阻断保存。
      if (loaded[key] !== null && typeof loaded[key] !== "undefined") {
        config[key] = loaded[key];
      }
    });
    // 统一只用 account_ids 表达账号限定；旧配置可能残留 account_id，
    // 必须删掉，否则提交会被宿主以未知字段拒绝。
    config.account_ids = selectedIDs.length ? selectedIDs.slice() : [];
    return config;
  }

  function configAccountIDs(config) {
    var ids = Array.isArray(config.account_ids) ? config.account_ids : [];
    // 兼容 0.4.x 的单账号配置：首次保存时迁移成 account_ids，之后不再回传旧字段。
    if (!Object.prototype.hasOwnProperty.call(config, "account_ids") && typeof config.account_id === "number") {
      ids = [config.account_id];
    }
    var normalized = [];
    ids.forEach(function (value) {
      if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) {
        return;
      }
      if (normalized.indexOf(value) < 0) {
        normalized.push(value);
      }
    });
    return normalized;
  }

  function sameAccountIDs(config, expected) {
    var actual = configAccountIDs(config);
    return actual.length === expected.length && expected.every(function (value) {
      return actual.indexOf(value) >= 0;
    });
  }

  function applyConfig(config) {
    loaded = config && typeof config === "object" && !Array.isArray(config) ? Object.assign({}, config) : {};
    selectedIDs = configAccountIDs(loaded);
    renderAccountList();
    renderAccountHint();
    scheduleResize();
  }

  function renderStatus(status) {
    var snapshot = status && typeof status === "object" ? status : {};
    var healthy = snapshot.healthy === true;
    setChip(healthy ? "运行中" : "未运行", healthy ? "ok" : "warn");

    var details = {};
    if (typeof snapshot.status_json === "string" && snapshot.status_json) {
      try {
        details = JSON.parse(snapshot.status_json) || {};
      } catch (error) {
        details = {};
      }
    }
    // 每个账号只渲染一个复选框，避免重复状态行显示不同的勾选状态。
    // 过滤后再判断空列表，保留已保存但当前不可见的账号选择。
    var seenIDs = [];
    accounts = (Array.isArray(details.accounts) ? details.accounts : []).filter(function (account) {
      if (!isSelectableAccount(account) || seenIDs.indexOf(account.id) >= 0) {
        return false;
      }
      seenIDs.push(account.id);
      return true;
    });
    renderAccountList();

    var line = [];
    if (details.plugin_version) {
      line.push("插件版本 " + details.plugin_version);
    }
    if (Array.isArray(details.models) && details.models.length) {
      line.push("模型 " + details.models.join(", "));
    }
    if (Array.isArray(details.account_ids) && details.account_ids.length) {
      line.push("固定 #" + details.account_ids.join(" #"));
    }
    line.push(snapshot.message || (healthy ? "ready" : "未运行"));
    id("version-line").textContent = line.join(" · ");
    scheduleResize();
  }

  function refreshStatus() {
    return bridge
      .status()
      .then(renderStatus)
      .catch(function (error) {
        setChip("宿主未响应", "warn");
        id("version-line").textContent = error.message + "。" + diagnosis();
        scheduleResize();
      });
  }

  // 读不到宿主响应时给出可操作的诊断，而不是只报"超时"。
  function diagnosis() {
    if (!bridge.hasToken) {
      return "页面 URL 里没有 bridge_token，请从插件管理页重新打开配置。";
    }
    return "请确认宿主管理会话仍有效，并从插件管理页重新打开配置；如果出现二次验证，请及时完成。";
  }

  // 页面自身的 JS 错误直接显示出来，便于远程区分"脚本挂了"和"宿主不回包"。
  global.onerror = function (message) {
    id("version-line").textContent = "页面脚本错误：" + message;
    return false;
  };

  function loadConfig() {
    if (loading || saving) {
      return;
    }
    loading = true;
    configReady = false;
    updateControls();
    setHint("正在连接宿主（最长等待 12 秒）…");
    return bridge
      .loadConfig()
      .then(function (config) {
        applyConfig(config);
        configReady = true;
        setHint("");
      })
      .catch(function (error) {
        // 未读到原配置时不能用默认值覆盖，保留选择并锁住保存。
        setHint("读取配置失败，暂不能保存：" + error.message + "。请点击重新读取。" + diagnosis(), "error");
      })
      .then(function () {
        loading = false;
        updateControls();
        scheduleResize();
      });
  }

  function handleSave(event) {
    event.preventDefault();
    if (saving || loading || !configReady) {
      return;
    }
    var submitted = readForm();
    saving = true;
    updateControls();
    var writeAcknowledged = false;
    setHint("正在保存…");
    bridge
      .saveConfig(submitted)
      .then(function (normalized) {
        if (!sameAccountIDs(normalized, submitted.account_ids)) {
          throw new Error("宿主返回的账号选择与提交内容不一致");
        }
        writeAcknowledged = true;
        setHint("正在重新读取配置，确认保存结果…");
        return bridge.loadConfig();
      })
      .then(function (persisted) {
        if (!sameAccountIDs(persisted, submitted.account_ids)) {
          throw new Error("重新读取的账号选择与提交内容不一致，请重试或检查宿主日志");
        }
        applyConfig(persisted);
        setHint("已保存，并已重新读取确认。", "ok");
        return refreshStatus();
      })
      .catch(function (error) {
        var prefix = writeAcknowledged ? "保存结果未确认：" : "保存失败：";
        setHint(prefix + error.message + "。" + diagnosis(), "error");
      })
      .then(function () {
        saving = false;
        updateControls();
        scheduleResize();
      });
  }

  function boot() {
    bridge.ready();
    // 宿主 sandbox 只有 allow-scripts；原生表单提交会在 submit 事件前被阻止。
    // 通过普通按钮点击发 bridge 消息，兼容宿主的 form-action 'none'。
    id("save-button").addEventListener("click", handleSave);
    id("select-all-button").addEventListener("click", selectAllAccounts);
    id("config-form").addEventListener("submit", function (event) {
      event.preventDefault();
    });
    id("retry-button").addEventListener("click", loadConfig);
    global.addEventListener("resize", scheduleResize);
    if (typeof global.ResizeObserver === "function") {
      new global.ResizeObserver(scheduleResize).observe(document.body);
    }
    void loadConfig();
    refreshStatus().then(function () {
      pollTimer = global.setInterval(function () {
        if (document.visibilityState === "visible") {
          refreshStatus();
        }
      }, STATUS_POLL_MS);
    });
    scheduleResize();
  }

  global.addEventListener("beforeunload", function () {
    if (pollTimer !== null) {
      global.clearInterval(pollTimer);
    }
    bridge.dispose();
  });

  boot();
})(window);

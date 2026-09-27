/* 配置页脚本：配置模型与账号路由。图片和截图直接随对话发送，无需额外配置。
 *
 * 语义：
 *   - 新增账号默认走 Basis Points，取消勾选的账号保存在排除名单里。
 *   - 旧账号白名单在成功读取目录后随第一次保存迁移，保留原有可见账号选择。
 *   - 只转发选中的模型，默认 gpt-6-astra、gpt-5.6-sol，保留各自模型名。
 *   - 模型全部取消勾选 → 所有模型原样透传，包括未来新增的账号。
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
  var MODEL_IDS = ["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna"];
  var MODEL_DISPLAY_IDS = ["gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"];
  var DEFAULT_MODELS = ["gpt-6-astra", "gpt-5.6-sol"];
  // 配置页保留当前版本受支持的其他配置字段。
  var CONFIG_KEYS = [
    "responses_url",
    "enabled_models",
    "timeout_seconds",
    "account_ids",
    "auto_select_new_accounts",
    "excluded_account_ids",
    "bps_reenabled_accounts",
    "bps_auto_disable_on_403",
    "auto_degradation_enabled",
    "auto_degradation_interval_minutes",
    "auto_degradation_manual_revision",
    "native_timezone_by_ip",
    "degradation_check_model",
    "max_response_bytes",
    "auth_mode",
    "tools_version_id",
    "rewrite_tools",
    "transform_responses",
    // 兼容清理旧版持久化检测触发位；主动检测只使用请求快照。
    "degradation_check",
  ];

  var bridge = bridgeFactory.create({});
  if (typeof bridge.onDiagnosticTask === "function") bridge.onDiagnosticTask(renderDiagnosticTask);
  var accountCheckUnavailableReason = bridge.accountCheckUnavailableReason || "尚未确认可原子绑定检测账号的插件任务能力，账号检测已暂停。请更新插件并重新打开配置页，无需修改宿主。";
  var loaded = {};
  var selectedIDs = [];
  var selectedIDSet = new Set();
  var excludedIDs = [];
  var autoSelectNewAccounts = false;
  var bpsAutoDisableOn403 = true;
  var autoDegradationEnabled = false;
  var nativeTimezoneByIP = false;
  var autoDegradationStatus = null;
  var autoDegradationAccounts = new Map();
  var accountSelectionEdited = false;
  var pendingManualAccountSelection = null;
  var accountDirectoryReady = false;
  var knownAccountIDs = [];
  var knownAccountIDSet = new Set();
  var statusRequestID = 0;
  var statusAppliedID = 0;
  var selectedModels = DEFAULT_MODELS.slice();
  var accounts = [];
  var pollTimer = null;
  var resizeScheduled = false;
  var saving = false;
  var checking = false;
  var checkingAccountID = 0;
  var pendingAccountCheck = null;
  var configReady = false;
  var loading = false;
  var accountChecks = Object.create(null);
  var accountCheckButtons = [];
  var renderedAccountSignature = null;
  var renderedDegradationSignature = null;
  var lastDegradationCheck = null;
  var disposed = false;
  var resizeObserver = null;
  var lastResizeHeight = null;
  var statusInFlight = null;
  var bpsDisabledAccounts = new Map();
  var pendingBpsReenabled = new Map();

  function id(name) {
    return document.getElementById(name);
  }

  function scheduleResize() {
    if (resizeScheduled || disposed) {
      return;
    }
    resizeScheduled = true;
    global.requestAnimationFrame(function () {
      resizeScheduled = false;
      if (disposed) return;
      var height = Math.max(document.body.scrollHeight, document.documentElement.scrollHeight);
      if (height !== lastResizeHeight) {
        lastResizeHeight = height;
        bridge.resize(height);
      }
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
    var locked = !configReady || saving || loading || checking;
    id("save-button").disabled = locked;
    id("account-fields").disabled = locked;
    id("model-fields").disabled = locked;
    id("retry-button").hidden = configReady || loading;
    id("retry-button").disabled = loading || saving || checking;
    id("bps-403-toggle").disabled = locked;
    id("auto-degradation-fields").disabled = locked;
    id("auto-degradation-toggle").disabled = locked;
    id("native-environment-fields").disabled = locked;
    id("native-timezone-toggle").disabled = locked;
    renderBps403Toggle();
    renderAutoDegradation();
    renderNativeTimezoneToggle();
    var degradationButton = id("degradation-check-button");
    var supported = supportsAccountChecks();
    var pendingCheck = hasPendingAccountCheck();
    var taskInfo = typeof bridge.diagnosticTaskInfo === "function" ? bridge.diagnosticTaskInfo() : null;
    var unavailableReason = taskInfo && taskInfo.error || accountCheckUnavailableReason;
    if (degradationButton) {
      degradationButton.disabled = locked || (!pendingCheck && (!supported || !accounts.some(canCheckAccount)));
      degradationButton.title = pendingCheck ? "仅查询上次检测任务，不重复提交检测请求" :
        (supported ? "检测当前列表账号的原生 Codex；自动模式仅展示结果，手动模式改选后需保存" : unavailableReason);
      degradationButton.textContent = checking && !checkingAccountID ? "正在检测列表账号…" :
        (pendingCheck ? "继续查询上次检测" : "一键检测原生 Codex");
    }
    accountCheckButtons.forEach(function (entry) {
      var pendingTarget = pendingCheck && pendingAccountCheck.accountID === entry.account.id;
      entry.button.disabled = locked || (pendingCheck ? !pendingTarget : (!supported || !canCheckAccount(entry.account)));
      entry.button.title = pendingCheck ? "上次检测尚未确认，请先继续查询原任务" : !supported ? unavailableReason :
        (!entry.account.schedulable ? "账号不可调度，请先恢复账号" : "仅检测此账号的原生 Codex，不按手动探针结果改选；BPS 403 保护不影响原生检测");
      entry.button.textContent = checking && checkingAccountID === entry.account.id ? "检测中…" : (pendingTarget ? "继续查询" : "降智检测");
    });
    var checkHint = id("account-check-hint");
    if (checkHint) checkHint.textContent = pendingCheck
      ? "上次检测结果未确认，可能已提交。请继续查询原任务；确认结束或过期前不会创建新检测。"
      : supported
        ? "手动检测请求 OpenAI 原生 Codex，会消耗额度，仅反映本次探针。单号不改选；自动模式下批量也不改选。手动模式批量发现疑似账号时勾选它们并取消符合规则账号的勾选，失败或跳过保持原选择，改选需保存。检测使用当前表单快照，不自动保存配置。BPS 403 保护不阻止原生检测，原生错误不会触发 BPS 停用。" +
          (supportsScopedChecks() ? "" : "兼容模式由当前插件运行实例串行执行，无需修改宿主；提交失败可能仅表示回执未确认。")
        : unavailableReason;
    updateAccountActions();
  }

  function supportsAccountChecks() {
    return supportsScopedChecks() || (typeof bridge.supportsDiagnosticTasks === "function" && bridge.supportsDiagnosticTasks() === true);
  }

  function supportsScopedChecks() {
    return typeof bridge.supportsScopedTest === "function" && bridge.supportsScopedTest() === true;
  }

  function hasPendingAccountCheck() {
    return !!pendingAccountCheck && typeof bridge.hasPendingDiagnosticTask === "function" && bridge.hasPendingDiagnosticTask();
  }

  function canCheckAccount(account) {
    return isSelectableAccount(account) && account.schedulable === true;
  }

  function statusLabel(account) {
    var parts = ["#" + account.id];
    parts.push(account.schedulable ? "可用" : (account.status || "暂停"));
    if (bpsDisabledAccounts.has(account.id)) parts.push(bpsAvailabilityLabel(account));
    return parts.join(" · ");
  }

  function hasPendingBpsRestore(accountID) {
    var blockID = bpsDisabledAccounts.get(accountID);
    return !!blockID && pendingBpsReenabled.get(accountID) === blockID;
  }

  function bpsAvailabilityLabel(account) {
    if (hasPendingBpsRestore(account.id)) return "BPS 待恢复（保存生效）";
    if (bpsDisabledAccounts.has(account.id)) return "BPS 已禁用（HTTP 403）";
    var auto = autoDegradationAccounts.get(account.id);
    if (auto && autoDegradationStatus.ready !== false && (autoDegradationStatus.enabled || auto.managed)) {
      return (isSelected(account.id) ? "BPS 已启用" : "原生 Codex（BPS 未启用）") +
        (autoDegradationStatus.enabled ? " · 自动管理" : " · 保留最后路由");
    }
    if (automaticAccountSelectionLocked()) return "BPS 路由状态待同步";
    return account.schedulable ? "可用" : (account.status || "暂停");
  }

  function validBpsBlockID(value) {
    return typeof value === "string" && /^[a-f0-9]{32}$/.test(value);
  }

  function configBpsAcknowledgements(config) {
    var source = config.bps_reenabled_accounts;
    var acknowledgements = {};
    if (!source || typeof source !== "object" || Array.isArray(source)) return acknowledgements;
    Object.keys(source).forEach(function (key) {
      var accountID = Number(key);
      if (Number.isSafeInteger(accountID) && accountID > 0 && validBpsBlockID(source[key])) {
        acknowledgements[String(accountID)] = source[key];
      }
    });
    return acknowledgements;
  }

  function applyBpsDisabledSelection() {
    if (!bpsDisabledAccounts.size) return;
    var excluded = new Set(excludedIDs);
    var blocked = new Set();
    bpsDisabledAccounts.forEach(function (_blockID, accountID) {
      if (!hasPendingBpsRestore(accountID)) {
        excluded.add(accountID);
        blocked.add(accountID);
      }
    });
    selectedIDs = selectedIDs.filter(function (accountID) { return !blocked.has(accountID); });
    selectedIDSet = new Set(selectedIDs);
    excludedIDs = Array.from(excluded);
  }

  function updateBpsDisabledAccounts(details) {
    if (!Array.isArray(details.bps_disabled_accounts) && !Array.isArray(details.bps_disabled_account_ids)) return;
    var next = new Map();
    configAccountIDs(details, "bps_disabled_account_ids").forEach(function (accountID) { next.set(accountID, ""); });
    (Array.isArray(details.bps_disabled_accounts) ? details.bps_disabled_accounts : []).forEach(function (entry) {
      if (!entry || !Number.isSafeInteger(entry.account_id) || entry.account_id <= 0) return;
      next.set(entry.account_id, validBpsBlockID(entry.block_id) ? entry.block_id : "");
    });
    pendingBpsReenabled.forEach(function (blockID, accountID) {
      if (next.get(accountID) !== blockID) pendingBpsReenabled.delete(accountID);
    });
    bpsDisabledAccounts = next;
    applyBpsDisabledSelection();
  }

  // Status reads never write configuration. Only a deliberate ordinary save
  // acknowledges the exact 403 version the user chose to restore.
  function mergeBpsAccountPolicy(config, includePendingRestores) {
    var result = Object.assign({}, config);
    var excluded = new Set(configAccountIDs(config, "excluded_account_ids"));
    var acknowledgements = configBpsAcknowledgements(config);
    var blocked = new Set();
    bpsDisabledAccounts.forEach(function (blockID, accountID) {
      if (includePendingRestores && hasPendingBpsRestore(accountID) && isSelected(accountID)) {
        acknowledgements[String(accountID)] = blockID;
        excluded.delete(accountID);
      } else {
        excluded.add(accountID);
        blocked.add(accountID);
      }
    });
    result.account_ids = configAccountIDs(config).filter(function (accountID) { return !blocked.has(accountID); });
    // With no usable directory, preserve a legacy whitelist's boundary. An
    // empty legacy list would unintentionally allow every other host account.
    if (result.auto_select_new_accounts !== true && !accountDirectoryReady && result.account_ids.length === 0) {
      result.account_ids = configAccountIDs(loaded).filter(function (accountID) { return blocked.has(accountID); });
    }
    if (excluded.size || Object.prototype.hasOwnProperty.call(config, "excluded_account_ids")) {
      result.excluded_account_ids = Array.from(excluded);
    }
    if (Object.keys(acknowledgements).length || Object.prototype.hasOwnProperty.call(config, "bps_reenabled_accounts")) {
      result.bps_reenabled_accounts = acknowledgements;
    }
    return result;
  }

  function degradationStatusLabel(status) {
    switch (status) {
      case "ok": return "符合检测规则";
      case "degraded": return "疑似降智";
      case "skipped": return "跳过";
      case "error": return "检测失败";
      case "running": return "检测中…";
      default: return status || "未知";
    }
  }

  function confirmedDegradedIDs(check) {
    var results = Array.isArray(check.results) ? check.results : [];
    var ids = Array.isArray(check.degraded_account_ids) ? check.degraded_account_ids : [];
    var confirmed = new Set();
    var conflicting = new Set();
    var seen = new Set();
    results.forEach(function (result) {
      if (!result) return;
      if (result.status !== "degraded") conflicting.add(result.account_id);
      else if (typeof result.answer === "string" && result.answer.trim() !== "") confirmed.add(result.account_id);
    });
    return ids.filter(function (value) {
      if (!Number.isSafeInteger(value) || value <= 0 || seen.has(value)) return false;
      seen.add(value);
      return confirmed.has(value) && !conflicting.has(value);
    });
  }

  function accountDisplayName(account) {
    if (typeof account.name === "string" && account.name.trim()) return account.name;
    return Number.isSafeInteger(account.id) && account.id > 0 ? String(account.id) : "账号";
  }

  function renderBps403Toggle() {
    var button = id("bps-403-toggle");
    var pending = bpsAutoDisableOn403 !== (loaded.bps_auto_disable_on_403 !== false);
    button.textContent = "403 自动停用：" + (bpsAutoDisableOn403 ? "已开启" : "已关闭") + (pending ? "（待保存）" : "");
    button.setAttribute("aria-pressed", String(bpsAutoDisableOn403));
  }

  function toggleBps403(event) {
    event.preventDefault();
    if (!configReady || saving || loading || checking) return;
    bpsAutoDisableOn403 = !bpsAutoDisableOn403;
    renderBps403Toggle();
    scheduleResize();
  }

  function renderNativeTimezoneToggle() {
    var pending = nativeTimezoneByIP !== (loaded.native_timezone_by_ip === true);
    var button = id("native-timezone-toggle");
    button.textContent = "请求时区跟随出口 IP：" + (nativeTimezoneByIP ? "已开启" : "已关闭") + (pending ? "（待保存）" : "");
    button.setAttribute("aria-pressed", String(nativeTimezoneByIP));
  }

  function toggleNativeTimezone(event) {
    event.preventDefault();
    if (!configReady || saving || loading || checking) return;
    nativeTimezoneByIP = !nativeTimezoneByIP;
    renderNativeTimezoneToggle();
    scheduleResize();
  }

  function verifyNativeTimezonePolicy(config, expected) {
    if ((config.native_timezone_by_ip === true) !== (expected.native_timezone_by_ip === true)) {
      throw new Error("宿主返回的请求时区开关与提交内容不一致，请重新保存");
    }
  }

  function automaticAccountSelectionLocked() {
    return autoDegradationEnabled || loaded.auto_degradation_enabled === true ||
      !!(autoDegradationStatus && autoDegradationStatus.ready === false);
  }

  function renderAutoDegradation() {
    var pending = autoDegradationEnabled !== (loaded.auto_degradation_enabled === true);
    var button = id("auto-degradation-toggle");
    button.textContent = "自动检测与切换：" + (autoDegradationEnabled ? "已开启" : "已关闭") + (pending ? "（待保存）" : "");
    button.setAttribute("aria-pressed", String(autoDegradationEnabled));
    var runtime = autoDegradationStatus;
    var parts = [runtime && runtime.ready === false ? "正在加载自动检测路由状态" :
      runtime && runtime.enabled ? (runtime.running ? "正在检测原生 Codex" : "自动检测运行中") : "自动检测已关闭"];
    if (runtime && runtime.last_run_at) parts.push("上次：" + displayCheckTime(runtime.last_run_at));
    if (runtime && runtime.enabled && runtime.next_run_at) parts.push("下次：" + displayCheckTime(runtime.next_run_at));
    if (runtime && runtime.error) parts.push("检测异常，保留路由：" + String(runtime.error).slice(0, 160));
    if (pending) parts.push("开关变更待保存");
    id("auto-degradation-status").textContent = parts.join(" · ");
  }

  function displayCheckTime(value) {
    var parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? String(value).slice(0, 80) : parsed.toLocaleString();
  }

  function toggleAutoDegradation(event) {
    event.preventDefault();
    if (!configReady || saving || loading || checking) return;
    if (!autoDegradationEnabled && loaded.auto_degradation_enabled !== true) {
      pendingManualAccountSelection = { selected: selectedIDs.slice(), excluded: excludedIDs.slice(), edited: accountSelectionEdited };
      accountSelectionEdited = false;
      applyAutoDegradationSelections();
    } else if (autoDegradationEnabled && loaded.auto_degradation_enabled !== true && pendingManualAccountSelection) {
      selectedIDs = pendingManualAccountSelection.selected;
      selectedIDSet = new Set(selectedIDs);
      excludedIDs = pendingManualAccountSelection.excluded;
      accountSelectionEdited = pendingManualAccountSelection.edited;
      pendingManualAccountSelection = null;
    }
    autoDegradationEnabled = !autoDegradationEnabled;
    renderAccountList();
    renderAccountHint();
    updateControls();
    scheduleResize();
  }

  function autoDegradationSettings(config) {
    return { enabled: config.auto_degradation_enabled === true,
      interval: config.auto_degradation_interval_minutes == null ? 30 : config.auto_degradation_interval_minutes,
      revision: config.auto_degradation_manual_revision == null ? 0 : config.auto_degradation_manual_revision,
      model: config.degradation_check_model == null ? "gpt-5.4" : config.degradation_check_model };
  }

  function verifyAutoDegradation(config, expected) {
    if (JSON.stringify(autoDegradationSettings(config)) !== JSON.stringify(autoDegradationSettings(expected))) {
      throw new Error("宿主返回的自动检测开关、周期、原生模型或手动选择版本与提交内容不一致，请重新保存");
    }
  }

  function readAutoDegradationSettings(config) {
    var interval = Number(id("auto-degradation-interval").value);
    var model = id("degradation-check-model").value;
    if (!Number.isInteger(interval) || interval < 5 || interval > 1440) {
      throw new Error("常规检测间隔必须是 5–1440 分钟的整数");
    }
    var modelBytes = typeof model === "string" ? Array.from(model).reduce(function (total, character) {
      var point = character.codePointAt(0);
      return total + (point < 128 ? 1 : point < 2048 ? 2 : point < 65536 ? 3 : 4);
    }, 0) : 0;
    if (!modelBytes || modelBytes > 128 || Array.from(model).some(function (character) {
      var code = character.codePointAt(0);
      return !character.trim() || code < 32 || (code >= 127 && code <= 159);
    })) {
      throw new Error("原生检测模型不能为空，不得包含空格或控制字符，且最多 128 字节");
    }
    config.auto_degradation_enabled = autoDegradationEnabled;
    config.auto_degradation_interval_minutes = interval;
    config.degradation_check_model = model;
  }

  function applyAutoDegradationSelections() {
    if (!autoDegradationStatus || autoDegradationStatus.ready === false || accountSelectionEdited) return;
    var selected = new Set(selectedIDs);
    var excluded = new Set(excludedIDs);
    autoDegradationAccounts.forEach(function (entry, accountID) {
      if (!autoDegradationStatus.enabled && entry.managed !== true) return;
      if (entry.bps_enabled) { selected.add(accountID); excluded.delete(accountID); }
      else { selected.delete(accountID); excluded.add(accountID); }
    });
    selectedIDs = Array.from(selected);
    selectedIDSet = selected;
    excludedIDs = Array.from(excluded);
    applyBpsDisabledSelection();
  }

  function updateAutoDegradationStatus(details) {
    var runtime = details.auto_degradation;
    if (!runtime || typeof runtime !== "object" || Array.isArray(runtime)) return;
    autoDegradationStatus = runtime;
    if (runtime.ready === false) { renderAutoDegradation(); return; }
    autoDegradationAccounts = new Map();
    (Array.isArray(runtime.accounts) ? runtime.accounts : []).forEach(function (entry) {
      if (!entry || !Number.isSafeInteger(entry.account_id) || entry.account_id <= 0 || typeof entry.bps_enabled !== "boolean") return;
      autoDegradationAccounts.set(entry.account_id, entry);
    });
    applyAutoDegradationSelections();
    renderAutoDegradation();
  }

  function verifyBpsPolicies(config, expected) {
    if ((config.bps_auto_disable_on_403 !== false) !== (expected.bps_auto_disable_on_403 !== false)) {
      throw new Error("宿主返回的 403 自动停用开关与提交内容不一致，请重新保存");
    }
  }

  function rememberAccountChecks(check) {
    (Array.isArray(check.results) ? check.results : []).forEach(function (result) {
      if (!result || !Number.isSafeInteger(result.account_id) || result.account_id <= 0) return;
      var status = result.status;
      var detail = result.answer || result.error || "";
      if (["ok", "degraded", "error", "skipped"].indexOf(status) < 0 ||
          ((status === "ok" || status === "degraded") &&
           (typeof result.answer !== "string" || !result.answer.trim()))) {
        status = "error";
        detail = "未获得有效检测回答";
      }
      accountChecks[result.account_id] = { status: status, detail: String(detail).slice(0, 160) };
    });
  }

  function answeredDegradationCount(check) {
    return (Array.isArray(check.results) ? check.results : []).filter(function (result) {
      return result && (result.status === "ok" || result.status === "degraded") &&
        typeof result.answer === "string" && result.answer.trim() !== "";
    }).length;
  }

  function renderDiagnosticTask(task) {
    var line = id("diagnostic-task-status");
    if (!line || disposed || !task || !task.task_id) return;
    var states = { preparing: "准备中", prepared: "已准备", committing: "提交中",
      queued: "排队中", running: "检测中", completed: "已完成",
      failed: "已失败", expired: "已过期", unknown: "结果未确认，仅继续查询" };
    line.hidden = false;
    line.textContent = "检测任务 " + task.task_id + " · " + (states[task.state] || states.unknown);
    scheduleResize();
  }

  function renderDegradationResult(check) {
    lastDegradationCheck = check && typeof check === "object" ? check : null;
    var container = id("degradation-result");
    if (!container) {
      return;
    }
    var results = check && Array.isArray(check.results) ? check.results : [];
    var accountByID = new Map();
    accounts.forEach(function (account) { accountByID.set(account.id, account); });
    var labels = results.map(function (result) {
      if (!result || typeof result !== "object") return "账号";
      var account = accountByID.get(result.account_id) || { id: result.account_id, name: result.name };
      var name = accountDisplayName(account);
      if (!Number.isSafeInteger(result.account_id) || result.account_id <= 0) return name;
      return name === String(result.account_id) ? "#" + result.account_id : name + "（#" + result.account_id + "）";
    });
    var signature = JSON.stringify([check, results.map(function (result, index) {
      return [result && accountChecks[result.account_id], labels[index]];
    })]);
    if (signature === renderedDegradationSignature) return;
    renderedDegradationSignature = signature;
    container.textContent = "";
    if (!check || typeof check !== "object") {
      return;
    }
    var degraded = confirmedDegradedIDs(check);
    if (check.completed !== true && check.state !== "done") {
      var state = document.createElement("span");
      state.className = "result-summary";
      state.textContent = check.state === "running" ? "正在检测账号…" : "降智检测尚未完成";
      container.appendChild(state);
      return;
    }
    var summary = document.createElement("span");
    summary.className = "result-summary";
    summary.textContent = (check.completed === true ? "检测完成：" : "检测未全部完成：") +
      (answeredDegradationCount(check) > 0
        ? degraded.length + " 个疑似降智账号（仅本次探针的自定义“苹果17”规则结果，不代表全面模型能力或可靠智力测评）"
        : "未获得有效回答。失败或跳过不等于降智。");
    container.appendChild(summary);
    results.forEach(function (result, index) {
      if (!result || typeof result !== "object") {
        return;
      }
      var line = document.createElement("span");
      var remembered = accountChecks[result.account_id];
      var status = String(remembered ? remembered.status : result.status || "");
      line.className = status === "degraded" ? "result-degraded" :
        (status === "ok" ? "result-ok" : "result-error");
      var detail = remembered ? remembered.detail : result.answer || result.error || "";
      line.textContent = labels[index] + " · " + degradationStatusLabel(status) +
        (detail ? " · " + String(detail).slice(0, 160) : "");
      container.appendChild(line);
    });
  }

  function isSelected(accountID) {
    return selectedIDSet.has(accountID);
  }

  function isSelectableAccount(account) {
    return account && Number.isSafeInteger(account.id) && account.id > 0;
  }

  function setAccountSelected(accountID, selected) {
    if (selected && bpsDisabledAccounts.has(accountID)) {
      var blockID = bpsDisabledAccounts.get(accountID);
      if (!blockID) return;
      pendingBpsReenabled.set(accountID, blockID);
    } else {
      pendingBpsReenabled.delete(accountID);
    }
    selectedIDs = selectedIDs.filter(function (value) { return value !== accountID; });
    excludedIDs = excludedIDs.filter(function (value) { return value !== accountID; });
    if (selected) selectedIDs.push(accountID);
    else excludedIDs.push(accountID);
    selectedIDSet = new Set(selectedIDs);
  }

  function syncAutomaticAccounts() {
    applyBpsDisabledSelection();
    if (!autoSelectNewAccounts) return;
    var excluded = new Set(excludedIDs);
    selectedIDs = selectedIDs.filter(function (value) { return !excluded.has(value); });
    selectedIDSet = new Set(selectedIDs);
    accounts.forEach(function (account) {
      if (!excluded.has(account.id) && !isSelected(account.id)) {
        selectedIDs.push(account.id);
        selectedIDSet.add(account.id);
      }
    });
    applyAutoDegradationSelections();
  }

  function adoptSubmittedAccountPolicy(config) {
    if (config.auto_select_new_accounts !== true || autoSelectNewAccounts) return;
    autoSelectNewAccounts = true;
    excludedIDs = configAccountIDs(config, "excluded_account_ids");
    syncAutomaticAccounts();
  }

  function updateAccountActions() {
    var selectable = accounts.filter(function (account) {
      return isSelectableAccount(account) &&
        (!bpsDisabledAccounts.has(account.id) || !!bpsDisabledAccounts.get(account.id));
    });
    var allSelected = selectable.length > 0 && selectable.every(function (account) { return isSelected(account.id); });
    var button = id("select-all-button");
    button.disabled = !configReady || saving || loading || checking || automaticAccountSelectionLocked() || !selectable.length;
    button.textContent = allSelected ? "已全选列表账号" : "全选列表账号";
    button.title = allSelected ? "当前列表账号已全部勾选；保存后生效" : "勾选当前列表账号，保存后生效";
  }

  function selectAllAccounts() {
    if (!configReady || saving || loading || checking || automaticAccountSelectionLocked()) {
      return;
    }
    accountSelectionEdited = true;
    // 只补选当前列表中的账号；保留已保存但暂未出现在状态列表里的账号。
    var visibleIDs = new Set();
    accounts.forEach(function (account) {
      if (bpsDisabledAccounts.has(account.id) && !bpsDisabledAccounts.get(account.id)) return;
      if (isSelectableAccount(account)) visibleIDs.add(account.id);
      if (isSelectableAccount(account) && !isSelected(account.id)) {
        if (bpsDisabledAccounts.has(account.id)) pendingBpsReenabled.set(account.id, bpsDisabledAccounts.get(account.id));
        selectedIDs.push(account.id);
        selectedIDSet.add(account.id);
      }
    });
    excludedIDs = excludedIDs.filter(function (accountID) { return !visibleIDs.has(accountID); });
    renderAccountList();
    renderAccountHint();
    scheduleResize();
  }

  function accountListSignature() {
    return JSON.stringify(accounts.map(function (account) {
      var check = accountChecks[account.id];
      return [account.id, accountDisplayName(account), !!account.schedulable, account.status || "",
        isSelected(account.id), check ? check.status : "", check ? check.detail : "",
        bpsDisabledAccounts.get(account.id), hasPendingBpsRestore(account.id), automaticAccountSelectionLocked(),
        autoDegradationStatus && autoDegradationStatus.enabled, autoDegradationStatus && autoDegradationStatus.ready, autoDegradationAccounts.get(account.id)];
    }));
  }

  function renderAccountList() {
    updateAccountActions();
    var signature = accountListSignature();
    if (signature === renderedAccountSignature) return;
    renderedAccountSignature = signature;
    var list = id("account-list");
    list.textContent = "";
    accountCheckButtons = [];

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
      var row = document.createElement("div");
      row.className = "account-row";
      var label = document.createElement("label");
      label.className = "check account-identity";
      label.title = statusLabel(account);
      var box = document.createElement("input");
      box.type = "checkbox";
      box.value = String(account.id);
      box.checked = isSelected(account.id);
      box.indeterminate = automaticAccountSelectionLocked() && (!autoDegradationStatus || autoDegradationStatus.ready === false || !autoDegradationAccounts.has(account.id));
      box.disabled = automaticAccountSelectionLocked() || (bpsDisabledAccounts.has(account.id) && !bpsDisabledAccounts.get(account.id));
      box.addEventListener("change", function () {
        if (!configReady || saving || loading || checking || automaticAccountSelectionLocked()) {
          box.checked = isSelected(account.id);
          return;
        }
        accountSelectionEdited = true;
        var value = Number.parseInt(box.value, 10);
        setAccountSelected(value, box.checked);
        if (bpsDisabledAccounts.has(value)) renderAccountList();
        else renderedAccountSignature = accountListSignature();
        renderAccountHint();
      });
      var copy = document.createElement("span");
      copy.className = "account-copy";
      var text = document.createElement("span");
      text.className = "account-name";
      text.textContent = accountDisplayName(account);
      text.title = "账号 ID：" + account.id;
      var availability = document.createElement("span");
      availability.className = "account-availability" + (bpsDisabledAccounts.has(account.id) ? " bps-disabled" : "");
      availability.textContent = "#" + account.id + " · " + bpsAvailabilityLabel(account);
      if (bpsDisabledAccounts.has(account.id)) availability.title = "仅停用此账号的 BPS 转发，宿主账号保留。重新勾选并保存可尝试恢复；开启自动停用时，再次收到 BPS 403 会重新停用。";
      copy.appendChild(text);
      copy.appendChild(availability);
      label.appendChild(box);
      label.appendChild(copy);
      row.appendChild(label);
      var result = document.createElement("span");
      var check = accountChecks[account.id];
      var autoCheck = autoDegradationAccounts.get(account.id);
      if (autoCheck && autoDegradationStatus.ready !== false && (autoDegradationStatus.enabled || autoCheck.managed)) {
        check = { status: autoCheck.status || "pending", detail: "原生 Codex 自动检测" };
        if (autoCheck.checked_at) check.detail += "；上次：" + displayCheckTime(autoCheck.checked_at);
        if (autoCheck.next_check_at && autoDegradationStatus.enabled) check.detail += "；下次：" + displayCheckTime(autoCheck.next_check_at);
        if (autoCheck.pending_status) check.detail += "；等待复测确认：" + degradationStatusLabel(autoCheck.pending_status) + "（" + (autoCheck.consecutive || 0) + "/2）";
      }
      result.className = "account-check-result" + (check ? " result-" + check.status : "");
      result.textContent = check && check.status !== "pending" ? degradationStatusLabel(check.status) : "未检测";
      if (autoCheck && autoCheck.pending_status) result.textContent += " · 待复测 " + (autoCheck.consecutive || 0) + "/2";
      result.title = check ? check.detail || "" : "";
      result.setAttribute("role", "status");
      row.appendChild(result);
      var button = document.createElement("button");
      button.type = "button";
      button.className = "ghost account-check-button";
      button.value = String(account.id);
      button.textContent = "降智检测";
      button.disabled = true;
      button.title = accountCheckUnavailableReason;
      button.setAttribute("aria-label", "检测账号 ID：" + account.id);
      button.addEventListener("click", function (event) { handleDegradationCheck(event, account.id); });
      accountCheckButtons.push({ account: account, button: button });
      row.appendChild(button);
      list.appendChild(row);
    });
    updateControls();
  }

  function renderAccountHint() {
    updateAccountActions();
    var hint = id("account-hint");
    var count = selectedIDs.length;
    if (automaticAccountSelectionLocked()) {
      hint.textContent = (!autoDegradationStatus || autoDegradationStatus.ready === false
        ? "正在获取后台有效路由，勾选状态待同步；"
        : "自动检测管理账号路由，当前 " + count + " 个账号使用 BPS；列表显示后台实际状态，") +
        "手动勾选已锁定。关闭自动并保存后可手动编辑，最后有效路由会保留。BPS 403 停用保护仍有效，自动检测不会解除封禁；恢复封禁请先关闭自动并保存，再重新勾选账号保存。";
      return;
    }
    hint.textContent =
      "已勾选 " + count + " 个账号；取消勾选的账号原样透传。" +
      (loaded.auto_select_new_accounts === true
        ? "新增账号会自动勾选并使用 BPS，无需再次打开配置页；全部取消只排除已有账号。"
        : (!autoSelectNewAccounts && !accountDirectoryReady
          ? "尚未成功获取账号目录，暂时保留旧白名单；获取目录后保存一次即可启用新增账号自动使用 BPS。"
          : "请保存一次以启用新增账号自动使用 BPS，之后无需再次打开配置页。"));
    if (bpsDisabledAccounts.size) {
      hint.textContent += " " + bpsDisabledAccounts.size + " 个账号因一次 BPS HTTP 403 已停用 BPS，宿主账号仍保留；重新勾选并保存可恢复。";
    }
  }

  function renderModelHint() {
    id("model-hint").textContent = selectedModels.length
      ? "已选择 " + selectedModels.length + " 个模型走 Basis Points，保留各自模型名；未选模型原样透传。默认选择 gpt-6-astra 和 gpt-5.6-sol。"
      : "未选择任何模型：所有模型原样透传，不走 Basis Points。";
  }

  function renderModelList() {
    var list = id("model-list");
    list.textContent = "";
    MODEL_DISPLAY_IDS.forEach(function (model) {
      var label = document.createElement("label");
      label.className = "check";
      var box = document.createElement("input");
      box.type = "checkbox";
      box.value = model;
      box.checked = selectedModels.indexOf(model) >= 0;
      box.addEventListener("change", function () {
        if (!configReady || saving || loading || checking) {
          box.checked = selectedModels.indexOf(model) >= 0;
          return;
        }
        var at = selectedModels.indexOf(model);
        if (box.checked && at < 0) selectedModels.push(model);
        if (!box.checked && at >= 0) selectedModels.splice(at, 1);
        renderModelHint();
        scheduleResize();
      });
      var text = document.createElement("span");
      text.textContent = model;
      label.appendChild(box);
      label.appendChild(text);
      list.appendChild(label);
    });
    renderModelHint();
  }

  function readForm(includePendingRestores) {
    var config = {};
    CONFIG_KEYS.forEach(function (key) {
      // null/undefined 代表缺省值。
      if (loaded[key] !== null && typeof loaded[key] !== "undefined") {
        config[key] = loaded[key];
      }
    });
    config.account_ids = selectedIDs.length ? selectedIDs.slice() : [];
    config.bps_auto_disable_on_403 = bpsAutoDisableOn403;
    config.native_timezone_by_ip = nativeTimezoneByIP;
    readAutoDegradationSettings(config);
    var revision = autoDegradationSettings(loaded).revision;
    if (!Number.isSafeInteger(revision) || revision < 0) {
      throw new Error("手动账号选择版本无效，请重新读取配置");
    }
    if (includePendingRestores === true && accountSelectionEdited && !automaticAccountSelectionLocked()) {
      if (revision === Number.MAX_SAFE_INTEGER) {
        throw new Error("手动账号选择版本已达到上限，无法保存账号修改；请先处理持久化版本后重试");
      }
      revision++;
    }
    config.auto_degradation_manual_revision = revision;
    // 旧非空白名单必须先拿到有效目录，才能保留已知未勾选账号的透传行为。
    if (autoSelectNewAccounts || accountDirectoryReady) {
      config.auto_select_new_accounts = true;
      config.excluded_account_ids = excludedIDs.slice();
      if (!autoSelectNewAccounts) {
        var exclusions = new Set(config.excluded_account_ids);
        knownAccountIDs.forEach(function (accountID) {
          if (isSelected(accountID)) exclusions.delete(accountID);
          else exclusions.add(accountID);
        });
        config.excluded_account_ids = Array.from(exclusions);
      }
    }
    config.enabled_models = MODEL_IDS.filter(function (model) {
      return selectedModels.indexOf(model) >= 0;
    });
    // 该字段只是 config.test 的一次性触发位，普通保存和检测收尾都必须清零。
    // 旧版宿主没有这个字段时不要凭空扩展保存载荷。
    if (Object.prototype.hasOwnProperty.call(loaded, "degradation_check")) {
      config.degradation_check = false;
    }
    delete config.degradation_check_account_id;
    delete config.degradation_check_account_ids;
    delete config.diagnostic_task;
    config = mergeBpsAccountPolicy(config, includePendingRestores === true);
    // Runtime decisions live in plugin storage. Do not replace their baseline
    // with a stale form snapshot, including the save that disables automation.
    if (automaticAccountSelectionLocked()) {
      ["account_ids", "auto_select_new_accounts", "excluded_account_ids"].forEach(function (key) {
        if (Object.prototype.hasOwnProperty.call(loaded, key)) config[key] = loaded[key];
        else delete config[key];
      });
    }
    return config;
  }

  function configAccountIDs(config, key) {
    var ids = Array.isArray(config[key || "account_ids"]) ? config[key || "account_ids"] : [];
    var normalized = [];
    var seen = new Set();
    ids.forEach(function (value) {
      if (typeof value !== "number" || !Number.isSafeInteger(value) || value <= 0) {
        return;
      }
      if (!seen.has(value)) {
        seen.add(value);
        normalized.push(value);
      }
    });
    return normalized;
  }

  function sameAccountIDs(config, expected) {
    var actual = configAccountIDs(config);
    var actualSet = new Set(actual);
    return actual.length === expected.length && expected.every(function (value) {
      return actualSet.has(value);
    });
  }

  function configModels(config) {
    // 缺失和 null 沿用默认，显式 [] 表示全部透传。
    if (config.enabled_models == null) return DEFAULT_MODELS.slice();
    var models = (Array.isArray(config.enabled_models) ? config.enabled_models : [])
      .filter(function (model) { return typeof model === "string"; })
      .map(function (model) { return model.trim(); });
    return MODEL_IDS.filter(function (model) { return models.indexOf(model) >= 0; });
  }

  function sameModels(config, expected) {
    var actual = configModels(config);
    return actual.length === expected.length && expected.every(function (model) {
      return actual.indexOf(model) >= 0;
    });
  }

  function sameAccountPolicy(config, expected) {
    if (!sameAccountIDs(config, configAccountIDs(expected))) return false;
    if ((config.auto_select_new_accounts === true) !== (expected.auto_select_new_accounts === true)) return false;
    var actual = configAccountIDs(config, "excluded_account_ids");
    var excluded = configAccountIDs(expected, "excluded_account_ids");
    var actualSet = new Set(actual);
    if (actual.length !== excluded.length || !excluded.every(function (value) { return actualSet.has(value); })) return false;
    var actualAcks = configBpsAcknowledgements(config);
    var expectedAcks = configBpsAcknowledgements(expected);
    var keys = Object.keys(expectedAcks);
    return Object.keys(actualAcks).length === keys.length && keys.every(function (key) { return actualAcks[key] === expectedAcks[key]; });
  }

  function applyConfig(config) {
    var previousRevision = autoDegradationSettings(loaded).revision;
    loaded = config && typeof config === "object" && !Array.isArray(config) ? Object.assign({}, config) : {};
    if (configReady && previousRevision !== autoDegradationSettings(loaded).revision) {
      // These cached decisions belong to the previous manual baseline. The
      // forced status refresh after saving will supply matching decisions.
      autoDegradationAccounts = new Map();
    }
    selectedIDs = configAccountIDs(loaded);
    selectedIDSet = new Set(selectedIDs);
    excludedIDs = configAccountIDs(loaded, "excluded_account_ids");
    autoSelectNewAccounts = loaded.auto_select_new_accounts === true || selectedIDs.length === 0;
    bpsAutoDisableOn403 = loaded.bps_auto_disable_on_403 !== false;
    autoDegradationEnabled = loaded.auto_degradation_enabled === true;
    nativeTimezoneByIP = loaded.native_timezone_by_ip === true;
    renderNativeTimezoneToggle();
    accountSelectionEdited = false;
    pendingManualAccountSelection = null;
    var autoSettings = autoDegradationSettings(loaded);
    id("auto-degradation-interval").value = String(autoSettings.interval);
    id("degradation-check-model").value = autoSettings.model;
    renderAutoDegradation();
    renderBps403Toggle();
    // A diagnostic save preserves confirmed acknowledgements only. Keep a
    // user's still-unsaved restore choice local until an ordinary save.
    var pendingRestores = new Set();
    pendingBpsReenabled.forEach(function (_blockID, accountID) {
      if (!hasPendingBpsRestore(accountID)) return;
      pendingRestores.add(accountID);
      if (!selectedIDSet.has(accountID)) {
        selectedIDs.push(accountID);
        selectedIDSet.add(accountID);
      }
    });
    excludedIDs = excludedIDs.filter(function (value) { return !pendingRestores.has(value); });
    syncAutomaticAccounts();
    applyAutoDegradationSelections();
    selectedModels = configModels(loaded);
    renderModelList();
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
    var seenIDs = new Set();
    var directory = Array.isArray(details.accounts) ? details.accounts : [];
    // 旧宿主把目录查询失败也显示成 []；空列表不能作为白名单迁移依据。
    if (directory.some(isSelectableAccount)) accountDirectoryReady = true;
    accounts = directory.filter(function (account) {
      if (!isSelectableAccount(account) || seenIDs.has(account.id)) {
        return false;
      }
      seenIDs.add(account.id);
      if (!knownAccountIDSet.has(account.id)) {
        knownAccountIDSet.add(account.id);
        knownAccountIDs.push(account.id);
      }
      return true;
    });
    updateBpsDisabledAccounts(details);
    syncAutomaticAccounts();
    updateAutoDegradationStatus(details);
    if (details.degradation_check && !(lastDegradationCheck && lastDegradationCheck.request_id)) {
      rememberAccountChecks(details.degradation_check);
      renderDegradationResult(details.degradation_check);
    } else if (lastDegradationCheck) {
      // Directory renames also refresh a locally completed diagnostic report.
      renderDegradationResult(lastDegradationCheck);
    }
    renderAccountList();
    renderAccountHint();
    updateControls();

    var line = [];
    if (details.plugin_version) {
      line.push("插件版本 " + details.plugin_version);
    }
    var activeModels = details.enabled_models;
    if (Array.isArray(activeModels)) {
      line.push(activeModels.length ? "已启用模型 " + activeModels.join(", ") : "未启用模型（全部透传）");
    }
    if (details.auto_degradation && details.auto_degradation.enabled) {
      line.push("原生 Codex 自动检测与切换");
    } else if (details.auto_select_new_accounts === true) {
      line.push("新增账号自动使用 BPS");
    } else if (Array.isArray(details.account_ids) && details.account_ids.length) {
      line.push("固定 #" + details.account_ids.join(" #"));
    }
    if (typeof details.bps_account_persistence_error === "string" && details.bps_account_persistence_error) {
      line.push("BPS 账号状态存储警告：" + details.bps_account_persistence_error);
    }
    line.push(snapshot.message || (healthy ? "ready" : "未运行"));
    id("version-line").textContent = line.join(" · ");
    scheduleResize();
  }

  function refreshStatus(force) {
    if (disposed || checking) return Promise.resolve();
    // Polls share an outstanding read. A verified save needs its own newer
    // snapshot so a pre-save response cannot hide the applied configuration.
    if (statusInFlight && !force) return statusInFlight;
    var requestID = ++statusRequestID;
    var pending = bridge
      .status()
      .then(function (status) {
        if (disposed || checking || requestID < statusAppliedID) return;
        statusAppliedID = requestID;
        renderStatus(status);
      })
      .catch(function (error) {
        if (disposed || checking || requestID < statusAppliedID) return;
        statusAppliedID = requestID;
        setChip("宿主未响应", "warn");
        id("version-line").textContent = error.message + "。" + diagnosis();
        scheduleResize();
      });
    statusInFlight = pending;
    return pending.then(function () {
      if (statusInFlight === pending) statusInFlight = null;
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
    if (loading || saving || checking) {
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
    if (saving || loading || checking || !configReady) {
      return;
    }
    var submitted;
    try { submitted = readForm(true); } catch (error) { setHint(error.message, "error"); return; }
    if (!automaticAccountSelectionLocked()) adoptSubmittedAccountPolicy(submitted);
    saving = true;
    updateControls();
    var writeAcknowledged = false;
    setHint("正在保存…");
    bridge
      .saveConfig(submitted)
      .then(function (normalized) {
        verifyBpsPolicies(normalized, submitted);
        verifyAutoDegradation(normalized, submitted);
        verifyNativeTimezonePolicy(normalized, submitted);
        if (!sameAccountPolicy(normalized, submitted)) {
          throw new Error("宿主返回的账号选择、自动接入模式或排除名单与提交内容不一致");
        }
        if (!sameModels(normalized, submitted.enabled_models)) {
          throw new Error("宿主返回的模型选择与提交内容不一致");
        }
        if (normalized.degradation_check === true || normalized.degradation_check_account_id || normalized.degradation_check_account_ids || normalized.diagnostic_task) {
          throw new Error("宿主未清除降智检测标记，请重新保存配置");
        }
        writeAcknowledged = true;
        setHint("正在重新读取配置，确认保存结果…");
        return bridge.loadConfig();
      })
      .then(function (persisted) {
        verifyBpsPolicies(persisted, submitted);
        verifyAutoDegradation(persisted, submitted);
        verifyNativeTimezonePolicy(persisted, submitted);
        if (!sameAccountPolicy(persisted, submitted)) {
          throw new Error("重新读取的账号选择、自动接入模式或排除名单与提交内容不一致，请重试或检查宿主日志");
        }
        if (!sameModels(persisted, submitted.enabled_models)) {
          throw new Error("重新读取的模型选择与提交内容不一致，请重试或检查宿主日志");
        }
        if (persisted.degradation_check === true || persisted.degradation_check_account_id || persisted.degradation_check_account_ids || persisted.diagnostic_task) {
          throw new Error("重新读取的配置仍有降智检测标记，请重新保存配置");
        }
        applyConfig(persisted);
        setHint("已保存，并已重新读取确认。", "ok");
        return refreshStatus(true);
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

  // The scoped host endpoint and plugin-owned tasks both bind the snapshot
  // to this request. Neither path saves the form or invokes legacy config.test.
  function handleDegradationCheck(event, accountID) {
    if (event) event.preventDefault();
    var resuming = hasPendingAccountCheck();
    if (!resuming && !supportsAccountChecks()) {
      setHint(accountCheckUnavailableReason, "error");
      return;
    }
    if (!configReady || saving || loading || checking || disposed) return;
    if (resuming) accountID = pendingAccountCheck.accountID;
    var single = Number.isSafeInteger(accountID) && accountID > 0;
    var target = single && accounts.find(function (account) { return account.id === accountID; });
    if (!resuming && (single ? !canCheckAccount(target) : !accounts.some(canCheckAccount))) return;
    var targets = resuming ? pendingAccountCheck.targets.slice() :
      (single ? [accountID] : accounts.filter(isSelectableAccount).map(function (account) { return account.id; }));
    var snapshot;
    try { snapshot = resuming ? pendingAccountCheck.snapshot : readForm(false); }
    catch (error) { setHint(error.message, "error"); return; }
    if (!resuming) {
      snapshot.degradation_check = true;
      if (single) snapshot.degradation_check_account_id = accountID;
      else snapshot.degradation_check_account_ids = targets.slice();
    }
    var previousCheck = resuming ? pendingAccountCheck.previousCheck : lastDegradationCheck;
    var compatible = resuming || !supportsScopedChecks();
    if (compatible && !resuming) pendingAccountCheck = { accountID: single ? accountID : 0, targets: targets.slice(), snapshot: snapshot, previousCheck: previousCheck };
    checking = true;
    checkingAccountID = single ? accountID : 0;
    // Invalidate passive replies started before this diagnostic.
    statusAppliedID = ++statusRequestID;
    updateControls();
    renderDegradationResult({ state: "running" });
    setHint(resuming ? "正在查询上次检测任务，不会重复提交…" : single ? "正在检测账号 #" + accountID + "…" : "正在检测当前列表账号…");
    scheduleResize();
    var operation = resuming ? bridge.resumeDiagnosticTask() : compatible ? bridge.testDiagnosticTask(snapshot) : bridge.testScoped(snapshot);
    return operation.then(function (result) {
      if (disposed) return;
      var details = result && typeof result.status_json === "string" ? JSON.parse(result.status_json) : null;
      var check = details && details.degradation_check;
      var expected = new Set(targets);
      var actualTargets = check && check.target_account_ids;
      var results = check && check.results;
      if (!check || typeof check.request_id !== "string" || !check.request_id ||
          !Array.isArray(actualTargets) || actualTargets.length !== targets.length ||
          new Set(actualTargets).size !== targets.length || actualTargets.some(function (value) { return !expected.has(value); }) ||
          !Array.isArray(results) || results.length !== targets.length ||
          new Set(results.map(function (row) { return row && row.account_id; })).size !== targets.length ||
          results.some(function (row) { return !row || !expected.has(row.account_id); })) {
        throw new Error("检测结果与本次账号快照不匹配，保留原账号选择");
      }
      var selectedBeforeBps = selectedIDs.slice();
      updateBpsDisabledAccounts(details);
      syncAutomaticAccounts();
      var bpsDeselected = selectedBeforeBps.filter(function (accountID) {
        return bpsDisabledAccounts.has(accountID) && !isSelected(accountID);
      });
      var bpsNotice = bpsDeselected.length
        ? " HTTP 403 自动停用已将 " + bpsDeselected.length + " 个账号取消勾选，无需再次保存。" : "";
      rememberAccountChecks(check);
      // A canceled scan may still include useful per-account failure details.
      check.state = "done";
      renderDiagnosticTask({ task_id: check.request_id, state: check.completed === true ? "completed" : "unknown" });
      renderDegradationResult(check);
      var degraded = confirmedDegradedIDs(check);
      if (!automaticAccountSelectionLocked() && !single && check.completed === true && degraded.length) {
        accountSelectionEdited = true;
        var selected = new Set(degraded);
        results.forEach(function (row) {
          if ((row.status === "ok" || row.status === "degraded") &&
              typeof row.answer === "string" && row.answer.trim() && !bpsDisabledAccounts.has(row.account_id)) {
            setAccountSelected(row.account_id, selected.has(row.account_id));
          }
        });
        setHint("检测完成，已勾选 " + degraded.length + " 个疑似降智账号，并取消符合规则账号的勾选；点击保存后生效。失败或跳过的账号保留原选择（403 自动停用除外）。" + bpsNotice, "ok");
      } else {
        setHint((check.completed === true
          ? "检测结束，未按探针结果调整账号选择。"
          : "检测未全部完成，未按探针结果调整账号选择：" + (result.message || "请查看逐账号结果")) + bpsNotice, check.completed === true ? "ok" : "error");
      }
      renderAccountList();
      renderAccountHint();
      pendingAccountCheck = null;
    }).catch(function (error) {
      if (disposed) return;
      renderDegradationResult(previousCheck);
      var pending = hasPendingAccountCheck();
      if (!pending) pendingAccountCheck = null;
      setHint("检测失败：" + error.message + (pending
        ? "。账号选择未自动保存；请继续查询上次检测，不会重复提交。"
        : "。账号选择未自动保存。"), "error");
    }).then(function () {
      checking = false;
      checkingAccountID = 0;
      if (disposed) return;
      updateControls();
      scheduleResize();
      return refreshStatus(true);
    });
  }

  function boot() {
    bridge.ready();
    renderModelList();
    // 宿主 sandbox 只有 allow-scripts；原生表单提交会在 submit 事件前被阻止。
    // 通过普通按钮点击发 bridge 消息，兼容宿主的 form-action 'none'。
    id("save-button").addEventListener("click", handleSave);
    id("select-all-button").addEventListener("click", selectAllAccounts);
    id("bps-403-toggle").addEventListener("click", toggleBps403);
    id("auto-degradation-toggle").addEventListener("click", toggleAutoDegradation);
    id("native-timezone-toggle").addEventListener("click", toggleNativeTimezone);
    var degradationButton = id("degradation-check-button");
    if (degradationButton) {
      degradationButton.addEventListener("click", handleDegradationCheck);
    }
    id("config-form").addEventListener("submit", function (event) {
      event.preventDefault();
    });
    id("retry-button").addEventListener("click", loadConfig);
    global.addEventListener("resize", scheduleResize);
    if (typeof global.ResizeObserver === "function") {
      resizeObserver = new global.ResizeObserver(scheduleResize);
      resizeObserver.observe(document.body);
    }
    void loadConfig();
    refreshStatus().then(function () {
      if (disposed) return;
      pollTimer = global.setInterval(function () {
        if (document.visibilityState === "visible") {
          refreshStatus();
        }
      }, STATUS_POLL_MS);
    });
    scheduleResize();
  }

  global.addEventListener("beforeunload", function () {
    disposed = true;
    if (pollTimer !== null) {
      global.clearInterval(pollTimer);
    }
    if (resizeObserver) resizeObserver.disconnect();
    bridge.dispose();
  });

  boot();
})(window);

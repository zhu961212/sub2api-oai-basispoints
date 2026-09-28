/* Sub2API 插件配置 UI —— UI Bridge v1 客户端。
 *
 * 约束（来自 backend/pkg/pluginapi/docs/ui-bridge.md）：
 *   - 宿主用 sandbox="allow-scripts" 的 iframe 加载本页，没有管理员 Token；
 *   - Bridge Token 只存在于 URL fragment，必须原样回传；
 *   - 接收消息必须校验 event.source === parent、来源标识、Bridge Token 与 request_id；
 *   - 每个请求都要有超时，页面卸载时清理。
 */
(function (global) {
  "use strict";

  var UI_SOURCE = "sub2api-plugin-ui";
  var HOST_SOURCE = "sub2api-plugin-host";
  // 读操作保持短超时，好让配置页尽快给出"读不到"的可见反馈；
  // 写入与主动测试会触发宿主的二次验证（step-up），因此单独给更长的超时。
  var DEFAULT_TIMEOUT_MS = 12000;
  var STEP_UP_TIMEOUT_MS = 90000;
  var DIAGNOSTIC_MAX_EXECUTION_MS = 1805000;
  var DIAGNOSTIC_POLL_GRACE_MS = 10000;

  function readBridgeToken() {
    var hash = String((global.location && global.location.hash) || "");
    if (!hash) {
      return "";
    }
    var match = /(?:^#|&)bridge_token=([^&]*)/.exec(hash);
    if (!match) {
      return "";
    }
    try {
      return decodeURIComponent(match[1]);
    } catch (error) {
      return match[1];
    }
  }

  function newRequestID() {
    var crypto = global.crypto;
    if (crypto && typeof crypto.randomUUID === "function") {
      return crypto.randomUUID();
    }
    return "req-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 10);
  }

  function createBridge(options) {
    var settings = options || {};
    var token = typeof settings.token === "string" && settings.token ? settings.token : readBridgeToken();
    var defaultTimeout = typeof settings.timeoutMs === "number" ? settings.timeoutMs : DEFAULT_TIMEOUT_MS;
    var pending = new Map();
    var disposed = false;
    var scopedTestsAvailable = false;
    var diagnosticInfo = null;
    var diagnosticTask = null;
    var diagnosticOperation = null;
    var diagnosticWaiters = new Map();
    var diagnosticPollMs = Number.isSafeInteger(settings.diagnosticPollMs) && settings.diagnosticPollMs > 0 && settings.diagnosticPollMs <= 10000 ? settings.diagnosticPollMs : 1000;
    var diagnosticMaxPolls = Number.isSafeInteger(settings.diagnosticMaxPolls) && settings.diagnosticMaxPolls > 0 ? settings.diagnosticMaxPolls : null;

    function onMessage(event) {
      if (disposed) {
        return;
      }
      // sandbox iframe 没有同源父页面，因此只能按 window 引用比对。
      if (event.source !== global.parent) {
        return;
      }
      var data = event.data;
      if (!data || typeof data !== "object" || data.source !== HOST_SOURCE) {
        return;
      }
      // A missing token means this page was not opened through a host UI
      // session. Reject every response instead of accepting an untrusted
      // parent message with an arbitrary token.
      if (!token || data.bridge_token !== token) {
        return;
      }
      var requestID = typeof data.request_id === "string" ? data.request_id.trim() : "";
      var entry = pending.get(requestID);
      if (!entry) {
        return;
      }
      pending.delete(requestID);
      global.clearTimeout(entry.timer);
      if (data.ok !== true) {
        entry.reject(new Error(typeof data.error === "string" && data.error ? data.error : "插件请求被宿主拒绝"));
        return;
      }
      entry.resolve(data);
    }

    global.addEventListener("message", onMessage);

    function post(type, payload) {
      if (disposed) {
        return;
      }
      var envelope = { source: UI_SOURCE, bridge_token: token, type: type };
      if (payload) {
        Object.keys(payload).forEach(function (key) {
          envelope[key] = payload[key];
        });
      }
      // sandbox iframe 的父页面没有固定 origin，只能使用 "*"；安全性由 Token 与 request_id 保证。
      global.parent.postMessage(envelope, "*");
    }

    function request(type, payload, timeoutMs) {
      return new Promise(function (resolve, reject) {
        if (disposed) {
          reject(new Error("配置页已关闭"));
          return;
        }
        if (!token) {
          reject(new Error("缺少 bridge_token，请从插件管理页重新打开配置"));
          return;
        }
        var requestID = payload && payload.request_id || newRequestID();
        var limit = typeof timeoutMs === "number" ? timeoutMs : defaultTimeout;
        var timer = global.setTimeout(function () {
          pending.delete(requestID);
          reject(new Error("等待宿主响应超时：" + type));
        }, limit);
        pending.set(requestID, { resolve: resolve, reject: reject, timer: timer });
        try {
          post(type, Object.assign({ request_id: requestID }, payload || {}));
        } catch (error) {
          pending.delete(requestID);
          global.clearTimeout(timer);
          reject(error);
        }
      });
    }

    function readConfigResponse(response) {
      if (!response.config || typeof response.config !== "object" || Array.isArray(response.config)) {
        throw new Error("宿主未返回有效配置，无法确认配置是否已保存");
      }
      return response.config;
    }

    function readDiagnosticInfo(result) {
      var details;
      try { details = result && typeof result.status_json === "string" ? JSON.parse(result.status_json) : null; }
      catch (_error) { details = null; }
      var info = details && details.diagnostic_tasks;
      diagnosticInfo = info && info.protocol === "config-job-v1" &&
        typeof info.owner_id === "string" && info.owner_id ? info : null;
      return diagnosticInfo;
    }

    function readStatus() {
      return request("plugin.status").then(function (response) {
        var result = response.result && typeof response.result === "object" ? response.result : {};
        readDiagnosticInfo(result);
        return result;
      });
    }

    function diagnosticBusyReason(info) {
      var busy = info && Array.isArray(info.tasks) && info.tasks.some(function (entry) {
        return entry && (entry.state === "prepared" || entry.state === "queued" || entry.state === "running");
      });
      return busy ? "当前插件实例已有检测任务正在准备、排队或执行，请稍后查询状态" : "";
    }

    function pendingDiagnosticError(message) {
      var error = new Error(message);
      error.diagnosticPending = !!diagnosticTask;
      return error;
    }

    function diagnosticDelay() {
      return new Promise(function (resolve, reject) {
        if (disposed) { reject(new Error("配置页已关闭")); return; }
        var timer = global.setTimeout(function () { diagnosticWaiters.delete(timer); resolve(); }, diagnosticPollMs);
        diagnosticWaiters.set(timer, reject);
      });
    }

    function notifyDiagnosticTask(task, state) {
      if (!disposed && typeof settings.onDiagnosticTask === "function") {
        settings.onDiagnosticTask({ task_id: task.taskID, owner_id: task.ownerID, state: state });
      }
    }

    function queryDiagnosticTask(task) {
      return readStatus().then(function (snapshot) {
        var info = readDiagnosticInfo(snapshot);
        if (!info || info.owner_id !== task.ownerID) {
          throw pendingDiagnosticError("检测运行实例已改变或暂不可达，原任务结果未确认；仅可继续查询");
        }
        var matches = (Array.isArray(info.tasks) ? info.tasks : []).filter(function (entry) {
          return entry && entry.task_id === task.taskID;
        });
        if (matches.length !== 1) throw pendingDiagnosticError("尚未查询到唯一的检测任务回执，结果未确认；仅可继续查询");
        notifyDiagnosticTask(task, matches[0].state);
        return matches[0];
      });
    }

    function verifiedDiagnosticResult(task, entry) {
      var result = entry.result;
      var details;
      try { details = result && typeof result.status_json === "string" ? JSON.parse(result.status_json) : null; }
      catch (_error) { details = null; }
      var check = details && details.degradation_check;
      var targets = check && check.target_account_ids;
      var rows = check && check.results;
      var expected = new Set(task.targets);
      if (!check || check.request_id !== task.taskID || !Array.isArray(targets) ||
          targets.length !== task.targets.length || new Set(targets).size !== targets.length ||
          targets.some(function (value) { return !expected.has(value); }) || !Array.isArray(rows) ||
          rows.length !== task.targets.length || new Set(rows.map(function (row) { return row && row.account_id; })).size !== rows.length ||
          rows.some(function (row) { return !row || !expected.has(row.account_id); })) {
        throw pendingDiagnosticError("检测结果与本次任务或账号快照不匹配，保留原账号选择；仅可继续查询");
      }
      diagnosticTask = null;
      return result;
    }

    function diagnosticPollWindow(task, entry) {
      var execution = entry.execution_timeout_ms;
      if (!Number.isSafeInteger(execution) || execution <= 0 || execution > DIAGNOSTIC_MAX_EXECUTION_MS) {
        var seconds = task.config && task.config.timeout_seconds;
        if (!Number.isSafeInteger(seconds) || seconds < 10 || seconds > 1800) seconds = 300;
        execution = seconds * 1000;
      }
      return execution + DIAGNOSTIC_POLL_GRACE_MS;
    }

    function followDiagnosticTask(task, entry, polls) {
      if (polls === 0) {
        var windowMs = diagnosticPollWindow(task, entry);
        task.pollDeadline = Date.now() + windowMs;
        task.maxPolls = diagnosticMaxPolls || Math.ceil(windowMs / diagnosticPollMs);
      }
      if (entry.state === "completed" || entry.state === "failed") {
        if (entry.result) return verifiedDiagnosticResult(task, entry);
        if (entry.state === "failed") {
          diagnosticTask = null;
          throw new Error(entry.error || "插件已确认检测任务失败，账号选择未改变");
        }
        throw pendingDiagnosticError("检测已结束但未返回可验证的结果；仅可继续查询");
      }
      if (entry.state === "expired") {
        diagnosticTask = null;
        throw new Error("检测任务已过期，未取得可验证结果；可重新发起检测");
      }
      if (entry.state !== "queued" && entry.state !== "running") {
        throw pendingDiagnosticError(entry.state === "prepared"
          ? "检测提交未确认，任务仍处于准备状态；仅继续查询，等待任务过期后再试"
          : "检测任务状态未知，可能已提交；仅可继续查询，不能重复发起");
      }
      if (polls >= task.maxPolls || Date.now() >= task.pollDeadline) throw pendingDiagnosticError("检测仍在执行或排队，当前轮询已结束；点击继续查询，不会重复提交");
      return diagnosticDelay().then(function () { return queryDiagnosticTask(task); })
        .then(function (next) { return followDiagnosticTask(task, next, polls + 1); });
    }

    function saveDiagnosticCommand(task, action, receipt) {
      notifyDiagnosticTask(task, action === "prepare" ? "preparing" : "committing");
      var command = { protocol: "config-job-v1", action: action, owner_id: task.ownerID, task_id: task.taskID };
      if (action === "prepare") command.config = task.config;
      else command.receipt = receipt;
      // The outer config is command-only. The plugin preserves production
      // config; the response must never replace this page's unsaved form.
      return request("config.save", { config: { diagnostic_task: command } }, STEP_UP_TIMEOUT_MS).then(readConfigResponse);
    }

    function commitDiagnosticTask(task, entry) {
      if (entry.state !== "prepared" || typeof entry.receipt !== "string" || !entry.receipt) {
        if (entry.state === "failed" || entry.state === "expired") return followDiagnosticTask(task, entry, 0);
        throw pendingDiagnosticError("插件尚未返回有效的准备回执；仅可继续查询");
      }
      task.commitSent = true;
      return saveDiagnosticCommand(task, "commit", entry.receipt).catch(function () {
        // An error/timeout does not prove rejection. Query; never resend.
      }).then(function () { return queryDiagnosticTask(task); })
        .then(function (next) { return followDiagnosticTask(task, next, 0); });
    }

    function runDiagnosticOperation(work) {
      if (diagnosticOperation) return Promise.reject(new Error("正在查询检测任务，请等待当前操作结束"));
      var operation = Promise.resolve().then(work).catch(function (error) {
        if (diagnosticTask) {
          error.diagnosticPending = true;
          notifyDiagnosticTask(diagnosticTask, "unknown");
        }
        throw error;
      });
      diagnosticOperation = operation;
      return operation.then(function (result) { diagnosticOperation = null; return result; }, function (error) {
        diagnosticOperation = null; throw error;
      });
    }

    return {
      token: token,
      // hasToken 为 false 时说明 URL fragment 里没有 bridge_token，
      // 宿主会静默丢弃消息，此时只能提示用户从插件管理页重新打开配置。
      hasToken: token !== "",
      // Legacy config.test uses shared saved config. Account diagnostics use
      // scoped requests or command-only plugin tasks, never save + config.test.
      accountCheckUnavailableReason: "尚未确认可原子绑定检测账号的插件任务能力，账号检测已暂停。请更新插件并重新打开配置页；无需修改宿主。账号状态查询和路由保存仍可使用。",
      ready: function () {
        post("sub2api.plugin.ready");
      },
      loadConfig: function () {
        return request("config.load").then(function (response) {
          var config = readConfigResponse(response);
          scopedTestsAvailable = Array.isArray(response.capabilities) && response.capabilities.indexOf("config.testScoped") >= 0;
          return config;
        });
      },
      supportsScopedTest: function () { return scopedTestsAvailable; },
      onDiagnosticTask: function (listener) { settings.onDiagnosticTask = listener; },
      supportsDiagnosticTasks: function () { return !!diagnosticInfo && diagnosticInfo.available === true && !diagnosticBusyReason(diagnosticInfo); },
      diagnosticTaskInfo: function () {
        return diagnosticInfo && Object.assign({}, diagnosticInfo, { error: diagnosticInfo.error || diagnosticBusyReason(diagnosticInfo) });
      },
      hasPendingDiagnosticTask: function () { return !!diagnosticTask; },
      testDiagnosticTask: function (config) {
        if (diagnosticTask) return Promise.reject(pendingDiagnosticError("上次检测结果未确认，请先继续查询"));
        return runDiagnosticOperation(function () {
          return readStatus().then(function (snapshot) {
            var info = readDiagnosticInfo(snapshot);
            if (!info || info.available !== true) {
              throw new Error(info && info.error || "插件任务能力尚不可用，请稍后刷新状态");
            }
            if (diagnosticBusyReason(info)) throw new Error(diagnosticBusyReason(info));
            var singleID = config && config.degradation_check_account_id;
            var targets = singleID ? [singleID] : config && config.degradation_check_account_ids;
            if (!Array.isArray(targets) || !targets.length || new Set(targets).size !== targets.length ||
                targets.some(function (value) { return !Number.isSafeInteger(value) || value <= 0; })) {
              throw new Error("检测账号快照无效");
            }
            var task = diagnosticTask = { taskID: newRequestID(), ownerID: info.owner_id,
              config: JSON.parse(JSON.stringify(config)), targets: targets.slice(), prepareAcknowledged: false, commitSent: false };
            return saveDiagnosticCommand(task, "prepare").then(function () {
              task.prepareAcknowledged = true;
            }, function () {
              // A lost prepare acknowledgement must never start a request.
            }).then(function () { return queryDiagnosticTask(task); }).then(function (entry) {
              if (!task.prepareAcknowledged) {
                if (entry.state === "expired" || entry.state === "failed") return followDiagnosticTask(task, entry, 0);
                throw pendingDiagnosticError("准备提交未确认，已查询任务但不会自动开始检测；请继续查询至任务过期");
              }
              return commitDiagnosticTask(task, entry);
            });
          });
        });
      },
      resumeDiagnosticTask: function () {
        if (!diagnosticTask) return Promise.reject(new Error("没有待确认的检测任务"));
        return runDiagnosticOperation(function () {
          var task = diagnosticTask;
          return queryDiagnosticTask(task).then(function (entry) {
            if (!task.commitSent && task.prepareAcknowledged && entry.state === "prepared") return commitDiagnosticTask(task, entry);
            return followDiagnosticTask(task, entry, 0);
          });
        });
      },
      testScoped: function (config) {
        if (!scopedTestsAvailable) return Promise.reject(new Error("宿主不支持按请求绑定账号的检测接口，请升级宿主后重新打开配置页"));
        var requestID = newRequestID();
        return request("config.testScoped", { config: config, request_id: requestID }, 150000).then(function (response) {
          var result = response.result;
          var details = result && typeof result.status_json === "string" ? JSON.parse(result.status_json) : null;
          if (!details || !details.degradation_check || details.degradation_check.request_id !== requestID) {
            throw new Error(result && result.message || "宿主返回的检测请求标识不匹配，请重新打开配置页");
          }
          return result;
        });
      },
      saveConfig: function (config) {
        return request("config.save", { config: config }, STEP_UP_TIMEOUT_MS).then(readConfigResponse);
      },
      testConfig: function () {
        return request("config.test", {}, STEP_UP_TIMEOUT_MS).then(function (response) {
          return response.result && typeof response.result === "object" ? response.result : {};
        });
      },
      status: function () {
        return readStatus();
      },
      notify: function (level, message) {
        post("ui.notify", { level: level, message: message });
      },
      resize: function (height) {
        post("ui.resize", { height: height });
      },
      dispose: function () {
        if (disposed) {
          return;
        }
        disposed = true;
        pending.forEach(function (entry) {
          global.clearTimeout(entry.timer);
          entry.reject(new Error("配置页已关闭"));
        });
        pending.clear();
        diagnosticWaiters.forEach(function (reject, timer) { global.clearTimeout(timer); reject(new Error("配置页已关闭")); });
        diagnosticWaiters.clear();
        global.removeEventListener("message", onMessage);
      },
    };
  }

  global.Sub2APIPluginBridge = { create: createBridge, readBridgeToken: readBridgeToken };
})(window);

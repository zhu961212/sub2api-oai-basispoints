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
        var requestID = newRequestID();
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

    return {
      token: token,
      // hasToken 为 false 时说明 URL fragment 里没有 bridge_token，
      // 宿主会静默丢弃消息，此时只能提示用户从插件管理页重新打开配置。
      hasToken: token !== "",
      // UI Bridge v1 tests shared saved config, not a request-local target.
      // Never implement account diagnostics as saveConfig followed by testConfig.
      accountCheckUnavailableReason: "当前宿主 UI Bridge v1 无法原子绑定检测账号；为避免多页面检测错账号，单账号和批量检测已暂停。需宿主提供按请求绑定目标的检测接口。账号状态查询、路由保存和宿主连通性测试仍可使用。",
      ready: function () {
        post("sub2api.plugin.ready");
      },
      loadConfig: function () {
        return request("config.load").then(readConfigResponse);
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
        return request("plugin.status").then(function (response) {
          return response.result && typeof response.result === "object" ? response.result : {};
        });
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
        global.removeEventListener("message", onMessage);
      },
    };
  }

  global.Sub2APIPluginBridge = { create: createBridge, readBridgeToken: readBridgeToken };
})(window);

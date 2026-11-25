"use strict";
const byID = (id) => document.getElementById(id);
const statusNode = byID("status");
let token = "";

async function api(path, method = "GET", data = undefined) {
  const response = await fetch("/api/" + path, {
    method,
    headers: { Authorization: "Bearer " + token, "Content-Type": "application/json" },
    body: data === undefined ? undefined : typeof data === "string" ? data : JSON.stringify(data),
    cache: "no-store",
  });
  if (!response.ok) {
    const message = await response.json().catch(() => ({}));
    if (response.status === 401) {
      token = "";
      byID("workspace").hidden = true;
    }
    throw new Error(message.error || "请求失败（" + response.status + "）");
  }
  return response.status === 204 ? null : response.json();
}

function report(message, error = false) {
  statusNode.textContent = message;
  statusNode.dataset.error = String(error);
}

function showSecret(label, value) {
  const output = byID("secret");
  output.textContent = label + "（仅此次显示） " + value;
  output.hidden = false;
}

function run(action) {
  return async () => {
    byID("secret").hidden = true;
    byID("secret").textContent = "";
    try {
      await action();
    } catch (error) {
      report(error.message, true);
    }
  };
}

function form(id, action) {
  const element = byID(id);
  element.addEventListener("submit", (event) => {
    event.preventDefault();
    const submit = element.querySelector("button");
    if (submit.disabled) return;
    submit.disabled = true;
    run(() => action(new FormData(element), element))().finally(() => { submit.disabled = false; });
  });
}

function button(text, action) {
  const node = document.createElement("button");
  node.textContent = text;
  node.className = "secondary";
  node.type = "button";
  node.addEventListener("click", async () => {
    node.disabled = true;
    await run(action)();
    node.disabled = false;
  });
  return node;
}

function render(id, items, columns, view) {
  const body = byID(id);
  body.replaceChildren();
  if (items.length === 0) {
    const cell = body.insertRow().insertCell();
    cell.colSpan = columns;
    cell.textContent = "暂无记录";
    return;
  }
  for (const item of items) {
    const row = body.insertRow();
    for (const value of view(item)) {
      const cell = row.insertCell();
      if (Array.isArray(value)) cell.append(...value);
      else if (value instanceof Node) cell.append(value);
      else cell.textContent = value;
    }
  }
}

async function loadAdmin() {
  const [accounts, apiKeys] = await Promise.all([api("codex/accounts"), api("model-keys")]);
  const states = { ready: "可续期", refreshing: "续期中", reauthorize: "需要重新授权" };
  render("accounts", accounts, 4, (item) => [
    item.id, item.label || "—", item.enabled ? states[item.refresh_state] || item.refresh_state : "已停用",
    [button(item.enabled ? "停用" : "启用", async () => {
      await api("codex/accounts/" + encodeURIComponent(item.id) + "/enabled", "PUT", { enabled: !item.enabled });
      await loadAdmin();
      report("已更新账号状态");
    })],
  ]);
  render("provider-keys", apiKeys, 3, (item) => [
    { openai: "OpenAI", deepseek: "DeepSeek", glm: "智谱 GLM" }[item.provider] || item.provider, item.label,
    [button("删除", async () => {
      if (!window.confirm("删除这条模型 API Key？")) return;
      await api("model-keys/" + encodeURIComponent(item.id), "DELETE");
      await loadAdmin();
      report("已删除 Key");
    })],
  ]);
}

form("login", async (_, element) => {
  token = byID("token").value.trim();
  element.reset();
  await loadAdmin();
  byID("workspace").hidden = false;
  report("已连接");
});

form("account", async (data, element) => {
  const file = data.get("credential");
  if (file.size > 128 * 1024) throw new Error("凭据文件不能超过 128 KB");
  await api("codex/accounts/import?label=" + encodeURIComponent(data.get("label")), "POST", await file.text());
  element.reset();
  await loadAdmin();
  report("已导入账号");
});
form("provider-key", async (data, element) => {
  await api("model-keys", "POST", Object.fromEntries(data));
  element.reset();
  await loadAdmin();
  report("已保存模型 API Key");
});
form("user", async (data) => {
  const result = await api("users/token", "POST", { user_id: data.get("user_id") });
  showSecret("用户控制台令牌", result.token);
  report("已签发用户令牌");
});

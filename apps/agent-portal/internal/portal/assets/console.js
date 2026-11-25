"use strict";
const byID = (id) => document.getElementById(id);
const gateway = document.body.dataset.gateway;
const statusNode = byID("status");
let token = "";
let editing = null;
const backendNames = new Map();

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

function configureEndpoint(item) {
  editing = item;
  const endpointForm = byID("endpoint");
  endpointForm.reset();
  byID("endpoint-heading").textContent = item ? "编辑接入端点" : "创建接入端点";
  byID("enabled-row").hidden = !item;
  byID("cancel-edit").hidden = !item;
  if (!item) return;
  endpointForm.elements.protocol.value = item.protocol;
  endpointForm.elements.backend.value = item.backend;
  endpointForm.elements.enabled.checked = item.enabled;
  endpointForm.elements.models.value = Object.entries(item.models).map(([name, model]) => name + "=" + model).join("\n");
  endpointForm.elements.protocol.focus();
}

function parseMappings(value) {
  const mappings = Object.create(null);
  for (const line of value.split("\n").map((entry) => entry.trim()).filter(Boolean)) {
    const parts = line.split("=").map((entry) => entry.trim());
    if (parts.length !== 2 || !parts[0] || !parts[1] || Object.hasOwn(mappings, parts[0])) {
      throw new Error("每行填写一组唯一映射，格式为 别名=上游模型");
    }
    mappings[parts[0]] = parts[1];
  }
  if (Object.keys(mappings).length === 0) throw new Error("请填写模型映射");
  return mappings;
}

async function loadEndpoints() {
  const endpoints = await api("endpoints");
  render("endpoints", endpoints, 5, (item) => {
    const url = document.createElement("code");
    url.textContent = gateway + "/e/" + item.id + (item.protocol === "responses" ? "/v1" : "");
    return [
      item.protocol === "responses" ? "Codex CLI" : "Claude Code",
      backendNames.get(item.backend) || item.backend,
      url,
      item.enabled ? "启用" : "停用",
      [
        button("编辑", () => configureEndpoint(item)),
        button("重置 Key", async () => {
          if (!window.confirm("重置后，旧端点 Key 会立即失效。继续？")) return;
          const result = await api("endpoints/" + encodeURIComponent(item.id) + "/rotate-key", "POST");
          showSecret("端点 Key", result.key);
          report("已重置端点 Key");
        }),
      ],
    ];
  });
}

form("login", async (_, element) => {
  token = byID("token").value.trim();
  element.reset();
  const options = byID("endpoint").elements.backend;
  const upstreams = await api("upstreams");
  options.replaceChildren();
  backendNames.clear();
  for (const item of upstreams) {
    const name = item.id === "codex-pool" ? "Codex OAuth 账号池" : item.name;
    options.add(new Option(name, item.id));
    backendNames.set(item.id, name);
  }
  configureEndpoint(null);
  await loadEndpoints();
  byID("workspace").hidden = false;
  report("已连接");
});

byID("cancel-edit").addEventListener("click", () => configureEndpoint(null));
form("endpoint", async (data) => {
  const payload = { protocol: data.get("protocol"), backend: data.get("backend"), models: parseMappings(data.get("models")) };
  if (editing) {
    await api("endpoints/" + encodeURIComponent(editing.id), "PUT", {
      ...payload, version: editing.version, enabled: data.has("enabled"),
    });
  } else {
    const created = await api("endpoints", "POST", payload);
    showSecret("端点 Key", created.key);
  }
  configureEndpoint(null);
  await loadEndpoints();
  report("已保存端点");
});

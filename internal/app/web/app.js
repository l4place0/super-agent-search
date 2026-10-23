"use strict";
const $ = (s) => document.querySelector(s);
let token = sessionStorage.getItem("sas-admin") || "",
  providers = [],
  keys = [];
const pages = {
  overview: ["运行概览", "所有搜索能力，一个入口。"],
  providers: ["服务商与密钥", "集中管理服务配置与多 Key 调度。"],
  playground: ["搜索工作台", "从问题出发，查看来源和执行过程。"],
  logs: ["请求记录", "查看调用结果、耗时与失败原因。"],
};
function node(tag, cls, text) {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (text !== undefined) el.textContent = String(text);
  return el;
}
function notice(text) {
  $("#notice").textContent = text;
  $("#notice").hidden = !text;
}
async function api(path, method = "GET", body) {
  const response = await fetch(path, {
    method,
    headers: {
      Authorization: "Bearer " + token,
      ...(body ? { "Content-Type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await response.json();
  if (response.status === 401) {
    sessionStorage.removeItem("sas-admin");
    token = "";
    if (!$("#login-dialog").open) $("#login-dialog").showModal();
    throw Error("令牌无效，请重新登录");
  }
  if (!response.ok) {
    const e = Error(data.error || "请求未成功，详见执行记录");
    e.data = data;
    throw e;
  }
  return data;
}
function show(view) {
  document
    .querySelectorAll(".view")
    .forEach((el) => (el.hidden = el.id !== "view-" + view));
  document
    .querySelectorAll("nav button")
    .forEach((el) => el.classList.toggle("active", el.dataset.view === view));
  $("#page-title").textContent = pages[view][0];
  $("#page-description").textContent = pages[view][1];
  notice("");
  if (view === "logs") loadLogs().catch((e) => notice(e.message));
}
document
  .querySelectorAll("[data-view]")
  .forEach((b) => b.addEventListener("click", () => show(b.dataset.view)));
document
  .querySelectorAll("[data-goto]")
  .forEach((b) => b.addEventListener("click", () => show(b.dataset.goto)));
function badge(text, cls = "") {
  return node("span", "badge " + cls, text);
}
function keyStatus(k) {
  if (!k.enabled) return badge("已停用", "off");
  if (k.cooldown_until > Date.now() / 1000)
    return badge(
      "冷却至 " + new Date(k.cooldown_until * 1000).toLocaleTimeString(),
      "warn",
    );
  return badge("可用");
}
function renderMetrics(o) {
  const items = [
    ["请求次数", o.requests, "最近保留记录"],
    [
      "成功率",
      o.requests ? Math.round((o.successes / o.requests) * 100) + "%" : "—",
      "获得有效结果的请求",
    ],
    ["平均耗时", o.average_ms + " ms", "搜索与页面读取"],
    ["启用的 Key", o.active_keys, o.cached_searches + " 条搜索缓存"],
  ];
  $("#metrics").replaceChildren(
    ...items.map(([label, value, foot]) => {
      const n = node("div", "metric");
      n.append(
        node("div", "metric-label", label),
        node("div", "metric-value", value),
        node("div", "metric-foot", foot),
      );
      return n;
    }),
  );
}
async function refresh() {
  if (!token) return;
  const [o, p, k] = await Promise.all([
    api("/api/admin/overview"),
    api("/api/admin/providers"),
    api("/api/admin/keys"),
  ]);
  providers = p;
  keys = k;
  renderMetrics(o);
  renderProviders();
  $("#connection").textContent = "服务在线 · " + o.version;
  $("#connection").className = "badge";
}
$("#refresh").addEventListener("click", () =>
  refresh().catch((e) => notice(e.message)),
);
$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  token = $("#admin-token").value.trim();
  try {
    await refresh();
    sessionStorage.setItem("sas-admin", token);
    $("#admin-token").value = "";
    $("#login-error").textContent = "";
    $("#login-dialog").close();
  } catch (e) {
    $("#login-error").textContent = e.message;
  }
});
$("#logout").addEventListener("click", () => {
  token = "";
  sessionStorage.removeItem("sas-admin");
  location.reload();
});
function safeLink(raw) {
  try {
    const u = new URL(raw);
    if (["http:", "https:"].includes(u.protocol)) return u.href;
  } catch {}
  return "#";
}
function renderResult(out) {
  $("#raw-details").hidden = false;
  $("#raw-output").textContent = JSON.stringify(out, null, 2);
  $("#result-meta").textContent =
    (out.elapsed_ms ?? 0) + " ms" + (out.cache_hit ? " · 缓存命中" : "");
  if (out.content !== undefined) {
    const card = node("article", "result-card"),
      link = node("a", "", out.title || out.url);
    link.href = safeLink(out.url);
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    card.append(
      link,
      node("div", "result-url", out.url),
      node("div", "fetch-content", out.content),
    );
    if (out.truncated)
      card.append(
        node(
          "p",
          "",
          "正文已截取；通过 CLI 使用 --offset " +
            out.next_offset +
            " 继续读取。",
        ),
      );
    $("#search-results").className = "";
    $("#search-results").replaceChildren(card);
    return;
  }
  const rows = (out.results || []).map((h, i) => {
    const card = node("article", "result-card"),
      link = node("a", "", i + 1 + ". " + h.title);
    link.href = safeLink(h.url);
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    card.append(
      link,
      node("div", "result-url", h.url),
      node("p", "", h.snippet),
    );
    if (h.author || h.published_at)
      card.append(
        node(
          "div",
          "result-url",
          [h.author, h.published_at].filter(Boolean).join(" · "),
        ),
      );
    if (h.basis === "model_citation") card.append(badge("模型提供，未核验"));
    for (const p of h.providers || []) card.append(badge(p));
    return card;
  });
  $("#search-results").className = rows.length ? "" : "empty-state";
  $("#search-results").replaceChildren(...rows);
  if (!rows.length)
    $("#search-results").textContent =
      "本次没有获得结果。请检查服务商与完整执行记录。";
  if (out.warnings?.length) notice(out.warnings.join("；"));
}
$("#search-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const b = $("#search-submit");
  b.disabled = true;
  b.textContent = "正在获取来源…";
  notice("");
  try {
    const op = $("input[name=operation]:checked").value;
    const body =
      op === "search"
        ? {
            query: $("#search-query").value,
            max_results: Number($("#max-results").value),
            include_domains: $("#include-domains")
              .value.split(",")
              .map((x) => x.trim())
              .filter(Boolean),
            no_cache: $("#no-cache").checked,
          }
        : { url: $("#search-query").value.trim() };
    const out = await api("/v1/" + op, "POST", body);
    renderResult(out);
    await refresh();
  } catch (e) {
    if (e.data) renderResult(e.data);
    notice(e.message);
  } finally {
    b.disabled = false;
    b.textContent = "执行请求 ↗";
  }
});
async function loadLogs() {
  const logs = await api("/api/admin/logs");
  const rows = logs.map((l) => {
    const tr = node("tr"),
      q = node("td", "", l.query);
    q.append(node("small", "", l.operation));
    const state = node("td");
    state.append(badge(l.ok ? "成功" : "失败", l.ok ? "" : "bad"));
    tr.append(
      node("td", "", new Date(l.created_at).toLocaleString()),
      q,
      state,
      node("td", "", l.elapsed_ms + " ms"),
      node("td", "", l.result_count),
    );
    return tr;
  });
  if (!rows.length) {
    const tr = node("tr"),
      td = node("td", "", "还没有请求记录。");
    td.colSpan = 5;
    tr.append(td);
    rows.push(tr);
  }
  $("#logs-table").replaceChildren(...rows);
}
if (token)
  refresh().catch((e) => {
    notice(e.message);
    if (!$("#login-dialog").open) $("#login-dialog").showModal();
  });
else $("#login-dialog").showModal();

import { marked } from "marked";
import DOMPurify from "dompurify";
import renderMathInElement from "katex/contrib/auto-render";

const app = document.querySelector("#app");
const labels = {
  CREATED: "已创建",
  RUNNING: "进行中",
  BLOCKED: "受阻",
  NEEDS_REVIEW: "待评审",
  READY: "可导出",
  FAILED: "已失败",
  CANCELLED: "已取消",
};
const state = {
  online: true,
  activeRequest: null,
  detailTimer: 0,
  listTimer: 0,
  lastSuccess: 0,
  listFilter: new URL(location.href).searchParams.get("state") || "",
  tab: "statement",
  draft: null,
  editing: false,
  pendingCreate: null,
  createDraft: null,
  reviewRoute: false,
  eventCursor: null,
  eventIds: new Set(),
  eventRunId: "",
  loadedEvents: [],
  route: 0,
  lastDetail: "",
  artifacts: new Map(),
  pendingActions: new Map(),
  renderEpoch: 0,
  actionKeys: new Map(),
  pendingReview: null,
};
const esc = (v) =>
  String(v ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const idUrl = (id) => "/runs/" + encodeURIComponent(String(id));
function apiError(obj, status) {
  const e = obj?.error || {};
  return Object.assign(new Error(e.message || `请求失败 (${status})`), {
    code: e.code || "",
    status,
    field: e.field || "",
  });
}
async function api(path, { method = "GET", body, signal } = {}) {
  const headers = { Accept: "application/json" };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    headers["X-CPGen-CSRF"] = "local-session";
  }
  let pendingKey;
  if (method === "POST" && body?.operation_key && path !== "/runs/create") {
    pendingKey = path;
    const pending = state.pendingActions.get(path);
    if (pending) {
      if (pending.sending)
        throw Object.assign(Error("请求正在提交"), { status: 409 });
      const comparable = ({
        operation_key,
        expected_run_version,
        ...payload
      }) => JSON.stringify(payload);
      if (comparable(body) !== comparable(pending.body))
        throw Object.assign(
          Error(
            "上次请求结果尚未确认，请恢复原填写内容后重试，以确认原请求结果",
          ),
          { status: 409 },
        );
      body = pending.body;
      pending.sending = true;
    } else state.pendingActions.set(path, { body, sending: true });
  }
  let res;
  try {
    res = await fetch("/api" + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "same-origin",
      signal,
      cache: "no-store",
    });
  } catch (e) {
    if (pendingKey) state.pendingActions.get(pendingKey).sending = false;
    if (e.name !== "AbortError") setOnline(false);
    throw e;
  }
  if (pendingKey) {
    if (!res.ok && res.status < 500) state.pendingActions.delete(pendingKey);
    else state.pendingActions.get(pendingKey).sending = false;
  }
  if (!res.ok) {
    let data;
    try {
      data = await res.json();
    } catch {}
    setOnline(true);
    throw apiError(data, res.status);
  }
  if (res.headers.get("content-type")?.includes("application/json")) {
    const x = await res.json();
    if (pendingKey) state.pendingActions.delete(pendingKey);
    state.lastSuccess = Date.now();
    return x.data ?? x;
  }
  const result = await res.text();
  if (pendingKey) state.pendingActions.delete(pendingKey);
  state.lastSuccess = Date.now();
  return result;
}
function setOnline(v) {
  state.online = v;
  document.querySelectorAll("[data-write]").forEach((b) => (b.disabled = !v));
  const x = document.querySelector("#connection");
  if (x)
    x.textContent = v
      ? `已连接 · 更新于 ${new Date(state.lastSuccess || Date.now()).toLocaleTimeString()}`
      : "连接中断 · 保留最近一次成功读取，写操作已停用";
  const retry = document.querySelector("#reconnect");
  if (retry) retry.hidden = v;
}
function fail(e) {
  if (e?.status) setOnline(true);
  else setOnline(false);
  const old = document.querySelector("#connection");
  if (old)
    old.textContent = e?.status
      ? `请求失败：${e.message}`
      : `连接中断：${e.message} · 写操作已停用`;
}
function nav(path) {
  history.pushState({}, "", path);
  state.draft = null;
  state.editing = false;
  renderRoute();
}
function common(title, content, sub = "") {
  return `<div class="toolbar"><div><h1>${esc(title)}</h1>${sub ? `<small class="muted">${esc(sub)}</small>` : ""}</div><div class="actions"><button data-nav="/runs/new">新建题目</button></div></div>${content}`;
}
function statusBadge(s) {
  return `<span class="badge state-${esc(String(s).toLowerCase())}">${esc(labels[s] || s || "状态未知")}</span>`;
}
function humanDate(v) {
  if (!v) return "—";
  const d = new Date(v);
  return isNaN(d) ? String(v) : d.toLocaleString();
}
function runTitle(r) {
  return (
    r.title ||
    r.problem_title ||
    r.name ||
    r.brief ||
    r.request?.brief ||
    r.run_id ||
    r.id ||
    "未命名任务"
  );
}
function filterRuns(rows) {
  if (!state.listFilter) return rows;
  const f = state.listFilter;
  return rows.filter((r) => {
    const s = r.state;
    return f === "active"
      ? ["CREATED", "RUNNING"].includes(s)
      : f === "ended"
        ? ["FAILED", "CANCELLED"].includes(s)
        : f === "blocked"
          ? s === "BLOCKED"
          : f === "ready"
            ? s === "READY"
            : s === f;
  });
}
async function listPage(reviewOnly = false) {
  const route = state.route;
  clearInterval(state.detailTimer);
  try {
    const serverState = reviewOnly
      ? "NEEDS_REVIEW"
      : ["NEEDS_REVIEW", "BLOCKED", "READY", "FAILED", "CANCELLED"].includes(
            state.listFilter,
          )
        ? state.listFilter
        : "";
    const d = await api(
      "/runs?limit=1000" + (serverState ? "&state=" + serverState : ""),
    );
    if (route !== state.route) return;
    setOnline(true);
    const all = Array.isArray(d.runs) ? d.runs : [];
    const rows = reviewOnly ? all : filterRuns(all);
    const filters = [
      "全部",
      "",
      "进行中",
      "active",
      "待评审",
      "NEEDS_REVIEW",
      "受阻",
      "blocked",
      "可导出",
      "ready",
      "已结束",
      "ended",
    ];
    const controls = reviewOnly
      ? ""
      : `<div class="filters" aria-label="任务状态筛选">${[
          ["全部", ""],
          ["进行中", "active"],
          ["待评审", "NEEDS_REVIEW"],
          ["受阻", "blocked"],
          ["可导出", "ready"],
          ["已结束", "ended"],
        ]
          .map(
            ([t, v]) =>
              `<button data-filter="${v}" aria-pressed="${state.listFilter === v}">${t}</button>`,
          )
          .join("")}</div>`;
    const content = `${controls}<div class="muted scope">最近 ${all.length} 条 · 服务端最多返回 ${esc(d.limit || 50)} 条</div>${
      rows.length
        ? `<div class="table-wrap"><table><thead><tr><th>题目名称</th><th>状态</th><th>当前阶段</th><th>创建时间</th><th>更新时间</th></tr></thead><tbody>${rows
            .map((r) => {
              const id = r.run_id || r.id;
              return `<tr><td><a href="${idUrl(id)}" data-nav="${idUrl(id)}">${esc(runTitle(r))}</a><div class="mono muted">${esc(id)}</div></td><td>${statusBadge(r.state)}</td><td>${esc(r.current_stage || r.stage || "—")}</td><td>${esc(humanDate(r.created_at))}</td><td>${esc(humanDate(r.updated_at))}</td></tr>`;
            })
            .join("")}</tbody></table></div>`
        : `<div class="empty">${reviewOnly ? "当前没有待评审任务" : state.listFilter ? "没有符合筛选条件的任务" : "尚无生成任务"}${!reviewOnly && !state.listFilter ? "。" : "。"} ${state.listFilter ? '<button data-filter="">清除筛选</button>' : '<button data-nav="/runs/new">新建题目</button>'}</div>`
    }`;
    app.innerHTML = common(reviewOnly ? "待评审" : "生成任务", content);
    if (!reviewOnly) {
      document.querySelectorAll("[data-filter]").forEach(
        (b) =>
          (b.onclick = () => {
            state.listFilter = b.dataset.filter;
            const u = new URL(location.href);
            if (state.listFilter) u.searchParams.set("state", state.listFilter);
            else u.searchParams.delete("state");
            history.pushState({}, "", u);
            listPage();
          }),
      );
    }
  } catch (e) {
    if (route !== state.route) return;
    fail(e);
    app.innerHTML = `<div class="empty error">读取任务失败：${esc(e.message)} <button data-retry>重试</button></div>`;
  }
}
function moneyToMicro(v) {
  if (!/^\d+(?:\.\d{1,6})?$/.test(v))
    throw Error("请输入非负十进制金额，最多六位小数");
  const [a, b = ""] = v.split(".");
  const n = BigInt(a) * 1000000n + BigInt((b + "000000").slice(0, 6));
  if (n > 9223372036854775807n) throw Error("金额超出服务端 64 位预算范围");
  return n.toString();
}
function int64String(v, label = "预算") {
  if (!/^(0|[1-9]\d*)$/.test(v)) throw Error(`${label}必须为非负整数`);
  const n = BigInt(v);
  if (n > 9223372036854775807n) throw Error(`${label}超出有符号 64 位范围`);
  return n.toString();
}
const budgetFields = [
  ["模型费用上限（USD）", "max_llm_cost_usd", "1.20", "money"],
  ["查重费用上限（USD）", "max_similarity_cost_usd", "0.20", "money"],
  ["模型调用次数", "max_llm_calls", "12"],
  ["模型输入 token", "max_llm_input_tokens", "500000"],
  ["模型输出 token", "max_llm_output_tokens", "30000"],
  ["查重调用次数", "max_similarity_calls", "4"],
  ["活跃运行时间（分钟）", "max_active_time_minutes", "15", "minutes"],
  ["沙箱容器创建次数", "max_sandbox_creates", "100"],
  ["制品总大小（MB）", "max_artifact_bytes_mb", "64", "mb"],
  ["题包大小（MB）", "max_package_bytes_mb", "16", "mb"],
  ["阶段变更次数", "max_mutations_per_stage", "0", "hidden"],
];
function normalizeTags(v) {
  const cmp = (a, b) => {
    const x = new TextEncoder().encode(a),
      y = new TextEncoder().encode(b);
    for (let i = 0; i < Math.min(x.length, y.length); i++)
      if (x[i] !== y[i]) return x[i] - y[i];
    return x.length - y.length;
  };
  return [
    ...new Set(
      String(v || "")
        .split(/[,，\n]/)
        .map((x) => x.normalize("NFC").trim().toLowerCase())
        .filter(Boolean),
    ),
  ].sort(cmp);
}
function newPage() {
  clearInterval(state.detailTimer);
  const d = state.createDraft || state.draft || {};
  const budgetInput = ([label, name, value, type]) =>
    `<label>${label}<input name="${name}" inputmode="${type === "money" ? "decimal" : "numeric"}" value="${esc(d[name] ?? value)}" required></label>`;
  const basics = budgetFields.slice(0, 3).map(budgetInput).join("");
  const advanced = budgetFields.slice(3, -1).map(budgetInput).join("");
  const budgetSummary = `模型费用上限 $${esc(d.max_llm_cost_usd ?? "1.20")} · 查重费用上限 $${esc(d.max_similarity_cost_usd ?? "0.20")} · 模型调用 ${esc(d.max_llm_calls ?? "12")} 次`;
  app.innerHTML = common(
    "新建题目",
    `<form id="create-form" class="form content-form"><label>出题要求<textarea name="brief" required rows="5" maxlength="30000" placeholder="描述题目目标、背景与希望考查的能力">${esc(d.brief || "")}</textarea></label><div class="fields"><label>算法标签（逗号分隔）<input name="tags" value="${esc(d.tags ?? "图论")}"><small class="muted">提交标签：<span id="normalized-tags">${esc(normalizeTags(d.tags ?? "图论").join(", ") || "—")}</span></small></label><label>难度<select name="difficulty"><option value="medium">中等</option><option value="easy">简单</option><option value="hard">困难</option></select></label><label>题面语言<select name="language"><option value="zh-CN">中文</option><option value="en">English</option></select></label><label>解题语言<output class="readonly">C++</output></label><label>时间限制（毫秒）<input name="time_limit_milliseconds" value="${esc(d.time_limit_milliseconds ?? "2000")}" required></label><label>内存限制（MB）<input name="memory_limit_megabytes" value="${esc(d.memory_limit_megabytes ?? "512")}" required></label></div><div class="budget-basic"><h2>本次生成预算</h2><div class="fields">${basics}</div></div><details><summary>高级题目要求与策略</summary><div class="fields"><label>必须包含（每行一项）<textarea name="required_features" rows="3">${esc(d.required_features || "")}</textarea></label><label>禁止包含（每行一项）<textarea name="forbidden_features" rows="3">${esc(d.forbidden_features || "")}</textarea></label><label>随机种子（可留空）<input name="seed" inputmode="numeric" value="${esc(d.seed || "")}" placeholder="十进制整数，按字符串传输"></label><label>工作模式<output class="readonly">manual</output></label><label>核验配置<output class="readonly">工作区默认</output></label><label>导出目标<output class="readonly">工作区默认</output></label></div></details><details><summary>完整预算上限</summary><div class="fields">${advanced}</div><p class="muted">费用按十进制输入，所有 64 位预算整数按十进制字符串无损传输。</p></details><div class="summary"><span id="budget-summary">${budgetSummary}</span><br>使用启动时加载的工作区配置。开始生成会调用外部服务并消耗预算。</div><div class="actions"><button class="primary" data-write type="submit">开始生成</button><button type="button" data-nav="/runs">取消</button></div><p id="form-error" role="alert" class="error"></p></form>`,
    "请求会创建为可查询任务；长时间执行由本机服务管理。",
  );
  const form = document.querySelector("#create-form");
  if (state.pendingCreate) {
    form
      .querySelectorAll("input,textarea,select")
      .forEach((x) => (x.disabled = true));
    form.querySelector("[type=submit]").textContent = "按同一请求身份重试";
    document.querySelector("#form-error").textContent =
      "上一次提交结果未确认，重试将使用原请求和操作身份。";
  }
  if (d.difficulty) form.elements.difficulty.value = d.difficulty;
  if (d.language) form.elements.language.value = d.language;
  form.oninput = () => {
    state.createDraft = Object.fromEntries(new FormData(form).entries());
    const normalized = normalizeTags(form.elements.tags.value);
    document.querySelector("#normalized-tags").textContent =
      normalized.join(", ") || "—";
    document.querySelector("#budget-summary").textContent =
      `模型费用上限 $${form.elements.max_llm_cost_usd.value} · 查重费用上限 $${form.elements.max_similarity_cost_usd.value} · 模型调用 ${form.elements.max_llm_calls.value} 次`;
  };
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (!state.online || form.dataset.sending === "true") return;
    form.dataset.sending = "true";
    const fd = new FormData(form);
    const get = (n) => String(fd.get(n) || "").trim();
    const error = document.querySelector("#form-error");
    try {
      let pending = state.pendingCreate;
      if (!pending) {
        const budget = {};
        for (const [, n, , type] of budgetFields) {
          let k = n.replace("_mb", "");
          let v = type === "hidden" ? "0" : get(n);
          if (type === "money") {
            moneyToMicro(v);
            budget[k] = v;
          } else if (type === "minutes") {
            budget.max_active_time_milliseconds = int64String(
              (BigInt(int64String(v)) * 60000n).toString(),
              "活跃时间",
            );
          } else if (type === "mb") {
            budget[k] = int64String(
              (BigInt(int64String(v)) * 1048576n).toString(),
              "容量",
            );
          } else budget[k] = int64String(v, "预算");
        }
        const seed = get("seed");
        if (seed && !/^(0|[1-9]\d*)$/.test(seed))
          throw Error("随机种子必须为非负十进制整数");
        if (seed && BigInt(seed) > 9223372036854775807n)
          throw Error("随机种子超出有符号 64 位范围");
        const request = {
          schema_version: "cpgen.request/v1",
          mode: "manual",
          brief: get("brief"),
          tags: get("tags")
            .split(/[,，\n]/)
            .map((x) => x.trim())
            .filter(Boolean),
          normalized_tags: normalizeTags(get("tags")),
          difficulty: get("difficulty"),
          language: get("language"),
          solution_language: "cpp",
          time_limit_milliseconds: get("time_limit_milliseconds"),
          memory_limit_megabytes: get("memory_limit_megabytes"),
          required_features: get("required_features")
            .split("\n")
            .map((x) => x.trim())
            .filter(Boolean),
          forbidden_features: get("forbidden_features")
            .split("\n")
            .map((x) => x.trim())
            .filter(Boolean),
          seed,
          verification_profile: "default",
          export_targets: ["internal"],
          budget_limits: budget,
        };
        if (!seed) delete request.seed;
        if (!request.brief) throw Error("请填写出题要求");
        for (const n of ["time_limit_milliseconds", "memory_limit_megabytes"]) {
          request[n] = int64String(request[n], "题目限制");
          if (BigInt(request[n]) < 1n) throw Error("题目限制必须为正整数");
        }
        state.createDraft = Object.fromEntries(fd.entries());
        pending = { key: crypto.randomUUID(), request };
        state.pendingCreate = pending;
      }
      const submit = form.querySelector("[type=submit]");
      submit.disabled = true;
      form
        .querySelectorAll("input,textarea,select")
        .forEach((x) => (x.disabled = true));
      const out = await api("/runs/create", {
        method: "POST",
        body: { operation_key: pending.key, request: pending.request },
      });
      const runId = out.run_id || out.id || out.run?.run_id;
      if (!runId) {
        form.dataset.sending = "false";
        error.textContent =
          "服务端已接受请求但未返回任务编号；可按相同身份安全重试或检查任务列表。";
        submit.disabled = false;
        submit.textContent = "按同一请求身份重试";
        return;
      }
      state.pendingCreate = null;
      state.createDraft = null;
      nav(idUrl(runId));
    } catch (err) {
      form.dataset.sending = "false";
      error.textContent = err.message;
      if (!state.createDraft)
        state.createDraft = Object.fromEntries(fd.entries());
      const submit = form.querySelector("[type=submit]");
      if (!state.pendingCreate || (err.status && err.status < 500)) {
        state.pendingCreate = null;
        form
          .querySelectorAll("input,textarea,select")
          .forEach((x) => (x.disabled = false));
        if (submit) submit.disabled = !state.online;
      } else {
        if (submit) {
          submit.disabled = true;
          submit.textContent = "按同一请求身份重试";
        }
        form.querySelectorAll("input,textarea,select").forEach((x) => {
          if (x.name !== "") x.disabled = true;
        });
        error.innerHTML = `请求结果暂时无法确认。请保持字段不变，恢复连接后以同一幂等身份重试。${esc(err.message)} <button type="button" data-check-connection>检查连接</button>`;
      }
    }
  };
}
function safeMarkdown(source) {
  const raw = String(source || "");
  const renderer = new marked.Renderer();
  renderer.html = ({ text }) => esc(text);
  renderer.image = ({ text }) => esc(text);
  const root = document.createElement("div");
  root.innerHTML = DOMPurify.sanitize(
    marked.parse(raw.slice(0, 500000), { renderer, async: false }),
    {
      FORBID_TAGS: [
        "img",
        "video",
        "audio",
        "iframe",
        "style",
        "form",
        "input",
      ],
      FORBID_ATTR: ["style"],
    },
  );
  renderMathInElement(root, {
    delimiters: [
      { left: "$$", right: "$$", display: true },
      { left: "\\[", right: "\\]", display: true },
      { left: "$", right: "$", display: false },
      { left: "\\(", right: "\\)", display: false },
    ],
    throwOnError: false,
    trust: false,
    output: "mathml",
    maxExpand: 100,
    maxSize: 20,
    strict: "ignore",
  });
  root.querySelectorAll("a").forEach((a) => {
    a.setAttribute("rel", "noopener noreferrer");
    a.setAttribute("target", "_blank");
  });
  if (raw.length > 500000)
    root.append(document.createTextNode("内容过长，预览已截断。"));
  return root.innerHTML;
}
function pretty(v) {
  if (v === null || v === undefined) return "不可用";
  if (typeof v === "object")
    return `<pre>${esc(JSON.stringify(v, null, 2))}</pre>`;
  return esc(v);
}
function rowsObject(obj) {
  if (!obj || typeof obj !== "object") return '<p class="muted">尚无数据</p>';
  return `<dl class="kv">${Object.entries(obj)
    .map(
      ([k, v]) =>
        `<dt>${esc(k.replaceAll("_", " "))}</dt><dd>${pretty(v)}</dd>`,
    )
    .join("")}</dl>`;
}
function artifactLabel(a) {
  return (
    a.logical_path ||
    a.name ||
    a.role ||
    a.kind ||
    a.artifact_type ||
    a.occurrence_id ||
    "制品"
  );
}
async function artifactContent(id, occ, markdown = false) {
  const key = id + "/" + occ + "/" + markdown;
  if (state.artifacts.has(key)) return state.artifacts.get(key);
  try {
    const d = await api(
      `/runs/${encodeURIComponent(id)}/artifacts/${encodeURIComponent(occ)}`,
    );
    const text =
      typeof d === "string" ? d : (d.text ?? d.content ?? d.body ?? "");
    const html =
      markdown || d.artifact?.media_type === "text/markdown"
        ? `<div class="markdown">${safeMarkdown(text)}</div>`
        : `<pre class="source">${esc(text)}</pre>`;
    state.artifacts.set(key, html);
    return html;
  } catch (e) {
    return `<p class="error">无法读取此制品：${esc(e.message)}</p>`;
  }
}
function detailTab(tab, d, id) {
  const arts = Array.isArray(d.artifacts) ? d.artifacts : [];
  if (tab === "events") {
    const ev = [...(d.recent_events || [])].reverse();
    return `<h2>运行记录</h2><div class="events">${ev.map((x) => `<article><time>${esc(humanDate(x.occurred_at || x.created_at))}</time><strong>${esc(x.type || x.event_type || "事件")}</strong><span>${esc(x.stage_name || x.stage || "")}</span><p>${esc(x.summary || x.message || x.reason || "")}</p></article>`).join("") || '<p class="muted">暂无运行事件</p>'}</div><button data-more-events>加载更早记录</button>`;
  }
  if (tab === "statement") {
    const a = arts.find((x) => /statement|problem/i.test(artifactLabel(x)));
    return a
      ? `<h2>题面 · ${esc(artifactLabel(a))}</h2>${a.preview || a.content || a.text ? `<div class="markdown">${safeMarkdown(a.preview || a.content || a.text)}</div>` : "<p>已提交题面，可读取完整内容。</p>"}<button data-artifact="${esc(a.occurrence_id || a.id)}" data-markdown="true">读取题面</button>`
      : '<p class="empty">当前阶段尚未提交内容。</p>';
  }
  if (tab === "solution") {
    const choices = arts.filter((x) =>
      /solution|editorial|reference|brute|\.cpp/i.test(artifactLabel(x)),
    );
    return choices.length
      ? `<h2>题解与程序</h2>${choices.map((a) => `<section class="artifact"><h3>${esc(artifactLabel(a))}</h3>${a.preview || a.content || a.text ? `<pre class="source">${esc(a.preview || a.content || a.text)}</pre>` : ""}<button data-artifact="${esc(a.occurrence_id || a.id)}">读取内容</button></section>`).join("")}`
      : '<p class="empty">当前阶段尚未提交内容。</p>';
  }
  if (tab === "checks") {
    const reports = arts.filter(
      (x) =>
        String(x.role || "").toUpperCase() === "EVIDENCE" ||
        /report|check|verify|test|quality|duplicate/i.test(artifactLabel(x)),
    );
    return `<h2>核验报告</h2>${reports.length ? reports.map((a) => `<section class="artifact"><h3>${esc(artifactLabel(a))}</h3><p>${esc(a.summary || "")}</p><button data-artifact="${esc(a.occurrence_id || a.id)}">展开报告</button></section>`).join("") : '<p class="empty">当前阶段尚未提交核验报告。</p>'}`;
  }
  return "<p>当前阶段尚未提交内容。</p>";
}
function renderBudget(b, used = {}, reserved = {}) {
  if (!b) return "<p>预算账本不可用</p>";
  const limits = b.limits || b;
  const specs = [
    ["模型调用", "LLM_CALLS", "max_llm_calls"],
    ["输入 token", "LLM_INPUT_TOKENS", "max_llm_input_tokens"],
    ["输出 token", "LLM_OUTPUT_TOKENS", "max_llm_output_tokens"],
    ["模型费用", "EXTERNAL_COST_MICRO_USD", "max_llm_cost_micro_usd", "money"],
    ["查重调用", "SIMILARITY_CALLS", "max_similarity_calls"],
    [
      "查重费用",
      "SIMILARITY_COST_MICRO_USD",
      "max_similarity_cost_micro_usd",
      "money",
    ],
    ["沙箱容器", "DOCKER_CONTAINER_CREATES", "max_sandbox_creates"],
    ["制品大小", "ARTIFACT_PHYSICAL_NEW_BYTES", "max_artifact_bytes", "bytes"],
    ["活跃时间", "ACTIVE_TIME_NS", "max_active_time_milliseconds", "duration"],
  ];
  const show = (v, type) => {
    if (v === undefined || v === null) return "不可用";
    try {
      if (type === "money")
        return (
          String.fromCharCode(36) +
          BigInt(v) / 1000000n +
          "." +
          (BigInt(v) % 1000000n)
            .toString()
            .padStart(6, "0")
            .replace(/0+$/, "")
            .padEnd(2, "0")
        );
      if (type === "bytes")
        return `${(Number(BigInt(v)) / 1048576).toFixed(2)} MB`;
      if (type === "duration") {
        const ms = BigInt(v) / 1000000n;
        return `${ms / 60000n}分${(ms % 60000n) / 1000n}秒`;
      }
      return String(v);
    } catch {
      return String(v);
    }
  };
  return `<div class="budget-list"><small>已用 / 上限 · 预留另列</small>${specs
    .map(([label, dim, lim, type]) => {
      const limit =
        lim === "max_active_time_milliseconds" && limits[lim] != null
          ? (BigInt(limits[lim]) * 1000000n).toString()
          : limits[lim];
      const usedValue = used[dim],
        reservedValue = reserved[dim];
      return `<div class="budget-item"><span>${esc(label)}</span><strong>${esc(show(usedValue, type))} / ${esc(show(limit, type))}</strong>${String(reservedValue) !== "0" ? `<small>预留 ${esc(show(reservedValue, type))}</small>` : ""}</div>`;
    })
    .join("")}</div>`;
}
function activeTime(r) {
  const raw = r.active_elapsed || r.active_elapsed_milliseconds;
  if (raw) return raw;
  if (r.active_elapsed_ns !== undefined) {
    try {
      const ms = BigInt(r.active_elapsed_ns) / 1000000n;
      return `${ms / 60000n} 分 ${(ms % 60000n) / 1000n} 秒`;
    } catch {
      return "不可用";
    }
  }
  return "不可用";
}
function actionButtons(d, r, id) {
  const a = d.available_actions || {},
    s = r.state;
  let out = "";
  if (d.pending_cancel)
    out += '<span class="muted">已请求取消，等待清理</span>';
  if (a.cancel) {
    out += '<button data-action="cancel" data-write>请求取消</button>';
  }
  if (a.resume) {
    out += `<button data-action="resume" data-write>${s === "NEEDS_REVIEW" ? "继续执行" : "恢复任务"}</button>`;
  }
  if (s === "NEEDS_REVIEW" && (a.review || a.review_retry || a.review_reject)) {
    out += '<button data-action="review" data-write>提交评审决定</button>';
  }
  if (s === "READY")
    out += `<a class="button primary" href="/api/runs/${encodeURIComponent(id)}/package">下载题包</a>`;
  return out || '<span class="muted">当前没有可执行操作</span>';
}
async function detailPage(id, { quiet = false } = {}) {
  if (state.activeRequest || state.editing) return;
  const route = state.route;
  const ctl = new AbortController();
  state.activeRequest = ctl;
  try {
    const d = await api("/runs/" + encodeURIComponent(id), {
      signal: ctl.signal,
    });
    if (route !== state.route) return;
    setOnline(true);
    const signature = JSON.stringify([
      id,
      d.run?.version,
      d.execution?.active,
      d.execution?.last_error,
    ]);
    if (quiet && signature === state.lastDetail) return;
    const r = d.run || {};
    const tab = new URL(location.href).searchParams.get("tab") || state.tab;
    state.tab = ["statement", "solution", "checks", "events"].includes(tab)
      ? tab
      : "statement";
    if (state.eventRunId !== id) {
      state.eventRunId = id;
      state.loadedEvents = [];
      state.eventIds = new Set();
      state.eventCursor = null;
    }
    const recent = [...(d.recent_events || [])];
    if (state.loadedEvents.length && recent.length) {
      const lastKnown = BigInt(
        state.loadedEvents[state.loadedEvents.length - 1].version,
      );
      let oldest = recent.reduce(
        (n, ev) => (BigInt(ev.version) < n ? BigInt(ev.version) : n),
        BigInt(recent[0].version),
      );
      while (oldest > lastKnown + 1n) {
        const page = await api(
          `/runs/${encodeURIComponent(id)}/events?before=${oldest}&limit=200`,
          { signal: ctl.signal },
        );
        if (route !== state.route) return;
        const older = page.events || [];
        if (!older.length) throw Error("事件历史存在缺口，请重新读取任务");
        const next = older.reduce(
          (n, ev) => (BigInt(ev.version) < n ? BigInt(ev.version) : n),
          oldest,
        );
        if (next >= oldest) throw Error("事件游标未前进，请重新读取任务");
        recent.push(...older);
        oldest = next;
      }
    }
    for (const ev of recent) {
      const key = eventKey(ev);
      if (!state.eventIds.has(key)) {
        state.eventIds.add(key);
        state.loadedEvents.push(ev);
      }
    }
    state.loadedEvents.sort((a, b) => {
      try {
        return BigInt(a.version || 0) < BigInt(b.version || 0) ? -1 : 1;
      } catch {
        return 0;
      }
    });
    if (state.eventCursor === null && d.before_version != null)
      state.eventCursor = String(d.before_version);
    d.recent_events = state.loadedEvents;
    state.lastDetail = signature;
    const stageList = (d.stages || []).map((x) =>
      typeof x === "string" ? { name: x } : x,
    );
    const stageHTML =
      stageList
        .map(
          (s) =>
            `<div class="stage"><span>${esc({ SUCCEEDED: "完成", RUNNING: "执行中", PENDING: "等待", NEEDS_REVIEW: "待评审", FAILED: "失败", BLOCKED: "受阻", CANCELLED: "取消" }[s.state] || s.state || "·")}</span> ${esc(s.name || s.stage || s.stage_name || s)}</div>`,
        )
        .join("") || '<p class="muted">没有阶段记录</p>';
    const tabs = [
      ["statement", "题面"],
      ["solution", "题解与代码"],
      ["checks", "核验报告"],
      ["events", "运行记录"],
    ];
    const canReview =
      (d.available_actions || {}).review ||
      (d.available_actions || {}).review_retry ||
      (d.available_actions || {}).review_reject;
    const note =
      state.reviewRoute && !canReview
        ? '<p class="summary">服务端当前未开放此任务的 Web 评审操作；任务状态与触发证据仍可在此查看。</p>'
        : "";
    app.innerHTML = `<div class="detail-head"><div class="toolbar"><div><h1>${esc(runTitle(r))}</h1><div class="meta">${statusBadge(r.state)}<span>${esc(r.current_stage || "—")}</span><span>创建于 ${esc(humanDate(r.created_at))}</span><code>${esc(id)}</code></div></div><div class="actions">${actionButtons(d, r, id)}</div></div></div><div class="tabs" role="tablist" aria-label="任务内容">${tabs.map(([k, t]) => `<button role="tab" id="tab-${k}" aria-controls="panel-${k}" aria-selected="${state.tab === k}" data-tab="${k}">${t}</button>`).join("")}</div><div class="reading"><article class="content" id="panel-${state.tab}" role="tabpanel" aria-labelledby="tab-${state.tab}">${note}${detailTab(state.tab, d, id)}</article><aside class="rail"><section><h3>生成阶段</h3>${stageHTML}</section><section><h3>预算与耗时</h3>${renderBudget(d.budget, d.budget_used, d.budget_reserved)}<p>活跃耗时：${esc(activeTime(r))}</p>${d.execution?.last_error ? `<p class="error">${esc(d.execution.last_error)}</p>` : ""}<p>观察状态：${d.execution?.active === true ? "本进程执行器活跃" : d.execution?.active === false ? "未观察到活跃执行器" : "不可用"}</p></section><section><h3>评审与下一步</h3>${d.review ? `<p>待执行：${d.review.kind === "REJECT" ? "拒绝" : "重试"}</p><p>${esc(d.review.reviewer)} · ${esc(humanDate(d.review.created_at))}</p><p>${esc(d.review.reason)}</p><small>点击“继续执行”应用此决定。</small>` : "<p>当前无待处理评审</p>"}</section></aside></div><div class="status">连接已恢复 · 更新时间 ${new Date().toLocaleTimeString()}</div>`;
    attachDetailActions(id, d);
    for (const b of document.querySelectorAll("[data-artifact]")) {
      const cached = state.artifacts.get(
        id + "/" + b.dataset.artifact + "/" + (b.dataset.markdown === "true"),
      );
      if (cached) {
        b.insertAdjacentHTML("afterend", cached);
        b.remove();
      }
    }
    if (state.reviewRoute && canReview) renderReviewForm(id, d);
    if (["CREATED", "RUNNING", "BLOCKED", "NEEDS_REVIEW"].includes(r.state)) {
      clearInterval(state.detailTimer);
      state.detailTimer = setInterval(
        () => {
          if (!document.hidden) detailPage(id, { quiet: true });
        },
        document.hidden ? 15000 : 2000,
      );
    }
  } catch (e) {
    if (route === state.route && e.name !== "AbortError") {
      fail(e);
      if (!quiet)
        app.innerHTML = `<div class="empty error">${esc(e.message)} <button data-retry>重试</button></div>`;
    }
  } finally {
    if (state.activeRequest === ctl) state.activeRequest = null;
  }
}
function attachDetailActions(id, d) {
  document.querySelectorAll("[data-tab]").forEach(
    (b) =>
      (b.onclick = () => {
        state.tab = b.dataset.tab;
        const u = new URL(location.href);
        u.searchParams.set("tab", state.tab);
        history.pushState({}, "", u);
        detailPage(id);
      }),
  );
  document.querySelectorAll("[data-artifact]").forEach(
    (b) =>
      (b.onclick = async () => {
        const content = await artifactContent(
          id,
          b.dataset.artifact,
          b.dataset.markdown === "true",
        );
        b.insertAdjacentHTML("afterend", content);
        b.remove();
      }),
  );
  const more = document.querySelector("[data-more-events]");
  if (more) {
    more.onclick = async () => {
      if (!state.eventCursor || state.eventCursor === "0") {
        more.textContent = "没有更早事件";
        more.disabled = true;
        return;
      }
      more.disabled = true;
      try {
        const page = await api(
          `/runs/${encodeURIComponent(id)}/events?before=${encodeURIComponent(state.eventCursor)}&limit=50`,
        );
        const older = page.events || [];
        const fresh = older.filter((x) => !state.eventIds.has(eventKey(x)));
        fresh.forEach((x) => {
          state.eventIds.add(eventKey(x));
          state.loadedEvents.push(x);
        });
        state.loadedEvents.sort((a, b) => {
          try {
            return BigInt(a.version || 0) < BigInt(b.version || 0) ? -1 : 1;
          } catch {
            return 0;
          }
        });
        if (older.length)
          state.eventCursor = String(
            page.before_version ?? older[0].version ?? state.eventCursor,
          );
        if (fresh.length) {
          const panel = document.querySelector("#panel-events");
          if (panel && state.eventRunId === id) {
            panel.innerHTML = detailTab(
              "events",
              { ...d, recent_events: state.loadedEvents },
              id,
            );
            attachDetailActions(id, d);
          }
        }
        more.disabled = false;
        if (!older.length) {
          state.eventCursor = "0";
          more.textContent = "没有更早事件";
          more.disabled = true;
        }
      } catch (e) {
        more.disabled = false;
        more.textContent = e.message;
      }
    };
  }
  document
    .querySelectorAll("[data-action]")
    .forEach((b) => (b.onclick = () => performAction(b.dataset.action, id, d)));
}
function eventKey(e) {
  return String(
    e.sequence ??
      e.version ??
      e.id ??
      `${e.occurred_at}:${e.type}:${e.stage_name}`,
  );
}
async function performAction(action, id, d) {
  if (!state.online) return;
  const panel = document.querySelector("#panel-" + state.tab);
  if (state.editing) return;
  if (action === "cancel") {
    state.editing = true;
    panel.insertAdjacentHTML(
      "afterbegin",
      `<form id="cancel-form" class="inline-form"><p>取消后将停止执行并清理资源。</p><button data-write>确认取消任务</button><button type="button" data-dismiss>返回</button><p role="alert"></p></form>`,
    );
    const f = document.querySelector("#cancel-form");
    f.onsubmit = async (e) => {
      e.preventDefault();
      try {
        await api(`/runs/${encodeURIComponent(id)}/cancel`, {
          method: "POST",
          body: {
            operation_key: crypto.randomUUID(),
            expected_run_version: String(d.run?.version ?? ""),
            reason: "用户在工作台取消任务",
          },
        });
        state.editing = false;
        f.innerHTML = "<p>已请求取消，等待执行停止与资源清理。</p>";
        detailPage(id);
      } catch (err) {
        const alert = f.querySelector("[role=alert]");
        alert.textContent =
          err.status === 409
            ? `${err.message}。请检查当前版本后再决定是否重试。`
            : err.message;
      }
    };
    f.querySelector("[data-dismiss]").onclick = () => {
      state.editing = false;
      f.remove();
    };
    return;
  }
  if (action === "resume") {
    bumpStatus("正在提交恢复请求…");
    try {
      await api(`/runs/${encodeURIComponent(id)}/resume`, {
        method: "POST",
        body: {
          operation_key: crypto.randomUUID(),
          expected_run_version: String(d.run?.version),
        },
      });
      await detailPage(id);
    } catch (e) {
      bumpStatus(e.message, true);
    }
    return;
  }
  if (action === "review") {
    renderReviewForm(id, d);
  }
}
function bumpStatus(s, error = false) {
  const el = document.querySelector("#connection");
  if (el) {
    el.textContent = s;
    el.classList.toggle("error", error);
  }
}
function reviewBudgetPatch(fd) {
  const patch = {};
  for (const [, name, , type] of budgetFields) {
    const v = String(fd.get(name) || "").trim();
    if (!v) continue;
    const key = name.replace("_mb", "");
    if (type === "money") {
      moneyToMicro(v);
      patch[key] = v;
    } else if (type === "minutes")
      patch.max_active_time_milliseconds = int64String(
        (BigInt(int64String(v)) * 60000n).toString(),
        "活跃时间",
      );
    else if (type === "mb")
      patch[key] = int64String(
        (BigInt(int64String(v)) * 1048576n).toString(),
        "容量",
      );
    else patch[key] = int64String(v, "预算");
  }
  return patch;
}
function renderReviewForm(id, d) {
  const panel = document.querySelector("#panel-" + state.tab);
  if (document.querySelector("#review-form")) return;
  const rv = d.review || {};
  const currentStage = String(d.run?.current_stage || "");
  const targets =
    currentStage === "judge"
      ? [["statement", "题面与样例"], ["data", "数据生成器与校验器"]]
      : currentStage === "solution_decision"
        ? [["solution", "题解"], ["statement", "题面与样例"]]
        : currentStage === "quality"
          ? [["solution", "题解"]]
          : [[currentStage, `当前阶段（${currentStage}）`]];
  const defaultTarget = targets.some(([stage]) => stage === currentStage)
    ? currentStage
    : targets[0]?.[0] || currentStage;
  state.editing = true;
  panel.insertAdjacentHTML(
    "afterbegin",
    `<form id="review-form" class="inline-form"><h2>人工评审</h2><p>${esc(rv.reason || rv.trigger_reason || "请先查看失败阶段与相关制品，再选择修订目标或其他决定。")}</p><label>评审人<input name="reviewer" required></label><details><summary>补充说明（建议写明报告和失败原因）</summary><label>说明<textarea name="reason" maxlength="4000"></textarea></label></details><label>决定<select name="kind"><option value="REVISE">修复并重做上游阶段</option><option value="RETRY">增加预算后重试当前阶段</option><option value="REJECT">拒绝继续</option></select></label><label>修订目标阶段<select name="revision_target_stage">${targets
      .map(([stage, label]) => `<option value="${esc(stage)}"${stage === defaultTarget ? " selected" : ""}>${esc(label)}</option>`)
      .join("")}</select></label><details><summary>预算增加（仅用于重试）</summary><div class="fields">${budgetFields
      .filter((x) => x[3] !== "hidden")
      .map(
        ([t, n]) =>
          `<label>${t}<input name="${n}" placeholder="留空表示不调整"></label>`,
      )
      .join(
        "",
      )}</div></details><button data-write type="submit">提交决定</button><button type="button" data-dismiss>返回</button><p role="alert"></p><p>修订会从所选阶段重做并重新运行其后的全部验证；保存后仍需显式继续执行。</p></form>`,
  );
  const f = document.querySelector("#review-form");
  f.querySelector("[data-dismiss]").onclick = () => {
    state.editing = false;
    f.remove();
  };
  f.onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(f);
    const kind = fd.get("kind");
    let patch = {};
    try {
      patch = reviewBudgetPatch(fd);
    } catch (err) {
      f.querySelector("[role=alert]").textContent = err.message;
      return;
    }
    if (kind !== "RETRY" && Object.keys(patch).length) {
      f.querySelector("[role=alert]").textContent = "修订或拒绝决定不能包含预算增加";
      return;
    }
    if (kind === "RETRY" && !Object.keys(patch).length) {
      f.querySelector("[role=alert]").textContent =
        "重试必须包含正的预算增加；证据引用暂不支持";
      return;
    }
    try {
      const out = await api(`/runs/${encodeURIComponent(id)}/review`, {
        method: "POST",
        body: {
          operation_key: crypto.randomUUID(),
          expected_run_version: String(d.run?.version ?? ""),
          workflow_revision:
            d.run?.workflow_revision || d.run?.workflow_version || "",
          kind,
          ...(kind === "REVISE"
            ? { revision_target_stage: fd.get("revision_target_stage") }
            : {}),
          reviewer: fd.get("reviewer"),
          reason:
            String(fd.get("reason") || "").trim() ||
            (kind === "REJECT"
              ? "用户拒绝继续执行"
              : kind === "REVISE"
                ? `根据失败证据修复并重做 ${fd.get("revision_target_stage")}`
                : "用户增加预算并请求重试"),
          budget_increase: patch,
        },
      });
      state.editing = false;
      f.innerHTML = `<p>决定 ${esc(out.decision_id || out.id || "已记录")} 已保存，状态：${esc(out.state || "PENDING")}。请显式继续执行。</p>`;
      await detailPage(id);
    } catch (err) {
      f.querySelector("[role=alert]").textContent =
        err.status === 409
          ? `${err.message}。已保留填写内容；请关闭表单刷新并核对最新版本后再提交。`
          : err.message;
    }
  };
}
async function settingsPage() {
  const route = state.route;
  clearInterval(state.detailTimer);
  try {
    const d = await api("/config");
    if (route !== state.route) return;
    setOnline(true);
    const cfg = d.config || d;
    app.innerHTML = common(
      "环境与配置",
      `<div class="content"><h2>启动时加载的配置摘要</h2>${rowsObject({ 工作区: cfg.paths?.state_root || cfg.storage?.state_root, 工作流: cfg.workflow?.revision || "本地 Fake 验证流程", 模型: cfg.llm?.model || "未启用", 模型服务: cfg.llm?.base_url || "未启用", 查重服务: cfg.similarity?.service_identity || "未启用", 沙箱: cfg.sandbox?.engine_endpoint || "未启用" })}<details><summary>完整脱敏配置</summary>${rowsObject(cfg)}</details><div class="summary">配置在服务启动时加载，修改后需要重启。密钥不会在页面显示。执行依赖在实际生成或恢复时检查；手动排障请使用 CLI doctor。</div></div>`,
      "只读",
    );
  } catch (e) {
    if (route !== state.route) return;
    fail(e);
    app.innerHTML = `<div class="empty error">读取配置失败：${esc(e.message)} <button data-retry>重试</button></div>`;
  }
}
async function renderRoute() {
  state.route++;
  state.activeRequest?.abort();
  state.activeRequest = null;
  state.lastDetail = "";
  clearInterval(state.detailTimer);
  const p = location.pathname;
  if (p === "/") return nav("/runs");
  if (p === "/runs/new") {
    state.reviewRoute = false;
    return newPage();
  }
  if (p === "/settings") {
    state.reviewRoute = false;
    return settingsPage();
  }
  if (p === "/reviews") {
    state.reviewRoute = false;
    state.listFilter = "NEEDS_REVIEW";
    return listPage(true);
  }
  if (p === "/runs" || p === "/runs/") {
    state.reviewRoute = false;
    return listPage(false);
  }
  const parts = p.split("/").filter(Boolean);
  if (parts[0] === "runs" && parts[1]) {
    state.reviewRoute = parts[2] === "review";
    return detailPage(decodeURIComponent(parts[1]));
  }
  return listPage(false);
}
document.addEventListener("click", (e) => {
  const n = e.target.closest("[data-nav]");
  if (n) {
    e.preventDefault();
    nav(n.dataset.nav);
    return;
  }
  if (e.target.closest("[data-retry]")) renderRoute();
});
document.addEventListener("click", async (e) => {
  if (!e.target.closest("[data-check-connection]")) return;
  const button = e.target.closest("[data-check-connection]");
  button.disabled = true;
  try {
    await api("/runs?limit=1");
    setOnline(true);
    const form = document.querySelector("#create-form");
    if (form) {
      form
        .querySelectorAll("input,textarea,select")
        .forEach((x) => (x.disabled = true));
      const submit = form.querySelector("[type=submit]");
      submit.disabled = false;
    }
  } catch (err) {
    button.disabled = false;
  }
});
document.addEventListener("keydown", (e) => {
  const t = e.target.closest("[role=tab]");
  if (!t) return;
  const all = [...document.querySelectorAll("[role=tab]")],
    i = all.indexOf(t);
  let j =
    e.key === "ArrowRight"
      ? (i + 1) % all.length
      : e.key === "ArrowLeft"
        ? (i + all.length - 1) % all.length
        : e.key === "Home"
          ? 0
          : e.key === "End"
            ? all.length - 1
            : -1;
  if (j >= 0) {
    e.preventDefault();
    all[j].focus();
    all[j].click();
  }
});
function scheduleListPoll() {
  clearInterval(state.listTimer);
  state.listTimer = setInterval(
    () => {
      if (!document.hidden && ["/runs", "/reviews"].includes(location.pathname))
        renderRoute();
    },
    document.hidden ? 15000 : 5000,
  );
}
window.addEventListener("popstate", () => {
  state.editing = false;
  renderRoute();
});
document.addEventListener("visibilitychange", () => {
  if (state.editing) return;
  if (
    location.pathname.startsWith("/runs/") &&
    location.pathname !== "/runs/new" &&
    !state.editing
  )
    renderRoute();
  else if (["/runs", "/reviews"].includes(location.pathname)) {
    scheduleListPoll();
    renderRoute();
  }
});
async function start() {
  const u = new URL(location.href);
  const token = u.searchParams.get("session");
  if (token) {
    try {
      const sessionResponse = await fetch("/api/session", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json",
        },
        credentials: "same-origin",
        body: JSON.stringify({ token }),
      });
      if (!sessionResponse.ok) throw Error("会话无效");
      u.searchParams.delete("session");
      history.replaceState({}, "", u.pathname + u.search + u.hash);
    } catch {
      app.innerHTML =
        '<p class="empty error">本机会话建立失败，请重新启动 cpgen serve。</p>';
      return;
    }
  }
  try {
    await api("/runs?limit=1");
    setOnline(true);
    renderRoute();
  } catch (e) {
    fail(e);
    app.innerHTML = `<p class="empty error">${esc(e.message)}。请确认本地服务仍在运行，然后<button data-retry>重试</button></p>`;
  }
  scheduleListPoll();
}
start();

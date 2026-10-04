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
  listRequest: 0,
  listRows: [],
  listCursor: "",
  listLoaded: false,
  listLoading: false,
  listPolling: false,
  listPaused: false,
  listError: "",
  listErrorMore: false,
  listReviewOnly: false,
  lastSuccess: 0,
  listFilter: new URL(location.href).searchParams.get("state") || "",
  tab: "statement",
  draft: null,
  editing: false,
  pendingCreate: null,
  createDraft: null,
  reviewRoute: false,
  reviewDismissed: false,
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
async function api(
  path,
  { method = "GET", body, signal, reportConnection = true } = {},
) {
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
    if (reportConnection && e.name !== "AbortError") setOnline(false);
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
    if (reportConnection) setOnline(true);
    throw apiError(data, res.status);
  }
  if (res.headers.get("content-type")?.includes("application/json")) {
    const x = await res.json();
    if (pendingKey) state.pendingActions.delete(pendingKey);
    if (reportConnection) state.lastSuccess = Date.now();
    return x.data ?? x;
  }
  const result = await res.text();
  if (pendingKey) state.pendingActions.delete(pendingKey);
  if (reportConnection) state.lastSuccess = Date.now();
  return result;
}
function setOnline(v) {
  state.online = v;
  document
    .querySelectorAll("[data-write]")
    .forEach(
      (b) => (b.disabled = !v || b.closest("form")?.dataset.sending === "true"),
    );
  const x = document.querySelector("#connection");
  if (x) {
    x.dataset.state = v ? "online" : "offline";
    x.textContent = v
      ? `已连接 · 更新于 ${new Date(state.lastSuccess || Date.now()).toLocaleTimeString()}`
      : "连接中断 · 保留最近一次成功读取，写操作已停用";
  }
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
  return `<div class="toolbar page-heading"><div><h1>${esc(title)}</h1>${sub ? `<p class="page-description">${esc(sub)}</p>` : ""}</div></div>${content}`;
}
function statusBadge(s) {
  const symbol =
    {
      CREATED: "○",
      RUNNING: "●",
      BLOCKED: "!",
      NEEDS_REVIEW: "!",
      READY: "✓",
      FAILED: "×",
      CANCELLED: "−",
    }[s] || "·";
  return `<span class="badge state-${esc(String(s).toLowerCase())}"><span class="status-icon" aria-hidden="true">${symbol}</span>${esc(labels[s] || s || "状态未知")}</span>`;
}
function emptyState(title, description = "", action = "", symbol = "＋") {
  return `<div class="empty-state"><span class="empty-symbol" aria-hidden="true">${symbol}</span><h2 class="empty-title">${esc(title)}</h2>${description ? `<p class="empty-description">${esc(description)}</p>` : ""}${action}</div>`;
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
function renderListPage(reviewOnly) {
  const rows = state.listRows;
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
  const summary = state.listLoading
    ? "正在加载…"
    : !state.listLoaded
      ? "尚未读取任务"
      : state.listCursor
        ? "可加载更多"
        : "没有更多任务";
  const content = `<div class="list-tools">${controls}<button data-list-refresh aria-disabled="${state.listLoading}">${state.listError && !state.listErrorMore ? "重试刷新" : "刷新列表"}</button></div>${
    state.listError
      ? `<p class="empty error" role="alert">读取任务失败：${esc(state.listError)}。${state.listErrorMore ? "已保留当前任务，可重试加载更多。" : state.listLoaded ? "已保留最近一次成功读取，可重试刷新。" : "请重试刷新。"}</p>`
      : ""
  }<div aria-busy="${state.listLoading}">${
    rows.length
      ? `<div class="table-wrap"><table class="run-table" aria-label="${reviewOnly ? "待评审任务" : "生成任务"}"><thead><tr><th scope="col" class="col-title">题目名称</th><th scope="col" class="col-state">状态</th><th scope="col" class="col-stage">当前阶段</th><th scope="col" class="col-created">创建时间</th><th scope="col" class="col-updated">更新时间</th></tr></thead><tbody>${rows
          .map((r) => {
            const id = r.run_id || r.id;
            const path = idUrl(id) + (reviewOnly ? "/review" : "");
            return `<tr><td class="col-title"><a class="run-link run-title" href="${path}" data-nav="${path}" title="${esc(runTitle(r))}">${esc(runTitle(r))}</a><div class="mono muted run-id" title="${esc(id)}">${esc(id)}</div></td><td class="col-state">${statusBadge(r.state)}</td><td class="col-stage">${esc(r.current_stage || r.stage || "—")}</td><td class="col-created">${esc(humanDate(r.created_at))}</td><td class="col-updated">${esc(humanDate(r.updated_at))}</td></tr>`;
          })
          .join("")}</tbody></table></div>`
      : !state.listLoaded
        ? ""
        : reviewOnly
          ? emptyState(
              "当前没有待评审任务",
              "需要人工处理的任务会显示在这里。",
              '<a class="button" href="/runs" data-nav="/runs">查看全部任务</a>',
              "✓",
            )
          : state.listFilter
            ? emptyState(
                "没有符合筛选条件的任务",
                "试试其他状态，或清除筛选查看全部任务。",
                '<button data-filter="">清除筛选</button>',
                "—",
              )
            : emptyState(
                "尚无生成任务",
                "从出题要求开始，生成题面、题解与测试数据。",
                '<button class="primary" data-nav="/runs/new">新建题目</button>',
              )
  }</div><div class="list-tools list-pagination"><div class="list-summary" data-list-summary role="status" tabindex="-1">已加载 ${rows.length} 项 · ${summary}${state.listPaused ? " · 自动刷新已暂停；刷新列表可查看最新任务" : ""}</div>${
    state.listCursor
      ? `<button data-list-more aria-disabled="${state.listLoading}">${state.listErrorMore ? "重试加载更多" : "加载更多"}</button>`
      : ""
  }</div>`;
  const focused = app.contains(document.activeElement)
    ? document.activeElement
    : null;
  const focusFilter = focused?.dataset.filter;
  const focusPath = focused?.dataset.nav;
  const focusMore = focused?.hasAttribute("data-list-more");
  const focusRefresh = focused?.hasAttribute("data-list-refresh");
  app.innerHTML = common(
    reviewOnly ? "待评审" : "生成任务",
    content,
    reviewOnly
      ? "查看触发原因与运行证据，提交决定后继续执行。"
      : "跟踪生成进度，审阅内容与核验结果，导出题包。",
  );
  let focusTarget;
  if (focusFilter !== undefined) {
    focusTarget = [...app.querySelectorAll("[data-filter]")].find(
      (b) => b.dataset.filter === focusFilter,
    );
  } else if (focusPath) {
    focusTarget = [...app.querySelectorAll("[data-nav]")].find(
      (b) => b.dataset.nav === focusPath,
    );
  } else if (focusMore) {
    focusTarget = app.querySelector("[data-list-more]");
  } else if (focusRefresh) {
    focusTarget = app.querySelector("[data-list-refresh]");
  }
  if (focused)
    (focusTarget || app.querySelector("[data-list-summary]"))?.focus({
      preventScroll: true,
    });
  app.querySelector("[data-list-refresh]").onclick = () => {
    if (!state.listLoading || state.listPolling) return listPage(reviewOnly);
  };
  const more = app.querySelector("[data-list-more]");
  if (more) more.onclick = () => listPage(reviewOnly, { more: true });
  if (!reviewOnly) {
    app.querySelectorAll("[data-filter]").forEach(
      (b) =>
        (b.onclick = () => {
          state.listFilter = b.dataset.filter;
          const u = new URL(location.href);
          if (state.listFilter) u.searchParams.set("state", state.listFilter);
          else u.searchParams.delete("state");
          history.pushState({}, "", u);
          return listPage(false, { reset: true });
        }),
    );
  }
}
async function listPage(
  reviewOnly = false,
  { more = false, reset = false, poll = false } = {},
) {
  if (
    !reset &&
    ((state.listLoading && !(state.listPolling && !poll)) ||
      (more && !state.listCursor))
  )
    return;
  if (reset) {
    state.listRows = [];
    state.listCursor = "";
    state.listLoaded = false;
    state.listPaused = false;
  }
  const route = state.route;
  const request = ++state.listRequest;
  const filter = state.listFilter;
  const cursor = more ? state.listCursor : "";
  state.listReviewOnly = reviewOnly;
  state.listLoading = true;
  state.listPolling = poll;
  state.listError = "";
  state.listErrorMore = false;
  // Paging pauses polling immediately, including while a page is in flight.
  if (more) state.listPaused = true;
  clearInterval(state.detailTimer);
  if (!poll) renderListPage(reviewOnly);
  const current = () =>
    route === state.route &&
    request === state.listRequest &&
    filter === state.listFilter &&
    reviewOnly === state.listReviewOnly;
  try {
    const serverState = reviewOnly ? "NEEDS_REVIEW" : filter;
    const d = await api(
      "/runs?limit=50" +
        (serverState ? "&state=" + encodeURIComponent(serverState) : "") +
        (cursor ? "&cursor=" + encodeURIComponent(cursor) : ""),
      { reportConnection: false },
    );
    if (!current()) return;
    const rows = Array.isArray(d.runs) ? d.runs : [];
    if (more) {
      const seen = new Set(state.listRows.map((r) => r.run_id || r.id));
      state.listRows = [
        ...state.listRows,
        ...rows.filter((r) => {
          const id = r.run_id || r.id;
          if (seen.has(id)) return false;
          seen.add(id);
          return true;
        }),
      ];
    } else {
      state.listRows = rows;
      state.listPaused = false;
    }
    state.listCursor = d.next_cursor || "";
    state.listLoaded = true;
    state.lastSuccess = Date.now();
    setOnline(true);
  } catch (e) {
    if (!current()) return;
    state.listError = e.message;
    state.listErrorMore = more;
    fail(e);
  } finally {
    if (current()) {
      state.listLoading = false;
      state.listPolling = false;
      renderListPage(reviewOnly);
    }
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
const splitTokenBudgetFields = [
  ["输入 token 上限", "max_llm_input_tokens", "150000"],
  ["输出 token 上限", "max_llm_output_tokens", "50000"],
];
const legacyBudgetFields = [
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
  const tokenInputs = splitTokenBudgetFields
    .map(
      ([label, name, value]) =>
        `<label>${label}<input name="${name}" inputmode="numeric" pattern="[0-9]+" value="${esc(d[name] ?? value)}" required></label>`,
    )
    .join("");
  const budgetSummary = `输入 token 上限 ${esc(d.max_llm_input_tokens ?? "150000")} · 输出 token 上限 ${esc(d.max_llm_output_tokens ?? "50000")}`;
  app.innerHTML = common(
    "新建题目",
    `<form id="create-form" class="form content-form">
      <section class="form-section form-section--requirements" aria-labelledby="requirements-heading">
        <div class="section-heading"><span class="section-number" aria-hidden="true">01</span><h2 id="requirements-heading">题目要求</h2></div>
        <label><span>出题要求 <span class="muted">必填</span></span><textarea name="brief" required rows="5" maxlength="30000" aria-describedby="brief-help" placeholder="例如：设计一道以轮渡为背景的图遍历题，考查最短路径，并能用小规模数据进行暴力核验。">${esc(d.brief || "")}</textarea></label>
        <p id="brief-help" class="field-help">说明想考查的能力、题目背景与数据范围，让生成方向更明确。</p>
        <div class="fields create-preferences">
          <label>算法标签<input name="tags" value="${esc(d.tags ?? "图论")}" aria-describedby="tags-help"><small id="tags-help" class="muted">用逗号分隔 · 已识别：<span id="normalized-tags">${esc(normalizeTags(d.tags ?? "图论").join(", ") || "—")}</span></small></label>
          <label>难度<select name="difficulty"><option value="medium">中等</option><option value="easy">简单</option><option value="hard">困难</option></select></label>
          <label>题面语言<select name="language"><option value="zh-CN">中文</option><option value="en">English</option></select></label>
        </div>
        <p class="create-policy">解题语言 <strong>C++</strong><span aria-hidden="true">·</span>工作模式 <strong>人工决策</strong></p>
        <details><summary>高级题目要求与策略</summary><div class="fields">
          <label>必须包含（每行一项）<textarea name="required_features" rows="3" placeholder="例如：图不保证连通">${esc(d.required_features || "")}</textarea></label>
          <label>禁止包含（每行一项）<textarea name="forbidden_features" rows="3" placeholder="例如：交互题、特殊判题">${esc(d.forbidden_features || "")}</textarea></label>
          <label>随机种子（可留空）<input name="seed" inputmode="numeric" value="${esc(d.seed || "")}" placeholder="填写非负整数以固定随机种子"></label>
        </div><p class="field-help">核验配置与导出目标使用工作区默认值。</p></details>
      </section>
      <section class="form-section form-section--limits" aria-labelledby="limits-heading">
        <div class="section-heading"><span class="section-number" aria-hidden="true">02</span><h2 id="limits-heading">题目运行限制</h2></div>
        <p class="field-help">用于题目中参赛程序的运行与核验。</p>
        <div class="fields">
          <label>时间限制（毫秒）<input name="time_limit_milliseconds" inputmode="numeric" pattern="[0-9]+" value="${esc(d.time_limit_milliseconds ?? "2000")}" required></label>
          <label>内存限制（MB）<input name="memory_limit_megabytes" inputmode="numeric" pattern="[0-9]+" value="${esc(d.memory_limit_megabytes ?? "512")}" required></label>
        </div>
      </section>
      <section class="form-section form-section--budget" aria-labelledby="budget-heading">
        <div class="section-heading"><span class="section-number" aria-hidden="true">03</span><h2 id="budget-heading">生成预算</h2></div>
        <p class="field-help">设置本次任务的资源上限；如预算不足，可在任务详情中查看原因与处理方式。</p>
        <div class="fields">${tokenInputs}</div>
        <p class="field-help">输入与输出分别累计，包含重试和重新生成，各自额度独立。</p>
      </section>
      <div class="form-footer">
        <div class="summary"><strong id="budget-summary">${budgetSummary}</strong><p>使用当前工作区配置。开始生成会调用外部服务并消耗预算。</p></div>
        <div class="actions"><button class="primary" data-write type="submit">开始生成</button><button type="button" data-nav="/runs">返回任务列表</button></div>
        <p id="form-error" role="alert" class="error" tabindex="-1"></p>
      </div>
    </form>`,
    "描述出题目标，设定运行限制与本次生成预算。",
  );
  const form = document.querySelector("#create-form");
  const error = form.querySelector("#form-error");
  let errorField = null;
  const clearFieldError = () => {
    form.querySelectorAll('[aria-invalid="true"]').forEach((field) => {
      field.removeAttribute("aria-invalid");
      field.removeAttribute("aria-errormessage");
    });
    errorField = null;
    error.textContent = "";
  };
  const showFieldError = (field, message) => {
    clearFieldError();
    error.textContent = message;
    if (field instanceof HTMLElement) {
      const section = field.closest("details");
      if (section) section.open = true;
      field.setAttribute("aria-invalid", "true");
      field.setAttribute("aria-errormessage", "form-error");
      errorField = field;
      field.focus();
    } else error.focus();
  };
  form.addEventListener(
    "invalid",
    (event) => {
      event.preventDefault();
      const field = event.target;
      if (field !== form.querySelector(":invalid")) return;
      const label =
        field.closest("label")?.firstChild?.textContent?.trim() || "此项";
      const message = field.validity.valueMissing
        ? `请填写${label.replace(/\s*必填$/, "")}。`
        : field.validity.patternMismatch
          ? `${label}请填写非负整数。`
          : field.validationMessage;
      showFieldError(field, message);
    },
    true,
  );
  form.querySelector("[type=submit]").disabled = !state.online;
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
  form.oninput = (event) => {
    if (!errorField || event.target === errorField) clearFieldError();
    state.createDraft = Object.fromEntries(new FormData(form).entries());
    const normalized = normalizeTags(form.elements.tags.value);
    document.querySelector("#normalized-tags").textContent =
      normalized.join(", ") || "—";
    document.querySelector("#budget-summary").textContent =
      `输入 token 上限 ${form.elements.max_llm_input_tokens.value} · 输出 token 上限 ${form.elements.max_llm_output_tokens.value}`;
  };
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (!state.online || form.dataset.sending === "true") return;
    form.dataset.sending = "true";
    const fd = new FormData(form);
    const get = (n) => String(fd.get(n) || "").trim();
    let validationField = "";
    try {
      let pending = state.pendingCreate;
      if (!pending) {
        const budget = {};
        for (const [label, name] of splitTokenBudgetFields) {
          validationField = name;
          budget[name] = int64String(get(name), label);
        }
        const seed = get("seed");
        validationField = "seed";
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
        validationField = "brief";
        if (!request.brief) throw Error("请填写出题要求");
        for (const n of ["time_limit_milliseconds", "memory_limit_megabytes"]) {
          validationField = n;
          request[n] = int64String(request[n], "题目限制");
          if (BigInt(request[n]) < 1n) throw Error("题目限制必须为正整数");
        }
        state.createDraft = Object.fromEntries(fd.entries());
        pending = { key: crypto.randomUUID(), request };
        state.pendingCreate = pending;
      }
      validationField = "";
      const submit = form.querySelector("[type=submit]");
      submit.disabled = true;
      submit.textContent = "正在创建任务…";
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
        if (submit) {
          submit.disabled = !state.online;
          submit.textContent = "开始生成";
        }
        const field = form.elements.namedItem(err.field || validationField);
        showFieldError(field, err.message);
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
const automaticArtifactReads = new Set();
const pendingArtifactReads = new Map();
function artifactCacheKey(id, a, d, markdown = false) {
  // This occurrence serves the draft until READY, then the verified package.
  const revision = /\/verified-draft\.md$/.test(artifactLabel(a))
    ? d.run?.state === "READY"
      ? "/final"
      : "/draft"
    : "";
  return id + "/" + (a.occurrence_id || a.id) + "/" + markdown + revision;
}
async function artifactContent(id, occ, markdown = false, key) {
  if (state.artifacts.has(key)) return state.artifacts.get(key);
  if (pendingArtifactReads.has(key)) return pendingArtifactReads.get(key);
  const pending = (async () => {
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
  })();
  pendingArtifactReads.set(key, pending);
  try {
    return await pending;
  } finally {
    pendingArtifactReads.delete(key);
  }
}
function artifactPanel(a, d, id, title, markdown = false, autoload = false) {
  const key = artifactCacheKey(id, a, d, markdown);
  const cached = state.artifacts.get(key);
  const pending = !cached && pendingArtifactReads.has(key);
  const statement = title === "题面";
  const version = statement
    ? /\/verified-draft\.md$/.test(artifactLabel(a))
      ? d.run?.state === "READY"
        ? "最终题面 · 来自已核验题包"
        : "题面草稿 · 样例尚未定稿"
      : "已提交题面"
    : "已提交内容";
  return `<section class="artifact"><div class="artifact-heading"><h2>${esc(title)}</h2><span class="artifact-version">${esc(version)}</span></div><p class="artifact-path"><code>${esc(artifactLabel(a))}</code></p><div class="artifact-body" aria-live="polite" ${pending ? 'aria-busy="true"' : ""}>${cached || `<p class="muted">${pending ? "正在读取已提交内容…" : "读取已提交内容后，可在此预览。"}</p>`}</div><button data-artifact="${esc(a.occurrence_id || a.id)}" data-markdown="${markdown}" data-cache-key="${esc(key)}" ${autoload ? "data-autoload" : ""} ${cached ? "hidden" : ""} ${pending ? "disabled" : ""}>${pending ? "正在读取…" : statement ? "读取题面" : "读取内容"}</button></section>`;
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
      ? artifactPanel(a, d, id, "题面", true, d.execution?.active !== true)
      : emptyState(
          "暂无可读取题面",
          "当前任务尚未提交题面，可查看生成阶段与运行记录。",
          "",
          "○",
        );
  }
  if (tab === "solution") {
    const choices = arts.filter((x) =>
      /solution|editorial|reference|brute|\.cpp/i.test(artifactLabel(x)),
    );
    return choices.length
      ? choices
          .map((a) =>
            artifactPanel(
              a,
              d,
              id,
              /brute/i.test(artifactLabel(a))
                ? "暴力解法"
                : /reference/i.test(artifactLabel(a))
                  ? "参考程序"
                  : /\.cpp$/i.test(artifactLabel(a))
                    ? "程序源码"
                    : "题解",
              a.media_type === "text/markdown",
            ),
          )
          .join("")
      : emptyState(
          "题解与代码尚未生成",
          "这里只展示已经提交并可读取的题解与程序。",
          "",
          "○",
        );
  }
  if (tab === "checks") {
    const reports = arts.filter(
      (x) =>
        String(x.role || "").toUpperCase() === "EVIDENCE" ||
        /report|check|verify|test|quality|duplicate/i.test(artifactLabel(x)),
    );
    return `<h2>核验报告</h2><p class="reading-note">报告记录已执行的核验；题意与算法仍需人工审阅。</p>${reports.length ? reports.map((a) => artifactPanel(a, d, id, `${stageLabel(a.stage_name)} · 核验记录`)).join("") : '<p class="empty">当前阶段尚未提交核验报告。</p>'}`;
  }
  return "<p>当前阶段尚未提交内容。</p>";
}
function renderBudget(b, used = {}, reserved = {}) {
  if (!b) return "<p>预算账本不可用</p>";
  const limits = b.limits || b;
  if (limits.split_token_budget)
    return renderSplitTokenBudget(limits, used, reserved);
  if (!limits.token_budget && limits.max_llm_tokens == null) {
    return renderLegacyBudget(b, used, reserved);
  }
  try {
    const total = (values) => {
      if (values.LLM_TOKENS != null) return BigInt(values.LLM_TOKENS);
      if (values.LLM_INPUT_TOKENS == null || values.LLM_OUTPUT_TOKENS == null)
        return null;
      return BigInt(values.LLM_INPUT_TOKENS) + BigInt(values.LLM_OUTPUT_TOKENS);
    };
    const tokenUsed = total(used);
    const tokenReserved = total(reserved);
    const tokenOccupied =
      tokenUsed != null && tokenReserved != null
        ? tokenUsed + tokenReserved
        : null;
    const tokenLimit = BigInt(limits.max_llm_tokens ?? 0);
    const show = (value) => (value == null ? "不可用" : String(value));
    return `<div class="budget-list"><div class="budget-item"><span>模型 token（已用＋预留 / 上限）</span><strong>${esc(show(tokenOccupied))} / ${esc(tokenLimit)}</strong><small>已用 ${esc(show(tokenUsed))} · 预留 ${esc(show(tokenReserved))}</small></div><details><summary>用量明细</summary><p>输入 ${esc(show(used.LLM_INPUT_TOKENS))} token · 输出 ${esc(show(used.LLM_OUTPUT_TOKENS))} token</p><p>模型调用 ${esc(show(used.LLM_CALLS))} 次</p></details></div>`;
  } catch {
    return "<p>预算账本不可用</p>";
  }
}
function renderSplitTokenBudget(limits, used, reserved) {
  const specs = [
    ["输入 token", "LLM_INPUT_TOKENS", "max_llm_input_tokens"],
    ["输出 token", "LLM_OUTPUT_TOKENS", "max_llm_output_tokens"],
  ];
  const show = (value) => (value == null ? "不可用" : String(value));
  try {
    const rows = specs
      .map(([label, dimension, limit]) => {
        const tokenUsed =
          used[dimension] == null ? null : BigInt(used[dimension]);
        const tokenReserved =
          reserved[dimension] == null ? null : BigInt(reserved[dimension]);
        const occupied =
          tokenUsed != null && tokenReserved != null
            ? tokenUsed + tokenReserved
            : null;
        return `<div class="budget-item"><span>${label}</span><strong>${esc(show(occupied))} / ${esc(show(limits[limit]))}</strong><small>已用 ${esc(show(tokenUsed))} · 预留 ${esc(show(tokenReserved))}</small></div>`;
      })
      .join("");
    return `<div class="budget-list"><small>已用＋预留 / 各自上限</small>${rows}<details><summary>用量明细</summary><p>模型调用 ${esc(show(used.LLM_CALLS))} 次</p></details></div>`;
  } catch {
    return "<p>预算账本不可用</p>";
  }
}
function renderLegacyBudget(b, used = {}, reserved = {}) {
  if (!b) return "<p>预算账本不可用</p>";
  const limits = b.limits || b;
  const specs = [
    ["模型费用", "EXTERNAL_COST_MICRO_USD", "max_llm_cost_micro_usd", "money"],
    [
      "查重费用",
      "SIMILARITY_COST_MICRO_USD",
      "max_similarity_cost_micro_usd",
      "money",
    ],
    ["模型调用", "LLM_CALLS", "max_llm_calls"],
    ["查重调用", "SIMILARITY_CALLS", "max_similarity_calls"],
    ["输入 token", "LLM_INPUT_TOKENS", "max_llm_input_tokens"],
    ["输出 token", "LLM_OUTPUT_TOKENS", "max_llm_output_tokens"],
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
function stageLabel(name) {
  return (
    {
      idea: "题目构思",
      statement: "题面与样例",
      similarity: "相似度检查",
      similarity_decision: "查重评审",
      solution: "题解与程序",
      solution_verify: "题解验证",
      solution_decision: "题解评审",
      data: "测试数据",
      data_verify: "数据验证",
      judge: "执行核验",
      quality: "质量检查",
      package: "题包打包",
      prepare: "准备（本地验证）",
      exercise: "执行（本地验证）",
      checkpoint: "评审检查点（本地验证）",
      slice2_checkpoint: "查重检查点",
      solution_checkpoint: "题解检查点",
    }[name] ||
    name ||
    "阶段"
  );
}
function stageStateLabel(status) {
  return (
    {
      SUCCEEDED: "完成",
      RUNNING: "执行中",
      PENDING: "等待",
      NEEDS_REVIEW: "待评审",
      FAILED: "失败",
      BLOCKED: "受阻",
      CANCELLED: "已取消",
      INTERRUPTED: "已中断",
    }[status] ||
    status ||
    "等待"
  );
}
function renderStage(s) {
  const name = s.name || s.stage || s.stage_name;
  const attempts = Array.isArray(s.attempts) ? s.attempts : [];
  const heading = `<span class="stage-state">${esc(stageStateLabel(s.state))}</span><span class="stage-name">${esc(stageLabel(name))}<code>${esc(name)}</code></span>`;
  const rows = attempts
    .map(
      (a) =>
        `<div class="stage-attempt"><strong>尝试 ${esc(a.ordinal)} · ${esc(stageStateLabel(a.state))}</strong><span>${esc(humanDate(a.started_at))}</span>${a.cause ? `<span>中断原因：${esc({ user_cancel: "用户取消", revision_invalidated: "上游修订", step_deadline: "阶段时限已到", run_budget_deadline: "活跃时间预算已到" }[a.cause] || a.cause)}</span>` : ""}${a.blocked_binding?.retry_after ? `<span>可重试时间：${esc(humanDate(a.blocked_binding.retry_after))}</span>` : ""}</div>`,
    )
    .join("");
  return attempts.length
    ? `<details class="stage-details" data-stage="${esc(name)}" data-status="${esc(s.state || "PENDING")}"><summary class="stage">${heading}</summary>${rows}</details>`
    : `<div class="stage" data-status="${esc(s.state || "PENDING")}">${heading}</div>`;
}
function detailNotice(d, r) {
  const a = d.available_actions || {};
  let title = "",
    text = "",
    kind = "neutral";
  if (d.pending_cancel) {
    title = "已请求取消，正在等待停止与清理";
    text = "任务终态将以执行器完成清理后保存的结果为准。";
  } else if (d.review) {
    title = "评审决定已保存，等待执行";
    text = a.resume
      ? "点击“继续执行”应用此决定。"
      : "决定尚未应用，请查看运行状态。";
    kind = "warning";
  } else if (r.state === "NEEDS_REVIEW") {
    title = "任务需要人工评审";
    text =
      a.review || a.review_retry || a.review_reject
        ? "先查看当前阶段与核验报告，再提交评审决定。"
        : "请查看当前阶段与核验报告，服务端暂未开放评审操作。";
    kind = "warning";
  } else if (r.state === "BLOCKED") {
    title = "任务受阻，已保存生成进度";
    text = a.resume
      ? "查看阶段记录与执行错误，处理后可尝试恢复任务。"
      : "查看阶段记录与执行错误，确认受阻原因。";
    kind = "warning";
  } else if (r.state === "RUNNING" && d.execution?.active !== true) {
    title = "执行状态待确认";
    text = a.resume
      ? "当前未观察到活跃执行器，可尝试恢复任务；服务端会检查执行锁。"
      : "持久化状态为进行中，当前执行器状态尚未确认。";
    kind = "warning";
  } else if (r.state === "READY") {
    title = "已通过当前核验门禁，可导出";
    text = "题意与算法仍需人工审阅。";
  } else if (r.state === "FAILED") {
    title = "任务已失败";
    text = "查看阶段记录、核验报告与执行错误。";
    kind = "warning";
  }
  return title
    ? `<div class="detail-notice" data-kind="${kind}"><strong>${esc(title)}</strong><span>${esc(text)}</span>${d.execution?.last_error ? `<p class="error">${esc(d.execution.last_error)}</p>` : ""}</div>`
    : "";
}
function actionButtons(d, r, id) {
  const a = d.available_actions || {},
    s = r.state;
  let out = "";
  if (d.pending_cancel)
    out += '<span class="muted">已请求取消，等待清理</span>';
  if (a.cancel) {
    out +=
      '<button class="danger" data-action="cancel" data-write>请求取消</button>';
  }
  if (a.resume) {
    out += `<button class="primary" data-action="resume" data-write>${s === "NEEDS_REVIEW" ? "继续执行" : "恢复任务"}</button>`;
  }
  if (s === "NEEDS_REVIEW" && (a.review || a.review_retry || a.review_reject)) {
    out +=
      '<button class="primary" data-action="review" data-write>提交评审决定</button>';
  }
  if (s === "READY")
    out += `<a class="button primary" href="/api/runs/${encodeURIComponent(id)}/package">下载题包</a>`;
  return out || '<span class="muted">当前没有可执行操作</span>';
}
function scheduleDetailPoll(id, runState, route) {
  clearInterval(state.detailTimer);
  state.detailTimer = 0;
  if (["CREATED", "RUNNING", "BLOCKED", "NEEDS_REVIEW"].includes(runState)) {
    state.detailTimer = setInterval(
      () => {
        if (route === state.route && !document.hidden)
          detailPage(id, { quiet: true });
      },
      document.hidden ? 15000 : 2000,
    );
  }
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
    const r = d.run || {};
    const signature = JSON.stringify([
      id,
      d.run?.version,
      d.execution?.active,
      d.execution?.last_error,
    ]);
    if (quiet && signature === state.lastDetail) {
      scheduleDetailPoll(id, r.state, route);
      return;
    }
    const tab = new URL(location.href).searchParams.get("tab") || "statement";
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
      stageList.map(renderStage).join("") ||
      '<p class="muted">没有阶段记录</p>';
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
    const focusTab = app.contains(document.activeElement)
      ? document.activeElement?.dataset.tab
      : null;
    const openStages = new Set(
      [...app.querySelectorAll(".stage-details[open]")].map(
        (el) => el.dataset.stage,
      ),
    );
    app.innerHTML = `<div class="breadcrumb"><a href="${state.reviewRoute ? "/reviews" : "/runs"}" data-nav="${state.reviewRoute ? "/reviews" : "/runs"}">${state.reviewRoute ? "待评审" : "生成任务"}</a><span aria-hidden="true">/</span><span>${state.reviewRoute ? "人工评审" : "任务详情"}</span></div><div class="detail-head"><div class="toolbar"><div><h1>${esc(runTitle(r))}</h1><div class="meta">${statusBadge(r.state)}<span>${esc(stageLabel(r.current_stage))}</span><span>创建于 ${esc(humanDate(r.created_at))}</span><code>${esc(id)}</code></div></div><div class="actions">${actionButtons(d, r, id)}</div></div></div>${detailNotice(d, r)}<div class="tabs" role="tablist" aria-label="任务内容">${tabs.map(([k, t]) => `<button role="tab" id="tab-${k}" aria-controls="panel-${k}" aria-selected="${state.tab === k}" tabindex="${state.tab === k ? "0" : "-1"}" data-tab="${k}">${t}</button>`).join("")}</div><div class="reading"><article class="content" id="panel-${state.tab}" role="tabpanel" tabindex="0" aria-labelledby="tab-${state.tab}">${note}${detailTab(state.tab, d, id)}</article><aside class="rail"><section><h3>生成阶段</h3>${stageHTML}</section><section><h3>预算与耗时</h3>${renderBudget(d.budget, d.budget_used, d.budget_reserved)}<p>活跃耗时：${esc(activeTime(r))}</p>${d.execution?.last_error ? `<p class="error">${esc(d.execution.last_error)}</p>` : ""}<p>观察状态：${d.execution?.active === true ? "已观察到活跃执行器" : d.execution?.active === false ? "未观察到活跃执行器" : "不可用"}</p></section><section><h3>评审与下一步</h3>${d.review ? `<p>待执行：${esc({ REJECT: "拒绝", RETRY: "重试", REVISE: "修订" }[d.review.kind] || d.review.kind)}</p><p>${esc(d.review.reviewer)} · ${esc(humanDate(d.review.created_at))}</p><p>${esc(d.review.reason)}</p><small>点击“继续执行”应用此决定。</small>` : r.state === "NEEDS_REVIEW" ? "<p>等待人工评审决定</p>" : r.state === "BLOCKED" ? "<p>处理受阻原因后，按可用操作恢复。</p>" : "<p>当前无待执行评审决定</p>"}</section></aside></div><div class="status" role="status">最近更新 · ${new Date().toLocaleTimeString()}</div>`;
    attachDetailActions(id, d);
    app.querySelectorAll(".stage-details").forEach((el) => {
      el.open = openStages.has(el.dataset.stage);
    });
    if (focusTab)
      document
        .querySelector(`#tab-${state.tab}`)
        ?.focus({ preventScroll: true });
    for (const b of app.querySelectorAll("[data-autoload]:not([hidden])")) {
      if (!automaticArtifactReads.has(b.dataset.cacheKey)) {
        automaticArtifactReads.add(b.dataset.cacheKey);
        b.click();
      }
    }
    if (state.reviewRoute && canReview && !state.reviewDismissed)
      renderReviewForm(id, d);
    // Retain retries if fetching missing events failed before rendering.
    scheduleDetailPoll(id, r.state, route);
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
        if (state.editing) return;
        state.activeRequest?.abort();
        state.activeRequest = null;
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
        if (b.disabled) return;
        let current = b;
        let body = b.closest(".artifact").querySelector(".artifact-body");
        const findCurrent = () => {
          current = b.isConnected
            ? b
            : [...app.querySelectorAll("[data-artifact]")].find(
                (el) => el.dataset.cacheKey === b.dataset.cacheKey,
              );
          body = current?.closest(".artifact").querySelector(".artifact-body");
          return !!body;
        };
        b.disabled = true;
        b.textContent = "正在读取…";
        body.setAttribute("aria-busy", "true");
        try {
          const content = await artifactContent(
            id,
            b.dataset.artifact,
            b.dataset.markdown === "true",
            b.dataset.cacheKey,
          );
          if (!findCurrent()) return;
          body.innerHTML = content;
          current.hidden = true;
        } catch (e) {
          if (!findCurrent()) return;
          body.innerHTML = `<p class="error">无法读取此内容：${esc(e.message)}</p>`;
          current.textContent = "重试读取";
        } finally {
          if (current?.isConnected) {
            current.disabled = false;
            body.removeAttribute("aria-busy");
          }
        }
      }),
  );
  const more = document.querySelector("[data-more-events]");
  if (more) {
    more.onclick = async () => {
      const route = state.route;
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
        if (route !== state.route || state.eventRunId !== id) return;
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
        if (route !== state.route || state.eventRunId !== id) return;
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
      if (!state.online || f.dataset.sending === "true") return;
      const route = state.route;
      const current = () =>
        route === state.route && document.querySelector("#cancel-form") === f;
      const path = `/runs/${encodeURIComponent(id)}/cancel`;
      const submit = f.querySelector("[data-write]");
      f.dataset.sending = "true";
      submit.disabled = true;
      try {
        // An uncertain write must replay its original identity and version,
        // even if cancellation has already changed the persisted run state.
        let body = state.pendingActions.get(path)?.body;
        if (!body) {
          const latest = await api(`/runs/${encodeURIComponent(id)}`, {
            reportConnection: false,
          });
          if (!current()) return;
          if (!latest.available_actions?.cancel) {
            state.editing = false;
            await detailPage(id);
            return;
          }
          body = {
            operation_key: crypto.randomUUID(),
            expected_run_version: String(latest.run?.version ?? ""),
            reason: "用户在工作台取消任务",
          };
        }
        await api(path, {
          method: "POST",
          reportConnection: false,
          body,
        });
        if (!current()) return;
        state.lastSuccess = Date.now();
        state.editing = false;
        f.innerHTML = "<p>已请求取消，等待执行停止与资源清理。</p>";
        detailPage(id);
      } catch (err) {
        if (!current()) return;
        if (err.name !== "AbortError") setOnline(!!err.status);
        const alert = f.querySelector("[role=alert]");
        alert.textContent =
          err.status === 409
            ? `${err.message}。可再次确认取消；重试前会读取最新任务状态。`
            : err.message;
      } finally {
        f.dataset.sending = "false";
        if (current()) submit.disabled = !state.online;
      }
    };
    f.querySelector("[data-dismiss]").onclick = () => {
      state.editing = false;
      f.remove();
    };
    return;
  }
  if (action === "resume") {
    const route = state.route;
    bumpStatus("正在提交恢复请求…");
    try {
      await api(`/runs/${encodeURIComponent(id)}/resume`, {
        method: "POST",
        reportConnection: false,
        body: {
          operation_key: crypto.randomUUID(),
          expected_run_version: String(d.run?.version),
        },
      });
      if (route !== state.route) return;
      state.lastSuccess = Date.now();
      await detailPage(id);
    } catch (e) {
      if (route !== state.route) return;
      if (e.name !== "AbortError") setOnline(!!e.status);
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
function legacyReviewBudgetPatch(fd) {
  const patch = {};
  for (const [, name, , type] of legacyBudgetFields) {
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
function splitReviewBudgetPatch(fd) {
  const patch = {};
  for (const [label, name] of splitTokenBudgetFields) {
    const value = String(fd.get(name) || "").trim();
    if (!value) continue;
    const tokens = int64String(value, label);
    if (tokens !== "0") patch[name] = tokens;
  }
  return patch;
}
function reviewBudgetPatch(fd) {
  const value = String(fd.get("max_llm_tokens") || "").trim();
  if (!value) return {};
  const tokens = int64String(value, "增加 token 数");
  if (tokens === "0") throw Error("增加 token 数必须为正整数");
  return { max_llm_tokens: tokens };
}
function renderReviewForm(id, d) {
  const panel = document.querySelector("#panel-" + state.tab);
  if (document.querySelector("#review-form")) return;
  const rv = d.review || {};
  const limits = d.budget?.limits || d.budget || {};
  const splitBudget = !!limits.split_token_budget;
  const legacyBudget =
    !splitBudget && !limits.token_budget && limits.max_llm_tokens == null;
  const currentStage = String(d.run?.current_stage || "");
  const targets = d.revision_targets || [];
  const defaultTarget = targets[0];
  state.editing = true;
  panel.insertAdjacentHTML(
    "afterbegin",
    `<form id="review-form" class="inline-form"><h2>人工评审</h2><p>${esc(rv.reason || rv.trigger_reason || "请先查看失败阶段与相关制品，再选择修订目标或其他决定。")}</p><label>评审人<input name="reviewer" required></label><details><summary>补充说明（建议写明报告和失败原因）</summary><label>说明<textarea name="reason" maxlength="4000"></textarea></label></details><label>决定<select name="kind"><option value="REVISE"${targets.length ? "" : " disabled"}>修复并重做上游阶段</option><option value="RETRY">增加预算后重试当前阶段</option><option value="REJECT">拒绝继续</option></select></label><label>修订目标阶段<select name="revision_target_stage"${targets.length ? "" : " disabled"}>${targets
      .map(
        (stage) =>
          `<option value="${esc(stage)}"${stage === defaultTarget ? " selected" : ""}>${esc(stageLabel(stage))}${stage === currentStage ? "（当前阶段）" : ""}</option>`,
      )
      .join(
        "",
      )}</select></label>${targets.length ? "" : "<p>当前工作流没有可用的修订目标，请刷新后查看可用决定。</p>"}${
      splitBudget
        ? '<div class="fields"><label>增加输入 token 数（仅用于重试）<input name="max_llm_input_tokens" inputmode="numeric" placeholder="留空表示不调整"></label><label>增加输出 token 数（仅用于重试）<input name="max_llm_output_tokens" inputmode="numeric" placeholder="留空表示不调整"></label></div>'
        : legacyBudget
          ? `<details><summary>历史任务预算增加（仅用于重试）</summary><div class="fields">${legacyBudgetFields
              .filter((field) => field[3] !== "hidden")
              .map(
                ([label, name]) =>
                  `<label>${label}<input name="${name}" placeholder="留空表示不调整"></label>`,
              )
              .join("")}</div></details>`
          : '<label>增加 token 数（仅用于重试）<input name="max_llm_tokens" inputmode="numeric" placeholder="留空表示不调整"></label>'
    }<button class="primary" data-write type="submit">提交决定</button><button type="button" data-dismiss>收起评审，查看内容</button><p role="alert"></p><p>修订会从所选阶段重做并重新运行其后的全部验证；保存后仍需显式继续执行。</p></form>`,
  );
  const f = document.querySelector("#review-form");
  f.querySelector("[data-dismiss]").onclick = () => {
    state.editing = false;
    state.reviewDismissed = true;
    f.remove();
  };
  f.onsubmit = async (e) => {
    e.preventDefault();
    const route = state.route;
    const fd = new FormData(f);
    const kind = fd.get("kind");
    if (
      kind === "REVISE" &&
      !targets.includes(fd.get("revision_target_stage"))
    ) {
      f.querySelector("[role=alert]").textContent =
        "请选择服务端提供的修订目标";
      return;
    }
    let patch = {};
    try {
      patch = splitBudget
        ? splitReviewBudgetPatch(fd)
        : legacyBudget
          ? legacyReviewBudgetPatch(fd)
          : reviewBudgetPatch(fd);
    } catch (err) {
      f.querySelector("[role=alert]").textContent = err.message;
      return;
    }
    if (kind !== "RETRY" && Object.keys(patch).length) {
      f.querySelector("[role=alert]").textContent =
        "修订或拒绝决定不能包含预算增加";
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
        reportConnection: false,
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
      if (route !== state.route) return;
      state.lastSuccess = Date.now();
      state.editing = false;
      f.innerHTML = `<p>决定 ${esc(out.decision_id || out.id || "已记录")} 已保存，状态：${esc(out.state || "PENDING")}。请显式继续执行。</p>`;
      await detailPage(id);
    } catch (err) {
      if (route !== state.route) return;
      if (err.name !== "AbortError") setOnline(!!err.status);
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
  const reviewPath = p === "/reviews" || /^\/runs\/[^/]+\/review$/.test(p);
  const section =
    p === "/settings" ? "/settings" : reviewPath ? "/reviews" : "/runs";
  document
    .querySelectorAll('nav[aria-label="主导航"] [data-nav]')
    .forEach((link) => {
      if (link.dataset.nav === section)
        link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    });
  document.title = `${p === "/runs/new" ? "新建题目" : section === "/settings" ? "环境与配置" : reviewPath ? "待评审" : "生成任务"} · CPGen`;
  if (app.dataset.route !== p) {
    app.dataset.route = p;
    state.reviewDismissed = false;
    app.innerHTML = '<p class="empty muted" role="status">正在读取工作区…</p>';
  }
  const createButton = document.querySelector('header [data-nav="/runs/new"]');
  if (createButton) createButton.hidden = p === "/runs/new";
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
    return listPage(true, { reset: true });
  }
  if (p === "/runs" || p === "/runs/") {
    state.reviewRoute = false;
    state.listFilter = new URL(location.href).searchParams.get("state") || "";
    return listPage(false, { reset: true });
  }
  const parts = p.split("/").filter(Boolean);
  if (parts[0] === "runs" && parts[1]) {
    state.reviewRoute = parts[2] === "review";
    return detailPage(decodeURIComponent(parts[1]));
  }
  return listPage(false, { reset: true });
}
document.addEventListener("click", (e) => {
  const n = e.target.closest("[data-nav]");
  if (n) {
    if (
      n instanceof HTMLAnchorElement &&
      (e.ctrlKey || e.metaKey || e.shiftKey || e.altKey || e.button !== 0)
    )
      return;
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
        .forEach((x) => (x.disabled = !!state.pendingCreate));
      const submit = form.querySelector("[type=submit]");
      submit.disabled = form.dataset.sending === "true";
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
function isListRoute() {
  return ["/runs", "/runs/", "/reviews"].includes(location.pathname);
}
function maybeListPoll() {
  if (
    !document.hidden &&
    isListRoute() &&
    !state.listPaused &&
    !state.listLoading
  )
    return listPage(location.pathname === "/reviews", { poll: true });
}
function scheduleListPoll() {
  clearInterval(state.listTimer);
  state.listTimer = setInterval(maybeListPoll, document.hidden ? 15000 : 5000);
}
window.addEventListener("popstate", () => {
  state.editing = false;
  renderRoute();
});
document.addEventListener("visibilitychange", () => {
  if (state.editing) return;
  if (isListRoute()) {
    scheduleListPoll();
    maybeListPoll();
  } else if (
    location.pathname.startsWith("/runs/") &&
    location.pathname !== "/runs/new" &&
    !state.editing
  )
    renderRoute();
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

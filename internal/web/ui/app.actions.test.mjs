import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { setImmediate } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { createContext, runInContext } from "node:vm";
import test from "node:test";

const sourcePath = fileURLToPath(new URL("app.ts", import.meta.url));
const source = readFileSync(sourcePath, "utf8")
  .replace(/^import .+;\r?\n/gm, "")
  .replace(/\nstart\(\);\s*$/, "\n");

// Run the real router, detail renderer, action handlers and API. Only the DOM,
// network and timer boundaries are simulated, so stale follow-up GETs are visible.
function harness() {
  let html = "",
    buttons = [],
    form = null,
    displayed = null,
    clock = 1000;
  const requests = [];
  const location = new URL("http://localhost/runs/A");
  const connection = {
    textContent: "",
    dataset: {},
    classList: { toggle() {} },
  };
  const button = (dataset, owner = null) => ({
    dataset,
    disabled: false,
    closest: () => owner,
  });
  const panel = {
    insertAdjacentHTML(_, value) {
      const alert = { textContent: "" };
      const dismiss = button({ dismiss: "" });
      form = {
        id: value.match(/id="([^"]+)"/)[1],
        innerHTML: value,
        dataset: {},
        alert,
        values: new Map([
          ["kind", "REJECT"],
          ["reviewer", "tester"],
          ["reason", "review reason"],
        ]),
        querySelector(selector) {
          if (selector === "[role=alert]") return alert;
          if (selector === "[data-dismiss]") return dismiss;
          if (selector === "[data-write]") return this.submit;
          return null;
        },
        remove() {
          form = null;
        },
      };
      form.submit = button({ write: "" }, form);
    },
  };
  const app = {
    dataset: {},
    get innerHTML() {
      return html;
    },
    set innerHTML(value) {
      html = value;
      form = null;
      buttons = [...value.matchAll(/data-action="([^"]+)"/g)].map((match) =>
        button({ action: match[1], write: "" }),
      );
    },
    contains: () => false,
    querySelectorAll: () => [],
  };
  const document = {
    hidden: false,
    activeElement: null,
    querySelector(selector) {
      if (selector === "#app") return app;
      if (selector === "#connection") return connection;
      if (selector.startsWith("#panel-")) return panel;
      if (selector === `#${form?.id}`) return form;
      return null;
    },
    querySelectorAll(selector) {
      if (selector === "[data-action]") return buttons;
      if (selector === "[data-write]")
        return form ? [...buttons, form.submit] : buttons;
      return [];
    },
    addEventListener() {},
  };
  const context = createContext({
    document,
    location,
    URL,
    AbortController,
    crypto: { randomUUID },
    Date: class extends Date {
      static now() {
        return ++clock;
      }
    },
    FormData: class {
      constructor(f) {
        this.get = (name) => f.values.get(name) ?? null;
      }
    },
    fetch(path, options) {
      return new Promise((resolve, reject) =>
        requests.push({
          path: path.slice(4),
          method: options.method,
          signal: options.signal,
          body: options.body && JSON.parse(options.body),
          resolve(data, status = 200) {
            resolve({
              ok: status < 400,
              status,
              headers: { get: () => "application/json" },
              json: async () => (status < 400 ? { data } : data),
            });
          },
          reject,
        }),
      );
    },
    window: { addEventListener() {} },
    history: {
      pushState(_, __, url) {
        location.href = new URL(url, location).href;
      },
    },
    setInterval: () => 1,
    clearInterval() {},
  });
  runInContext(source, context, { filename: sourcePath });
  const api = runInContext("({ state, nav })", context);
  return {
    state: api.state,
    app,
    requests,
    location,
    connection,
    form: () => form,
    navigate(id) {
      api.nav(`/runs/${id}`);
      const request = requests.at(-1);
      assert.equal(request.path, `/runs/${id}`);
      assert.equal(request.method, "GET");
      return request;
    },
    async show(id, version = "1", runState = "NEEDS_REVIEW") {
      displayed = detail(id, version, runState);
      this.navigate(id).resolve(displayed);
      await setImmediate();
      assert.match(html, new RegExp(`<h1>Task ${id}</h1>`));
    },
    click(action) {
      const node = buttons.find((b) => b.dataset.action === action);
      assert.ok(node, `missing ${action} button`);
      return node.onclick();
    },
    async start(action) {
      const clicked = this.click(action);
      const pending =
        action === "resume" ? clicked : form.onsubmit({ preventDefault() {} });
      if (action === "cancel" && requests.at(-1).method === "GET") {
        requests.at(-1).resolve(displayed);
        await setImmediate();
      }
      const request = requests.at(-1);
      assert.equal(request.method, "POST");
      assert.equal(request.path, `${location.pathname}/${action}`);
      assert.equal(
        request.signal,
        undefined,
        "navigation must not abort submitted writes",
      );
      return { pending, request, form };
    },
    snapshot() {
      return JSON.stringify({
        html,
        formHTML: form?.innerHTML,
        alert: form?.alert.textContent,
        editing: api.state.editing,
        online: api.state.online,
        lastSuccess: api.state.lastSuccess,
        connection: connection.textContent,
        disabled: [...buttons, ...(form ? [form.submit] : [])].map(
          (b) => b.disabled,
        ),
        eventRunId: api.state.eventRunId,
        lastDetail: api.state.lastDetail,
        requests: requests.length,
        location: location.href,
      });
    },
  };
}

function detail(id, version = "1", runState = "NEEDS_REVIEW") {
  return {
    run: {
      run_id: id,
      title: `Task ${id}`,
      version,
      state: runState,
      current_stage: "judge",
      workflow_revision: "v3",
    },
    available_actions: { resume: true, cancel: true, review: true },
    budget: { token_budget: true, max_llm_tokens: "100" },
  };
}
function finish(request, outcome) {
  if (outcome === "network failure") request.reject(new Error("offline"));
  else if (outcome === "conflict")
    request.resolve({ error: { message: "version conflict" } }, 409);
  else request.resolve({ decision_id: "decision-A", state: "PENDING" });
}

for (const action of ["resume", "cancel", "review"]) {
  for (const navigation of [
    "different task",
    "new task editor",
    "new detail still loading",
    "same task after leaving and returning",
  ]) {
    for (const outcome of ["success", "network failure", "conflict"]) {
      test(`${action}: late ${outcome} leaves UI untouched after ${navigation}`, async () => {
        const h = harness();
        await h.show("A");
        const write = await h.start(action);
        if (navigation === "new detail still loading") h.navigate("B");
        else {
          await h.show("B");
          if (navigation === "same task after leaving and returning")
            await h.show("A", "2");
          if (navigation === "new task editor") await h.click("review");
        }
        const before = h.snapshot();
        const oldFormHTML = write.form?.innerHTML;
        finish(write.request, outcome);
        await setImmediate();
        assert.equal(h.snapshot(), before);
        await write.pending;
        assert.equal(write.form?.innerHTML, oldFormHTML);
        assert.equal(write.form?.alert.textContent ?? "", "");
        const pending = h.state.pendingActions.get(write.request.path);
        if (outcome === "network failure") {
          assert.equal(pending.sending, false);
          assert.equal(
            pending.body.operation_key,
            write.request.body.operation_key,
          );
        } else assert.equal(pending, undefined);
      });
    }
  }

  test(`${action}: current success refreshes the same task`, async () => {
    const h = harness();
    await h.show("A");
    const write = await h.start(action);
    finish(write.request, "success");
    await setImmediate();
    const refresh = h.requests.at(-1);
    assert.equal(refresh.path, "/runs/A");
    assert.equal(refresh.method, "GET");
    assert.equal(h.state.editing, false);
    refresh.resolve(detail("A", "2"));
    await write.pending;
    await setImmediate();
    assert.match(h.app.innerHTML, /<h1>Task A<\/h1>/);
    assert.equal(h.state.pendingActions.size, 0);
    assert.equal(h.state.online, true);
  });

  for (const outcome of ["network failure", "conflict"]) {
    test(`${action}: current ${outcome} remains visible and preserves form input`, async () => {
      const h = harness();
      await h.show("A");
      const write = await h.start(action);
      finish(write.request, outcome);
      await write.pending;
      assert.equal(h.requests.length, action === "cancel" ? 3 : 2);
      assert.equal(h.state.online, outcome === "conflict");
      const error =
        action === "resume"
          ? h.connection.textContent
          : h.form().alert.textContent;
      assert.match(
        error,
        outcome === "conflict" ? /version conflict/ : /offline/,
      );
      if (action !== "resume") {
        assert.equal(h.state.editing, true);
        assert.equal(h.form(), write.form);
        assert.equal(h.form().values.get("reason"), "review reason");
      }
    });
  }

  test(`${action}: retry after navigation keeps an uncertain write's original identity and version`, async () => {
    const h = harness();
    await h.show("A");
    const original = await h.start(action);
    await h.show("B");
    // 5xx, like a transport failure, cannot prove whether a write was accepted.
    original.request.resolve(
      { error: { message: "temporary server failure" } },
      503,
    );
    await original.pending;
    await h.show("A", "2");
    const beforeRetry = h.requests.length;
    const retry = await h.start(action);
    assert.equal(h.requests.length, beforeRetry + 1);
    assert.deepEqual(retry.request.body, original.request.body);
    assert.equal(retry.request.body.expected_run_version, "1");
    retry.request.resolve({}, 409);
    await retry.pending;
  });
}

test("cancel: confirmation reads the heartbeat version and suppresses duplicate submits", async () => {
  const h = harness();
  await h.show("A", "3", "RUNNING");
  await h.click("cancel");
  const form = h.form();
  const pending = form.onsubmit({ preventDefault() {} });
  const refresh = h.requests.at(-1);
  assert.equal(refresh.method, "GET");
  assert.equal(refresh.path, "/runs/A");
  assert.equal(form.submit.disabled, true);
  await form.onsubmit({ preventDefault() {} });
  assert.equal(h.requests.length, 2);

  refresh.resolve(detail("A", "4", "RUNNING"));
  await setImmediate();
  const write = h.requests.at(-1);
  assert.equal(write.method, "POST");
  assert.equal(write.body.expected_run_version, "4");
  await form.onsubmit({ preventDefault() {} });
  assert.equal(h.requests.length, 3);
  write.resolve(
    { error: { code: "version_conflict", message: "version conflict" } },
    409,
  );
  await pending;
  assert.equal(form.submit.disabled, false);
});

test("cancel: confirming again after a version conflict reads a new version", async () => {
  const h = harness();
  await h.show("A", "3", "RUNNING");
  const original = await h.start("cancel");
  original.request.resolve(
    { error: { code: "version_conflict", message: "version conflict" } },
    409,
  );
  await original.pending;
  assert.equal(h.form(), original.form);
  assert.equal(h.state.pendingActions.size, 0);
  assert.match(h.form().alert.textContent, /再次确认/);

  const pending = h.form().onsubmit({ preventDefault() {} });
  const refresh = h.requests.at(-1);
  assert.equal(refresh.method, "GET");
  refresh.resolve(detail("A", "5", "RUNNING"));
  await setImmediate();
  const retry = h.requests.at(-1);
  assert.equal(retry.method, "POST");
  assert.equal(retry.body.expected_run_version, "5");
  assert.notEqual(
    retry.body.operation_key,
    original.request.body.operation_key,
  );
  retry.resolve({ cancel_requested: true });
  await pending;
  h.requests.at(-1).resolve(detail("A", "6", "CANCELLED"));
  await setImmediate();
  assert.equal(h.state.editing, false);
  assert.match(h.app.innerHTML, /state-cancelled/);
});

for (const runState of ["READY", "CANCELLED", "RUNNING"]) {
  test(`cancel: skips a new write when cancellation is unavailable in ${runState}`, async () => {
    const h = harness();
    await h.show("A", "3", "RUNNING");
    await h.click("cancel");
    const pending = h.form().onsubmit({ preventDefault() {} });
    const latest = {
      ...detail("A", "5", runState),
      available_actions: { cancel: false },
      ...(runState === "RUNNING" ? { pending_cancel: { id: "cancel-A" } } : {}),
    };
    h.requests.at(-1).resolve(latest);
    await setImmediate();
    assert.equal(h.requests.length, 3);
    assert.equal(h.requests.at(-1).method, "GET");
    h.requests.at(-1).resolve(latest);
    await pending;
    assert.equal(
      h.requests.some((request) => request.method === "POST"),
      false,
    );
    assert.equal(h.state.editing, false);
    assert.equal(h.form(), null);
  });
}

for (const outcome of ["success", "network failure", "conflict"]) {
  test(`cancel: navigation during the version refresh ignores its late ${outcome}`, async () => {
    const h = harness();
    await h.show("A", "3", "RUNNING");
    await h.click("cancel");
    const pending = h.form().onsubmit({ preventDefault() {} });
    const refresh = h.requests.at(-1);
    await h.show("B");
    const before = h.snapshot();
    if (outcome === "success") refresh.resolve(detail("A", "4", "RUNNING"));
    else finish(refresh, outcome);
    await pending;
    assert.equal(h.snapshot(), before);
    assert.equal(
      h.requests.some((request) => request.method === "POST"),
      false,
    );
  });
}

test("cancel: dismissing the confirmation during the version refresh prevents a write", async () => {
  const h = harness();
  await h.show("A", "3", "RUNNING");
  await h.click("cancel");
  const form = h.form();
  const pending = form.onsubmit({ preventDefault() {} });
  form.querySelector("[data-dismiss]").onclick();
  h.requests.at(-1).resolve(detail("A", "4", "RUNNING"));
  await pending;
  assert.equal(h.form(), null);
  assert.equal(h.state.editing, false);
  assert.equal(
    h.requests.some((request) => request.method === "POST"),
    false,
  );
});

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createContext, runInContext } from "node:vm";
import { fileURLToPath } from "node:url";
import { setImmediate } from "node:timers/promises";
import test from "node:test";

const sourcePath = fileURLToPath(new URL("app.ts", import.meta.url));
const source = readFileSync(sourcePath, "utf8")
  .replace(/^import .+;\r?\n/gm, "")
  .replace(/\nstart\(\);\s*$/, "\n");

// Drive the real API, list renderer, button handlers, route and timer listeners.
// Only the DOM surface they use and the network/timer boundaries are simulated.
function harness(path = "/runs") {
  let html = "",
    nodes = [],
    timerID = 0;
  const listeners = new Map(),
    intervals = new Map(),
    requests = [];
  const location = new URL(path, "http://localhost");
  const select = (selector) =>
    nodes.filter((node) => node.hasAttribute(selector.slice(1, -1)));
  const app = {
    dataset: {},
    get innerHTML() {
      return html;
    },
    set innerHTML(value) {
      html = value;
      nodes = [...value.matchAll(/<(?:button|a|div)\b([^>]*)>/g)].map(
        (match) => {
          const attrs = new Map(
            [...match[1].matchAll(/([\w-]+)(?:="([^"]*)")?/g)].map((a) => [
              a[1],
              a[2] ?? "",
            ]),
          );
          return {
            dataset: Object.fromEntries(
              [...attrs]
                .filter(([key]) => key.startsWith("data-"))
                .map(([key, value]) => [
                  key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase()),
                  value,
                ]),
            ),
            hasAttribute: (name) => attrs.has(name),
            getAttribute: (name) => attrs.get(name),
            focus() {
              document.activeElement = this;
            },
          };
        },
      );
    },
    contains: (node) => nodes.includes(node),
    querySelector: (selector) => select(selector)[0] || null,
    querySelectorAll: select,
  };
  const document = {
    hidden: false,
    activeElement: null,
    querySelector: (selector) => (selector === "#app" ? app : null),
    querySelectorAll: () => [],
    addEventListener: (name, fn) => listeners.set(name, fn),
  };
  const context = createContext({
    document,
    location,
    URL,
    fetch(path) {
      assert.ok(path.startsWith("/api/"));
      return new Promise((resolve, reject) =>
        requests.push({
          path: path.slice(4),
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
    window: { addEventListener: (name, fn) => listeners.set(name, fn) },
    history: {
      pushState(_, __, url) {
        location.href = new URL(url, location).href;
      },
    },
    setInterval(fn) {
      const id = ++timerID;
      intervals.set(id, fn);
      return id;
    },
    clearInterval: (id) => intervals.delete(id),
  });
  runInContext(source, context, { filename: sourcePath });
  const api = runInContext(
    "({ state, renderRoute, scheduleListPoll, stubDetail() { detailPage = async () => { app.innerHTML = 'detail'; }; } })",
    context,
  );
  api.stubDetail();
  api.scheduleListPoll();
  return {
    state: api.state,
    app,
    document,
    requests,
    open: api.renderRoute,
    node: (name) => app.querySelector(`[data-list-${name}]`),
    click(name) {
      const node = this.node(name);
      assert.ok(node, `missing ${name} button`);
      node.focus();
      return node.onclick();
    },
    filter(value) {
      const node = nodes.find((n) => n.dataset.filter === value);
      assert.ok(node);
      node.focus();
      return node.onclick();
    },
    async navigate(path) {
      location.href = new URL(path, location).href;
      return api.renderRoute();
    },
    back(path) {
      location.href = new URL(path, location).href;
      listeners.get("popstate")();
    },
    tick: () => intervals.get(api.state.listTimer)(),
    visibility(hidden) {
      document.hidden = hidden;
      listeners.get("visibilitychange")();
    },
    ids: () => Array.from(api.state.listRows, (row) => row.run_id),
    snapshot: () =>
      JSON.stringify({
        rows: api.state.listRows,
        cursor: api.state.listCursor,
        loading: api.state.listLoading,
        paused: api.state.listPaused,
        error: api.state.listError,
        online: api.state.online,
        lastSuccess: api.state.lastSuccess,
        html,
      }),
  };
}
const run = (id, state = "RUNNING") => ({ run_id: id, title: id, state });
const page = (runs, next_cursor = "") => ({ runs, next_cursor, limit: 50 });
async function seed(h, runs = [run("A")], cursor = "page-2") {
  const pending = h.open();
  h.requests.at(-1).resolve(page(runs, cursor));
  await pending;
}

test("all supported filters are sent to the server with a 50-row page", async () => {
  for (const filter of [
    "",
    "active",
    "ended",
    "blocked",
    "ready",
    "CREATED",
    "RUNNING",
    "NEEDS_REVIEW",
    "BLOCKED",
    "READY",
    "FAILED",
    "CANCELLED",
  ]) {
    const h = harness(`/runs${filter ? `?state=${filter}` : ""}`);
    await seed(h);
    assert.equal(
      h.requests[0].path,
      `/runs?limit=50${filter ? `&state=${filter}` : ""}`,
    );
    assert.deepEqual(h.ids(), ["A"]); // The server owns filtering; the UI does not truncate/filter again.
  }
});

test("loads successive cursor pages, deduplicates IDs, and focuses the terminal status", async () => {
  const h = harness();
  await seed(h, [run("A")], "older/+&=");
  const pending = h.click("more");
  assert.equal(h.requests[1].path, "/runs?limit=50&cursor=older%2F%2B%26%3D");
  assert.equal(h.node("more").getAttribute("aria-disabled"), "true");
  assert.equal(h.document.activeElement, h.node("more"));
  await h.click("more");
  assert.equal(h.requests.length, 2);
  h.requests[1].resolve(page([run("A"), run("B"), run("B")]));
  await pending;
  assert.deepEqual(h.ids(), ["A", "B"]);
  assert.equal(h.node("more"), null);
  assert.equal(h.document.activeElement, h.node("summary"));
  assert.match(h.app.innerHTML, /已加载 2 项 · 没有更多任务/);
});

test("an empty terminal page keeps earlier rows and stops pagination", async () => {
  const h = harness();
  await seed(h);
  const pending = h.click("more");
  h.requests[1].resolve(page([]));
  await pending;
  assert.deepEqual(h.ids(), ["A"]);
  assert.equal(h.state.listCursor, "");
  assert.equal(h.node("more"), null);
  assert.doesNotMatch(h.app.innerHTML, /尚无生成任务/);
});

test("initial failures retain filters and expose retry without claiming an empty list", async () => {
  const h = harness();
  const initial = h.open();
  h.requests[0].reject(new Error("offline"));
  await initial;
  assert.match(h.app.innerHTML, /role="alert".*offline/);
  assert.match(h.app.innerHTML, /重试刷新/);
  assert.doesNotMatch(h.app.innerHTML, /尚无生成任务/);
  const retry = h.click("refresh");
  h.requests[1].resolve(page([]));
  await retry;
  assert.match(h.app.innerHTML, /尚无生成任务/);
  assert.match(h.app.innerHTML, /没有更多任务/);
  assert.equal(h.document.activeElement, h.node("refresh"));
});

test("a failed next page preserves rows and cursor and can retry the same page", async () => {
  const h = harness();
  await seed(h);
  const pending = h.click("more");
  h.requests[1].reject(new Error("temporary failure"));
  await pending;
  assert.deepEqual(h.ids(), ["A"]);
  assert.equal(h.state.listCursor, "page-2");
  assert.equal(h.state.listPaused, true);
  assert.equal(h.node("more").getAttribute("aria-disabled"), "false");
  assert.match(h.app.innerHTML, /重试加载更多/);
  const retry = h.click("more");
  assert.equal(h.requests[2].path, h.requests[1].path);
  h.requests[2].resolve(page([run("B")]));
  await retry;
  assert.deepEqual(h.ids(), ["A", "B"]);
});

test("polling and visibility changes preserve pending and completed history until explicit refresh", async () => {
  const h = harness();
  await seed(h);
  const route = h.state.route;
  const more = h.click("more");
  await h.tick();
  h.visibility(true);
  h.visibility(false);
  assert.equal(h.requests.length, 2);
  assert.equal(h.state.route, route);
  h.requests[1].resolve(page([run("B")], "page-3"));
  await more;
  await h.tick();
  h.visibility(true);
  h.visibility(false);
  assert.equal(h.requests.length, 2);
  assert.deepEqual(h.ids(), ["A", "B"]);
  const refresh = h.click("refresh");
  h.requests[2].resolve(page([run("C")], "new-page-2"));
  await refresh;
  assert.deepEqual(h.ids(), ["C"]);
  assert.equal(h.state.listPaused, false);
  const poll = h.tick();
  assert.equal(h.requests[3].path, "/runs?limit=50");
  h.requests[3].resolve(page([run("D")]));
  await poll;
  assert.deepEqual(h.ids(), ["D"]);
  assert.equal(h.state.route, route);
});

for (const action of ["more", "refresh"]) {
  test(`explicit ${action} supersedes an in-flight poll and ignores its late response`, async () => {
    const h = harness();
    await seed(h);
    const poll = h.tick();
    assert.equal(h.state.listPolling, true);
    const pending = h.click(action);
    assert.equal(h.requests.length, 3);
    const before = h.snapshot();
    h.requests[1].resolve(page([run("stale")]));
    await poll;
    assert.equal(h.snapshot(), before);
    h.requests[2].resolve(page([run("B")]));
    await pending;
    assert.deepEqual(h.ids(), action === "more" ? ["A", "B"] : ["B"]);
  });
}

for (const outcome of ["success", "failure"]) {
  test(`filter changes reset rows and ignore a late page ${outcome}`, async () => {
    const h = harness();
    await seed(h);
    const more = h.click("more");
    const filtered = h.filter("ready");
    assert.deepEqual(h.ids(), []);
    assert.equal(h.state.listCursor, "");
    assert.equal(h.state.listPaused, false);
    assert.equal(h.requests[2].path, "/runs?limit=50&state=ready");
    h.requests[2].resolve(page([run("R", "READY")], "ready-page-2"));
    await filtered;
    const before = h.snapshot();
    if (outcome === "success") h.requests[1].resolve(page([run("stale")]));
    else h.requests[1].reject(new Error("stale failure"));
    await more;
    assert.equal(h.snapshot(), before);
    assert.equal(h.document.activeElement.dataset.filter, "ready");
  });
}

test("review queue paginates with its fixed filter, and leaving/returning resets list state", async () => {
  const h = harness("/reviews");
  await seed(h, [run("R", "NEEDS_REVIEW")]);
  assert.equal(h.requests[0].path, "/runs?limit=50&state=NEEDS_REVIEW");
  assert.match(h.app.innerHTML, /href="\/runs\/R\/review"/);
  const more = h.click("more");
  assert.equal(
    h.requests[1].path,
    "/runs?limit=50&state=NEEDS_REVIEW&cursor=page-2",
  );
  await h.navigate("/runs/A");
  h.back("/reviews");
  assert.deepEqual(h.ids(), []);
  assert.equal(h.state.listCursor, "");
  assert.equal(h.state.listPaused, false);
  const before = h.snapshot();
  h.requests[1].resolve(page([run("old-review", "NEEDS_REVIEW")]));
  await more;
  assert.equal(h.snapshot(), before);
  h.requests[2].resolve(page([run("new-review", "NEEDS_REVIEW")]));
  await setImmediate();
  assert.deepEqual(h.ids(), ["new-review"]);
});

test("leaving a list ignores late responses before another list opens", async () => {
  const h = harness();
  const pending = h.open();
  await h.navigate("/runs/A");
  const before = h.snapshot();
  h.requests[0].resolve(page([run("stale")]));
  await pending;
  assert.equal(h.snapshot(), before);
  assert.equal(h.app.innerHTML, "detail");
});

test("same-filter reset ignores the previous request even without a route/filter change", async () => {
  const h = harness();
  const first = h.open();
  const second = h.filter("");
  h.requests[1].resolve(page([run("new")]));
  await second;
  const before = h.snapshot();
  h.requests[0].resolve(page([run("stale")]));
  await first;
  assert.equal(h.snapshot(), before);
});

test("the trailing-slash list handles visibility and polling as a list route", async () => {
  const h = harness("/runs/");
  await seed(h);
  const route = h.state.route;
  const more = h.click("more");
  h.visibility(false);
  assert.equal(h.state.route, route);
  assert.equal(h.requests.length, 2);
  h.requests[1].resolve(page([run("B")]));
  await more;
  await h.tick();
  assert.deepEqual(h.ids(), ["A", "B"]);
  assert.equal(h.requests.length, 2);
});

for (const outcome of ["network failure", "HTTP failure", "success"]) {
  test(`a superseded poll's ${outcome} cannot change current connection state`, async () => {
    const h = harness();
    await seed(h);
    const poll = h.tick();
    const more = h.click("more");
    if (outcome === "HTTP failure") {
      // A stale HTTP response must not turn a newer network failure online.
      h.requests[2].reject(new TypeError("current network failure"));
    } else {
      h.requests[2].resolve(page([run("B")]));
    }
    await more;
    assert.equal(h.state.online, outcome !== "HTTP failure");
    assert.equal(h.state.listPaused, true);
    // Use a sentinel to detect stale timestamp writes even within one clock tick.
    h.state.lastSuccess = 123;
    const before = h.snapshot();
    if (outcome === "network failure") {
      h.requests[1].reject(new TypeError("stale network failure"));
    } else if (outcome === "HTTP failure") {
      h.requests[1].resolve({ error: { message: "stale HTTP failure" } }, 503);
    } else {
      h.requests[1].resolve(page([run("stale")]));
    }
    await poll;
    assert.equal(h.snapshot(), before);
  });
}

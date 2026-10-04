import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { setImmediate } from "node:timers/promises";
import { createContext, runInContext } from "node:vm";
import { fileURLToPath } from "node:url";
import test from "node:test";

const sourcePath = fileURLToPath(new URL("app.ts", import.meta.url));
const source = readFileSync(sourcePath, "utf8")
  .replace(/^import .+;\r?\n/gm, "")
  .replace(/\nstart\(\);\s*$/, "\n");

// Exercise the actual detail renderer, API, routes, and visibility listener.
// Fetch deliberately permits late responses after abort so route guards matter.
function harness() {
  let timerID = 0;
  const intervals = new Map(),
    listeners = new Map(),
    requests = [];
  const location = new URL("http://localhost/runs/A");
  const app = {
    dataset: {},
    innerHTML: "",
    contains: () => false,
    querySelectorAll: () => [],
  };
  const document = {
    hidden: false,
    querySelector: (selector) => (selector === "#app" ? app : null),
    querySelectorAll: () => [],
    addEventListener: (name, fn) => listeners.set(name, fn),
  };
  const context = createContext({
    document,
    location,
    URL,
    AbortController,
    window: { addEventListener: (name, fn) => listeners.set(name, fn) },
    setInterval(fn, delay) {
      const id = ++timerID;
      intervals.set(id, { fn, delay });
      return id;
    },
    clearInterval: (id) => intervals.delete(id),
    fetch(path, options) {
      assert.equal(options.method, "GET");
      return new Promise((resolve, reject) => {
        requests.push({
          path,
          signal: options.signal,
          resolve: (data) =>
            resolve({
              ok: true,
              headers: { get: () => "application/json" },
              json: async () => ({ data }),
            }),
          reject,
        });
      });
    },
  });
  runInContext(source, context, { filename: sourcePath });
  const api = runInContext("({ state, renderRoute })", context);
  return {
    state: api.state,
    app,
    intervals,
    requests,
    open: api.renderRoute,
    navigate(path) {
      location.href = new URL(path, location).href;
      return api.renderRoute();
    },
    tick() {
      for (const { fn } of [...intervals.values()]) fn();
    },
    visibility(hidden) {
      document.hidden = hidden;
      listeners.get("visibilitychange")();
    },
    async respond(data) {
      requests.at(-1).resolve(data);
      await setImmediate();
    },
  };
}
const detail = (state, version = "1", id = "A") => ({
  run: { run_id: id, title: id, state, version },
  execution: { active: false },
});
async function seed(h, state = "RUNNING") {
  const pending = h.open();
  await h.respond(detail(state));
  await pending;
  assert.match(h.app.innerHTML, /任务详情/);
}

for (const active of ["CREATED", "RUNNING"]) {
  for (const terminal of ["READY", "FAILED", "CANCELLED"]) {
    test(`${active} → ${terminal} stops periodic detail requests`, async () => {
      const h = harness();
      await seed(h, active);
      assert.equal(h.intervals.size, 1);
      assert.equal([...h.intervals.values()][0].delay, 2000);
      h.tick();
      assert.equal(h.requests.length, 2);
      await h.respond(detail(terminal, "2"));
      assert.match(
        h.app.innerHTML,
        new RegExp(`state-${terminal.toLowerCase()}`),
      );
      assert.equal(h.intervals.size, 0);
      for (let i = 0; i < 5; i++) h.tick();
      assert.equal(h.requests.length, 2);
    });
  }
}

test("unchanged active details keep exactly one polling timer", async () => {
  for (const active of ["CREATED", "RUNNING", "BLOCKED", "NEEDS_REVIEW"]) {
    const h = harness();
    await seed(h, active);
    const html = h.app.innerHTML;
    for (let i = 0; i < 3; i++) {
      h.tick();
      assert.equal(h.requests.length, i + 2);
      h.tick(); // An outstanding request suppresses overlapping polls.
      assert.equal(h.requests.length, i + 2);
      await h.respond(detail(active));
      assert.equal(h.app.innerHTML, html);
      assert.equal(h.intervals.size, 1);
    }
  }
});

test("a terminal response stops polling before the quiet signature shortcut", async () => {
  const h = harness();
  await seed(h);
  h.tick();
  // The observed state must govern polling even if the render signature matches.
  await h.respond(detail("READY"));
  assert.equal(h.intervals.size, 0);
  h.tick();
  assert.equal(h.requests.length, 2);
});

test("a failed poll retains its timer and retries before stopping at a terminal state", async () => {
  const h = harness();
  await seed(h);
  const html = h.app.innerHTML;
  h.tick();
  h.requests.at(-1).reject(new Error("temporary failure"));
  await setImmediate();
  assert.equal(h.app.innerHTML, html);
  assert.equal(h.state.online, false);
  assert.equal(h.intervals.size, 1);
  h.tick();
  assert.equal(h.requests.length, 3);
  await h.respond(detail("READY", "2"));
  assert.equal(h.state.online, true);
  assert.equal(h.intervals.size, 0);
});

test("visibility changes preserve hidden polling suppression and terminal timer cleanup", async () => {
  const h = harness();
  await seed(h);
  h.visibility(true);
  await h.respond(detail("RUNNING"));
  assert.equal(h.intervals.size, 1);
  assert.equal([...h.intervals.values()][0].delay, 15000);
  const count = h.requests.length;
  h.tick();
  assert.equal(h.requests.length, count);
  h.visibility(false);
  await h.respond(detail("RUNNING"));
  assert.equal(h.intervals.size, 1);
  assert.equal([...h.intervals.values()][0].delay, 2000);
  h.tick();
  await h.respond(detail("CANCELLED", "2"));
  for (const hidden of [true, false]) {
    h.visibility(hidden); // Visibility still explicitly refreshes the detail.
    await h.respond(detail("CANCELLED", "2"));
    assert.equal(h.intervals.size, 0);
  }
  const terminalCount = h.requests.length;
  h.tick();
  assert.equal(h.requests.length, terminalCount);
});

test("a late detail response cannot restart polling after navigation", async () => {
  const h = harness();
  await seed(h);
  h.tick();
  const stale = h.requests.at(-1);
  const pending = h.navigate("/runs/B");
  assert.equal(stale.signal.aborted, true);
  await h.respond(detail("READY", "1", "B"));
  await pending;
  const html = h.app.innerHTML;
  stale.resolve(detail("RUNNING", "2"));
  await setImmediate();
  assert.equal(h.app.innerHTML, html);
  assert.equal(h.intervals.size, 0);
  h.tick();
  assert.equal(h.requests.length, 3);
});

test("a queued callback from a previous route cannot restart its detail polling", async () => {
  const h = harness();
  await seed(h);
  const queued = [...h.intervals.values()][0].fn;
  const pending = h.navigate("/runs/B");
  await h.respond(detail("READY", "1", "B"));
  await pending;
  queued();
  assert.equal(h.requests.length, 2);
  assert.equal(h.intervals.size, 0);
});

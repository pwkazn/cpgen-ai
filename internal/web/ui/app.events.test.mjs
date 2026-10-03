import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createContext, runInContext } from "node:vm";
import { fileURLToPath } from "node:url";
import test from "node:test";

const sourcePath = fileURLToPath(new URL("app.ts", import.meta.url));
const source = readFileSync(sourcePath, "utf8")
  // These imports serve artifact rendering, outside this event pagination test.
  .replace(/^import .+;\r?\n/gm, "")
  // Prevent app startup; drive the registered event handler directly.
  .replace(/\nstart\(\);\s*$/, "\n");

function harness() {
  let currentButton;
  let currentPanel;
  const app = { innerHTML: "" };
  const document = {
    querySelector(selector) {
      if (selector === "#app") return app;
      if (selector === "[data-more-events]") return currentButton;
      if (selector === "#panel-events") return currentPanel;
      return null;
    },
    querySelectorAll: () => [],
    addEventListener() {},
  };
  const context = createContext({
    document,
    window: { addEventListener() {} },
    location: { href: "http://localhost/runs/A?tab=events" },
    URL,
  });
  runInContext(source, context, { filename: sourcePath });
  const api = runInContext(
    "({ state, attachDetailActions, setApi(fn) { api = fn; } })",
    context,
  );
  let resolveResponse, rejectResponse;
  let requestedPath;
  api.setApi((path) => {
    requestedPath = path;
    return new Promise((resolve, reject) => {
      resolveResponse = resolve;
      rejectResponse = reject;
    });
  });
  function show(id, cursor, events) {
    api.state.route++;
    api.state.eventRunId = id;
    api.state.eventCursor = cursor;
    api.state.loadedEvents = events;
    api.state.eventIds = new Set(events.map((event) => event.version));
    currentButton = { disabled: false, textContent: "加载更早记录" };
    currentPanel = { innerHTML: `events for ${id}` };
    api.attachDetailActions(id, { run: { run_id: id } });
  }
  return {
    state: api.state,
    show,
    load: () => currentButton.onclick(),
    resolve: (page) => resolveResponse(page),
    reject: (error) => rejectResponse(error),
    path: () => requestedPath,
    button: () => currentButton,
    panel: () => currentPanel.innerHTML,
    snapshot: () =>
      JSON.stringify({
        id: api.state.eventRunId,
        events: api.state.loadedEvents,
        eventIds: [...api.state.eventIds],
        cursor: api.state.eventCursor,
        panel: currentPanel.innerHTML,
      }),
  };
}
const event = (id, version) => ({
  run_id: id,
  version,
  type: `${id}-${version}`,
});

for (const navigation of [
  "different task",
  "route changed before new detail loaded",
  "same task after leaving and returning",
]) {
  test(`ignores a late event page after ${navigation}`, async () => {
    const h = harness();
    h.show("A", "100", [event("A", "100")]);
    const pending = h.load();
    assert.equal(h.path(), "/runs/A/events?before=100&limit=50");
    if (navigation === "different task")
      h.show("B", "200", [event("B", "200")]);
    else if (navigation === "route changed before new detail loaded")
      h.state.route++;
    else {
      h.show("B", "200", [event("B", "200")]);
      h.show("A", "110", [event("A", "110")]);
    }
    const before = h.snapshot();
    h.resolve({ events: [event("A", "99")], before_version: "99" });
    await pending;
    assert.equal(h.snapshot(), before);
  });
}

test("a stale empty page cannot mark the new task's event history as exhausted", async () => {
  const h = harness();
  h.show("A", "100", [event("A", "100")]);
  const pending = h.load();
  h.show("B", "200", [event("B", "200")]);
  const before = h.snapshot();
  h.resolve({ events: [], before_version: "0" });
  await pending;
  assert.equal(h.snapshot(), before);
  assert.equal(h.button().disabled, false);
});

test("a current page merges unique events in order and advances its cursor", async () => {
  const h = harness();
  h.show("A", "100", [event("A", "100")]);
  const pending = h.load();
  h.resolve({
    events: [event("A", "99"), event("A", "100"), event("A", "98")],
    before_version: "98",
  });
  await pending;
  assert.deepEqual(
    h.state.loadedEvents.map((e) => e.version),
    ["98", "99", "100"],
  );
  assert.equal(h.state.eventIds.size, 3);
  assert.equal(h.state.eventCursor, "98");
  assert.match(h.panel(), /A-98/);
  assert.equal(h.button().disabled, false);
});

test("a current empty page disables further pagination", async () => {
  const h = harness();
  h.show("A", "100", [event("A", "100")]);
  const pending = h.load();
  h.resolve({ events: [], before_version: "0" });
  await pending;
  assert.equal(h.state.eventCursor, "0");
  assert.equal(h.button().disabled, true);
  assert.equal(h.button().textContent, "没有更早事件");
});

test("a current failed page keeps history intact and permits retry", async () => {
  const h = harness();
  h.show("A", "100", [event("A", "100")]);
  const before = h.snapshot();
  const pending = h.load();
  h.reject(new Error("temporary failure"));
  await pending;
  assert.equal(h.snapshot(), before);
  assert.equal(h.button().disabled, false);
  assert.equal(h.button().textContent, "temporary failure");
});

test("a late failure leaves the new task and its pagination button untouched", async () => {
  const h = harness();
  h.show("A", "100", [event("A", "100")]);
  const pending = h.load();
  h.show("B", "200", [event("B", "200")]);
  const before = h.snapshot();
  h.reject(new Error("late failure"));
  await pending;
  assert.equal(h.snapshot(), before);
  assert.equal(h.button().disabled, false);
  assert.equal(h.button().textContent, "加载更早记录");
});

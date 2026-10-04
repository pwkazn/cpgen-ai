import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createContext, runInContext } from "node:vm";
import { fileURLToPath } from "node:url";
import test from "node:test";

const sourcePath = fileURLToPath(new URL("app.ts", import.meta.url));
const source = readFileSync(sourcePath, "utf8")
  .replace(/^import .+;\r?\n/gm, "")
  .replace(/\nstart\(\);\s*$/, "\n");

// Exercise the shipped form renderer, submit handler and HTTP serializer. Only
// DOM/FormData, network and the subsequent detail refresh are simulated.
function harness(current, targets) {
  let form,
    html = "";
  const requests = [],
    refreshed = [];
  const app = { innerHTML: "" };
  const alert = { textContent: "" },
    dismiss = {};
  const panel = {
    insertAdjacentHTML(_, value) {
      html = value;
      const selects = new Map(
        [
          ...value.matchAll(/<select name="([^"]+)"([^>]*)>(.*?)<\/select>/gs),
        ].map(([, name, attrs, contents]) => {
          const options = [
            ...contents.matchAll(
              /<option value="([^"]+)"([^>]*)>(.*?)<\/option>/gs,
            ),
          ].map(([, value, attrs, label]) => ({
            value,
            label,
            disabled: attrs.includes("disabled"),
            selected: attrs.includes("selected"),
          }));
          return [name, { disabled: attrs.includes("disabled"), options }];
        }),
      );
      form = {
        selects,
        fields: new Map([
          ["reviewer", "web-reviewer"],
          ...[...selects].map(([name, { options }]) => [
            name,
            (
              options.find((o) => o.selected) ||
              options.find((o) => !o.disabled)
            )?.value || "",
          ]),
        ]),
        querySelector: (selector) =>
          selector === "[role=alert]" ? alert : dismiss,
        remove() {
          form = null;
        },
      };
    },
  };
  const context = createContext({
    document: {
      querySelector: (selector) =>
        ({ "#app": app, "#panel-statement": panel, "#review-form": form })[
          selector
        ] || null,
      querySelectorAll: () => [],
      addEventListener() {},
    },
    window: { addEventListener() {} },
    location: { href: "http://localhost/runs/A/review" },
    URL,
    crypto: { randomUUID: () => "review-operation" },
    FormData: class {
      constructor(form) {
        this.fields = new Map(form.fields);
      }
      get(name) {
        return this.fields.get(name) ?? null;
      }
    },
    fetch: async (path, options) => {
      requests.push({ path, body: JSON.parse(options.body) });
      return {
        ok: true,
        status: 201,
        headers: { get: () => "application/json" },
        json: async () => ({
          data: { decision_id: "review_1", state: "PENDING" },
        }),
      };
    },
    refreshed,
  });
  runInContext(source, context, { filename: sourcePath });
  runInContext("detailPage = async (id) => { refreshed.push(id); };", context);
  const render = runInContext("renderReviewForm", context);
  render("A", {
    run: {
      current_stage: current,
      version: "9007199254740993",
      workflow_revision: "frozen-revision",
    },
    revision_targets: targets,
    budget: { limits: { split_token_budget: true } },
  });
  return {
    form,
    requests,
    refreshed,
    html,
    alert,
    submit: () => form.onsubmit({ preventDefault() {} }),
  };
}

test("similarity review offers upstream content and submits either selected producer", async () => {
  for (const target of ["idea", "statement"]) {
    const targets = ["idea", "statement", "similarity", "similarity_decision"];
    const h = harness("similarity_decision", targets);
    const choices = h.form.selects.get("revision_target_stage");
    assert.deepEqual(
      choices.options.map((o) => o.value),
      targets,
    );
    assert.equal(h.form.fields.get("revision_target_stage"), "idea");
    assert.match(h.html, /题目构思/);
    assert.match(h.html, /题面与样例/);
    h.form.fields.set("revision_target_stage", target);
    await h.submit();
    assert.equal(h.requests.length, 1);
    assert.equal(h.requests[0].path, "/api/runs/A/review");
    assert.equal(h.requests[0].body.revision_target_stage, target);
    assert.equal(h.requests[0].body.expected_run_version, "9007199254740993");
    assert.equal(h.requests[0].body.workflow_revision, "frozen-revision");
    assert.deepEqual(h.refreshed, ["A"]);
  }
});

test("the form honors server ordering and historical stages without inventing choices", () => {
  for (const [current, targets] of [
    ["judge", ["statement", "data", "judge"]],
    ["solution_decision", ["solution", "statement", "solution_decision"]],
    ["quality", ["solution", "quality"]],
    ["checkpoint", ["checkpoint", "prepare", "exercise"]],
    [
      "slice2_checkpoint",
      ["idea", "statement", "similarity", "slice2_checkpoint"],
    ],
  ]) {
    const h = harness(current, targets);
    assert.deepEqual(
      h.form.selects.get("revision_target_stage").options.map((o) => o.value),
      targets,
    );
    assert.equal(h.form.fields.get("revision_target_stage"), targets[0]);
  }
});

test("unavailable revision targets disable revise and never produce a fabricated target", async () => {
  for (const targets of [undefined, []]) {
    const h = harness("future_stage", targets);
    assert.equal(
      h.form.selects.get("kind").options.find((o) => o.value === "REVISE")
        .disabled,
      true,
    );
    assert.equal(h.form.selects.get("revision_target_stage").disabled, true);
    assert.equal(h.form.selects.get("revision_target_stage").options.length, 0);
    h.form.fields.set("kind", "REVISE");
    h.form.fields.set("revision_target_stage", "future_stage");
    await h.submit();
    assert.equal(h.requests.length, 0);
    assert.match(h.alert.textContent, /修订目标/);
  }
});

test("a target outside the server list cannot be submitted", async () => {
  const h = harness("similarity_decision", ["idea", "statement"]);
  h.form.fields.set("revision_target_stage", "solution");
  await h.submit();
  assert.equal(h.requests.length, 0);
  assert.match(h.alert.textContent, /修订目标/);
});

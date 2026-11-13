// build_bounds.cjs exercises generated numeric validators in an isolated copy
// of the page sources. Synthetic bounds cover number and maximum constraints
// that no current page result declares, without changing the owning design.
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const esbuild = require("esbuild");

const pageSource = path.resolve(__dirname, "..");
const root = path.resolve(pageSource, "../..");
const temporary = process.argv[2];
assert(temporary, "the owning Go test must supply its temporary folder");
const copiedPage = path.join(temporary, "ui/checkpoint");
fs.cpSync(pageSource, copiedPage, { recursive: true, filter: (source) => path.basename(source) !== "node_modules" });
fs.symlinkSync(path.join(pageSource, "node_modules"), path.join(copiedPage, "node_modules"), "dir");

const contract = JSON.parse(fs.readFileSync(path.join(root, "gen/http/openapi3.json"), "utf8"));
const probes = {
  integer_min: { type: "integer", minimum: 0 },
  integer_max: { type: "integer", maximum: 2 },
  integer_between: { type: "integer", minimum: -1, maximum: 1 },
  number_min: { type: "number", minimum: 0.25 },
  number_max: { type: "number", maximum: 0.75 },
  number_between: { type: "number", minimum: -0.5, maximum: 0.5 },
};
Object.assign(contract.components.schemas.TeamSnapshot.properties, probes);
contract.components.schemas.TeamSnapshot.required.push(...Object.keys(probes));
fs.mkdirSync(path.join(temporary, "gen/http"), { recursive: true });
fs.writeFileSync(path.join(temporary, "gen/http/openapi3.json"), JSON.stringify(contract));
execFileSync(process.execPath, [path.join(copiedPage, "build.mjs")], {
  cwd: temporary, stdio: ["ignore", "pipe", "pipe"], timeout: 30000, maxBuffer: 8 * 1024 * 1024,
});
const validatorsPath = path.join(copiedPage, "numeric_types.cjs");
const generated = fs.readFileSync(path.join(copiedPage, "src/types.ts"), "utf8");
fs.writeFileSync(validatorsPath, esbuild.transformSync(generated, { loader: "ts", format: "cjs", target: "node22" }).code);
const types = require(validatorsPath);

const base = { team: "bounds", roles: [], integer_min: 0, integer_max: 2, integer_between: 0,
  number_min: 0.25, number_max: 0.75, number_between: 0 };
// Values immediately outside, at and inside each inclusive boundary cover both
// one-sided bounds and the intersection of lower and upper bounds.
for (const [field, cases] of Object.entries({
  integer_min: [[-1, false], [0, true], [1, true], [0.5, false]],
  integer_max: [[1, true], [2, true], [3, false]],
  integer_between: [[-2, false], [-1, true], [0, true], [1, true], [2, false]],
  number_min: [[0, false], [0.25, true], [0.5, true]],
  number_max: [[0.5, true], [0.75, true], [1, false]],
  number_between: [[-0.75, false], [-0.5, true], [0, true], [0.5, true], [0.75, false]],
})) {
  for (const [value, expected] of [...cases, [NaN, false], [Infinity, false], [-Infinity, false], ["1", false]]) {
    assert.equal(types.ROUTES.get_team.decode({ ...base, [field]: value }), expected, field + "=" + value);
  }
}

const saved = { request_revision: "revision", started_at: "2026-11-13T00:00:00Z", in_force: true,
  required_test_count: 0, builder: { read: false }, test_writer: { read: false, report: 0, tests: { state: "assigned" } } };
for (const op of ["get_build", "start_build"]) {
  const answer = (build) => op === "get_build"
    ? { change_id: "chg", build: { state: "started", ...build } }
    : { build, team: { state: "window_busy", step: "Close the open dialog." }, working_copy: { state: "not_used" } };
  for (const value of [-1, 0, 1]) {
    assert.equal(types.ROUTES[op].decode(answer({ ...saved, required_test_count: value })), value >= 0, op + " count=" + value);
    assert.equal(types.ROUTES[op].decode(answer({ ...saved, test_writer: { ...saved.test_writer, report: value } })), value >= 0, op + " report=" + value);
  }
}
for (const count of [1, 2, 3]) {
  assert.equal(types.ROUTES.start_build.decode({ build: saved,
    team: { state: "ambiguous_space", count, step: "Close the duplicate spaces." }, working_copy: { state: "not_used" } }), count >= 2, "ambiguous count=" + count);
}
// Malformed numeric constraints fail generation rather than becoming a silent
// omission or a JavaScript comparison against a coerced string.
for (const bound of ["minimum", "maximum"]) {
  const malformed = structuredClone(contract);
  malformed.components.schemas.TeamSnapshot.properties.number_between[bound] = "0";
  fs.writeFileSync(path.join(temporary, "gen/http/openapi3.json"), JSON.stringify(malformed));
  assert.throws(() => execFileSync(process.execPath, [path.join(copiedPage, "build.mjs")], {
    cwd: temporary, stdio: ["ignore", "pipe", "pipe"], timeout: 30000, maxBuffer: 8 * 1024 * 1024,
  }), (error) => error.status !== 0 && String(error.stderr).includes("Invalid numeric " + bound));
}
console.log("NUMERIC-BOUNDS-PASSED");

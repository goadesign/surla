// build.mjs derives ui/checkpoint/src/types.ts from the Goa-generated tool
// result schemas exported by internal/controlplane/export_schemas_gen.go,
// compiles the Surla Checkpoint Studio React + OpenUI + shadcn/ui application
// into internal/controlplane/checkpoint.html, and exports the OpenUI catalog
// prompt to internal/goldenpath/decision_catalog.md.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import * as esbuild from "esbuild";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(__dirname, "../..");
const checkOnly = process.argv.includes("--check");

const typesTsPath = path.join(__dirname, "src/types.ts");
const checkpointHtmlPath = path.join(repoRoot, "internal/controlplane/checkpoint.html");
const decisionCatalogPath = path.join(repoRoot, "internal/goldenpath/decision_catalog.md");
const openApiPath = path.join(repoRoot, "gen/http/openapi3.json");

const SUPPORTED_SCHEMA_KEYS = new Set([
  "type",
  "$ref",
  "enum",
  "required",
  "additionalProperties",
  "items",
  "properties",
  "$defs",
  "oneOf",
]);

const IGNORED_ANNOTATION_KEYS = new Set([
  "$schema",
  "title",
  "description",
  "example",
  "examples",
  "default",
]);

const SUPPORTED_TYPES = new Set([
  "string",
  "boolean",
  "integer",
  "number",
  "array",
  "object",
]);

// compactSchema strips non-constraining annotation keys while keeping every
// supported structural JSON Schema constraint and failing the build if any
// unsupported keyword, type, or format constraint appears in a generated schema.
function compactSchema(node, schemaPath = "$") {
  if (!node || typeof node !== "object" || Array.isArray(node)) {
    throw new Error(`Invalid JSON Schema node at ${schemaPath}`);
  }
  for (const key of Object.keys(node)) {
    if (key === "format") {
      if (
        !(node.type === "integer" && node.format === "int64") &&
        !(node.type === "string" && node.format === "date-time")
      ) {
        throw new Error(
          `Unsupported JSON Schema format ${JSON.stringify(node.format)} on type ${JSON.stringify(
            node.type
          )} at ${schemaPath}`
        );
      }
      continue;
    }
    if (!SUPPORTED_SCHEMA_KEYS.has(key) && !IGNORED_ANNOTATION_KEYS.has(key)) {
      throw new Error(`Unsupported JSON Schema keyword ${JSON.stringify(key)} at ${schemaPath}`);
    }
  }
  const out = {};
  if (node.type !== undefined) {
    if (typeof node.type !== "string" || !SUPPORTED_TYPES.has(node.type)) {
      throw new Error(
        `Unsupported JSON Schema type ${JSON.stringify(node.type)} at ${schemaPath}`
      );
    }
    out.type = node.type;
  }
  if (node.$ref !== undefined) {
    if (typeof node.$ref !== "string" || !node.$ref.startsWith("#/$defs/")) {
      throw new Error(
        `Unsupported JSON Schema $ref ${JSON.stringify(node.$ref)} at ${schemaPath}`
      );
    }
    out.$ref = node.$ref;
  }
  if (node.enum !== undefined) {
    if (!Array.isArray(node.enum) || !node.enum.every((v) => typeof v === "string")) {
      throw new Error(`Unsupported non-string enum at ${schemaPath}`);
    }
    out.enum = node.enum;
  }
  if (node.format === "date-time") {
    out.format = node.format;
  }
  if (node.oneOf !== undefined) {
    if (!Array.isArray(node.oneOf) || node.oneOf.length === 0) {
      throw new Error(`Invalid oneOf array at ${schemaPath}`);
    }
    out.oneOf = node.oneOf.map((branch, index) =>
      compactSchema(branch, `${schemaPath}.oneOf[${index}]`)
    );
  }
  if (node.required !== undefined) {
    if (!Array.isArray(node.required) || !node.required.every((v) => typeof v === "string")) {
      throw new Error(`Invalid required array at ${schemaPath}`);
    }
    out.required = node.required;
  }
  if (node.additionalProperties !== undefined) {
    if (typeof node.additionalProperties === "boolean") {
      out.additionalProperties = node.additionalProperties;
    } else if (
      node.additionalProperties &&
      typeof node.additionalProperties === "object" &&
      !Array.isArray(node.additionalProperties)
    ) {
      out.additionalProperties = compactSchema(
        node.additionalProperties,
        `${schemaPath}.additionalProperties`
      );
    } else {
      throw new Error(`Unsupported additionalProperties at ${schemaPath}`);
    }
  }
  if (node.items !== undefined) {
    out.items = compactSchema(node.items, `${schemaPath}.items`);
  }
  if (node.properties !== undefined) {
    if (typeof node.properties !== "object" || Array.isArray(node.properties)) {
      throw new Error(`Invalid properties map at ${schemaPath}`);
    }
    out.properties = {};
    for (const [k, v] of Object.entries(node.properties)) {
      out.properties[k] = compactSchema(v, `${schemaPath}.properties.${k}`);
    }
  }
  if (node.$defs !== undefined) {
    if (typeof node.$defs !== "object" || Array.isArray(node.$defs)) {
      throw new Error(`Invalid $defs map at ${schemaPath}`);
    }
    out.$defs = {};
    for (const [k, v] of Object.entries(node.$defs)) {
      out.$defs[k] = compactSchema(v, `${schemaPath}.$defs.${k}`);
    }
  }
  return out;
}

// Verify at build startup that compactSchema rejects unsupported assertions.
try {
  compactSchema({ type: "string", minLength: 1 }, "$.selfCheck");
  throw new Error("compactSchema failed to reject unsupported keyword minLength");
} catch (err) {
  if (!String(err.message || err).includes('Unsupported JSON Schema keyword "minLength"')) {
    throw err;
  }
}

// tsTypeForProperty converts a JSON Schema property node into its TypeScript
// type expression.
function tsTypeForProperty(prop) {
  if (Array.isArray(prop.oneOf)) {
    return `(${prop.oneOf.map(tsTypeForProperty).join(" | ")})`;
  }
  if (typeof prop.$ref === "string") {
    return prop.$ref.replace(/^#\/\$defs\//, "");
  }
  if (prop.type === "string") {
    if (Array.isArray(prop.enum) && prop.enum.length > 0) {
      return prop.enum.map((v) => JSON.stringify(v)).join(" | ");
    }
    return "string";
  }
  if (prop.type === "boolean") {
    return "boolean";
  }
  if (prop.type === "integer" || prop.type === "number") {
    return "number";
  }
  if (prop.type === "array") {
    return prop.items ? `${tsTypeForProperty(prop.items)}[]` : "unknown[]";
  }
  if (prop.type === "object") {
    if (prop.properties) {
      const required = new Set(prop.required || []);
      const fields = Object.entries(prop.properties).map(([name, value]) =>
        `${JSON.stringify(name)}${required.has(name) ? "" : "?"}: ${tsTypeForProperty(value)}`
      );
      return `{ ${fields.join("; ")} }`;
    }
    if (prop.additionalProperties && typeof prop.additionalProperties === "object") {
      return `Record<string, ${tsTypeForProperty(prop.additionalProperties)}>`;
    }
    return "Record<string, unknown>";
  }
  return "unknown";
}

// emitInterface renders a single TypeScript interface from a JSON Schema object.
function emitInterface(name, schema) {
  const req = new Set(Array.isArray(schema.required) ? schema.required : []);
  const props = schema.properties || {};
  const lines = [`export interface ${name} {`];
  for (const [propName, propSchema] of Object.entries(props)) {
    const optional = req.has(propName) ? "" : "?";
    lines.push(`  ${propName}${optional}: ${tsTypeForProperty(propSchema)};`);
  }
  lines.push("}");
  return lines.join("\n");
}

// generateTypesModule invokes internal/controlplane/export_schemas_gen.go to
// obtain the generated ResultSchemas (via ToolSchemas() and ToolSchema(name))
// and MCP OutputSchemas (via NewMCPAdapter(nil, nil).ToolsList), verifies
// parity against gen/http/openapi3.json, and generates ui/checkpoint/src/types.ts.
function generateTypesModule() {
  const exportedRaw = execFileSync(
    "go",
    ["run", "./internal/controlplane/export_schemas_gen.go"],
    { cwd: repoRoot, encoding: "utf8" }
  );
  const exported = JSON.parse(exportedRaw);
  const openApi = JSON.parse(fs.readFileSync(openApiPath, "utf8"));

  const rawByTool = exported.registry || {};
  const mcpByTool = exported.mcp || {};
  for (const [tool, regSchema] of Object.entries(rawByTool)) {
    const mcpName = tool.replace(/^surla\./, "");
    const mcpSchema = mcpByTool[mcpName];
    if (!mcpSchema) {
      throw new Error(`Missing MCP OutputSchema for ${mcpName} from ToolsList`);
    }
    const oaType = mcpSchema.title;
    const oaSchema = openApi.components?.schemas?.[oaType];
    if (
      !oaSchema ||
      JSON.stringify(regSchema.properties) !== JSON.stringify(mcpSchema.properties) ||
      JSON.stringify(regSchema.required) !== JSON.stringify(mcpSchema.required) ||
      JSON.stringify(regSchema.required) !== JSON.stringify(oaSchema.required)
    ) {
      throw new Error(`Schema parity mismatch for ${tool} (${oaType})`);
    }
  }

  if (
    JSON.stringify(rawByTool["surla.propose_change"]) !==
      JSON.stringify(rawByTool["surla.revise_change"]) ||
    JSON.stringify(rawByTool["surla.propose_change"]) !==
      JSON.stringify(rawByTool["surla.confirm_intent"]) ||
    JSON.stringify(rawByTool["surla.propose_change"]) !==
      JSON.stringify(rawByTool["surla.get_change"]) ||
    JSON.stringify(rawByTool["surla.propose_change"]) !==
      JSON.stringify(rawByTool["surla.approve_merge"])
  ) {
    throw new Error(
      "ChangeCardResult tool schemas (propose_change, revise_change, confirm_intent, get_change, approve_merge) diverged"
    );
  }
  if (
    JSON.stringify(rawByTool["surla.verify_change"]) !==
    JSON.stringify(rawByTool["surla.get_verification"])
  ) {
    throw new Error("VerificationStatusResult tool schemas diverged");
  }

  const rootSchemas = {
    ChangeCardResult: compactSchema(
      rawByTool["surla.propose_change"],
      "ChangeCardResult"
    ),
    ArchitecturePreviewResult: compactSchema(
      rawByTool["surla.preview_architecture"],
      "ArchitecturePreviewResult"
    ),
    DiffResult: compactSchema(rawByTool["surla.diff"], "DiffResult"),
    DraftOverrideResult: compactSchema(
      rawByTool["surla.draft_override"],
      "DraftOverrideResult"
    ),
    VerificationStatusResult: compactSchema(
      rawByTool["surla.get_verification"],
      "VerificationStatusResult"
    ),
    VerificationListResult: compactSchema(
      rawByTool["surla.list_verifications"],
      "VerificationListResult"
    ),
    LiveTargetResult: compactSchema(
      rawByTool["surla.start_live_target"],
      "LiveTargetResult"
    ),
    StopLiveTargetResult: compactSchema(
      rawByTool["surla.stop_live_target"],
      "StopLiveTargetResult"
    ),
  };

  const emittedDefs = new Set();
  const interfaceBlocks = [];
  for (const [rootName, rootSchema] of Object.entries(rootSchemas)) {
    for (const [defName, defSchema] of Object.entries(rootSchema.$defs || {})) {
      if (!emittedDefs.has(defName)) {
        emittedDefs.add(defName);
        interfaceBlocks.push(emitInterface(defName, defSchema));
      }
    }
    interfaceBlocks.push(emitInterface(rootName, rootSchema));
  }

  return `// Code generated by ui/checkpoint/build.mjs from gen/surla/toolsets/surla; DO NOT EDIT.
//
// Package types declares the generated MCP tool result interfaces and
// JSON-Schema-driven runtime validators for Surla Checkpoint Studio.

import { z } from "zod";

${interfaceBlocks.join("\n\n")}

interface JSONSchemaNode {
  type?: string;
  $ref?: string;
  enum?: string[];
  format?: "date-time";
  oneOf?: JSONSchemaNode[];
  required?: string[];
  additionalProperties?: boolean | JSONSchemaNode;
  items?: JSONSchemaNode;
  properties?: Record<string, JSONSchemaNode>;
  $defs?: Record<string, JSONSchemaNode>;
}

const GENERATED_RESULT_SCHEMAS: Record<string, JSONSchemaNode> = ${JSON.stringify(
    rootSchemas,
    null,
    2
  )};

// dateTimeSchema checks the RFC3339 date/time values produced by Goa, including
// calendar dates and explicit seconds and timezone. Date.parse also checks
// numeric timezone offsets before the value is accepted.
const dateTimeSchema = z.string().datetime({ offset: true }).regex(
  /T\\d{2}:\\d{2}:\\d{2}(?:\\.\\d+)?(?:Z|[+-]\\d{2}:\\d{2})$/
);

// isPlainObject reports whether v is a non-null, non-array JavaScript object.
export function isPlainObject(v: unknown): v is Record<string, unknown> {
  return Boolean(v) && typeof v === "object" && !Array.isArray(v);
}

// matchesJSONSchema validates an unknown runtime value against a generated
// Draft 2020-12 JSON Schema node and its root $defs dictionary.
function matchesJSONSchema(
  value: unknown,
  schema: JSONSchemaNode,
  defs: Record<string, JSONSchemaNode>
): boolean {
  if (schema.oneOf) {
    let matches = 0;
    for (const branch of schema.oneOf) {
      if (matchesJSONSchema(value, branch, defs)) {
        matches++;
      }
    }
    if (matches !== 1) {
      return false;
    }
  }
  if (typeof schema.$ref === "string") {
    const defName = schema.$ref.replace(/^#\\/\\$defs\\//, "");
    const target = defs[defName];
    return Boolean(target) && matchesJSONSchema(value, target, defs);
  }
  if (schema.type === "string") {
    if (typeof value !== "string") {
      return false;
    }
    if (
      schema.format === "date-time" &&
      (!dateTimeSchema.safeParse(value).success || !Number.isFinite(Date.parse(value)))
    ) {
      return false;
    }
    return !Array.isArray(schema.enum) || schema.enum.includes(value);
  }
  if (schema.type === "boolean") {
    return typeof value === "boolean";
  }
  if (schema.type === "integer" || schema.type === "number") {
    return (
      typeof value === "number" &&
      Number.isFinite(value) &&
      (schema.type !== "integer" || Number.isInteger(value))
    );
  }
  if (schema.type === "array") {
    if (!Array.isArray(value)) {
      return false;
    }
    return !schema.items || value.every((item) => matchesJSONSchema(item, schema.items!, defs));
  }
  if (schema.type === "object") {
    if (!isPlainObject(value)) {
      return false;
    }
    const props = schema.properties || {};
    const req = schema.required || [];
    for (let i = 0; i < req.length; i++) {
      if (value[req[i]] === undefined) {
        return false;
      }
    }
    for (const key of Object.keys(value)) {
      const propVal = value[key];
      if (propVal === undefined) {
        continue;
      }
      if (Object.prototype.hasOwnProperty.call(props, key)) {
        if (!matchesJSONSchema(propVal, props[key], defs)) {
          return false;
        }
      } else if (schema.additionalProperties === false) {
        return false;
      } else if (
        schema.additionalProperties &&
        typeof schema.additionalProperties === "object"
      ) {
        if (!matchesJSONSchema(propVal, schema.additionalProperties, defs)) {
          return false;
        }
      }
    }
    return true;
  }
  return Boolean(schema.oneOf);
}

function validateGeneratedRoot(name: string, value: unknown): boolean {
  const schema = GENERATED_RESULT_SCHEMAS[name];
  return Boolean(schema) && matchesJSONSchema(value, schema, schema.$defs || {});
}

// isChangeCardResult validates that a payload conforms to the generated ChangeCardResult schema.
export function isChangeCardResult(p: unknown): p is ChangeCardResult {
  return validateGeneratedRoot("ChangeCardResult", p);
}

// isArchitecturePreviewResult validates that a payload conforms to the generated ArchitecturePreviewResult schema.
export function isArchitecturePreviewResult(p: unknown): p is ArchitecturePreviewResult {
  return validateGeneratedRoot("ArchitecturePreviewResult", p);
}

// isDiffResult validates that a payload conforms to the generated DiffResult schema.
export function isDiffResult(p: unknown): p is DiffResult {
  return validateGeneratedRoot("DiffResult", p);
}

// isDraftOverrideResult validates that a payload conforms to the generated DraftOverrideResult schema.
export function isDraftOverrideResult(p: unknown): p is DraftOverrideResult {
  return validateGeneratedRoot("DraftOverrideResult", p);
}

// isVerificationStatusResult validates the generated CP4 state and report.
export function isVerificationStatusResult(p: unknown): p is VerificationStatusResult {
  return validateGeneratedRoot("VerificationStatusResult", p);
}

// isVerificationListResult validates the generated CP4 saved-run list.
export function isVerificationListResult(p: unknown): p is VerificationListResult {
  return validateGeneratedRoot("VerificationListResult", p);
}

// isLiveTargetResult validates a start_live_target result.
export function isLiveTargetResult(p: unknown): p is LiveTargetResult {
  return validateGeneratedRoot("LiveTargetResult", p);
}

// isStopLiveTargetResult validates a stop_live_target result.
export function isStopLiveTargetResult(p: unknown): p is StopLiveTargetResult {
  return validateGeneratedRoot("StopLiveTargetResult", p);
}
`;
}

const stubCiInfoPlugin = {
  name: "stub-ci-info",
  setup(build) {
    build.onResolve({ filter: /^ci-info$/ }, () => ({
      path: "ci-info",
      namespace: "stub-ci-info",
    }));
    build.onLoad({ filter: /.*/, namespace: "stub-ci-info" }, () => ({
      contents: "export const id = null; export const isCI = false;",
      loader: "js",
    }));
  },
};

async function buildArtifacts() {
  const generatedTypesTs = generateTypesModule();
  if (!checkOnly) {
    fs.writeFileSync(typesTsPath, generatedTypesTs, "utf8");
  }

  const stylesCss = fs.readFileSync(path.join(__dirname, "src/styles.css"), "utf8").trim();

  const appBundle = await esbuild.build({
    entryPoints: [path.join(__dirname, "src/app.tsx")],
    bundle: true,
    write: false,
    platform: "browser",
    format: "iife",
    target: ["es2020"],
    minify: true,
    legalComments: "none",
    define: {
      "process.env.NODE_ENV": '"production"',
    },
    plugins: [stubCiInfoPlugin],
  });

  const rawJs = appBundle.outputFiles[0].text.trim();
  // Escape any "</script" sequence inside string literals so the inline <script>
  // tag in checkpoint.html is never prematurely closed by the HTML parser.
  const safeJs = rawJs.replace(/<\/script/gi, "<\\/script");

  const html = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline';" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Surla Checkpoint Studio</title>
  <style>
${stylesCss}
  </style>
</head>
<body>
  <main id="surla-checkpoint-root" data-profile="mcp-app" data-state="waiting">
    <header>
      <h1>Surla Checkpoint Studio</h1>
      <p>Review and confirm domain decisions, concrete examples, architecture previews, prompt updates, and verification results for your change.</p>
      <details class="surla-details">
        <summary>Details</summary>
        <p><small>Submitting Checkpoint 1 edits dispatches <code>confirm_intent</code> through the MCP Apps host; server authentication and <code>request_revision</code> checks remain authoritative.</small></p>
      </details>
    </header>
    <p id="surla-checkpoint-status" role="status" aria-live="polite">Connecting to checkpoint studio...</p>
    <nav id="surla-checkpoint-nav" aria-label="Checkpoint views" hidden></nav>
    <section id="surla-checkpoint-view" aria-label="Checkpoint Payload"></section>
  </main>
  <script>
  /* MCP Apps bridge origin guard: event.source !== window.parent */
  ${safeJs}
  </script>
</body>
</html>
`;

  const catalogBundle = await esbuild.build({
    entryPoints: [path.join(__dirname, "src/openui/library.tsx")],
    bundle: true,
    write: false,
    platform: "node",
    format: "cjs",
    target: ["node20"],
    define: {
      "process.env.NODE_ENV": '"production"',
    },
    plugins: [stubCiInfoPlugin],
  });

  const cjsCode = catalogBundle.outputFiles[0].text;
  const req = createRequire(import.meta.url);
  const mod = { exports: {} };
  const wrapper = vm.runInNewContext(
    "(function(exports, require, module, __filename, __dirname) { " + cjsCode + "\n})",
    {
      console,
      process: { env: { NODE_ENV: "production", OPENUI_TELEMETRY_DISABLED: "1" } },
      Buffer,
      TextEncoder,
      TextDecoder,
      URL,
      Set,
      Map,
      WeakMap,
      WeakSet,
      Symbol,
      Promise,
      Object,
      Array,
      String,
      Number,
      Boolean,
      RegExp,
      Error,
      TypeError,
      Math,
      Date,
      JSON,
      setTimeout,
      clearTimeout,
      setInterval,
      clearInterval,
    }
  );
  wrapper(mod.exports, req, mod, "library.cjs", __dirname);
  const catalogMarkdown = mod.exports.generateDecisionCatalogMarkdown().trim() + "\n";

  if (checkOnly) {
    const existingTypes = fs.existsSync(typesTsPath)
      ? fs.readFileSync(typesTsPath, "utf8")
      : "";
    const existingHtml = fs.existsSync(checkpointHtmlPath)
      ? fs.readFileSync(checkpointHtmlPath, "utf8")
      : "";
    const existingCatalog = fs.existsSync(decisionCatalogPath)
      ? fs.readFileSync(decisionCatalogPath, "utf8")
      : "";
    if (existingTypes !== generatedTypesTs) {
      console.error("ERROR: ui/checkpoint/src/types.ts is out of date with gen/surla/toolsets/surla.");
      process.exit(1);
    }
    if (existingHtml !== html) {
      console.error("ERROR: internal/controlplane/checkpoint.html is out of date with ui/checkpoint/src.");
      process.exit(1);
    }
    if (existingCatalog !== catalogMarkdown) {
      console.error("ERROR: internal/goldenpath/decision_catalog.md is out of date with ui/checkpoint/src/openui/library.tsx.");
      process.exit(1);
    }
    console.log("OK: types.ts, checkpoint.html, and decision_catalog.md are up to date.");
    return;
  }

  fs.mkdirSync(path.dirname(checkpointHtmlPath), { recursive: true });
  fs.mkdirSync(path.dirname(decisionCatalogPath), { recursive: true });
  fs.writeFileSync(checkpointHtmlPath, html, "utf8");
  fs.writeFileSync(decisionCatalogPath, catalogMarkdown, "utf8");
  console.log(
    `Built types.ts (${Buffer.byteLength(generatedTypesTs)} bytes), checkpoint.html (${Buffer.byteLength(html)} bytes), and decision_catalog.md (${Buffer.byteLength(catalogMarkdown)} bytes).`
  );
}

buildArtifacts().catch((err) => {
  console.error(err);
  process.exit(1);
});

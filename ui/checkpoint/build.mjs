// build.mjs derives ui/checkpoint/src/types.ts from the Goa-generated HTTP
// contract (gen/http/openapi3.json): the request types, route table and
// result types and validators of exactly the operations the views page
// calls. It then compiles the views page (React + OpenUI + shadcn/ui) into
// internal/controlplane/checkpoint.html, which the Surla server serves at
// /views, writes the SHA-256 digest of the page's one inline script to
// internal/controlplane/views_page_script_gen.go for the page's security
// policy, and exports the OpenUI catalog prompt to
// internal/goldenpath/decision_catalog.md.

import crypto from "node:crypto";
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
const scriptHashPath = path.join(repoRoot, "internal/controlplane/views_page_script_gen.go");
const decisionCatalogPath = path.join(repoRoot, "internal/goldenpath/decision_catalog.md");
const openApiPath = path.join(repoRoot, "gen/http/openapi3.json");

// PAGE_OPERATIONS lists, by OpenAPI operationId (service#method), every HTTP
// operation the views page sends. Only these get a generated request type,
// route and result validator; the page cannot address anything else.
const PAGE_OPERATIONS = [
  "project#exchange_browser_link",
  "surla#list_changes",
  "surla#wait_for_changes",
  "surla#get_change",
  "surla#get_story_refresh",
  "surla#confirm_intent",
  "surla#decline_revision",
  "surla#approve_merge",
  "surla#preview_architecture",
  "surla#list_verifications",
  "surla#get_verification",
  "surla#list_traces",
];

// ERROR_SCHEMA is Goa's error response body, which every failed operation
// returns; the page shows its message.
const ERROR_SCHEMA = "Error";

// TS_NAMES renames component schemas whose OpenAPI name would shadow a
// JavaScript global in the generated module.
const TS_NAMES = { Error: "ServerError" };

// STRUCTURAL_KEYS are the JSON Schema keywords that decide a value's shape.
const STRUCTURAL_KEYS = new Set([
  "type",
  "$ref",
  "enum",
  "required",
  "additionalProperties",
  "items",
  "properties",
  "anyOf",
  "format",
]);

// IGNORED_KEYS are annotations, plus value constraints (lengths, ranges,
// patterns) that the server enforces and that do not change a value's
// TypeScript shape. Any other keyword fails the build rather than being
// silently dropped.
const IGNORED_KEYS = new Set([
  "title",
  "description",
  "example",
  "examples",
  "default",
  "minLength",
  "maxLength",
  "minItems",
  "maxItems",
  "minimum",
  "maximum",
  "pattern",
]);

const SUPPORTED_TYPES = new Set(["string", "boolean", "integer", "number", "array", "object"]);

// tsName returns the generated TypeScript name of a component schema.
function tsName(component) {
  return TS_NAMES[component] || component;
}

// refName returns the component name an OpenAPI $ref points at.
function refName(ref, at) {
  const prefix = "#/components/schemas/";
  if (typeof ref !== "string" || !ref.startsWith(prefix)) {
    throw new Error(`Unsupported $ref ${JSON.stringify(ref)} at ${at}`);
  }
  return ref.slice(prefix.length);
}

// compactSchema keeps the shape-deciding keywords of one OpenAPI schema node,
// rewrites component references to the generated module's $defs, and fails
// on any keyword or type it cannot represent. Goa encodes exactly the
// declared attributes of a result, so an object with declared properties is
// closed: undeclared fields in a response fail its validator.
function compactSchema(node, at, refs) {
  if (!node || typeof node !== "object" || Array.isArray(node)) {
    throw new Error(`Invalid schema node at ${at}`);
  }
  for (const key of Object.keys(node)) {
    if (!STRUCTURAL_KEYS.has(key) && !IGNORED_KEYS.has(key)) {
      throw new Error(`Unsupported schema keyword ${JSON.stringify(key)} at ${at}`);
    }
  }
  const out = {};
  if (node.$ref !== undefined) {
    const name = refName(node.$ref, at);
    refs.add(name);
    out.$ref = `#/$defs/${tsName(name)}`;
  }
  if (node.type !== undefined) {
    if (!SUPPORTED_TYPES.has(node.type)) {
      throw new Error(`Unsupported schema type ${JSON.stringify(node.type)} at ${at}`);
    }
    out.type = node.type;
  }
  if (node.format !== undefined) {
    if (node.type === "string" && node.format === "date-time") {
      out.format = "date-time";
    } else if (!(node.type === "integer" && node.format === "int64")) {
      throw new Error(`Unsupported format ${JSON.stringify(node.format)} at ${at}`);
    }
  }
  if (node.enum !== undefined) {
    if (!Array.isArray(node.enum) || !node.enum.every((v) => typeof v === "string")) {
      throw new Error(`Unsupported non-string enum at ${at}`);
    }
    out.enum = node.enum;
  }
  if (node.anyOf !== undefined) {
    if (!Array.isArray(node.anyOf) || node.anyOf.length === 0) {
      throw new Error(`Invalid anyOf at ${at}`);
    }
    out.anyOf = node.anyOf.map((branch, i) => compactSchema(branch, `${at}.anyOf[${i}]`, refs));
  }
  if (node.required !== undefined) {
    out.required = node.required;
  }
  if (node.items !== undefined) {
    out.items = compactSchema(node.items, `${at}.items`, refs);
  }
  if (node.properties !== undefined) {
    out.properties = {};
    for (const [name, prop] of Object.entries(node.properties)) {
      out.properties[name] = compactSchema(prop, `${at}.properties.${name}`, refs);
    }
    out.additionalProperties = false;
  }
  if (node.additionalProperties !== undefined) {
    if (node.properties !== undefined) {
      throw new Error(`Object with both properties and additionalProperties at ${at}`);
    }
    out.additionalProperties =
      typeof node.additionalProperties === "boolean"
        ? node.additionalProperties
        : compactSchema(node.additionalProperties, `${at}.additionalProperties`, refs);
  }
  if (out.$ref === undefined && out.type === undefined && out.anyOf === undefined) {
    throw new Error(`Schema without type, $ref or anyOf at ${at}`);
  }
  return out;
}

// tsType renders the TypeScript type of a compact schema node.
function tsType(node) {
  if (node.anyOf) {
    return `(${node.anyOf.map(tsType).join(" | ")})`;
  }
  if (node.$ref) {
    return node.$ref.replace(/^#\/\$defs\//, "");
  }
  switch (node.type) {
    case "string":
      return node.enum ? node.enum.map((v) => JSON.stringify(v)).join(" | ") : "string";
    case "boolean":
      return "boolean";
    case "integer":
    case "number":
      return "number";
    case "array":
      return node.items ? `${tsType(node.items)}[]` : "unknown[]";
    case "object":
      if (node.properties) {
        const required = new Set(node.required || []);
        const fields = Object.entries(node.properties).map(
          ([name, prop]) => `${JSON.stringify(name)}${required.has(name) ? "" : "?"}: ${tsType(prop)}`
        );
        return `{ ${fields.join("; ")} }`;
      }
      if (node.additionalProperties && typeof node.additionalProperties === "object") {
        return `Record<string, ${tsType(node.additionalProperties)}>`;
      }
      return "Record<string, unknown>";
    default:
      throw new Error(`Cannot render schema ${JSON.stringify(node)}`);
  }
}

// emitDeclaration renders one component schema as an interface when it is an
// object with properties, or as a type alias otherwise.
function emitDeclaration(name, node) {
  if (node.type === "object" && node.properties && !node.anyOf) {
    const required = new Set(node.required || []);
    const lines = [`export interface ${name} {`];
    for (const [prop, schema] of Object.entries(node.properties)) {
      lines.push(`  ${prop}${required.has(prop) ? "" : "?"}: ${tsType(schema)};`);
    }
    lines.push("}");
    return lines.join("\n");
  }
  return `export type ${name} = ${tsType(node)};`;
}

// pascal turns a Goa method name such as get_change into GetChange.
function pascal(method) {
  return method
    .split("_")
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join("");
}

// findOperations locates each page operation in the OpenAPI document and
// reads its method, path, parameters, body and single success response.
function findOperations(openApi, refs) {
  const byId = new Map();
  for (const [route, methods] of Object.entries(openApi.paths)) {
    for (const [method, op] of Object.entries(methods)) {
      byId.set(op.operationId, { route, method: method.toUpperCase(), op });
    }
  }
  return PAGE_OPERATIONS.map((id) => {
    const found = byId.get(id);
    if (!found) {
      throw new Error(`The views page calls ${id}, which gen/http/openapi3.json does not declare`);
    }
    const { route, method, op } = found;
    const name = id.split("#")[1];
    const params = (op.parameters || []).map((p) => {
      if (p.in !== "path" && p.in !== "query") {
        throw new Error(`${id}: unsupported ${p.in} parameter ${p.name}`);
      }
      return {
        name: p.name,
        in: p.in,
        required: Boolean(p.required),
        schema: compactSchema(p.schema, `${id}.parameters.${p.name}`, refs),
      };
    });
    let body = null;
    if (op.requestBody) {
      const schema = op.requestBody.content?.["application/json"]?.schema;
      body = tsName(refName(schema?.$ref, `${id}.requestBody`));
      refs.add(refName(schema.$ref, `${id}.requestBody`));
    }
    const successes = Object.entries(op.responses).filter(([status]) => status.startsWith("2"));
    if (successes.length !== 1) {
      throw new Error(`${id} must declare exactly one success response`);
    }
    const [status, response] = successes[0];
    const resultSchema = response.content?.["application/json"]?.schema;
    const result = tsName(refName(resultSchema?.$ref, `${id}.responses.${status}`));
    refs.add(refName(resultSchema.$ref, `${id}.responses.${status}`));
    return { id, name, route, method, params, body, success: Number(status), result };
  });
}

// requestDeclaration renders the request type of one operation: its body
// payload and its path and query parameters as one flat object.
function requestDeclaration(op) {
  const typeName = `${pascal(op.name)}Request`;
  const fields = op.params.map(
    (p) => `${JSON.stringify(p.name)}${p.required ? "" : "?"}: ${tsType(p.schema)}`
  );
  if (op.body && fields.length > 0) {
    return `export type ${typeName} = ${op.body} & { ${fields.join("; ")} };`;
  }
  if (op.body) {
    return `export type ${typeName} = ${op.body};`;
  }
  if (fields.length > 0) {
    return `export interface ${typeName} {\n${fields.map((f) => `  ${f};`).join("\n")}\n}`;
  }
  return `export type ${typeName} = Record<string, never>;`;
}

// encoderSource renders the function that turns one operation's request into
// its address and JSON body.
function encoderSource(op) {
  const typeName = `${pascal(op.name)}Request`;
  const pathParams = op.params.filter((p) => p.in === "path");
  const queryParams = op.params.filter((p) => p.in === "query");
  const segments = op.route.split(/(\{[a-zA-Z0-9_]+\})/).filter((s) => s !== "");
  const pathExpr = segments
    .map((segment) => {
      const match = /^\{([a-zA-Z0-9_]+)\}$/.exec(segment);
      if (!match) {
        return JSON.stringify(segment);
      }
      if (!pathParams.some((p) => p.name === match[1])) {
        throw new Error(`${op.id}: path segment ${segment} has no path parameter`);
      }
      return `encodeURIComponent(String(request.${match[1]}))`;
    })
    .join(" + ");
  const lines = [`function encode${pascal(op.name)}(request: ${typeName}): EncodedRequest {`];
  let address = pathExpr;
  if (queryParams.length > 0) {
    lines.push("  const query = new URLSearchParams();");
    for (const p of queryParams) {
      const set = `query.set(${JSON.stringify(p.name)}, String(request.${p.name}));`;
      lines.push(p.required ? `  ${set}` : `  if (request.${p.name} !== undefined) {\n    ${set}\n  }`);
    }
    lines.push("  const search = query.toString();");
    address = `${pathExpr} + (search === "" ? "" : "?" + search)`;
  }
  let body = "undefined";
  if (op.body && pathParams.length + queryParams.length > 0) {
    const omitted = [...pathParams, ...queryParams].map((p) => `${p.name}: _${p.name}`).join(", ");
    lines.push(`  const { ${omitted}, ...payload } = request;`);
    body = "JSON.stringify(payload)";
  } else if (op.body) {
    body = "JSON.stringify(request)";
  } else if (op.params.length === 0) {
    lines[0] = `function encode${pascal(op.name)}(_request: ${typeName}): EncodedRequest {`;
  }
  lines.push(`  return { path: ${address}, body: ${body} };`);
  lines.push("}");
  return lines.join("\n");
}

// generateTypesModule reads gen/http/openapi3.json and generates
// ui/checkpoint/src/types.ts.
function generateTypesModule() {
  const openApi = JSON.parse(fs.readFileSync(openApiPath, "utf8"));
  const components = openApi.components?.schemas || {};
  const refs = new Set([ERROR_SCHEMA]);
  const operations = findOperations(openApi, refs);

  // Compact every component reachable from the page's operations.
  const defs = {};
  const pending = [...refs];
  while (pending.length > 0) {
    const component = pending.shift();
    const name = tsName(component);
    if (defs[name]) {
      continue;
    }
    if (!components[component]) {
      throw new Error(`Missing component schema ${component}`);
    }
    const found = new Set();
    defs[name] = compactSchema(components[component], `components.schemas.${component}`, found);
    for (const next of found) {
      if (!defs[tsName(next)]) {
        pending.push(next);
      }
    }
  }
  const defNames = Object.keys(defs).sort();
  const results = [...new Set([...operations.map((op) => op.result), tsName(ERROR_SCHEMA)])].sort();

  return `// Code generated by ui/checkpoint/build.mjs from gen/http/openapi3.json; DO NOT EDIT.
//
// Package types declares the views page's view of the Surla HTTP contract:
// the request and result types of each operation the page sends, the route
// table that addresses them, and validators for the JSON each one returns.

import { z } from "zod";

${defNames.map((name) => emitDeclaration(name, defs[name])).join("\n\n")}

${operations.map(requestDeclaration).join("\n\n")}

// Operations maps each operation the page sends to its request and result.
export interface Operations {
${operations
  .map((op) => `  ${op.name}: { request: ${pascal(op.name)}Request; result: ${op.result} };`)
  .join("\n")}
}

// OperationName names one operation the page sends.
export type OperationName = keyof Operations;

// EncodedRequest is a request's same-origin address and JSON body.
export interface EncodedRequest {
  // path is the address relative to the page's origin, including any query.
  path: string;
  // body is the JSON request body, or undefined when the operation has none.
  body: string | undefined;
}

// Route is how one operation is sent and how its success is recognized.
export interface Route<K extends OperationName> {
  // method is the HTTP method.
  method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  // success is the HTTP status of a successful answer.
  success: number;
  // encode builds the operation's address and body from its request.
  encode: (request: Operations[K]["request"]) => EncodedRequest;
  // decode reports whether a parsed answer is the operation's result.
  decode: (value: unknown) => value is Operations[K]["result"];
}

${operations.map(encoderSource).join("\n\n")}

// ROUTES addresses every operation the page sends.
export const ROUTES: { [K in OperationName]: Route<K> } = {
${operations
  .map(
    (op) =>
      `  ${op.name}: { method: ${JSON.stringify(op.method)}, success: ${op.success}, encode: encode${pascal(op.name)}, decode: is${op.result} },`
  )
  .join("\n")}
};

interface JSONSchemaNode {
  type?: string;
  $ref?: string;
  enum?: string[];
  format?: "date-time";
  anyOf?: JSONSchemaNode[];
  required?: string[];
  additionalProperties?: boolean | JSONSchemaNode;
  items?: JSONSchemaNode;
  properties?: Record<string, JSONSchemaNode>;
}

const SCHEMA_DEFS: Record<string, JSONSchemaNode> = ${JSON.stringify(
    Object.fromEntries(defNames.map((name) => [name, defs[name]])),
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

// matchesSchema validates an unknown parsed value against a generated schema
// node, resolving references through SCHEMA_DEFS.
function matchesSchema(value: unknown, schema: JSONSchemaNode): boolean {
  if (schema.anyOf && !schema.anyOf.some((branch) => matchesSchema(value, branch))) {
    return false;
  }
  if (typeof schema.$ref === "string") {
    const target = SCHEMA_DEFS[schema.$ref.replace(/^#\\/\\$defs\\//, "")];
    return Boolean(target) && matchesSchema(value, target);
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
    return Array.isArray(value) && (!schema.items || value.every((item) => matchesSchema(item, schema.items!)));
  }
  if (schema.type === "object") {
    if (!isPlainObject(value)) {
      return false;
    }
    const props = schema.properties || {};
    for (const name of schema.required || []) {
      if (value[name] === undefined) {
        return false;
      }
    }
    for (const key of Object.keys(value)) {
      const propValue = value[key];
      if (Object.prototype.hasOwnProperty.call(props, key)) {
        if (!matchesSchema(propValue, props[key])) {
          return false;
        }
      } else if (schema.additionalProperties === false) {
        return false;
      } else if (isPlainObject(schema.additionalProperties)) {
        if (!matchesSchema(propValue, schema.additionalProperties)) {
          return false;
        }
      }
    }
    return true;
  }
  return Boolean(schema.anyOf);
}

${results
  .map(
    (name) => `// is${name} validates a parsed answer against the generated ${name} schema.
export function is${name}(value: unknown): value is ${name} {
  return matchesSchema(value, { $ref: "#/$defs/${name}" });
}`
  )
  .join("\n\n")}
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

  // scriptText is the exact text between the page's <script> tags. A browser
  // hashes this text as UTF-8 after normalizing line endings and refuses to
  // run it unless the digest matches the policy, so it must contain no
  // carriage return or NUL that parsing would change.
  const scriptText = `\n  ${safeJs}\n  `;
  if (/[\r\0]/.test(scriptText)) {
    throw new Error("the views page script contains a carriage return or NUL character");
  }
  const scriptDigest = crypto.createHash("sha256").update(scriptText, "utf8").digest("base64");
  const scriptHashGo = `// Code generated by ui/checkpoint/build.mjs. DO NOT EDIT.

package controlplane

// viewsPageScriptSource is the Content-Security-Policy source that allows the
// views page's one inline script: the SHA-256 digest of its exact text.
const viewsPageScriptSource = "'sha256-${scriptDigest}'"
`;

  // The page is a fixed shell with no project data. Its security policy is
  // sent by the server as a response header (internal/controlplane/views.go).
  const html = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Surla Architecture Workspace</title>
  <style>
${stylesCss}
  </style>
</head>
<body>
  <main id="surla-checkpoint-root" data-state="starting">
    <header class="surla-workspace-header">
      <h1>Surla Architecture Workspace</h1>
      <p>Review and confirm this project's changes: domain decisions, architecture, verification proof, and promotion approvals.</p>
    </header>
    <p id="surla-checkpoint-status" role="status" aria-live="polite">Opening Surla...</p>
    <nav id="surla-checkpoint-nav" aria-label="Workspace stages" hidden></nav>
    <section id="surla-checkpoint-view" aria-label="Workspace View"></section>
  </main>
  <script>${scriptText}</script>
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
    const existingScriptHash = fs.existsSync(scriptHashPath)
      ? fs.readFileSync(scriptHashPath, "utf8")
      : "";
    if (existingTypes !== generatedTypesTs) {
      console.error("ERROR: ui/checkpoint/src/types.ts is out of date with gen/http/openapi3.json.");
      process.exit(1);
    }
    if (existingHtml !== html) {
      console.error("ERROR: internal/controlplane/checkpoint.html is out of date with ui/checkpoint/src.");
      process.exit(1);
    }
    if (existingScriptHash !== scriptHashGo) {
      console.error("ERROR: internal/controlplane/views_page_script_gen.go is out of date with checkpoint.html.");
      process.exit(1);
    }
    if (existingCatalog !== catalogMarkdown) {
      console.error("ERROR: internal/goldenpath/decision_catalog.md is out of date with ui/checkpoint/src/openui/library.tsx.");
      process.exit(1);
    }
    console.log("OK: types.ts, checkpoint.html, views_page_script_gen.go, and decision_catalog.md are up to date.");
    return;
  }

  fs.mkdirSync(path.dirname(checkpointHtmlPath), { recursive: true });
  fs.mkdirSync(path.dirname(decisionCatalogPath), { recursive: true });
  fs.writeFileSync(checkpointHtmlPath, html, "utf8");
  fs.writeFileSync(scriptHashPath, scriptHashGo, "utf8");
  fs.writeFileSync(decisionCatalogPath, catalogMarkdown, "utf8");
  console.log(
    `Built types.ts (${Buffer.byteLength(generatedTypesTs)} bytes), checkpoint.html (${Buffer.byteLength(html)} bytes), views_page_script_gen.go, and decision_catalog.md (${Buffer.byteLength(catalogMarkdown)} bytes).`
  );
}

buildArtifacts().catch((err) => {
  console.error(err);
  process.exit(1);
});

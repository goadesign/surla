// browser_harness.cjs drives a real Chromium-family browser through the
// DevTools protocol for the opt-in Go test
// internal/project/views_browser_test.go (build tag `browser`). The Go test
// starts the project server, a second listener it owns on other origins, and
// the browser (headless, with a throwaway profile); it then runs
//
//   node browser_harness.cjs <input.json>
//
// This script opens tabs in that browser, does what the scenario below says,
// and prints what the browser itself sent, enforced and showed as one JSON
// object on stdout. It asserts nothing: the Go test judges the observations.
// It needs only Node's built-in WebSocket and no package.
"use strict";

const fs = require("node:fs");

// STEP_TIMEOUT_MS bounds every DevTools command, every awaited event, and
// every awaited page condition.
const STEP_TIMEOUT_MS = 15000;

// POLL_MS is the interval between two checks of an awaited page condition.
const POLL_MS = 25;

// VIOLATION_COLLECTOR runs in every document before the document's own
// script, through DevTools rather than the page, so the page's policy does not
// apply to it. It keeps every securitypolicyviolation the document reports.
const VIOLATION_COLLECTOR =
  "(" +
  function collect() {
    const seen = [];
    Object.defineProperty(window, "__surlaViolations", { value: seen });
    document.addEventListener("securitypolicyviolation", (event) => {
      seen.push({
        effective_directive: event.effectiveDirective,
        violated_directive: event.violatedDirective,
        blocked_uri: event.blockedURI,
        disposition: event.disposition,
        source_file: event.sourceFile,
      });
    });
  }.toString() +
  ")();";

// DevTools is one connection to the browser's DevTools endpoint. Commands to
// a tab carry that tab's flat session ID; events are handed to listeners.
class DevTools {
  // connect opens the browser-wide DevTools WebSocket at url.
  static connect(url) {
    return withDeadline(
      new Promise((resolve, reject) => {
        const ws = new WebSocket(url);
        ws.addEventListener("open", () => resolve(new DevTools(ws)), { once: true });
        ws.addEventListener("error", () => reject(new Error("could not connect to DevTools at " + url)), {
          once: true,
        });
      }),
      "connecting to DevTools"
    );
  }

  constructor(ws) {
    this.ws = ws;
    this.nextId = 1;
    this.pending = new Map();
    this.listeners = new Set();
    ws.addEventListener("message", (event) => this.receive(JSON.parse(String(event.data))));
    ws.addEventListener("close", () => {
      for (const { reject, method } of this.pending.values()) {
        reject(new Error("DevTools connection closed during " + method));
      }
      this.pending.clear();
    });
  }

  // send runs one DevTools command, in the tab of sessionId when given, and
  // resolves to its result.
  send(method, params = {}, sessionId) {
    const id = this.nextId++;
    const message = { id, method, params };
    if (sessionId) {
      message.sessionId = sessionId;
    }
    const answered = new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject, method });
      this.ws.send(JSON.stringify(message));
    });
    return withDeadline(answered, method).finally(() => this.pending.delete(id));
  }

  // on calls listener with every event until the returned function is called.
  on(listener) {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  // waitForEvent resolves to the first event matching predicate.
  waitForEvent(predicate, what) {
    let off = () => {};
    const seen = new Promise((resolve) => {
      off = this.on((message) => {
        if (predicate(message)) {
          resolve(message);
        }
      });
    });
    return withDeadline(seen, what).finally(() => off());
  }

  // receive settles a command's answer or hands an event to the listeners.
  receive(message) {
    if (message.id !== undefined) {
      const call = this.pending.get(message.id);
      if (!call) {
        return;
      }
      if (message.error) {
        call.reject(new Error(call.method + ": " + message.error.message));
      } else {
        call.resolve(message.result);
      }
      return;
    }
    for (const listener of Array.from(this.listeners)) {
      listener(message);
    }
  }

  close() {
    this.ws.close();
  }
}

// withDeadline rejects with what when promise has not settled within
// STEP_TIMEOUT_MS.
function withDeadline(promise, what) {
  let timer;
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error("timed out after " + STEP_TIMEOUT_MS + "ms: " + what)), STEP_TIMEOUT_MS);
  });
  return Promise.race([promise, deadline]).finally(() => clearTimeout(timer));
}

// sleep resolves after ms milliseconds; it only spaces out condition checks.
function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// openTab creates a new tab on about:blank and starts recording what the
// browser does in it, including in frames that run in other processes.
async function openTab(dt) {
  const { targetId } = await dt.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await dt.send("Target.attachToTarget", { targetId, flatten: true });
  const tab = {
    dt,
    targetId,
    // sessionId is the tab's own DevTools session.
    sessionId,
    // sessions holds the tab's session and those of frames and workers that
    // run in other processes; their events are recorded as the tab's.
    sessions: new Set([sessionId]),
    // frameSessions maps a frame in another process to its session.
    frameSessions: new Map(),
    // requests lists every network request in the order the browser began it.
    requests: [],
    byId: new Map(),
    // console holds console, log and target messages as text.
    console: [],
    // frames and contexts are every frame commit and script context.
    frames: [],
    contexts: [],
    // session is the first Authorization the page sent; it is used only to
    // try it from other origins and never printed.
    session: null,
  };
  dt.on((message) => {
    if (!tab.sessions.has(message.sessionId)) {
      return;
    }
    if (message.method === "Target.attachedToTarget") {
      void attachChild(tab, message.params);
      return;
    }
    if (message.method === "Target.detachedFromTarget") {
      tab.console.push("detached " + message.params.sessionId);
      return;
    }
    observe(tab, message);
  });
  await enableDomains(tab, sessionId, true);
  await dt.send("Target.setAutoAttach", { autoAttach: true, waitForDebuggerOnStart: true, flatten: true }, sessionId);
  return tab;
}

// enableDomains turns on the events this script records in one session.
async function enableDomains(tab, sessionId, isFrame) {
  if (isFrame) {
    await tab.dt.send("Page.enable", {}, sessionId);
    await tab.dt.send("Page.addScriptToEvaluateOnNewDocument", { source: VIOLATION_COLLECTOR }, sessionId);
  }
  await tab.dt.send("Runtime.enable", {}, sessionId);
  await tab.dt.send("Network.enable", {}, sessionId);
  await tab.dt.send("Log.enable", {}, sessionId);
}

// attachChild records a frame or worker that runs in another process as part
// of tab, then lets it start.
async function attachChild(tab, { sessionId, targetInfo }) {
  tab.sessions.add(sessionId);
  if (targetInfo.type === "iframe") {
    tab.frameSessions.set(targetInfo.targetId, sessionId);
  }
  tab.console.push("attached " + targetInfo.type + " " + targetInfo.url);
  try {
    await enableDomains(tab, sessionId, targetInfo.type === "iframe");
    await tab.dt.send("Target.setAutoAttach", { autoAttach: true, waitForDebuggerOnStart: true, flatten: true }, sessionId);
  } catch (err) {
    tab.console.push("could not observe " + targetInfo.type + ": " + err.message);
  }
  try {
    await tab.dt.send("Runtime.runIfWaitingForDebugger", {}, sessionId);
  } catch (err) {
    tab.console.push("could not start " + targetInfo.type + ": " + err.message);
  }
}

// observe records one DevTools event of tab.
function observe(tab, { method, params }) {
  switch (method) {
    case "Network.requestWillBeSent": {
      const entry = requestEntry(tab, params.requestId);
      entry.method = params.request.method;
      entry.url = params.request.url;
      entry.type = params.type || "";
      entry.page_headers = headerMap(params.request.headers);
      const auth = params.request.headers.Authorization || params.request.headers.authorization;
      if (tab.session === null && auth && auth.startsWith("Bearer ")) {
        tab.session = auth;
      }
      break;
    }
    case "Network.requestWillBeSentExtraInfo":
      requestEntry(tab, params.requestId).sent_headers = headerMap(params.headers);
      break;
    case "Network.responseReceived": {
      const entry = requestEntry(tab, params.requestId);
      entry.status = entry.status || params.response.status;
      break;
    }
    case "Network.responseReceivedExtraInfo":
      requestEntry(tab, params.requestId).status = params.statusCode;
      break;
    case "Network.loadingFailed": {
      const parts = [params.errorText];
      if (params.blockedReason) {
        parts.push("blocked: " + params.blockedReason);
      }
      if (params.corsErrorStatus) {
        parts.push("cors: " + params.corsErrorStatus.corsError);
      }
      if (params.canceled) {
        parts.push("canceled");
      }
      requestEntry(tab, params.requestId).failure = parts.join("; ");
      break;
    }
    case "Runtime.consoleAPICalled":
      tab.console.push(
        "console." + params.type + ": " + params.args.map((arg) => (arg.value !== undefined ? String(arg.value) : arg.description)).join(" ")
      );
      break;
    case "Runtime.exceptionThrown":
      tab.console.push(
        "exception: " + ((params.exceptionDetails.exception && params.exceptionDetails.exception.description) || params.exceptionDetails.text)
      );
      break;
    case "Log.entryAdded":
      tab.console.push("log " + params.entry.source + "/" + params.entry.level + ": " + params.entry.text);
      break;
    case "Page.frameNavigated":
      tab.frames.push(frameFacts(params.frame));
      break;
    case "Runtime.executionContextCreated":
      tab.contexts.push({
        origin: params.context.origin,
        frame_id: (params.context.auxData && params.context.auxData.frameId) || "",
        is_default: Boolean(params.context.auxData && params.context.auxData.isDefault),
      });
      break;
    default:
  }
}

// requestEntry returns tab's record of requestId, creating it in send order.
function requestEntry(tab, requestId) {
  let entry = tab.byId.get(requestId);
  if (!entry) {
    entry = { id: requestId, method: "", url: "", type: "", page_headers: {}, sent_headers: null, status: 0, failure: "" };
    tab.byId.set(requestId, entry);
    tab.requests.push(entry);
  }
  return entry;
}

// headerMap lowercases header names and hides credential values, keeping
// only their kind, so the observations never carry a session.
function headerMap(headers) {
  const out = {};
  for (const [name, value] of Object.entries(headers || {})) {
    const lower = name.toLowerCase();
    out[lower] = lower === "authorization" ? String(value).split(" ")[0] + " <redacted>" : String(value);
  }
  return out;
}

// frameFacts keeps the parts of a DevTools frame that say what it shows.
function frameFacts(frame) {
  return {
    id: frame.id,
    parent_id: frame.parentId || "",
    url: frame.url,
    security_origin: frame.securityOrigin || "",
    unreachable_url: frame.unreachableUrl || "",
  };
}

// navigate opens url in tab and resolves once the page's load event fired.
async function navigate(tab, url) {
  const loaded = tab.dt.waitForEvent(
    (message) => message.sessionId === tab.sessionId && message.method === "Page.loadEventFired",
    "loading " + url
  );
  const result = await tab.dt.send("Page.navigate", { url }, tab.sessionId);
  if (result.errorText) {
    throw new Error("navigating to " + url + " failed: " + result.errorText);
  }
  await loaded;
}

// reload reloads tab and resolves once the reloaded page's load event fired.
async function reload(tab) {
  const loaded = tab.dt.waitForEvent(
    (message) => message.sessionId === tab.sessionId && message.method === "Page.loadEventFired",
    "reloading the page"
  );
  await tab.dt.send("Page.reload", {}, tab.sessionId);
  await loaded;
}

// evaluate runs expression in tab's main frame, awaiting a promise result.
async function evaluate(tab, expression, sessionId = tab.sessionId, contextId) {
  const params = { expression, returnByValue: true, awaitPromise: true };
  if (contextId !== undefined) {
    params.contextId = contextId;
  }
  const { result, exceptionDetails } = await tab.dt.send("Runtime.evaluate", params, sessionId);
  if (exceptionDetails) {
    throw new Error(
      "page evaluation failed: " + ((exceptionDetails.exception && exceptionDetails.exception.description) || exceptionDetails.text)
    );
  }
  return result.value;
}

// until checks expression in tab until it is truthy and returns
// { value, timed_out }; it never throws, so a condition that never holds
// becomes an observation the Go test judges.
async function until(tab, expression) {
  const deadline = Date.now() + STEP_TIMEOUT_MS;
  for (;;) {
    try {
      const value = await evaluate(tab, expression);
      if (value) {
        return { value, timed_out: false };
      }
    } catch (err) {
      // The page may be between documents during a navigation.
    }
    if (Date.now() > deadline) {
      return { value: null, timed_out: true };
    }
    await sleep(POLL_MS);
  }
}

// settle waits until every request of tab matching select failed, or was
// put on the wire with its network headers and, unless it is still open on
// purpose, answered. A request the browser refused to send only fails. It
// resolves to whether that happened before the deadline.
async function settle(tab, select, stillOpen = () => false) {
  const deadline = Date.now() + STEP_TIMEOUT_MS;
  const settled = (r) => r.failure !== "" || (r.sent_headers !== null && (r.status !== 0 || stillOpen(r)));
  for (;;) {
    const waiting = tab.requests.filter((r) => select(r) && !settled(r));
    if (waiting.length === 0) {
      return true;
    }
    if (Date.now() > deadline) {
      return false;
    }
    await sleep(POLL_MS);
  }
}

// violationsSince returns the securitypolicyviolation reports of tab's page
// after the first count, once there are more than count.
function violationsSince(tab, count) {
  return until(tab, "window.__surlaViolations.length > " + count + " ? window.__surlaViolations.slice(" + count + ") : null");
}

// frameView reads what one frame of tab shows through sessionId, in an
// isolated world so the frame's own policy and script cannot affect the
// reading.
async function frameView(tab, frameId, sessionId) {
  try {
    const { executionContextId } = await tab.dt.send(
      "Page.createIsolatedWorld",
      { frameId, worldName: "surla-browser-test" },
      sessionId
    );
    return await evaluate(
      tab,
      "({ href: location.href, origin: location.origin, has_views_root: document.getElementById('surla-checkpoint-root') !== null, text: (document.body ? document.body.innerText : '').slice(0, 300) })",
      sessionId,
      executionContextId
    );
  } catch (err) {
    return { error: err.message };
  }
}

// flattenFrames lists a DevTools frame tree depth first.
function flattenFrames(node, out = []) {
  out.push(frameFacts(node.frame));
  for (const child of node.childFrames || []) {
    flattenFrames(child, out);
  }
  return out;
}

// fetchOutcome is a page expression that fetches url with init and resolves
// to what the page could see: the answer's type and status, or the error.
function fetchOutcome(url, init) {
  return (
    "fetch(" +
    JSON.stringify(url) +
    ", " +
    init +
    ").then((r) => 'answered: ' + r.type + ' ' + r.status, (e) => 'refused: ' + e.name + ': ' + e.message)"
  );
}

// main runs the scenario. Each section names the Go subtest that judges it.
async function main(input, facts) {
  const dt = await DevTools.connect(input.devtools);
  try {
    const version = await dt.send("Browser.getVersion");
    facts.browser = { product: version.product, protocol: version.protocolVersion, user_agent: version.userAgent };
    const server = (r) => r.url.startsWith(input.origin + "/");
    const longPoll = (r) => new URL(r.url).pathname === "/change-updates";

    // (a) Open the page with the link, as `surla` opens it.
    const views = await openTab(dt);
    await navigate(views, input.origin + "/views#link=" + input.link);
    const listed = await until(
      views,
      "(() => { const ids = Array.from(document.querySelectorAll('#surla-change-list [data-change-id]'), (item) => item.dataset.changeId); return ids.length >= " +
        input.change_ids.length +
        " ? ids : null; })()"
    );
    const { currentIndex, entries } = await dt.send("Page.getNavigationHistory", {}, views.sessionId);
    const { targetInfo } = await dt.send("Target.getTargetInfo", { targetId: views.targetId });
    facts.views = {
      change_ids: listed.value || [],
      change_list_timed_out: listed.timed_out,
      href: await evaluate(views, "location.href"),
      history_urls: entries.map((entry) => entry.url),
      current_history_url: entries[currentIndex].url,
      target_url: targetInfo.url,
    };

    // (b) Every request the page sent so far, with the headers the browser put on the wire.
    facts.views.requests_settled = await settle(views, server, longPoll);
    facts.views.requests = views.requests.filter(server).map((r) => ({ ...r }));

    // (c) What the browser kept for the page.
    facts.views.storage = await evaluate(
      views,
      "indexedDB.databases().then((dbs) => ({ local_storage: localStorage.length, session_storage: sessionStorage.length, cookie: document.cookie, indexed_db: dbs.length }))"
    );
    facts.views.storage.browser_cookies = (await dt.send("Storage.getCookies", {})).cookies.length;

    // (d) The page's policy: its own load, an injected inline script, a fetch to another origin.
    facts.views.load_violations = await evaluate(views, "window.__surlaViolations.slice()");
    let count = facts.views.load_violations.length;
    await evaluate(
      views,
      "(() => { const s = document.createElement('script'); s.textContent = 'window.__surlaInjectedRan = true;'; document.body.appendChild(s); return true; })()"
    );
    const injected = await violationsSince(views, count);
    facts.views.injected_script_ran = await evaluate(views, "window.__surlaInjectedRan === true");
    facts.views.injected_violations = injected.value || [];
    count += facts.views.injected_violations.length;
    const probe = input.same_site_origin + "/csp-probe";
    facts.views.foreign_fetch = await evaluate(views, fetchOutcome(probe, "{ mode: 'no-cors' }"));
    const blocked = await violationsSince(views, count);
    facts.views.foreign_fetch_violations = blocked.value || [];
    await settle(views, (r) => r.url === probe);
    facts.views.foreign_fetch_requests = views.requests.filter((r) => r.url === probe).map((r) => ({ ...r }));

    // (e) A page on another origin frames the views page.
    const framing = await openTab(dt);
    await navigate(framing, input.same_site_origin + "/frame");
    await settle(framing, server);
    // A frame in another process belongs to its own DevTools session, so
    // every session's frame tree is read, each frame once.
    const frames = [];
    for (const sessionId of [framing.sessionId, ...framing.frameSessions.values()]) {
      const { frameTree } = await dt.send("Page.getFrameTree", {}, sessionId);
      for (const frame of flattenFrames(frameTree)) {
        if (!frames.some((known) => known.id === frame.id)) {
          frame.view = await frameView(framing, frame.id, sessionId);
          frames.push(frame);
        }
      }
    }
    facts.framing = {
      frames,
      navigated: framing.frames,
      contexts: framing.contexts,
      requests: framing.requests.filter(server),
      console: framing.console,
    };

    // (f) Pages on other origins send requests to the server's data routes,
    // one without credentials as an image or form could, one with the page's
    // own session.
    const session = views.session || "Bearer missing";
    facts.other_pages = [];
    for (const origin of [input.same_site_origin, input.cross_site_origin]) {
      const other = await openTab(dt);
      await navigate(other, origin + "/blank");
      const plain = await evaluate(other, fetchOutcome(input.origin + "/changes", "{ mode: 'no-cors' }"));
      const withSession = await evaluate(
        other,
        fetchOutcome(input.origin + "/changes", "{ headers: { Authorization: " + JSON.stringify(session) + " } }")
      );
      await settle(other, server);
      facts.other_pages.push({
        origin,
        without_credentials: plain,
        with_session: withSession,
        requests: other.requests.filter(server),
        console: other.console,
      });
    }
    facts.views.session_seen = views.session !== null;

    // (g) Reload the views page.
    const beforeReload = views.requests.length;
    await reload(views);
    const signedOut = await until(views, "(document.getElementById('surla-signed-out') || {}).textContent");
    const shownAfterReload = views.requests.slice(beforeReload).map((r) => r.id);
    const probeURL = input.origin + "/change-updates?since=after-reload";
    const status = await evaluate(views, "fetch(" + JSON.stringify(probeURL) + ").then((r) => r.status, (e) => 'refused: ' + e.message)");
    await settle(views, (r) => r.url === probeURL);
    facts.reload = {
      signed_out_text: signedOut.value || "",
      signed_out_timed_out: signedOut.timed_out,
      href: await evaluate(views, "location.href"),
      page_requests: views.requests.filter((r) => shownAfterReload.includes(r.id)),
      data_request_status: status,
      data_request: views.requests.filter((r) => r.url === probeURL),
    };
    facts.views.console = views.console;
  } finally {
    dt.close();
  }
}

const facts = {};
main(JSON.parse(fs.readFileSync(process.argv[2], "utf8")), facts).then(
  () => process.stdout.write(JSON.stringify(facts), () => process.exit(0)),
  (err) => {
    console.error((err && err.stack) || err);
    process.stdout.write(JSON.stringify(facts), () => process.exit(1));
  }
);

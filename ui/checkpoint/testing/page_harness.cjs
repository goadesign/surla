// page_harness.cjs opens the built views page in jsdom against a running
// Surla test server, for the Go tests that drive the page. The page is
// fetched from the server's GET /views and runs its own script. Its fetch is
// the only bridge to the server: each call becomes a real HTTP request to
// the page's own address carrying the marks a browser adds to a same-origin
// request (Sec-Fetch-Site, and Origin on requests that are not reads). The
// harness records every request so a test can check exactly what the page
// sent.
"use strict";

const path = require("node:path");
const { JSDOM } = require(path.join(__dirname, "../node_modules/jsdom"));

// expect throws message when value is falsy.
function expect(value, message) {
  if (!value) {
    throw new Error(message);
  }
}

// sleep resolves after ms milliseconds.
function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// until polls predicate every 10ms and fails with message after timeoutMs.
async function until(predicate, message, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    let value;
    try {
      value = predicate();
    } catch (err) {
      value = false;
    }
    if (value) {
      return value;
    }
    if (Date.now() > deadline) {
      throw new Error("timed out: " + message);
    }
    await sleep(10);
  }
}

// browserFetch sends one page request to origin the way a browser sends a
// same-origin fetch, and records it. A request to any other address fails as
// the page's connect-src 'self' policy would make it fail. Once the page is
// closed, like a closed tab, its requests are stopped and never answer it.
function browserFetch(win, origin, requests, tab, input, init) {
  const url = new URL(String(input), win.location.href);
  if (url.origin !== origin) {
    throw new win.TypeError("Refused to connect to " + url.origin + " (connect-src 'self')");
  }
  const method = String(init.method || "GET").toUpperCase();
  const headers = {};
  for (const [name, value] of Object.entries(init.headers || {})) {
    headers[name] = String(value);
  }
  headers["Sec-Fetch-Site"] = "same-origin";
  headers["Sec-Fetch-Mode"] = "cors";
  headers["Sec-Fetch-Dest"] = "empty";
  if (method !== "GET" && method !== "HEAD") {
    headers["Origin"] = origin;
  }
  let body;
  if (typeof init.body === "string") {
    body = JSON.parse(init.body);
  }
  requests.push({ method, path: url.pathname, search: url.search, body, headers });
  const controller = new AbortController();
  if (init.signal) {
    if (init.signal.aborted) {
      controller.abort();
    } else {
      init.signal.addEventListener("abort", () => controller.abort());
    }
  }
  tab.inFlight.add(controller);
  const settle = (promise) =>
    new Promise((resolve, reject) => {
      promise.then(
        (value) => !tab.closed && resolve(value),
        (err) => !tab.closed && reject(err)
      );
    });
  const answered = fetch(url, { method, headers, body: init.body, signal: controller.signal, redirect: "manual" });
  void answered.then(
    () => {},
    () => tab.inFlight.delete(controller)
  );
  return settle(answered).then((response) => ({
    status: response.status,
    text: () =>
      settle(response.text()).finally(() => {
        tab.inFlight.delete(controller);
      }),
  }));
}

// openPage loads origin/views (with #link=<link> when link is given) and runs
// the page. hidden starts the tab hidden.
async function openPage(origin, { link, hidden } = {}) {
  const shell = await fetch(origin + "/views", {
    headers: { "Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document" },
  });
  expect(shell.status === 200, "GET /views answered " + shell.status);
  const html = await shell.text();
  const requests = [];
  const tab = { closed: false, inFlight: new Set() };
  let isHidden = Boolean(hidden);
  const dom = new JSDOM(html, {
    url: origin + "/views" + (link ? "#link=" + link : ""),
    runScripts: "dangerously",
    pretendToBeVisual: true,
    beforeParse(win) {
      Object.defineProperty(win.document, "hidden", { configurable: true, get: () => isHidden });
      Object.defineProperty(win.document, "visibilityState", {
        configurable: true,
        get: () => (isHidden ? "hidden" : "visible"),
      });
      win.fetch = (input, init = {}) => browserFetch(win, origin, requests, tab, input, init);
    },
  });
  const win = dom.window;
  const doc = win.document;
  const page = {
    dom,
    win,
    doc,
    requests,
    headers: shell.headers,
    $: (selector) => doc.querySelector(selector),
    $$: (selector) => Array.from(doc.querySelectorAll(selector)),
    text: (selector) => (doc.querySelector(selector) || { textContent: "" }).textContent,
    state: () => doc.getElementById("surla-checkpoint-root").dataset.state,
    status: () => doc.getElementById("surla-checkpoint-status").textContent,
    // sent returns the recorded requests to method and path, in order.
    sent: (method, pathname) => requests.filter((r) => r.method === method && r.path === pathname),
    // type replaces a field's text and dispatches the input event a user's typing would.
    type(element, value) {
      element.value = value;
      element.dispatchEvent(new win.Event("input", { bubbles: true }));
    },
    // setHidden shows or hides the tab and dispatches visibilitychange.
    setHidden(value) {
      isHidden = value;
      doc.dispatchEvent(new win.Event("visibilitychange"));
    },
    // beforeUnload dispatches a cancelable beforeunload and reports whether
    // the page asked the browser to confirm leaving.
    beforeUnload() {
      const event = new win.Event("beforeunload", { cancelable: true });
      win.dispatchEvent(event);
      return event.defaultPrevented;
    },
    // close ends the page as closing its tab would: open requests stop and
    // the page's script runs no further.
    close() {
      tab.closed = true;
      for (const controller of tab.inFlight) {
        controller.abort();
      }
      win.close();
    },
  };
  return page;
}

// control posts body to the test server's /__test/<name> endpoint.
async function control(origin, name, body) {
  const response = await fetch(origin + "/__test/" + name, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  expect(response.status === 204, "test control " + name + " answered " + response.status);
}

// api calls the server as a program would, with a bearer credential and no
// browser marks, and returns the status and parsed body.
async function api(origin, method, pathname, body, token) {
  const headers = { Accept: "application/json" };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (token) {
    headers.Authorization = "Bearer " + token;
  }
  const response = await fetch(origin + pathname, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  return { status: response.status, body: text === "" ? undefined : JSON.parse(text) };
}

module.exports = { api, control, expect, openPage, sleep, until };

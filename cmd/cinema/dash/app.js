// CInema dashboard. Data only ever enters the DOM through textContent/attributes:
// logs and argv come from commands the user ran and must stay inert text.
"use strict";

let csrf = "";
let selected = "";

const $ = (id) => document.getElementById(id);

function el(tag, props = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v);
  }
  for (const c of children) if (c != null) n.append(c);
  return n;
}

function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${Math.round(s)}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 172800) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

function dur(ms) {
  return ms < 1000 ? `${ms} ms` : ms < 60000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`;
}

const stateText = {
  reusable: "reusable", stale: "stale", missing: "never run", failed: "failed",
  always: "always runs in CI", unknown: "can't tell",
};

async function api(path, opts = {}) {
  const res = await fetch(path, { credentials: "same-origin", ...opts });
  if (res.status === 401) { $("auth").hidden = false; throw new Error("unauthorized"); }
  if (!res.ok) throw new Error(await res.text());
  return res.status === 204 || res.status === 202 ? null : res.json();
}

async function load() {
  const s = await api("/api/state");
  csrf = s.csrf;
  $("auth").hidden = true;
  $("repo").textContent = s.repo.split("/").pop() || s.repo;
  $("repo").title = s.repo;
  $("head").textContent = s.head ? `HEAD ${s.head.slice(0, 12)}` : "no commits yet";
  $("dirty").hidden = !s.dirty;
  $("platform").textContent = s.platform;
  document.title = `${$("repo").textContent} · cinema`;

  const err = $("config-error");
  err.hidden = !s.config_error;
  if (s.config_error) err.textContent = `${s.config_error.reason_code}: ${s.config_error.message}`;
  renderChecks(s.tasks || []);
  renderRuns(s.runs || []);
}

function renderChecks(tasks) {
  const body = $("checks").tBodies[0];
  body.replaceChildren(...tasks.map((t) => {
    const why = el("td", { class: "why" }, t.reason_code || "");
    if (t.detail) why.append(el("span", { class: "detail-line", text: t.detail }));

    const strip = el("span", { class: "strip", role: "list", "aria-label": `recent runs of ${t.id}` });
    for (const f of t.frames || []) {
      const label = `${f.state}${f.covers ? ", covers this tree" : ""}`;
      strip.append(el("button", {
        class: `frame ${f.state}${f.covers ? " covers" : ""}`, role: "listitem",
        title: label, "aria-label": label, onclick: () => showRun(f.id),
      }));
    }

    let action = null;
    if (t.satisfiable_here && t.state !== "always" && t.state !== "reusable") {
      action = el("button", {
        class: "run", text: t.running ? "Running…" : "Run",
        onclick: (e) => startRun(t.id, e.currentTarget),
      });
      if (t.running) action.disabled = true;
    }

    return el("tr", {},
      el("th", { scope: "row" },
        el("span", { class: "check-name", text: t.id }),
        el("code", { class: "check-argv", text: (t.argv || []).join(" ") })),
      el("td", {}, el("span", { class: `state ${t.state}`, text: stateText[t.state] || t.state })),
      why,
      el("td", {}, strip),
      el("td", { class: "ci", text: "not set up" }),
      el("td", {}, action));
  }));
}

function renderRuns(runs) {
  $("no-runs").hidden = runs.length > 0;
  $("runs").replaceChildren(...runs.map((r) => {
    const failed = r.state !== "passed";
    const result = el("span", { class: "run-result" },
      el("span", { class: failed ? "bad" : "ok", text: r.state === "passed" ? "passed" : r.state === "failed" ? `exit ${r.exit_code}` : r.state }),
      ` in ${dur(r.duration_ms)}`);
    if (r.changed_source) result.append(", changed files");
    return el("li", {}, el("button", {
      "data-id": r.id, "aria-current": String(r.id === selected), onclick: () => showRun(r.id),
    },
      el("span", { class: r.task_id ? "run-title" : "run-title plain", text: r.task_id || r.argv.join(" ") }),
      el("span", { class: "run-when", text: ago(r.started_at) }),
      result));
  }));
}

async function showRun(id) {
  selected = id;
  for (const b of $("runs").querySelectorAll("button")) b.setAttribute("aria-current", String(b.dataset.id === id));
  const d = await api(`/api/runs/${encodeURIComponent(id)}`);
  const r = d.run;
  const facts = el("dl", { class: "facts" });
  const fact = (k, v, cls) => facts.append(el("dt", { text: k }), el("dd", cls ? { class: cls, text: v } : { text: v }));
  fact("Check", r.task_id || "not a check (history only)");
  fact("Directory", r.cwd || ".");
  fact("Started", `${new Date(r.started_at).toLocaleString()} (${ago(r.started_at)})`);
  fact("Took", dur(r.duration_ms));
  fact("Result", r.signal ? `killed by ${r.signal}` : `exit ${r.exit_code}`, r.state === "passed" ? null : "warn");
  fact("Checkout", r.capture === "clean" ? "clean checkout of a commit" : r.capture === "workspace" ? "your working tree" : "outside a repo");
  if (r.changed_source) fact("Note", "the command changed files while running, so this run can't vouch for the tree", "warn");

  const logs = [];
  for (const [name, text, clipped, total, truncated] of [
    ["stdout", d.stdout, d.stdout_clipped, r.stdout_total, r.stdout_truncated],
    ["stderr", d.stderr, d.stderr_clipped, r.stderr_total, r.stderr_truncated],
  ]) {
    if (!text && !total) continue;
    let label = name;
    if (clipped || truncated) label += ` (showing the end of ${total.toLocaleString()} bytes)`;
    logs.push(el("p", { class: "log-label", text: label }));
    logs.push(el("pre", { class: "log", tabindex: "0", text: text || "(output expired; logs of passing runs are kept 7 days)" }));
  }
  if (!logs.length) logs.push(el("p", { class: "empty", text: "No output." }));
  $("detail").replaceChildren(el("h3", { text: r.argv.join(" ") }), facts, ...logs);
}

async function startRun(id, button) {
  button.disabled = true;
  button.textContent = "Running…";
  try {
    await api("/api/run", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify({ task: id }),
    });
  } catch (e) {
    button.disabled = false;
    button.textContent = "Run";
    button.title = String(e.message || e);
  }
}

function listen() {
  const es = new EventSource("/api/events");
  es.onopen = () => { $("live").dataset.on = "true"; $("live").textContent = "live"; load().catch(() => {}); };
  es.onerror = () => { $("live").dataset.on = "false"; $("live").textContent = "reconnecting"; };
  es.addEventListener("changed", () => load().catch(() => {}));
}

async function start() {
  const m = location.hash.match(/(?:^#|&)t=([0-9a-f]+)/);
  if (m) {
    history.replaceState(null, "", "/"); // scrub the token from the address bar and history
    await fetch("/api/session", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token: m[1] }),
    });
  }
  try { await load(); listen(); } catch { /* auth notice already shown */ }
}

start();

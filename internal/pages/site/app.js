// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

"use strict";

const PAGE_SIZE = 50;
let index = null;
let current = { tab: "queue", series: [], page: 0 };
// Downloaded series lists by data path, kept until Refresh.
const cache = new Map();
const open = new Set();

function el(tag, attrs, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") e.className = v;
    else if (k === "onclick") e.onclick = v;
    else e.setAttribute(k, v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

function link(href, text) {
  return href ? el("a", { href, target: "_blank", rel: "noopener" }, text) : text;
}

async function getJSON(path) {
  const r = await fetch(path, { cache: "no-cache" });
  if (!r.ok) throw new Error(path + ": HTTP " + r.status);
  return r.json();
}

function age(ts) {
  if (!ts) return "";
  const s = (Date.now() - Date.parse(ts)) / 1000;
  if (s < 3600) return Math.max(1, Math.round(s / 60)) + "m ago";
  if (s < 86400) return Math.round(s / 3600) + "h ago";
  return Math.round(s / 86400) + "d ago";
}

const RESULTS = ["success", "warning", "fail", "pending"];
const FINISHED = ["done", "apply-failed", "not-applied", "skipped"];

// result is the check state of a run as posted to Patchwork.
function result(r) {
  if (r.result) return RESULTS.includes(r.result) ? r.result : "warning";
  return r.status === "completed" ? "warning" : "pending";
}

function runLabel(r) {
  if (r.kind === "apply") return "apply";
  if (r.kind === "missing") return (r.workflow || "workflows") + " (missing)";
  return r.workflow || r.file || "workflow";
}

function countChecks(runs) {
  const c = { success: 0, warning: 0, fail: 0, pending: 0 };
  for (const r of runs) c[result(r)]++;
  return c;
}

// counts shows the checks like Patchwork: passed, warnings, failed,
// and the ones still pending.
function counts(c) {
  const title = c.success + " passed, " + c.warning + " warnings, " + c.fail + " failed, " +
    c.pending + " pending";
  return el("span", { class: "counts", title },
    RESULTS.map(k => el("span", { class: "count " + (c[k] ? k : "zero") },
      c[k] ? String(c[k]) : "-")));
}

function when(ts) {
  if (!ts) return el("span", { class: "muted when" });
  return el("time", { class: "muted when", datetime: ts,
    title: new Date(ts).toLocaleString() + " (" + ts + ")" }, age(ts));
}

function archiveLink(msgid, format) {
  format = format || index.archive_url_format;
  if (!format || !msgid) return null;
  return format.replace("{}", encodeURIComponent(msgid.replace(/^<|>$/g, "")));
}

function archiveName(url) {
  return /lore\.kernel\.org|\binbox\./.test(url) ? "lore" : "archive";
}

function links(items) {
  const out = items.filter(i => i && i[0]);
  return el("span", { class: "links" },
    out.map(([href, text], i) => [i ? " " : "", link(href, text)]));
}

// toggle returns a small button and the panel it shows and hides, so
// that details take no space until asked for.
function toggle(text, panel) {
  if (!panel) return [null, null];
  panel.hidden = true;
  const b = el("button", { class: "toggle", type: "button" }, text);
  b.onclick = e => {
    e.stopPropagation();
    panel.hidden = !panel.hidden;
    b.classList.toggle("on", !panel.hidden);
  };
  return [b, panel];
}

function decisions(list) {
  if (!list || !list.length) return [null, null];
  const rows = list.map(d => el("li", {},
    when(d.time), " ", el("strong", {}, d.action.replace(/-/g, " ")), ": ", d.detail, " ",
    d.run_url ? link(d.run_url, "bot run logs") : null));
  return toggle("history (" + list.length + ")", el("ol", { class: "decisions" }, rows));
}

// seriesState says where the bot is with a series and whether its
// checks are final.
function seriesState(s) {
  const patches = s.patches || [];
  const finished = patches.filter(p => FINISHED.includes(p.status)).length;
  switch (s.status) {
  case "new":
    return ["active", s.checked ? "incomplete" : "new", s.checked
      ? "Waiting for Patchwork to receive all patches." : "Checking if it can be tested."];
  case "queued":
    return ["active", "queued", "Waiting for free CI capacity."];
  case "waiting":
    return ["active", s.reason || "waiting", "Waiting for a dependency."];
  case "testing":
    return ["active", "testing " + finished + "/" + patches.length,
      finished + " of " + patches.length + " patches have final checks."];
  case "done":
    return ["done", "done", "All checks are final."];
  case "skipped":
    return ["skipped", "skipped" + (s.reason ? ": " + s.reason : ""), s.reason || "Not tested."];
  default:
    return ["error", s.status + (s.reason ? ": " + s.reason : ""), s.reason || ""];
  }
}

// patchState explains a patch whose checks do not tell the story.
function patchState(s, p) {
  switch (p.status) {
  case "pending": return "waiting to be applied";
  case "applied": return "applied, waiting for CI capacity";
  case "pushed": return "testing";
  case "not-applied": return "not applied: an earlier patch failed to apply";
  case "skipped": return "not tested" + (s.reason ? ": " + s.reason : "");
  default: return null;
  }
}

function renderPatch(s, p) {
  const runs = p.runs || [];
  // A badge leads to its report when it was sent, which links the run,
  // and to the run otherwise.
  const chips = runs.map(r => {
    const st = result(r);
    const report = archiveLink(r.final && r.final.message_id);
    const title = [r.conclusion, r.details, r.failure && r.failure.last_error,
      report ? "Opens the report." : r.url ? "Opens the workflow run." : ""]
      .filter(Boolean).join("\n");
    return link(report || r.url,
      el("span", { class: "badge " + st, title: title || null }, runLabel(r) + ": " + st));
  });
  const mail = archiveLink(p.msgid, index.patch_archive_url_format);
  const [historyButton, history] = decisions(p.decisions);
  const state = patchState(s, p);
  return el("div", { class: "patch" },
    el("div", { class: "row" },
      el("span", { class: "pname" }, p.name || "", " ",
        links([[index.patchwork_web + "/patch/" + p.id + "/", "pw"],
          [mail, mail && archiveName(mail)]])),
      el("span", { class: "detail" },
        p.failure ? el("span", { class: "error" }, p.failure.op + ": " +
          p.failure.last_error) : null,
        state ? el("span", { class: "state " + p.status }, state) : null,
        chips,
        historyButton)),
    history);
}

function renderSeries(s) {
  const pwSeries = s.web_url || index.patchwork_web + "/project/" + index.project +
    "/list/?series=" + s.id + "&state=*&archive=both";
  const branch = s.branch
    ? "https://" + index.git_host + "/" + index.test_repo + "/tree/" + s.branch
    : null;
  const [cls, label, explanation] = seriesState(s);
  const runs = (s.patches || []).flatMap(p => p.runs || []);
  const head = el("div", { class: "head" },
    el("span", { class: "muted num" }, "#" + s.id),
    el("span", { class: "name" }, s.name || "(details not fetched yet)", " ",
      links([[pwSeries, "pw"], [branch, "branch"]]), " ",
      el("span", { class: "state " + cls, title: explanation || null }, label)),
    el("span", { class: "right" },
      el("span", { class: "muted who" },
        (s.submitter && (s.submitter.name || s.submitter.email)) || ""),
      counts(countChecks(runs)),
      when(s.finished || s.started || s.discovered)));
  const info = [];
  // The state already shows the reason of skipped, failed and waiting
  // series.
  if (s.reason && !["skipped", "error", "waiting"].includes(s.status)) info.push(s.reason);
  if (s.base) {
    info.push("base: " + (s.base.branch || "") + (s.base.why ? " (" + s.base.why + ")" : ""));
  }
  if (s.hold) info.push("on hold");
  const [historyButton, history] = decisions(s.decisions);
  const box = el("div", { class: open.has(s.id) ? "series open" : "series" }, head,
    el("div", { class: "body" },
      el("div", { class: "info" }, info.map((t, i) => [i ? " · " : "", t]),
        s.failure ? el("span", { class: "error" }, " " + s.failure.op + ": " +
          s.failure.last_error + " (" + s.failure.attempts + " attempts)") : null,
        historyButton),
      history,
      (s.patches || []).map(p => renderPatch(s, p))));
  head.onclick = e => {
    if (e.target.closest && e.target.closest("a, button")) return;
    if (box.classList.toggle("open")) open.add(s.id);
    else open.delete(s.id);
  };
  return box;
}

function filtered() {
  const q = document.getElementById("filter").value.trim().toLowerCase();
  const st = document.getElementById("status").value;
  return current.series.filter(s => {
    if (st && s.status !== st) return false;
    if (!q) return true;
    const sub = s.submitter || {};
    const patches = (s.patches || []).map(p => "pw" + p.id);
    return [String(s.id), s.name, sub.name, sub.email].concat(patches)
      .some(v => v && v.toLowerCase().includes(q));
  });
}

function render() {
  const list = filtered();
  const pages = Math.max(1, Math.ceil(list.length / PAGE_SIZE));
  current.page = Math.min(current.page, pages - 1);
  const main = document.getElementById("list");
  main.replaceChildren();
  if (!list.length) main.append(el("p", { class: "muted" }, "Nothing here."));
  for (const s of list.slice(current.page * PAGE_SIZE, (current.page + 1) * PAGE_SIZE)) {
    main.append(renderSeries(s));
  }
  const pager = document.getElementById("pager");
  pager.replaceChildren();
  if (pages > 1) {
    pager.append(
      el("button", {
        onclick: () => { current.page--; render(); },
        disabled: current.page === 0,
      }, "‹"),
      " " + (current.page + 1) + " / " + pages + " ",
      el("button", {
        onclick: () => { current.page++; render(); },
        disabled: current.page >= pages - 1,
      }, "›"));
  }
}

function markActive() {
  for (const b of document.querySelectorAll("nav button")) {
    b.classList.toggle("active", b.dataset.tab === current.tab);
  }
}

async function selectTab(tab, keepPage) {
  current.tab = tab;
  if (!keepPage) current.page = 0;
  location.hash = tab;
  markActive();
  const path = tab === "queue" ? "data/queue.json" : "data/history/" + tab + ".json";
  if (!cache.has(path)) {
    document.getElementById("list").replaceChildren(el("p", { class: "muted" }, "Loading..."));
    try {
      const data = await getJSON(path);
      cache.set(path, (data.series || []).slice().sort((a, b) => b.id - a.id));
    } catch (e) {
      if (current.tab === tab) {
        current.series = [];
        document.getElementById("list").replaceChildren(
          el("p", { class: "error" }, e.message));
      }
      return;
    }
  }
  // Another tab may have been selected while this one was loading.
  if (current.tab !== tab) return;
  current.series = cache.get(path);
  render();
}

async function loadIndex() {
  index = await getJSON("data/index.json");
  document.title = index.title;
  document.getElementById("title").textContent = index.title;
  const mode = index.mode;
  document.getElementById("meta").replaceChildren(
    "Updated " + age(index.generated) + " · ",
    link(index.patchwork_web + "/project/" + index.project + "/list/", "patchwork"), " · ",
    link("https://" + index.git_host + "/" + index.test_repo, index.test_repo),
    ...(index.run_url ? [" · ", link(index.run_url, "last run")] : []),
    " · push " + mode.push + ", checks " + mode.checks + ", email " + mode.email);
  const tabs = document.getElementById("tabs");
  tabs.replaceChildren();
  const add = (tab, text) => {
    const b = el("button", { onclick: () => selectTab(tab) }, text);
    b.dataset.tab = tab;
    tabs.append(b);
  };
  add("queue", "In progress (" + index.queue + ")");
  for (const m of index.months) add(m.month, m.month + " (" + m.series + ")");
  markActive();
}

function knownTab(tab) {
  return tab === "queue" || index.months.some(m => m.month === tab);
}

async function refresh() {
  const button = document.getElementById("refresh");
  button.disabled = true;
  try {
    cache.clear();
    await loadIndex();
    const tab = knownTab(current.tab) ? current.tab : "queue";
    await selectTab(tab, tab === current.tab);
  } catch (e) {
    document.getElementById("list").replaceChildren(el("p", { class: "error" }, e.message));
  } finally {
    button.disabled = false;
  }
}

async function main() {
  await loadIndex();
  document.getElementById("refresh").onclick = refresh;
  document.getElementById("filter").oninput = () => { current.page = 0; render(); };
  document.getElementById("status").onchange = () => { current.page = 0; render(); };
  const wanted = location.hash.slice(1);
  await selectTab(knownTab(wanted) ? wanted : "queue");
}

main().catch(e => {
  document.getElementById("list").replaceChildren(el("p", { class: "error" }, e.message));
});

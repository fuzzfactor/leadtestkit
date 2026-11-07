// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

"use strict";

const PAGE_SIZE = 50;
let index = null;
let current = { tab: "queue", series: [], page: 0 };

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

function runState(r) {
  if (r.result) return r.result;
  if (r.status === "completed") return "warning";
  if (!r.status || r.status === "requested" || r.status === "dispatching") return "queued";
  return r.status === "in_progress" ? "running" : r.status;
}

function runLabel(r) {
  if (r.kind === "apply") return r.workflow || "apply";
  if (r.kind === "missing") return (r.workflow || "workflows") + " (missing)";
  return r.workflow || r.file || "workflow";
}

function archiveLink(msgid, format) {
  format = format || index.archive_url_format;
  if (!format || !msgid) return null;
  return format.replace("{}", encodeURIComponent(msgid.replace(/^<|>$/g, "")));
}

function renderDecisions(decisions) {
  if (!decisions || !decisions.length) return null;
  const rows = decisions.slice().reverse().map(d => el("li", {},
    el("time", { datetime: d.time }, d.time), " · ",
    el("strong", {}, d.action.replace(/-/g, " ")), ": ", d.detail, " ",
    d.run_url ? link(d.run_url, "bot run logs") : null));
  return el("details", { class: "decisions" },
    el("summary", {}, "Decision history (" + decisions.length + ")"),
    el("ol", {}, rows));
}

function renderPatch(s, p) {
  const runs = el("div", { class: "runs" });
  for (const r of p.runs || []) {
    const st = runState(r);
    const title = [r.conclusion, r.details, r.failure && r.failure.last_error]
      .filter(Boolean).join("\n");
    const badge = el("span", { class: "badge " + st, title: title || null },
      runLabel(r) + ": " + st);
    runs.append(link(r.url, badge));
    const report = archiveLink(r.final && r.final.message_id);
    if (report) runs.append(link(report, "report"));
  }
  const pwLink = index.patchwork_web + "/patch/" + p.id + "/";
  return el("div", { class: "patch" },
    el("span", { class: "badge " + p.status }, p.status), " ",
    link(pwLink, "pw" + p.id), " ",
    el("span", { class: "pname" }, p.name || ""),
    ...(archiveLink(p.msgid, index.patch_archive_url_format)
      ? [" ", link(archiveLink(p.msgid, index.patch_archive_url_format), "email")] : []),
    p.sha ? el("span", { class: "muted" }, " " + p.sha.slice(0, 12)) : null,
    p.failure ? el("div", { class: "error" }, p.failure.op + ": " + p.failure.last_error) : null,
    runs, renderDecisions(p.decisions));
}

function renderSeries(s) {
  const pwSeries = s.web_url || index.patchwork_web + "/project/" + index.project +
    "/list/?series=" + s.id + "&state=*&archive=both";
  const branch = s.branch
    ? "https://" + index.git_host + "/" + index.test_repo + "/tree/" + s.branch
    : null;
  const head = el("div", { class: "head" },
    el("span", { class: "badge " + s.status }, s.status),
    link(pwSeries, "#" + s.id),
    el("span", { class: "name" }, s.name || "(details not fetched yet)"),
    el("span", { class: "muted" }, (s.submitter && (s.submitter.name || s.submitter.email)) || ""),
    el("span", { class: "muted" }, age(s.finished || s.started || s.discovered)));
  const info = [];
  if (s.reason) info.push(s.reason);
  if (s.base) {
    info.push("base: " + (s.base.branch || "") + (s.base.why ? " (" + s.base.why + ")" : ""));
  }
  if (s.hold) info.push("on hold");
  const box = el("div", { class: "series" }, head,
    el("div", { class: "info" }, ...info.map((t, i) => (i ? " · " : "") + t),
      branch ? [" · ", link(branch, s.branch)] : []),
    s.failure ? el("div", { class: "info error" }, s.failure.op + ": " + s.failure.last_error +
      " (" + s.failure.attempts + " attempts)") : null,
    el("div", { class: "patches" }, renderDecisions(s.decisions),
      ...(s.patches || []).map(p => renderPatch(s, p))));
  head.onclick = () => box.classList.toggle("open");
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

async function selectTab(tab) {
  current.tab = tab;
  current.page = 0;
  location.hash = tab;
  for (const b of document.querySelectorAll("nav button")) {
    b.classList.toggle("active", b.dataset.tab === tab);
  }
  document.getElementById("list").replaceChildren(el("p", { class: "muted" }, "Loading..."));
  const path = tab === "queue" ? "data/queue.json" : "data/history/" + tab + ".json";
  try {
    const data = await getJSON(path);
    current.series = (data.series || []).slice().sort((a, b) => b.id - a.id);
  } catch (e) {
    current.series = [];
    document.getElementById("list").replaceChildren(el("p", { class: "error" }, e.message));
    return;
  }
  render();
}

async function main() {
  index = await getJSON("data/index.json");
  document.title = index.title;
  document.getElementById("title").textContent = index.title;
  const mode = index.mode;
  document.getElementById("meta").append(
    "Updated " + age(index.generated) + " · ",
    link(index.patchwork_web + "/project/" + index.project + "/list/", "patchwork"), " · ",
    link("https://" + index.git_host + "/" + index.test_repo, index.test_repo),
    ...(index.run_url ? [" · ", link(index.run_url, "last run")] : []),
    " · push " + mode.push + ", checks " + mode.checks + ", email " + mode.email);
  const tabs = document.getElementById("tabs");
  const add = (tab, text) => {
    const b = el("button", { onclick: () => selectTab(tab) }, text);
    b.dataset.tab = tab;
    tabs.append(b);
  };
  add("queue", "In progress (" + index.queue + ")");
  for (const m of index.months) add(m.month, m.month + " (" + m.series + ")");
  document.getElementById("filter").oninput = () => { current.page = 0; render(); };
  document.getElementById("status").onchange = () => { current.page = 0; render(); };
  const wanted = location.hash.slice(1);
  const known = wanted === "queue" || index.months.some(m => m.month === wanted);
  await selectTab(known ? wanted : "queue");
}

main().catch(e => {
  document.getElementById("list").replaceChildren(el("p", { class: "error" }, e.message));
});

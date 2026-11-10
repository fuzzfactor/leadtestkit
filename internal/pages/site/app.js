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

// The Patchwork result of a GitHub conclusion; the result differs when
// the bot made a successful run a warning.
function conclusionResult(c) {
  return c === "success" ? "success" : c === "failure" ? "fail" : "warning";
}

const VERSION = /^v(\d+)$/i;
const POSITION = /^\d+\/\d+$/;

// tags splits "[ovs-dev,v2,1/3] subject" into its tags and the subject.
function tags(name) {
  const m = /^\[([^\]]*)\]\s*(.*)$/.exec(name || "");
  if (!m) return [[], name || ""];
  return [m[1].split(",").map(t => t.trim()).filter(Boolean), m[2]];
}

// topics are the tags of a subject that say what it is for, like "net"
// or "RFC", without the list's tag, the version and the position.
function topics(name) {
  return tags(name)[0].filter(t =>
    t !== index.list_tag && !VERSION.test(t) && !POSITION.test(t));
}

// common lists the topics all patches of a series share; the series
// shows them once.
function common(s) {
  const names = (s.patches || []).map(p => p.name).filter(Boolean);
  if (!names.length) return topics(s.name);
  return topics(names[0]).filter(t => names.every(n => topics(n).includes(t)));
}

// version shows the version of a series unless it is an implied v1.
function version(s) {
  const named = [s.name].concat((s.patches || []).map(p => p.name))
    .some(n => tags(n)[0].some(t => VERSION.test(t)));
  return s.version > 1 || (named && s.version) ? "v" + s.version : "";
}

// subject leaves out the tags that the series line shows.
function subject(name, shown) {
  const [list, rest] = tags(name);
  const kept = list.filter(t =>
    t !== index.list_tag && !VERSION.test(t) && !shown.includes(t));
  return kept.length ? "[" + kept.join(",") + "] " + rest : rest;
}

function person(p) {
  if (!p) return "";
  return p.name && p.email ? p.name + " <" + p.email + ">" : p.name || p.email || "";
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

// ago shows how long ago something happened, like "3h ago", and span
// how long something has lasted, like "3h".  tick keeps both current.
function ago(ts, cls) {
  return el("time", { class: cls, datetime: ts, "data-age": "ago",
    title: new Date(ts).toLocaleString() + " (" + ts + ")" }, age(ts));
}

function span(ts) {
  return el("time", { datetime: ts, "data-age": "span",
    title: new Date(ts).toLocaleString() + " (" + ts + ")" },
  age(ts).replace(/ ago$/, ""));
}

function tick() {
  for (const t of document.querySelectorAll("time[data-age]")) {
    const text = age(t.getAttribute("datetime"));
    t.textContent = t.dataset.age === "span" ? text.replace(/ ago$/, "") : text;
  }
  if (index) showProblems();
  if (current.tab === "availability") renderAvailability();
}

function when(ts) {
  if (!ts) return el("span", { class: "muted when" });
  return ago(ts, "muted when");
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

// stamp shows a time as local "YYYY-MM-DD HH:MM", for logs of events.
function stamp(ts) {
  const d = new Date(ts);
  const pad = n => String(n).padStart(2, "0");
  return el("time", { class: "muted", datetime: ts, title: ts },
    d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " +
    pad(d.getHours()) + ":" + pad(d.getMinutes()));
}

function decisions(list) {
  if (!list || !list.length) return [null, null];
  const rows = list.map(d => el("li", {},
    stamp(d.time), " ", el("strong", {}, d.action.replace(/-/g, " ")), ": ", d.detail, " ",
    links([[d.workflow_url, "workflow run"], [d.run_url, "bot run logs"]])));
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

function renderPatch(s, p, shown, ver) {
  const runs = p.runs || [];
  // A badge leads to its report when it was sent, which links the run,
  // and to the run otherwise.  Its color tells the result.
  const chips = runs.map(r => {
    const st = result(r);
    const report = archiveLink(r.final && r.final.message_id);
    // Attempts after the first are reruns.
    const times = r.attempt > 1 ? " ×" + r.attempt : "";
    const title = [runLabel(r) + ": " + st,
      times ? "Ran " + r.attempt + " times, " + (r.attempt - 1) +
        (r.attempt > 2 ? " reruns." : " rerun.") : null,
      r.conclusion && conclusionResult(r.conclusion) !== st
        ? "GitHub conclusion: " + r.conclusion : null,
      r.details, r.failure && r.failure.last_error,
      r.check_failure && "Pending check: " + r.check_failure.last_error,
      report ? "Opens the report." : r.url ? "Opens the workflow run." : ""]
      .filter(Boolean).join("\n");
    return link(report || r.url, el("span", { class: "badge " + st, title,
      "aria-label": runLabel(r) + times + ": " + st }, runLabel(r) + times));
  });
  const mail = archiveLink(p.msgid, index.patch_archive_url_format);
  const [historyButton, history] = decisions(p.decisions);
  const state = patchState(s, p);
  return el("div", { class: "patch" },
    el("div", { class: "row" },
      el("span", { class: "pname", title: p.name || null },
        el("span", { class: "muted pid" }, "#" + p.id), " ",
        ver ? [el("span", { class: "muted pid" }, ver), " "] : null,
        subject(p.name, shown) || "", " ",
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
  const shown = common(s);
  // Patchwork names series with a cover letter after it, without tags.
  const cover = s.cover_id && s.total > 1 ? "[0/" + s.total + "] " : "";
  const who = person(s.submitter);
  const head = el("div", { class: "head" },
    el("span", { class: "muted num" }, "#" + s.id),
    el("span", { class: "muted ver" }, version(s)),
    el("span", { class: "name", title: s.name || null },
      s.name ? cover + subject(s.name, shown) : "(details not fetched yet)", " ",
      links([[pwSeries, "pw"], [branch, "branch"]]), " ",
      el("span", { class: "state " + cls, title: explanation || null }, label)),
    el("span", { class: "right" },
      el("span", { class: "muted topics" }, shown.join(" ")),
      el("span", { class: "muted who", title: who || null },
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
      (s.patches || []).map(p => renderPatch(s, p, shown, version(s)))));
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
  document.getElementById("show-availability").classList.toggle("active",
    current.tab === "availability");
}

// alertIcon is an exclamation mark in an outlined circle for an outage,
// tinted like the badges, or in an outlined triangle otherwise.
function alertIcon(kind) {
  const svg = (tag, attrs) => {
    const e = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
    return e;
  };
  const icon = svg("svg", { class: "alert " + kind, viewBox: "0 0 16 16",
    "aria-hidden": "true" });
  if (kind === "outage") {
    icon.append(svg("circle", { cx: 8, cy: 8, r: 6.8, class: "frame" }),
      svg("path", { d: "M8 4.6v4.2", class: "mark" }),
      svg("circle", { cx: 8, cy: 11.3, r: 0.95, class: "dot" }));
  } else {
    icon.append(svg("path", { d: "M8 1.9L14.9 14H1.1Z", class: "frame" }),
      svg("path", { d: "M8 6.2v3.6", class: "mark" }),
      svg("circle", { cx: 8, cy: 11.9, r: 0.9, class: "dot" }));
  }
  return icon;
}

// availabilityButton marks the button when a service is unavailable:
// red during an outage, yellow otherwise.
function availabilityButton() {
  const outages = Object.keys(index.outages || {}).length;
  const down = (index.unavailable || []).map(s => SERVICES[s] || s);
  const kind = outages ? "outage" : down.length ? "short" : "";
  document.getElementById("show-availability").replaceChildren(
    kind ? alertIcon(kind) : null, "Availability");
  document.getElementById("show-availability").title = down.length
    ? "Unavailable now: " + down.join(", ") + (outages ? " (outage)" : "")
    : "When the services the bot uses were unavailable";
}

async function selectTab(tab, keepPage) {
  current.tab = tab;
  if (!keepPage) current.page = 0;
  location.hash = tab;
  markActive();
  document.getElementById("controls").hidden = tab === "availability";
  if (tab === "availability") return showAvailability();
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
    "Updated ", ago(index.generated), " · ",
    link(index.patchwork_web + "/project/" + index.project + "/list/", "patchwork"), " · ",
    link("https://" + index.git_host + "/" + index.test_repo, index.test_repo),
    ...(index.run_url ? [" · ", link(index.run_url, "last run")] : []),
    " · push " + mode.push + ", checks " + mode.checks + ", email " + mode.email);
  showProblems();
  const tabs = document.getElementById("tabs");
  tabs.replaceChildren();
  const add = (tab, text) => {
    const b = el("button", { onclick: () => selectTab(tab) }, text);
    b.dataset.tab = tab;
    tabs.append(b);
  };
  add("queue", "In progress (" + index.queue + ")");
  for (const m of index.months) add(m.month, m.month + " (" + m.series + ")");
  availabilityButton();
  markActive();
}

// showProblems shows outages and whether the bot seems to have stopped.
function showProblems() {
  const problems = Object.entries(index.outages || {}).sort().map(([kind, o]) =>
    el("div", {}, outageText(kind, o)));
  if (index.run_failed) {
    problems.unshift(el("div", {}, "The last bot run failed; see ",
      link(index.run_url, "its log"), "."));
  }
  const stale = (Date.now() - Date.parse(index.generated)) / 1000;
  if (index.stale_after && stale > index.stale_after) {
    problems.unshift(el("div", {}, "Not updated for ", span(index.generated),
      ": the bot may not be running."));
  }
  const outage = document.getElementById("outage");
  outage.hidden = !problems.length;
  outage.replaceChildren(...problems);
}

function outageText(kind, o) {
  const reason = ": " + o.reason.replace(/\.$/, "") + ". ";
  switch (kind) {
  case "runs":
  case "dispatch":
    return ["Workflows have not run on ", index.test_repo, " for ", span(o.since), reason,
      "New patches wait until they run again."];
  case "github":
    return ["GitHub has refused the GitHub App of the bot for ", span(o.since), reason,
      "Testing waits until it accepts it again."];
  case "email":
    return ["Emails could not be sent for ", span(o.since), reason,
      "Reports wait until they can be sent."];
  case "reports-archive":
    return ["The reports archive could not be read for ", span(o.since), reason,
      "Final Patchwork checks wait until it can be read."];
  case "patchwork":
    if (o.checks) {
      return ["Patchwork has refused the checks of the bot for ", span(o.since), reason,
        "Checks wait until it accepts them."];
    }
    return ["Patchwork has been unavailable for ", span(o.since), reason,
      "New series, recheck requests and checks wait until it is available."];
  case "dev-archive":
    return ["The dev list archive could not be read for ", span(o.since), reason,
      "Recheck requests wait until it can be read."];
  default:
    return [kind, " has been unavailable for ", span(o.since), reason];
  }
}

function knownTab(tab) {
  return tab === "queue" || tab === "availability" ||
    index.months.some(m => m.month === tab);
}

const SERVICES = {
  "patchwork": "Patchwork",
  "patchwork-checks": "Patchwork checks",
  "github": "GitHub",
  "workflows": "Workflows",
  "email": "Email",
  "reports-archive": "Reports archive",
  "dev-archive": "Dev list archive",
};
// Always shown; the others only once they were unavailable.
const MAIN_SERVICES = ["patchwork", "github", "workflows", "email"];
const WINDOWS = [["Last hour", 3600], ["Last 24 hours", 86400],
  ["Last 7 days", 7 * 86400], ["Last 30 days", 30 * 86400]];
const TIMELINE_DAYS = 30;

// duration shows seconds like "2d 3h", "3h 5m" or "12m".
function duration(s) {
  if (s < 60) return s > 0 ? "<1m" : "0";
  const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600),
    m = Math.floor(s % 3600 / 60);
  if (d) return d + "d" + (h ? " " + h + "h" : "");
  if (h) return h + "h" + (m ? " " + m + "m" : "");
  return m + "m";
}

// periods lists the periods of unavailability, the ongoing ones last,
// with times in seconds; an ongoing one lasts until now.
function periods(data) {
  const now = Date.now() / 1000;
  return (data.periods || []).concat(Object.values(data.failing || {})).map(p => ({
    ...p, from: Date.parse(p.since) / 1000,
    to: p.until ? Date.parse(p.until) / 1000 : now,
  }));
}

function downtime(list, service, seconds) {
  const now = Date.now() / 1000, start = now - seconds;
  return list.filter(p => p.service === service).reduce((sum, p) =>
    sum + Math.max(0, Math.min(p.to, now) - Math.max(p.from, start)), 0);
}

async function showAvailability() {
  const path = "data/availability.json";
  if (!cache.has(path)) {
    document.getElementById("list").replaceChildren(el("p", { class: "muted" }, "Loading..."));
    try {
      cache.set(path, await getJSON(path));
    } catch (e) {
      if (current.tab === "availability") {
        document.getElementById("list").replaceChildren(
          el("p", { class: "error" }, e.message));
      }
      return;
    }
  }
  if (current.tab === "availability") renderAvailability();
}

function renderAvailability() {
  const data = cache.get("data/availability.json");
  document.getElementById("pager").replaceChildren();
  if (!data) return;
  const list = periods(data);
  const services = MAIN_SERVICES.concat(Object.keys(SERVICES).filter(s =>
    !MAIN_SERVICES.includes(s) && list.some(p => p.service === s)));
  const now = Date.now() / 1000, start = now - TIMELINE_DAYS * 86400;
  const summary = el("table", { class: "avail" },
    el("tr", {}, el("th", {}, "Service"), el("th", {}, "Now"),
      WINDOWS.map(([name]) => el("th", {}, name)), el("th", {}, TIMELINE_DAYS + " days")),
    services.map(service => {
      const failing = (data.failing || {})[service];
      return el("tr", {},
        el("td", {}, SERVICES[service] || service),
        el("td", { class: failing ? "down" : "up" },
          failing ? ["unavailable for ", span(failing.since)] : "available"),
        WINDOWS.map(([, seconds]) => {
          const down = downtime(list, service, seconds);
          return el("td", { class: down ? "num down" : "num muted",
            title: (100 - 100 * down / seconds).toFixed(2) + "% available" },
          duration(down));
        }),
        el("td", { class: "timeline" }, el("div", { class: "bar" },
          list.filter(p => p.service === service && p.to > start).map(p => {
            const from = Math.max(p.from, start);
            return el("span", { class: p.outage ? "outage" : "short",
              style: "left: " + (100 * (from - start) / (now - start)) + "%; width: " +
                (100 * (p.to - from) / (now - start)) + "%",
              title: periodText(p) });
          }))));
    }));
  const rows = list.slice().sort((a, b) => b.from - a.from).map(p => el("tr", {},
    el("td", {}, SERVICES[p.service] || p.service),
    el("td", {}, stamp(p.since)),
    el("td", {}, p.until ? stamp(p.until) : "ongoing"),
    el("td", { class: "num" }, duration(p.to - p.from)),
    el("td", {}, p.outage ? "outage" : ""),
    el("td", { class: "reason" }, p.reason)));
  document.getElementById("list").replaceChildren(el("div", { class: "wide" }, summary),
    el("p", { class: "muted" }, "Times are as the bot saw them: from the first request ",
      "that failed to the first that worked, as precise as its polling cycles.  Periods ",
      "that became outages are shown darker.  The bot keeps them for 90 days."),
    rows.length ? el("div", { class: "wide" }, el("table", { class: "avail periods" },
      el("tr", {}, ["Service", "From", "To", "Duration", "", "Reason"].map(h =>
        el("th", {}, h))), rows)) : el("p", { class: "muted" }, "No unavailability recorded."));
}

function periodText(p) {
  return (SERVICES[p.service] || p.service) + (p.outage ? " outage" : " unavailable") +
    ", " + new Date(p.since).toLocaleString() + " to " +
    (p.until ? new Date(p.until).toLocaleString() : "now") + " (" +
    duration(p.to - p.from) + "): " + p.reason;
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
  document.getElementById("show-availability").onclick = () => selectTab("availability");
  document.getElementById("filter").oninput = () => { current.page = 0; render(); };
  document.getElementById("status").onchange = () => { current.page = 0; render(); };
  setInterval(tick, 30000);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) tick();
  });
  const wanted = location.hash.slice(1);
  await selectTab(knownTab(wanted) ? wanted : "queue");
}

main().catch(e => {
  document.getElementById("list").replaceChildren(el("p", { class: "error" }, e.message));
});

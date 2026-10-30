// Dashboard client: a small single-page app with hash routes (#/websites,
// #/jobs/<id>, ...). Uses textContent only (never innerHTML) so data from
// monitored sites can never inject markup. Served under a strict CSP.
"use strict";

const CHECKS = ["availability", "tls", "cert", "headers", "api"];
const $ = (id) => document.getElementById(id);
let last = null;   // last /api/status
let me = null;     // {user, csrf_token, node_name}
let timer = null;
let route = { tab: "dashboard", sub: "", id: "", q: new URLSearchParams() };

// ---------- helpers ----------

// Every API call: same-origin cookie session + CSRF header for writes.
async function api(path, opts = {}) {
  const headers = Object.assign({ "X-Requested-With": "sitemonitor-ui" }, opts.headers || {});
  if (opts.method && opts.method !== "GET" && me && me.csrf_token) headers["X-CSRF-Token"] = me.csrf_token;
  const res = await fetch(path, Object.assign({}, opts, { headers, credentials: "same-origin", cache: "no-store" }));
  if (res.status === 401) { showLogin(); throw new Error("signed out"); }
  return res;
}
async function apiJSON(path) {
  const res = await api(path);
  if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
  return res.json();
}
async function errorText(res) {
  try { return (await res.json()).error || `HTTP ${res.status}`; } catch (_) { return `HTTP ${res.status}`; }
}

function el(tag, opts = {}, ...children) {
  const e = document.createElement(tag);
  if (opts.cls) e.className = opts.cls;
  if (opts.text !== undefined) e.textContent = opts.text;
  if (opts.title) e.title = opts.title;
  if (opts.href) e.href = opts.href;
  for (const c of children) if (c) e.append(c);
  return e;
}
const badge = (status, text) => el("span", { cls: "badge s-" + (status || "NONE"), text: text || status || "—" });
const fmtTime = (t) => (t ? new Date(t).toLocaleString() : "—");
const ago = (t) => {
  if (!t) return "never";
  const s = Math.round((Date.now() - new Date(t)) / 1000);
  return s < 60 ? s + "s ago" : s < 3600 ? Math.round(s / 60) + "m ago" : s < 86400 ? Math.round(s / 3600) + "h ago" : Math.round(s / 86400) + "d ago";
};
const pct = (v) => (v === null || v === undefined ? "—" : v.toFixed(2) + "%");
const isRole = (r) => me && { viewer: 1, operator: 2, admin: 3 }[me.user.role] >= { viewer: 1, operator: 2, admin: 3 }[r];
const kv = (pairs) => pairs.flatMap(([k, v]) => [el("dt", { text: k }), typeof v === "string" ? el("dd", { text: v }) : el("dd", {}, v)]);
function clickableRow(cells, onClick, title) {
  const tr = el("tr", { cls: "clickable", title: title || "" }, ...cells);
  tr.addEventListener("click", onClick);
  tr.tabIndex = 0;
  tr.addEventListener("keydown", (e) => { if (e.key === "Enter") onClick(); });
  return tr;
}
function emptyRow(table, cols, text) {
  $(table).tBodies[0].replaceChildren(el("tr", {}, Object.assign(el("td", { cls: "muted", text }), { colSpan: cols })));
}

// ---------- routing ----------

function parseHash() {
  const h = location.hash.replace(/^#\/?/, "");
  const [path, query] = h.split("?");
  const parts = path.split("/").filter(Boolean);
  const r = { tab: parts[0] || "dashboard", sub: "", id: "", q: new URLSearchParams(query || "") };
  if (r.tab === "jobs") {
    if (parts[1] === "add" || parts[1] === "history") r.sub = parts[1];
    else if (parts[1]) { r.sub = "detail"; r.id = parts[1]; }
    else r.sub = "running";
  }
  return r;
}

function go(hash) { if (location.hash !== hash) location.hash = hash; else onRoute(); }

function onRoute() {
  route = parseHash();
  const known = ["dashboard", "websites", "certificates", "incidents", "events", "clusters", "jobs"];
  if (!known.includes(route.tab)) route.tab = "dashboard";
  document.querySelectorAll(".view").forEach((v) => { v.hidden = v.dataset.view !== route.tab; });
  document.querySelectorAll("nav.tabs a").forEach((a) => a.classList.toggle("active", a.dataset.tab === route.tab));
  if (route.tab === "jobs") {
    document.querySelectorAll(".subview").forEach((v) => { v.hidden = v.dataset.subview !== route.sub; });
    document.querySelectorAll("nav.subtabs a").forEach((a) => a.classList.toggle("active", a.dataset.sub === route.sub));
  }
  // filters taken from the URL (dashboard "Open full list" links set them)
  if (route.tab === "websites") { $("f-status").value = route.q.get("status") || ""; $("f-region").value = route.q.get("region") || ""; }
  if (route.tab === "incidents") $("f-inc").value = route.q.get("status") || "";
  if (route.tab === "events") { $("f-ev-sev").value = route.q.get("sev") || ""; $("f-ev-q").value = route.q.get("q") || ""; }
  window.scrollTo(0, 0);
  renderCurrent();
}
window.addEventListener("hashchange", onRoute);

// ---------- data refresh ----------

async function refresh() {
  try {
    last = await apiJSON("api/status");
    renderHeader(last);
    await renderCurrent();
    $("updated").textContent = "updated " + new Date().toLocaleTimeString();
  } catch (err) {
    $("updated").textContent = "update failed: " + err.message;
  }
}

async function renderCurrent() {
  if (!last) return;
  switch (route.tab) {
    case "dashboard": return renderDashboard(last);
    case "websites": renderSites(last); return renderTLS(last);
    case "certificates": return renderCerts(last);
    case "incidents": return renderIncidents(last);
    case "events": return renderEvents(last);
    case "clusters": return renderCluster(last);
    case "jobs": return renderJobs();
  }
}

function renderHeader(d) {
  const c = d.cluster;
  const mb = $("mode-badge");
  mb.className = "badge s-" + (c.mode === "NORMAL" ? "OK" : c.mode === "TAKEOVER" ? "WARN" : "INFO");
  mb.textContent = c.mode + " · peer " + c.peer_status;
  $("whoami").textContent = `${c.node_name} · region ${c.region} · monitoring ${(c.owned_regions || []).join(" + ") || "nothing (standby)"}`;
  const open = d.summary.open_incidents;
  $("nav-incidents").textContent = open ? String(open) : "";
  $("nav-incidents").className = "count" + (d.summary.critical_incidents ? " crit" : open ? " warn" : "");
}

// ---------- dashboard: tiles, each selects a short list ----------

function siteStats(s, check) { return (s.stats || {})[check || "availability"]; }
function siteMessage(s) {
  const bad = CHECKS.map((c) => s.results[c]).filter((r) => r && r.status === s.status);
  return bad.length ? `${bad[0].check}: ${bad[0].message}` : "";
}
function siteRow(s) {
  const st = siteStats(s);
  return clickableRow([
    el("td", {}, el("span", { cls: "site-name", text: s.name }), el("span", { cls: "site-url", text: s.url })),
    el("td", {}, badge("INFO", s.region)),
    el("td", {}, badge(s.status || null, s.status || "pending")),
    el("td", { cls: "msg-cell", text: siteMessage(s) || "all checks OK" }),
    el("td", { text: st && st.uptime_24h !== null ? st.uptime_24h.toFixed(1) + "%" : "—" }),
  ], () => openSite(s), "Open the site's checks");
}
const SITE_HEAD = ["Site", "Region", "Status", "Problem", "Uptime 24h"];

const TILES = [
  { key: "websites", label: "Websites", value: (m) => m.websites, more: "#/websites",
    cls: () => "", title: (m) => (m.pending ? `${m.pending} without results yet` : ""),
    rows: (d) => d.sites, head: SITE_HEAD, row: siteRow },
  { key: "healthy", label: "Healthy", value: (m) => m.healthy, more: "#/websites?status=OK", cls: () => "ok",
    rows: (d) => d.sites.filter((s) => s.status === "OK"), head: SITE_HEAD, row: siteRow },
  { key: "warning", label: "Warning", value: (m) => m.warning, more: "#/websites?status=WARN", cls: (m) => (m.warning ? "warn" : ""),
    rows: (d) => d.sites.filter((s) => s.status === "WARN"), head: SITE_HEAD, row: siteRow },
  { key: "critical", label: "Critical", value: (m) => m.critical, more: "#/websites?status=CRIT", cls: (m) => (m.critical ? "crit" : ""),
    rows: (d) => d.sites.filter((s) => s.status === "CRIT"), head: SITE_HEAD, row: siteRow },
  { key: "certs", label: (m) => `Certs ≤ ${m.cert_warn_days} days`, value: (m) => m.certs_expiring, more: "#/certificates", cls: (m) => (m.certs_expiring ? "warn" : ""),
    rows: (d) => d.sites.filter((s) => { const c = siteStats(s, "cert"); return c && c.cert_days_left !== undefined && c.cert_days_left <= d.summary.cert_warn_days; })
      .sort((a, b) => siteStats(a, "cert").cert_days_left - siteStats(b, "cert").cert_days_left),
    head: ["Site", "Days left", "Expires", "Status"],
    row: (s) => { const c = siteStats(s, "cert"); const r = s.results.cert || { details: {} };
      return clickableRow([el("td", { cls: "site-name", text: s.name }), el("td", {}, badge(c.cert_days_left <= 7 ? "CRIT" : "WARN", String(c.cert_days_left))),
        el("td", { text: fmtTime(r.details.not_after) }), el("td", { cls: "msg-cell", text: r.message || "" })], () => showDetail(s, r)); } },
  { key: "availability", label: "Availability 24h", value: (m) => pct(m.availability_24h), more: "#/websites",
    cls: (m) => (m.availability_24h !== null && m.availability_24h < 99 ? "warn" : "ok"),
    rows: (d) => d.sites.filter((s) => siteStats(s)).sort((a, b) => (siteStats(a).uptime_24h ?? 101) - (siteStats(b).uptime_24h ?? 101)),
    head: ["Site", "Uptime 24h", "Requests", "Failed", "Uptime 30d"],
    row: (s) => { const st = siteStats(s);
      return clickableRow([el("td", { cls: "site-name", text: s.name }), el("td", {}, badge(st.uptime_24h === null ? null : st.uptime_24h >= 99 ? "OK" : st.uptime_24h >= 95 ? "WARN" : "CRIT", pct(st.uptime_24h))),
        el("td", { cls: "num", text: String(st.requests_24h) }), el("td", { cls: "num", text: String(st.failures_24h) }), el("td", { text: pct(st.uptime_30d) })], () => openSite(s)); } },
  { key: "requests", label: "Requests 24h", value: (m) => m.requests_24h, more: "#/websites", cls: () => "", title: (m) => `${m.failures_24h} failed`,
    rows: (d) => d.sites.map((s) => ({ s, n: Object.values(s.stats || {}).reduce((a, c) => a + c.requests_24h, 0), f: Object.values(s.stats || {}).reduce((a, c) => a + c.failures_24h, 0) }))
      .filter((x) => x.n > 0).sort((a, b) => b.n - a.n),
    head: ["Site", "Requests 24h", "Failed", "Checks"],
    row: (x) => clickableRow([el("td", { cls: "site-name", text: x.s.name }), el("td", { cls: "num", text: String(x.n) }), el("td", { cls: "num", text: String(x.f) }),
      el("td", { text: Object.keys(x.s.stats || {}).join(", ") })], () => openSite(x.s)),
    siteOf: (x) => x.s },
  { key: "failed", label: "Failed 24h", value: (m) => m.failures_24h, more: "#/events?sev=CRIT", cls: (m) => (m.failures_24h ? "crit" : ""),
    rows: (d) => d.sites.flatMap((s) => Object.values(s.stats || {}).filter((c) => c.failures_24h > 0).map((c) => ({ s, c }))).sort((a, b) => b.c.failures_24h - a.c.failures_24h),
    head: ["Site", "Check", "Failed 24h", "Last failure", "Now"],
    row: (x) => clickableRow([el("td", { cls: "site-name", text: x.s.name }), el("td", { text: x.c.check }), el("td", { cls: "num", text: String(x.c.failures_24h) }),
      el("td", { text: ago(x.c.last_failure) }), el("td", {}, badge((x.s.results[x.c.check] || {}).status || null))], () => showDetail(x.s, x.s.results[x.c.check] || { check: x.c.check, details: {} })),
    siteOf: (x) => x.s, eventSev: ["CRIT", "WARN"] },
  { key: "incidents", label: "Open incidents", value: (m) => m.open_incidents, more: "#/incidents?status=OPEN",
    cls: (m) => (m.critical_incidents ? "crit" : m.open_incidents ? "warn" : ""), title: (m) => `${m.critical_incidents} critical`,
    rows: (d) => (d.incidents || []).filter((i) => i.status === "OPEN"),
    head: ["Severity", "Problem", "Since", "Seen"],
    row: (i) => clickableRow([el("td", {}, badge(i.severity)), el("td", {}, el("span", { cls: "site-name", text: subject(i) }), el("span", { cls: "msg", text: i.message })),
      el("td", { text: ago(i.first_seen) }), el("td", { cls: "num", text: String(i.occurrences) })], () => showIncident(i)),
    eventFilter: (rows) => { const keys = new Set(rows.map((i) => i.key)); return (e) => keys.has(e.key); } },
];

function renderDashboard(d) {
  const m = d.summary;
  const selected = route.q.get("tile") || "";
  $("kpis").replaceChildren(...TILES.map((t) => {
    const label = typeof t.label === "function" ? t.label(m) : t.label;
    const b = el("button", { cls: "kpi " + (t.cls(m) || "") + (selected === t.key ? " selected" : ""), title: t.title ? t.title(m) : "" },
      el("div", { cls: "v", text: String(t.value(m)) }), el("div", { cls: "l", text: label }));
    b.setAttribute("aria-pressed", selected === t.key ? "true" : "false");
    b.addEventListener("click", () => go(selected === t.key ? "#/" : "#/?tile=" + t.key));
    return b;
  }));
  const tile = TILES.find((t) => t.key === selected);
  $("selection").hidden = !tile;
  $("sel-hint").hidden = !!tile;
  if (!tile) return;
  const rows = tile.rows(d);
  const label = typeof tile.label === "function" ? tile.label(m) : tile.label;
  $("sel-title").textContent = `${label} — ${rows.length} item${rows.length === 1 ? "" : "s"}`;
  $("sel-more").href = tile.more;
  $("sel-table").tHead.replaceChildren(el("tr", {}, ...tile.head.map((h) => el("th", { text: h }))));
  if (rows.length) $("sel-table").tBodies[0].replaceChildren(...rows.slice(0, 25).map(tile.row));
  else emptyRow("sel-table", tile.head.length, "nothing here");
  // events related to the selected items
  let match;
  if (tile.eventFilter) match = tile.eventFilter(rows);
  else {
    const names = new Set(rows.map((r) => (tile.siteOf ? tile.siteOf(r) : r).name));
    match = (e) => names.has(e.site) && (!tile.eventSev || tile.eventSev.includes(e.severity));
  }
  const evs = d.events.filter(match).slice(0, 15);
  $("sel-events").replaceChildren(...(evs.length ? evs.map(eventItem) : [el("li", { cls: "muted", text: "no recent events for these items" })]));
}

function openSite(s) {
  const r = (s.status && CHECKS.map((c) => s.results[c]).find((x) => x && x.status === s.status)) || s.results.availability;
  if (r) showDetail(s, r); else go("#/websites");
}

// ---------- websites, certificates, incidents, events, clusters ----------

function checkCell(site, check) {
  if (!site.checks.includes(check)) return el("td", { cls: "muted", text: "n/a" });
  const r = site.results[check];
  if (!r) return el("td", {}, badge(null, "pending"));
  const td = el("td", { cls: "clickable", title: "Click for details" }, badge(r.status), el("span", { cls: "msg", text: r.message }));
  td.addEventListener("click", () => showDetail(site, r));
  return td;
}

function uptimeCell(s) {
  const st = siteStats(s);
  if (!st || st.uptime_24h === null) return el("td", { text: "—" });
  return el("td", { title: `30d: ${pct(st.uptime_30d)} of ${st.requests_30d}\nall time: ${st.ok} ok · ${st.warn} warn · ${st.crit} failed` },
    el("span", { text: st.uptime_24h.toFixed(1) + "%" }), el("span", { cls: "msg", text: `${st.requests_24h} req · ${st.failures_24h} failed` }));
}

function renderSites(d) {
  const fr = $("f-region").value, fs = $("f-status").value;
  const rows = d.sites.filter((s) => (!fr || s.region === fr) && (!fs || (fs === "PENDING" ? !s.status : s.status === fs))).map((s) => {
    const from = new Set(Object.values(s.results).map((r) => r.node_region));
    const fromTxt = [...from].join(", ") || "—";
    const failover = [...from].some((r) => r !== s.region);
    return el("tr", {},
      el("td", {}, el("span", { cls: "site-name", text: s.name }), el("span", { cls: "site-url", text: s.url })),
      el("td", {}, badge("INFO", s.region)),
      el("td", { cls: failover ? "failover" : "", text: failover ? fromTxt + " (failover)" : fromTxt }),
      ...CHECKS.map((c) => checkCell(s, c)),
      uptimeCell(s));
  });
  if (rows.length) $("sites").tBodies[0].replaceChildren(...rows); else emptyRow("sites", 9, "no websites match the filter");
}

function renderCerts(d) {
  const list = d.sites.filter((s) => s.results.cert && s.results.cert.details && s.results.cert.details.days_left !== undefined)
    .map((s) => ({ s, r: s.results.cert, days: Number(s.results.cert.details.days_left) })).sort((a, b) => a.days - b.days);
  if (!list.length) return emptyRow("certs", 5, "no certificate results yet");
  $("certs").tBodies[0].replaceChildren(...list.map(({ s, r, days }) => clickableRow([
    el("td", { cls: "site-name", text: s.name }),
    el("td", {}, badge(days <= 7 ? "CRIT" : days <= 30 ? "WARN" : "OK", String(days))),
    el("td", { text: fmtTime(r.details.not_after) }),
    el("td", { text: r.details.issuer || "—" }),
    el("td", { text: r.details.chain || "—" })], () => showDetail(s, r))));
}

function renderTLS(d) {
  const cell = (v, goodWhenEnabled) => {
    if (!v) return el("td", { text: "—" });
    const on = v === "enabled";
    return el("td", {}, badge(on === goodWhenEnabled ? "OK" : goodWhenEnabled ? "WARN" : "CRIT", on ? "on" : "off"));
  };
  const rows = d.sites.filter((s) => s.results.tls && s.results.tls.details).map((s) => {
    const t = s.results.tls.details;
    const weak = t.weak_cipher && t.weak_cipher !== "none accepted";
    return el("tr", {}, el("td", { text: s.name }), cell(t["TLS1.0"], false), cell(t["TLS1.1"], false), cell(t["TLS1.2"], true), cell(t["TLS1.3"], true),
      el("td", {}, badge(weak ? "WARN" : "OK", weak ? t.weak_cipher : "none")));
  });
  if (rows.length) $("tls").tBodies[0].replaceChildren(...rows); else emptyRow("tls", 6, "no TLS results yet");
}

function eventItem(e) {
  return el("li", {}, el("time", { text: fmtTime(e.time) }), badge(e.severity, e.kind),
    el("span", { text: `${e.site ? e.site + "/" + e.check + ": " : ""}${e.message} — ${e.node}` }));
}

function renderEvents(d) {
  const kinds = [...new Set(d.events.map((e) => e.kind))].sort();
  const sel = $("f-ev-kind");
  const cur = route.q.get("kind") || sel.value;
  sel.replaceChildren(el("option", { text: "All" }), ...kinds.map((k) => Object.assign(el("option", { text: k }), { value: k })));
  sel.firstChild.value = "";
  sel.value = kinds.includes(cur) ? cur : "";
  const sev = $("f-ev-sev").value, kind = sel.value, q = $("f-ev-q").value.trim().toLowerCase();
  const list = d.events.filter((e) => (!sev || e.severity === sev) && (!kind || e.kind === kind) &&
    (!q || `${e.site} ${e.check} ${e.message} ${e.node}`.toLowerCase().includes(q)));
  $("events").replaceChildren(...(list.length ? list.map(eventItem) : [el("li", { cls: "muted", text: "no events match the filter" })]));
}

const STATE_SEV = { RAISED: null, SEVERITY: null, CLEARED: "OK", REAPPEARED: "CRIT" };
function subject(inc) {
  if (inc.site) return `${inc.site} / ${inc.check}`;
  return inc.key.replace(/^[a-z-]+:/, (m) => m.slice(0, -1) + " ");
}

function renderIncidents(d) {
  const all = d.incidents || [];
  const open = all.filter((i) => i.status === "OPEN").length;
  $("incidents-summary").textContent = `${open} open · ${all.length - open} cleared`;
  const f = $("f-inc").value;
  const list = all.filter((i) => !f || i.status === f);
  if (!list.length) return emptyRow("incidents", 8, "no incidents");
  $("incidents").tBodies[0].replaceChildren(...list.map((inc) => clickableRow([
    el("td", {}, badge(inc.status === "OPEN" ? inc.severity : "OK", inc.status)),
    el("td", {}, el("span", { cls: "site-name", text: subject(inc) }), el("span", { cls: "msg", text: inc.message })),
    el("td", { text: fmtTime(inc.first_seen) }),
    el("td", { text: inc.status === "OPEN" ? ago(inc.last_seen) : "cleared " + fmtTime(inc.cleared_at) }),
    el("td", { cls: "num", text: String(inc.occurrences) }),
    el("td", { cls: "num", text: String(inc.cleared) }),
    el("td", { cls: "num", text: String(inc.reappeared) }),
    el("td", { text: (inc.seen_by || []).join(", ") })], () => showIncident(inc), "Click for full history")));
}

function renderCluster(d) {
  const c = d.cluster;
  const peerSev = { HEALTHY: "OK", SUSPECT: "WARN", DOWN: "CRIT" }[c.peer_status] || "NONE";
  const s = d.scheduler;
  const self = el("article", { cls: "node self" },
    el("header", {}, el("h3", { text: `${c.node_name} [${c.region}] (this server)` }), badge(c.mode === "NORMAL" ? "OK" : c.mode === "TAKEOVER" ? "WARN" : "INFO", c.mode)),
    el("dl", {}, ...kv([
      ["Server ID", c.node],
      ["Monitors regions", (c.owned_regions || []).join(", ") || "none"],
      ["Epoch", String(c.epoch)],
      ["Config", `${d.config.version} ${c.config_match ? "✔ matches peer" : "✖ differs from peer (" + (c.peer_config_version || "?") + ")"}`],
      ["Check tasks", `${s.jobs} scheduled · ${s.busy_workers}/${s.workers} running · ${s.queue_depth}/${s.queue_cap} waiting`],
      ["Check runs", `${s.completed} completed · ${s.dropped} dropped · ${s.skipped} skipped · ${s.shed} shed`],
      ["Replication", `${c.replication_queue} queued · lag ${c.replication_lag_seconds.toFixed(1)}s`],
      ["Witness", c.witness_enabled ? (c.witness_reachable ? "reachable" : "UNREACHABLE") : "not configured"],
    ])));
  const peer = el("article", { cls: "node" },
    el("header", {}, el("h3", { text: `${c.peer_name} [${c.peer_region}] (peer)` }), badge(peerSev, c.peer_status)),
    el("dl", {}, ...kv([
      ["Server ID", c.peer_id],
      ["Peer mode", c.peer_mode || "—"],
      ["Monitors regions", (c.peer_owned_regions || []).join(", ") || "none"],
      ["Last heartbeat", c.heartbeat_age_seconds < 0 ? "never" : c.heartbeat_age_seconds.toFixed(1) + "s ago"],
      ["Missed beats", String(c.missed_beats)],
      ["Last error", c.last_error || "—"],
      ...(c.takeover_blocked ? [["Takeover blocked", c.takeover_blocked]] : []),
    ])));
  $("cluster").replaceChildren(self, peer);
}

// ---------- detail dialogs ----------

function showIncident(inc) {
  $("detail-title").textContent = subject(inc);
  const body = kv([
    ["Status", inc.status], ["Severity", inc.severity], ["First seen", fmtTime(inc.first_seen)],
    ["Last seen", fmtTime(inc.last_seen)], ["Occurrences", String(inc.occurrences)],
    ["Cleared", `${inc.cleared} time(s)`], ["Reappeared", `${inc.reappeared} time(s)`],
    ["Seen by", (inc.seen_by || []).join(", ")],
  ]);
  body.push(el("dt", { text: "History" }), el("dd", {}, el("ol", { cls: "timeline" },
    ...inc.history.map((t) => el("li", {}, badge(STATE_SEV[t.state] || t.severity, t.state),
      el("time", { text: " " + fmtTime(t.time) + " " }), el("span", { text: `${t.message} — ${t.node}` }))))));
  $("detail-body").replaceChildren(...body);
  $("detail").showModal();
}

function showDetail(site, r) {
  $("detail-title").textContent = `${site.name} · ${r.check}`;
  const st = (site.stats || {})[r.check];
  const pairs = [
    ["Status", r.status || "—"], ["Message", r.message || "—"], ["Checked by", r.node ? `${r.node} (${r.node_region})` : "—"],
    ["When", r.time ? `${fmtTime(r.time)} (${ago(r.time)})` : "—"], ["Latency", r.latency_ms !== undefined ? r.latency_ms + " ms" : "—"],
    ...(st ? [
      ["Requests (all time)", `${st.total}: ${st.ok} ok · ${st.warn} warn · ${st.crit} failed`],
      ["Uptime 24h / 30d", `${pct(st.uptime_24h)} (${st.requests_24h} req) / ${pct(st.uptime_30d)} (${st.requests_30d} req)`],
      ["Latency avg / max", `${Math.round(st.avg_latency_ms)} ms / ${st.max_latency_ms} ms`],
      ["Consecutive failures", String(st.consecutive_failures)],
      ["Last success", st.last_success ? fmtTime(st.last_success) : "never"],
      ["Last failure", st.last_failure ? fmtTime(st.last_failure) : "never"],
    ] : []),
    ...Object.entries(r.details || {}).sort().map(([k, v]) => [k, String(v)]),
  ];
  $("detail-body").replaceChildren(...kv(pairs));
  $("detail").showModal();
}

// ---------- user jobs ----------
// All data and every action goes through the same API that agents use.

let jobOpts = null;
let jobsAvailable = false;
let jobsCache = { jobs: [], bins: [] };

async function initJobs() {
  const res = await api("api/jobs/options");
  jobsAvailable = res.ok;
  $("nav-jobs").hidden = !jobsAvailable;
  if (!jobsAvailable) return;
  jobOpts = await res.json();
  $("sub-add").hidden = !isRole("operator");
  const f = $("job-form");
  const opt = (v, t) => { const o = document.createElement("option"); o.value = v; o.textContent = t || v; return o; };
  f.every.replaceChildren(...jobOpts.intervals.map((v) => opt(v)));
  f.every.value = "5m";
  f.credential.replaceChildren(...(jobOpts.credentials.length ? jobOpts.credentials.map((v) => opt(v)) : [opt("", "(none configured by the admin)")]));
  f.region.value = (last && last.cluster && last.cluster.region) || "IDC";
  jobFormVisibility();
}

function scheduleText(j) {
  if (j.schedule.type === "startup") return "at server start";
  return "every " + j.schedule.every + (j.schedule.ends_at ? " until " + fmtTime(j.schedule.ends_at) : j.scope === "admin" ? " (no end)" : "");
}
function whereText(j) {
  return j.target ? `${j.target.host}:${j.target.port} (${j.target.os}, ${j.target.credential})` : `${j.region} monitor`;
}
const STATE_BADGE = { active: "OK", scheduled: "INFO", expired: "NONE", disabled: "NONE", pending: "WARN", rejected: "CRIT" };
const jobState = (j) => badge(STATE_BADGE[j.state] || "NONE", j.state);
const lastRunCell = (j) => el("td", {}, j.last_run ? badge(j.last_run.status, `exit ${j.last_run.details.exit_code}`) : el("span", { cls: "muted", text: "—" }),
  j.last_run ? el("span", { cls: "msg", text: ago(j.last_run.time) + " · " + j.last_run.message }) : null);

async function loadJobsData(withBins) {
  const [jobs, bins] = await Promise.all([apiJSON("api/jobs"), withBins ? apiJSON("api/binaries") : Promise.resolve(jobsCache.bins)]);
  jobsCache = { jobs, bins };
  const active = jobs.filter((j) => j.state === "active").length;
  $("nav-jobs-count").textContent = active ? String(active) : "";
  return jobsCache;
}

async function renderJobs() {
  if (!jobsAvailable) return;
  if (route.sub === "running") return renderRunning(await loadJobsData(false));
  if (route.sub === "add") return renderAdd(await loadJobsData(true));
  if (route.sub === "history") return renderHistory(await loadJobsData(false));
  if (route.sub === "detail") return renderJobDetail(route.id);
}

function renderRunning({ jobs }) {
  const scope = $("f-job-scope").value, mine = $("jobs-mine").checked, all = $("jobs-all").checked;
  const list = jobs.filter((j) => (all || j.state === "active" || j.state === "scheduled" || j.state === "pending") &&
    (!scope || j.scope === scope) && (!mine || j.owner === me.user.username));
  const merges = jobs.filter((j) => j.merge_request && j.merge_request.status === "pending").length;
  $("jobs-summary").textContent = `${list.length} shown · ${jobs.filter((j) => j.state === "active").length} active` + (merges ? ` · ${merges} merge request${merges > 1 ? "s" : ""} waiting` : "");
  if (!list.length) return emptyRow("jobs", 8, isRole("operator") ? "no running jobs — add one under “Add job & programs”" : "no running jobs");
  $("jobs").tBodies[0].replaceChildren(...list.map((j) => {
    const st = j.stats;
    const mr = j.merge_request && j.merge_request.status === "pending";
    return clickableRow([
      el("td", {}, el("span", { cls: "site-name", text: j.name }), el("span", { cls: "site-url", text: `${j.owner}${j.description ? " · " + j.description : ""}` })),
      el("td", {}, badge(j.scope === "admin" ? "INFO" : "NONE", j.scope), mr ? el("span", { cls: "msg pending-merge", text: "merge requested" }) : null),
      el("td", {}, jobState(j)),
      el("td", {}, el("span", { text: j.binary }), el("span", { cls: "msg", text: (j.args || []).join(" ") })),
      el("td", { text: scheduleText(j) }),
      el("td", { text: whereText(j) }),
      lastRunCell(j),
      el("td", { text: st ? `${st.ok + st.warn}/${st.total}` : "0", title: st ? `${st.ok} ok · ${st.warn} warn · ${st.crit} failed` : "" }),
    ], () => go("#/jobs/" + j.id), "Open the job");
  }));
}

function renderAdd({ bins }) {
  if (!isRole("operator")) return;
  const f = $("job-form");
  const cur = f.binary.value;
  f.binary.replaceChildren(...(bins.length ? bins.map((b) => { const o = document.createElement("option"); o.value = b.name; o.textContent = `${b.name} (${b.kind})`; return o; })
    : [Object.assign(document.createElement("option"), { value: "", textContent: "(upload a program first)" })]));
  if (cur && bins.some((b) => b.name === cur)) f.binary.value = cur;
  $("bins-summary").textContent = `(${bins.length})`;
  if (!bins.length) return emptyRow("bins", 6, "no programs uploaded yet");
  $("bins").tBodies[0].replaceChildren(...bins.map((b) => {
    const del = el("button", { text: "Delete", cls: "danger" });
    del.addEventListener("click", async () => {
      if (!confirm(`Delete ${b.name}?`)) return;
      const res = await api(`api/binaries/${encodeURIComponent(b.name)}`, { method: "DELETE" });
      if (!res.ok) alert(await errorText(res)); else renderJobs();
    });
    return el("tr", {}, el("td", { text: b.name }), el("td", { text: b.kind }), el("td", { text: (b.size / 1024).toFixed(1) + " KB" }),
      el("td", { text: b.sha256.slice(0, 16) + "…", title: b.sha256 }), el("td", { text: b.uploaded_by }),
      el("td", {}, isRole("admin") || b.uploaded_by === me.user.username ? del : null));
  }));
}

async function renderHistory({ jobs }) {
  const sel = $("f-run-job");
  const cur = sel.value;
  sel.replaceChildren(Object.assign(el("option", { text: "All" }), { value: "" }), ...jobs.map((j) => Object.assign(el("option", { text: j.name }), { value: j.name })));
  sel.value = jobs.some((j) => j.name === cur) ? cur : "";
  const params = new URLSearchParams({ limit: "200" });
  if ($("f-run-status").value) params.set("status", $("f-run-status").value);
  if (sel.value) params.set("job", sel.value);
  const runs = await apiJSON("api/jobs/runs?" + params);
  const byName = Object.fromEntries(jobs.map((j) => [j.name, j]));
  if (runs.length) {
    $("runs").tBodies[0].replaceChildren(...runs.map((r) => {
      const name = r.site.replace(/^job:/, "");
      const j = byName[name];
      return clickableRow([
        el("td", { text: fmtTime(r.time) }), el("td", { cls: "site-name", text: name }),
        el("td", {}, badge(r.status, `exit ${r.details.exit_code}`)), el("td", { text: r.details.trigger || "" }),
        el("td", { text: r.node }), el("td", { text: r.details.target || "local" }), el("td", { cls: "num", text: r.latency_ms + " ms" }),
        el("td", { cls: "msg-cell", text: r.message }),
      ], () => (j ? go("#/jobs/" + j.id) : showRun(r)), j ? "Open the job" : "Show the run");
    }));
  } else emptyRow("runs", 8, "no runs yet");
  const done = jobs.filter((j) => ["expired", "disabled", "rejected"].includes(j.state));
  if (!done.length) return emptyRow("jobs-done", 6, "no finished jobs");
  $("jobs-done").tBodies[0].replaceChildren(...done.map((j) => clickableRow([
    el("td", { cls: "site-name", text: j.name }), el("td", { text: j.scope }), el("td", {}, jobState(j)), el("td", { text: j.binary }),
    el("td", { text: j.schedule.ends_at ? fmtTime(j.schedule.ends_at) : "—" }), el("td", { text: j.stats ? String(j.stats.total) : "0" }),
  ], () => go("#/jobs/" + j.id))));
}

function showRun(r) {
  $("detail-title").textContent = `Run of ${r.site.replace(/^job:/, "")}`;
  $("detail-body").replaceChildren(...kv([["When", fmtTime(r.time)], ["Result", r.message], ...Object.entries(r.details || {}).map(([k, v]) => [k, String(v)])]));
  $("detail").showModal();
}

// ---------- job detail page ----------

async function jobPost(path, body) {
  const res = await api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  if (!res.ok) { alert(await errorText(res)); return null; }
  renderJobs();
  return res;
}

function jobButtons(j) {
  const mine = j.owner === me.user.username && j.scope === "user";
  const canEdit = isRole("admin") || (isRole("operator") && mine);
  const btn = (label, fn, cls) => { const b = el("button", { text: label, cls: cls || "" }); b.addEventListener("click", (ev) => fn(ev)); return b; };
  const out = [];
  const mr = j.merge_request && j.merge_request.status === "pending";
  if (canEdit && j.status === "approved") out.push(btn("Run now", async (ev) => {
    const b = ev.currentTarget; b.disabled = true; b.textContent = "Running…";
    await jobPost(`api/jobs/${j.id}/run?wait=true`);
  }, "primary"));
  if (canEdit && j.status === "approved") out.push(btn("Disable", () => jobPost(`api/jobs/${j.id}/disable`, { reason: prompt("Reason (optional)") || "" })));
  if (canEdit && j.status === "disabled") out.push(btn("Enable", () => jobPost(`api/jobs/${j.id}/enable`)));
  if (mine && isRole("operator") && !mr && j.scope === "user") out.push(btn("Request merge", () => {
    const note = prompt("Why should this become an admin periodic activity?");
    if (note !== null) jobPost(`api/jobs/${j.id}/merge-request`, { note });
  }));
  if (isRole("admin") && j.scope === "user") out.push(btn(mr ? "Approve merge" : "Merge into admin activities", () => jobPost(`api/jobs/${j.id}/merge`), mr ? "primary" : ""));
  if (isRole("admin") && mr) out.push(btn("Reject merge", () => {
    const reason = prompt("Reason for rejecting the merge");
    if (reason !== null) jobPost(`api/jobs/${j.id}/merge/reject`, { reason });
  }));
  if (isRole("admin") && j.status === "pending") out.push(btn("Approve", () => jobPost(`api/jobs/${j.id}/approve`)));
  if (isRole("admin") && j.last_run && /changed since approval/.test(j.last_run.message)) out.push(btn("Re-approve program", () => jobPost(`api/jobs/${j.id}/approve`)));
  if (canEdit) out.push(btn("Delete", async () => {
    if (!confirm(`Delete job ${j.name}?`)) return;
    const res = await api(`api/jobs/${j.id}`, { method: "DELETE" });
    if (!res.ok) alert(await errorText(res)); else go("#/jobs");
  }, "danger"));
  return out;
}

async function renderJobDetail(id) {
  let j, runs;
  try {
    [j, runs] = await Promise.all([apiJSON(`api/jobs/${id}`), apiJSON(`api/jobs/${id}/runs?limit=50`)]);
  } catch (_) {
    $("jd-title").textContent = "Job not found";
    ["jd-actions", "jd-def", "jd-stats", "jd-runs"].forEach((x) => $(x).replaceChildren());
    return;
  }
  $("jd-title").replaceChildren(el("span", { text: j.name + " " }), jobState(j), el("span", { text: " " }), badge(j.scope === "admin" ? "INFO" : "NONE", j.scope));
  $("jd-actions").replaceChildren(...jobButtons(j));
  const def = [
    ["Description", j.description || "—"], ["Owner", j.owner], ["Program", j.binary], ["Arguments", (j.args || []).length ? j.args.join("  ·  ") : "none"],
    ["Schedule", scheduleText(j)], ["Window", j.schedule.start_at ? `${fmtTime(j.schedule.start_at)} → ${j.schedule.ends_at ? fmtTime(j.schedule.ends_at) : "no end"}` : "—"],
    ["Where", whereText(j)], ["Region", j.region], ["Timeout", j.timeout],
    ["Pinned SHA-256", j.binary_sha256 || "—"], ["Approved", j.approved_by ? `${j.approved_by}, ${fmtTime(j.approved_at)}` : "—"],
    ["Created", fmtTime(j.created_at)], ["Last change", `${fmtTime(j.updated_at)} by ${j.updated_by}`],
  ];
  if (j.merged_by) def.push(["Merged", `by ${j.merged_by}, ${fmtTime(j.merged_at)}`]);
  if (j.merge_request) {
    const m = j.merge_request;
    def.push(["Merge request", `${m.status} — by ${m.requested_by}, ${fmtTime(m.requested_at)}${m.note ? ": " + m.note : ""}${m.decided_by ? ` (decided by ${m.decided_by}${m.reason ? ": " + m.reason : ""})` : ""}`]);
  }
  if (j.reason) def.push(["Reason", j.reason]);
  $("jd-def").replaceChildren(...kv(def));
  const st = j.stats;
  $("jd-stats").replaceChildren(...kv(st ? [
    ["Runs (all time)", `${st.total}: ${st.ok} ok · ${st.warn} warn · ${st.crit} failed`],
    ["Last 24h", `${st.requests_24h} runs · ${st.failures_24h} failed · ${pct(st.uptime_24h)} OK`],
    ["Duration avg / max", `${Math.round(st.avg_latency_ms)} ms / ${st.max_latency_ms} ms`],
    ["Failures in a row", String(st.consecutive_failures)],
    ["Last success", st.last_success ? fmtTime(st.last_success) : "never"],
    ["Last failure", st.last_failure ? fmtTime(st.last_failure) : "never"],
  ] : [["Runs", "none yet"]]));
  $("jd-runs").replaceChildren(...(runs.length ? runs.slice().reverse().map((r) => el("article", { cls: "run" },
    el("header", {}, badge(r.status, `exit ${r.details.exit_code}`),
      el("span", { text: ` ${fmtTime(r.time)} · ${r.details.trigger} · ${r.latency_ms} ms on ${r.node} · ${r.details.target || "local"}` })),
    r.details.stdout ? el("pre", { cls: "out", text: r.details.stdout }) : null,
    r.details.stderr ? el("pre", { cls: "out err", text: r.details.stderr }) : null,
    !r.details.stdout && !r.details.stderr ? el("div", { cls: "muted", text: r.message }) : null)) : [el("p", { cls: "muted", text: "no runs yet" })]));
}

// ---------- add-job and upload forms ----------

function jobFormVisibility() {
  if (!jobOpts) return;
  const f = $("job-form");
  const interval = f.type.value === "interval";
  document.querySelectorAll("#job-form .when-interval").forEach((x) => { x.hidden = !interval; });
  document.querySelectorAll("#job-form .when-remote").forEach((x) => { x.hidden = f.where.value !== "remote"; });
  document.querySelectorAll("#job-form .admin-only").forEach((x) => { x.hidden = !isRole("admin"); });
  const asAdmin = isRole("admin") && !f.as_user.checked;
  const prev = f.run_for.value;
  const opts = (asAdmin ? [["", "no end (admin activity)"]] : []).concat(jobOpts.run_for.map((v) => [v, v]));
  f.run_for.replaceChildren(...opts.map(([v, t]) => { const o = document.createElement("option"); o.value = v; o.textContent = t; return o; }));
  f.run_for.value = opts.some(([v]) => v === prev) ? prev : opts[asAdmin ? 0 : opts.length - 1][0];
}

["type", "where", "as_user"].forEach((n) => $("job-form")[n].addEventListener("change", jobFormVisibility));
$("job-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  $("job-error").textContent = ""; $("job-ok").textContent = "";
  const body = {
    name: f.elements["name"].value.trim(), description: f.description.value.trim(), binary: f.binary.value, region: f.region.value,
    args: f.args.value.split("\n").map((a) => a.replace(/\r$/, "")).filter((a) => a !== ""),
    schedule: f.type.value === "startup" ? { type: "startup", run_for: f.run_for.value } : { type: "interval", every: f.every.value, run_for: f.run_for.value },
    timeout: f.timeout.value.trim(), scope: isRole("admin") && !f.as_user.checked ? "admin" : "user",
  };
  if (f.where.value === "remote") body.target = { host: f.host.value.trim(), port: Number(f.port.value), os: f.os.value, credential: f.credential.value };
  const res = await api("api/jobs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!res.ok) { $("job-error").textContent = await errorText(res); return; }
  const j = await res.json();
  f.reset(); jobFormVisibility();
  go("#/jobs/" + j.id); // open the new job's page
});

$("upload-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  $("upload-error").textContent = ""; $("upload-ok").textContent = "";
  const file = f.file.files[0];
  if (!file) return;
  const name = f.elements["name"].value.trim() || file.name;
  const res = await api(`api/binaries/${encodeURIComponent(name)}`, { method: "PUT", headers: { "Content-Type": "application/octet-stream" }, body: file });
  if (!res.ok) { $("upload-error").textContent = await errorText(res); return; }
  const b = await res.json();
  f.reset();
  $("upload-ok").textContent = `Uploaded ${b.name} (${b.kind}, ${(b.size / 1024).toFixed(1)} KB). You can now choose it in “Add job”.`;
  await renderJobs();
  $("job-form").binary.value = b.name;
});

// ---------- filters ----------

$("f-region").addEventListener("change", () => last && renderSites(last));
$("f-status").addEventListener("change", () => last && renderSites(last));
$("f-inc").addEventListener("change", () => last && renderIncidents(last));
["f-ev-sev", "f-ev-kind"].forEach((id) => $(id).addEventListener("change", () => { route.q.delete("kind"); last && renderEvents(last); }));
$("f-ev-q").addEventListener("input", () => last && renderEvents(last));
["f-job-scope", "jobs-mine", "jobs-all"].forEach((id) => $(id).addEventListener("change", () => renderRunning(jobsCache)));
["f-run-status", "f-run-job"].forEach((id) => $(id).addEventListener("change", () => renderHistory(jobsCache)));

// ---------- login ----------

async function showLogin() {
  if (timer) { clearInterval(timer); timer = null; }
  me = null;
  $("app-view").hidden = true;
  $("login-view").hidden = false;
  try {
    const info = await (await fetch("api/auth/info", { cache: "no-store" })).json();
    $("login-node").textContent = info.node_name;
    const names = { ldap: "Active Directory", local: "local account" };
    $("login-providers").textContent = "Sign in with: " + info.providers.map((p) => names[p] || p).join(" or ");
  } catch (_) { /* informational only */ }
  $("login-user").focus();
}

async function showApp() {
  $("login-view").hidden = true;
  $("app-view").hidden = false;
  const u = me.user;
  $("user").textContent = `${u.display_name} · ${u.role} (${u.provider})`;
  await initJobs();
  route = parseHash();
  await refresh();
  onRoute();
  timer = setInterval(() => {
    // don't redraw a form the user is typing in
    if (route.tab === "jobs" && route.sub === "add" && document.activeElement && document.activeElement.closest("form")) return;
    refresh();
  }, 5000);
}

$("login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  $("login-error").textContent = "";
  try {
    const res = await fetch("api/login", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: $("login-user").value, password: $("login-pass").value }),
    });
    if (!res.ok) { $("login-error").textContent = (await res.text()).trim(); return; }
    $("login-pass").value = "";
    me = await (await api("api/me")).json();
    showApp();
  } catch (err) {
    $("login-error").textContent = err.message;
  }
});

$("logout").addEventListener("click", async () => {
  await fetch("api/logout", { method: "POST", credentials: "same-origin" });
  showLogin();
});

(async () => {
  try {
    const res = await fetch("api/me", { credentials: "same-origin", cache: "no-store", headers: { "X-Requested-With": "sitemonitor-ui" } });
    if (!res.ok) return showLogin();
    me = await res.json();
    showApp();
  } catch (_) { showLogin(); }
})();

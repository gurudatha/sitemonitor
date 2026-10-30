// Dashboard client. Uses textContent only (never innerHTML) so data from
// monitored sites can never inject markup. Served under a strict CSP.
"use strict";

const CHECKS = ["availability", "tls", "cert", "headers", "api"];
const $ = (id) => document.getElementById(id);
let last = null;
let me = null;          // {user, csrf_token, node_name}
let timer = null;

// Every API call: same-origin cookie session + CSRF header for writes.
async function api(path, opts = {}) {
  const headers = Object.assign({ "X-Requested-With": "sitemonitor-ui" }, opts.headers || {});
  if (opts.method && opts.method !== "GET" && me && me.csrf_token) headers["X-CSRF-Token"] = me.csrf_token;
  const res = await fetch(path, Object.assign({}, opts, { headers, credentials: "same-origin", cache: "no-store" }));
  if (res.status === 401) { showLogin(); throw new Error("signed out"); }
  return res;
}

function el(tag, opts = {}, ...children) {
  const e = document.createElement(tag);
  if (opts.cls) e.className = opts.cls;
  if (opts.text !== undefined) e.textContent = opts.text;
  if (opts.title) e.title = opts.title;
  for (const c of children) if (c) e.append(c);
  return e;
}
const badge = (status, text) => el("span", { cls: "badge s-" + (status || "NONE"), text: text || status || "—" });
const fmtTime = (t) => (t ? new Date(t).toLocaleString() : "—");
const ago = (t) => {
  if (!t) return "never";
  const s = Math.round((Date.now() - new Date(t)) / 1000);
  return s < 60 ? s + "s ago" : s < 3600 ? Math.round(s / 60) + "m ago" : Math.round(s / 3600) + "h ago";
};

function worst(site) {
  return site.status || null; // stored by the server (worst check status)
}

// KPI values come from the database summary (d.summary); nothing is counted here.
function renderKPIs(d) {
  const m = d.summary;
  const pctTxt = (v) => (v === null || v === undefined ? "—" : v.toFixed(2) + "%");
  const k = (label, value, cls, title) => el("div", { cls: "kpi " + (cls || ""), title: title || "" }, el("div", { cls: "v", text: String(value) }), el("div", { cls: "l", text: label }));
  $("kpis").replaceChildren(
    k("Websites", m.websites, "", m.pending ? `${m.pending} without results yet` : ""),
    k("Healthy", m.healthy, "ok"),
    k("Warning", m.warning, m.warning ? "warn" : ""),
    k("Critical", m.critical, m.critical ? "crit" : ""),
    k(`Certs ≤ ${m.cert_warn_days} days`, m.certs_expiring, m.certs_expiring ? "warn" : ""),
    k("Availability 24h", pctTxt(m.availability_24h), m.availability_24h !== null && m.availability_24h < 99 ? "warn" : "ok"),
    k("Requests 24h", m.requests_24h, "", `${m.failures_24h} failed`),
    k("Failed 24h", m.failures_24h, m.failures_24h ? "crit" : ""),
    k("Open incidents", m.open_incidents, m.critical_incidents ? "crit" : m.open_incidents ? "warn" : "", `${m.critical_incidents} critical`),
  );
}

function renderCluster(d) {
  const c = d.cluster;
  const peerSev = { HEALTHY: "OK", SUSPECT: "WARN", DOWN: "CRIT" }[c.peer_status] || "NONE";
  const row = (k, v) => [el("dt", { text: k }), el("dd", { text: v })];
  const self = el("article", { cls: "node self" },
    el("header", {}, el("h3", { text: `${c.node_name} [${c.region}] (this server)` }), badge(c.mode === "NORMAL" ? "OK" : c.mode === "TAKEOVER" ? "WARN" : "INFO", c.mode)),
    el("dl", {}, ...[
      row("Server ID", c.node),
      row("Monitors regions", (c.owned_regions || []).join(", ") || "none"),
      row("Epoch", String(c.epoch)),
      row("Config", `${d.config.version} ${c.config_match ? "✔ matches peer" : "✖ differs from peer (" + (c.peer_config_version || "?") + ")"}`),
      row("Workers", `${d.scheduler.busy_workers}/${d.scheduler.workers} busy · queue ${d.scheduler.queue_depth}/${d.scheduler.queue_cap} · dropped ${d.scheduler.dropped}`),
      row("Replication", `${c.replication_queue} queued · lag ${c.replication_lag_seconds.toFixed(1)}s`),
      row("Witness", c.witness_enabled ? (c.witness_reachable ? "reachable" : "UNREACHABLE") : "not configured"),
    ].flat()),
  );
  const peer = el("article", { cls: "node" },
    el("header", {}, el("h3", { text: `${c.peer_name} [${c.peer_region}] (peer)` }), badge(peerSev, c.peer_status)),
    el("dl", {}, ...[
      row("Server ID", c.peer_id),
      row("Peer mode", c.peer_mode || "—"),
      row("Monitors regions", (c.peer_owned_regions || []).join(", ") || "none"),
      row("Last heartbeat", c.heartbeat_age_seconds < 0 ? "never" : c.heartbeat_age_seconds.toFixed(1) + "s ago"),
      row("Missed beats", String(c.missed_beats)),
      row("Last error", c.last_error || "—"),
      c.takeover_blocked ? row("Takeover blocked", c.takeover_blocked) : [],
    ].flat()),
  );
  $("cluster").replaceChildren(self, peer);

  const mb = $("mode-badge");
  mb.className = "badge s-" + (c.mode === "NORMAL" ? "OK" : c.mode === "TAKEOVER" ? "WARN" : "INFO");
  mb.textContent = c.mode + " · peer " + c.peer_status;
  $("whoami").textContent = `${c.node_name} (${c.node}) · region ${c.region} · monitoring ${(c.owned_regions || []).join(" + ") || "nothing (standby)"} · peer ${c.peer_name}`;
}

function checkCell(site, check) {
  if (!site.checks.includes(check)) return el("td", { cls: "muted", text: "n/a" });
  const r = site.results[check];
  if (!r) return el("td", {}, badge(null, "pending"));
  const td = el("td", { cls: "clickable", title: "Click for details" }, badge(r.status), el("span", { cls: "msg", text: r.message }));
  td.addEventListener("click", () => showDetail(site, r));
  return td;
}

// Uptime: stored 24h/30d values and counters of the availability check.
function uptimeCell(s) {
  const st = (s.stats || {}).availability;
  if (!st || st.uptime_24h === null) return el("td", { text: "—" });
  const td = el("td", { title: `24h: ${st.requests_24h} requests, ${st.failures_24h} failed\n30d: ${st.uptime_30d === null ? "—" : st.uptime_30d.toFixed(2) + "%"} of ${st.requests_30d}\nall time: ${st.ok} ok · ${st.warn} warn · ${st.crit} failed` },
    el("span", { text: st.uptime_24h.toFixed(1) + "%" }),
    el("span", { cls: "msg", text: `${st.requests_24h} req · ${st.failures_24h} failed` }));
  return td;
}

function renderSites(d) {
  const fr = $("f-region").value, fs = $("f-status").value;
  const rows = [];
  for (const s of d.sites) {
    if (fr && s.region !== fr) continue;
    if (fs && worst(s) !== fs) continue;
    const from = new Set(Object.values(s.results).map((r) => r.node_region));
    const fromTxt = [...from].join(", ") || "—";
    const failover = [...from].some((r) => r !== s.region);
    const tr = el("tr", {},
      el("td", {}, el("span", { cls: "site-name", text: s.name }), el("span", { cls: "site-url", text: s.url })),
      el("td", {}, badge("INFO", s.region)),
      el("td", { cls: failover ? "failover" : "", text: failover ? fromTxt + " (failover)" : fromTxt }),
      ...CHECKS.map((c) => checkCell(s, c)),
      uptimeCell(s),
    );
    rows.push(tr);
  }
  $("sites").tBodies[0].replaceChildren(...rows);
}

function renderCerts(d) {
  const list = d.sites.filter((s) => s.results.cert && s.results.cert.details && s.results.cert.details.days_left !== undefined)
    .map((s) => ({ s, r: s.results.cert, days: Number(s.results.cert.details.days_left) }))
    .sort((a, b) => a.days - b.days);
  $("certs").tBodies[0].replaceChildren(...list.map(({ s, r, days }) =>
    el("tr", {},
      el("td", { text: s.name }),
      el("td", {}, badge(days <= 7 ? "CRIT" : days <= 30 ? "WARN" : "OK", String(days))),
      el("td", { text: fmtTime(r.details.not_after) }),
      el("td", { text: r.details.chain || "—" }),
    )));
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
    return el("tr", {},
      el("td", { text: s.name }),
      cell(t["TLS1.0"], false), cell(t["TLS1.1"], false), cell(t["TLS1.2"], true), cell(t["TLS1.3"], true),
      el("td", {}, badge(weak ? "WARN" : "OK", weak ? t.weak_cipher : "none")),
    );
  });
  $("tls").tBodies[0].replaceChildren(...rows);
}

function renderEvents(d) {
  $("events").replaceChildren(...d.events.map((e) =>
    el("li", {},
      el("time", { text: fmtTime(e.time) }),
      badge(e.severity, e.kind),
      el("span", { text: `${e.site ? e.site + "/" + e.check + ": " : ""}${e.message} — ${e.node}` }),
    )));
}

const STATE_SEV = { RAISED: null, SEVERITY: null, CLEARED: "OK", REAPPEARED: "CRIT" };

function subject(inc) {
  if (inc.site) return `${inc.site} / ${inc.check}`;
  return inc.key.replace(/^[a-z-]+:/, (m) => m.slice(0, -1) + " ");
}

function renderIncidents(d) {
  const list = d.incidents || [];
  const open = list.filter((i) => i.status === "OPEN").length;
  $("incidents-summary").textContent = `${open} open · ${list.length - open} cleared`;
  $("incidents").tBodies[0].replaceChildren(...list.map((inc) => {
    const tr = el("tr", { cls: "clickable", title: "Click for full history" },
      el("td", {}, badge(inc.status === "OPEN" ? inc.severity : "OK", inc.status)),
      el("td", {}, el("span", { cls: "site-name", text: subject(inc) }), el("span", { cls: "msg", text: inc.message })),
      el("td", { text: fmtTime(inc.first_seen) }),
      el("td", { text: inc.status === "OPEN" ? ago(inc.last_seen) : "cleared " + fmtTime(inc.cleared_at) }),
      el("td", { cls: "num", text: String(inc.occurrences) }),
      el("td", { cls: "num", text: String(inc.cleared) }),
      el("td", { cls: "num", text: String(inc.reappeared) }),
      el("td", { text: (inc.seen_by || []).join(", ") }),
    );
    tr.addEventListener("click", () => showIncident(inc));
    return tr;
  }));
}

function showIncident(inc) {
  $("detail-title").textContent = subject(inc);
  const pairs = [
    ["Status", inc.status], ["Severity", inc.severity], ["First seen", fmtTime(inc.first_seen)],
    ["Last seen", fmtTime(inc.last_seen)], ["Occurrences", String(inc.occurrences)],
    ["Cleared", `${inc.cleared} time(s)`], ["Reappeared", `${inc.reappeared} time(s)`],
    ["Seen by", (inc.seen_by || []).join(", ")],
  ];
  const body = pairs.flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: v })]);
  body.push(el("dt", { text: "History" }), el("dd", {}, el("ol", { cls: "timeline" },
    ...inc.history.map((t) => el("li", {},
      badge(STATE_SEV[t.state] || t.severity, t.state),
      el("time", { text: " " + fmtTime(t.time) + " " }),
      el("span", { text: `${t.message} — ${t.node}` }))))));
  $("detail-body").replaceChildren(...body);
  $("detail").showModal();
}

function showDetail(site, r) {
  $("detail-title").textContent = `${site.name} · ${r.check}`;
  const st = (site.stats || {})[r.check];
  const pc = (v) => (v === null || v === undefined ? "—" : v.toFixed(2) + "%");
  const pairs = [
    ["Status", r.status], ["Message", r.message], ["Checked by", `${r.node} (${r.node_region})`],
    ["When", `${fmtTime(r.time)} (${ago(r.time)})`], ["Latency", r.latency_ms + " ms"],
    ...(st ? [
      ["Requests (all time)", `${st.total}: ${st.ok} ok · ${st.warn} warn · ${st.crit} failed`],
      ["Uptime 24h / 30d", `${pc(st.uptime_24h)} (${st.requests_24h} req) / ${pc(st.uptime_30d)} (${st.requests_30d} req)`],
      ["Latency avg / max", `${Math.round(st.avg_latency_ms)} ms / ${st.max_latency_ms} ms`],
      ["Consecutive failures", String(st.consecutive_failures)],
      ["Last success", st.last_success ? fmtTime(st.last_success) : "never"],
      ["Last failure", st.last_failure ? fmtTime(st.last_failure) : "never"],
    ] : []),
    ...Object.entries(r.details || {}).sort(),
  ];
  $("detail-body").replaceChildren(...pairs.flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: String(v) })]));
  $("detail").showModal();
}

function render(d) {
  last = d;
  renderKPIs(d);
  renderCluster(d);
  renderSites(d);
  renderCerts(d);
  renderTLS(d);
  renderIncidents(d);
  renderEvents(d);
  $("updated").textContent = "updated " + new Date().toLocaleTimeString();
}

async function refresh() {
  try {
    const res = await api("api/status");
    if (!res.ok) throw new Error("HTTP " + res.status);
    render(await res.json());
    await loadJobs();
  } catch (err) {
    $("updated").textContent = "update failed: " + err.message;
  }
}

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
  await refresh();
  timer = setInterval(refresh, 5000);
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

$("f-region").addEventListener("change", () => last && renderSites(last));
$("f-status").addEventListener("change", () => last && renderSites(last));

(async () => {
  try {
    const res = await fetch("api/me", { credentials: "same-origin", cache: "no-store", headers: { "X-Requested-With": "sitemonitor-ui" } });
    if (!res.ok) return showLogin();
    me = await res.json();
    showApp();
  } catch (_) { showLogin(); }
})();

// ---------- periodic jobs ----------
// All data and every action goes through the same API that agents use.

let jobOpts = null;
let jobsAvailable = false;
const isRole = (r) => me && { viewer: 1, operator: 2, admin: 3 }[me.user.role] >= { viewer: 1, operator: 2, admin: 3 }[r];

async function initJobs() {
  const res = await api("api/jobs/options");
  jobsAvailable = res.ok;
  $("jobs-section").hidden = !jobsAvailable;
  if (!jobsAvailable) return;
  jobOpts = await res.json();
  $("jobs-actions").hidden = !isRole("operator");
  const f = $("job-form");
  const opt = (v, t) => { const o = document.createElement("option"); o.value = v; o.textContent = t || v; return o; };
  f.every.replaceChildren(...jobOpts.intervals.map((v) => opt(v)));
  f.every.value = "5m";
  f.credential.replaceChildren(...(jobOpts.credentials.length ? jobOpts.credentials.map((v) => opt(v)) : [opt("", "(none configured by the admin)")]));
  f.region.value = (last && last.cluster && last.cluster.region) || "IDC";
}

function scheduleText(j) {
  if (j.schedule.type === "startup") return "at server start";
  return "every " + j.schedule.every + (j.schedule.ends_at ? " until " + fmtTime(j.schedule.ends_at) : j.scope === "admin" ? " (no end)" : "");
}
function whereText(j) {
  return j.target ? `${j.target.host}:${j.target.port} (${j.target.os}, ${j.target.credential})` : `${j.region} monitor`;
}

async function jobPost(path, body) {
  const res = await api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  if (!res.ok) {
    let msg = "HTTP " + res.status;
    try { msg = (await res.json()).error || msg; } catch (_) { /* not json */ }
    alert(msg);
    return null;
  }
  await loadJobs();
  return res;
}

function jobButtons(j) {
  const me_ = me.user.username;
  const mine = j.owner === me_ && j.scope === "user";
  const canEdit = isRole("admin") || (isRole("operator") && mine);
  const btn = (label, fn, cls) => { const b = el("button", { text: label, cls: cls || "" }); b.addEventListener("click", (e) => { e.stopPropagation(); fn(); }); return b; };
  const out = [];
  const mr = j.merge_request && j.merge_request.status === "pending";
  if (canEdit && j.status === "approved") out.push(btn("Run now", () => jobPost(`api/jobs/${j.id}/run`)));
  if (canEdit && j.status === "approved") out.push(btn("Disable", () => jobPost(`api/jobs/${j.id}/disable`, { reason: prompt("Reason (optional)") || "" })));
  if (canEdit && j.status === "disabled") out.push(btn("Enable", () => jobPost(`api/jobs/${j.id}/enable`)));
  if (mine && isRole("operator") && !mr && j.scope === "user") out.push(btn("Request merge", () => {
    const note = prompt("Why should this become an admin periodic activity?");
    if (note !== null) jobPost(`api/jobs/${j.id}/merge-request`, { note });
  }));
  if (isRole("admin") && j.scope === "user") out.push(btn(mr ? "Approve merge" : "Merge", () => jobPost(`api/jobs/${j.id}/merge`), mr ? "primary" : ""));
  if (isRole("admin") && mr) out.push(btn("Reject merge", () => {
    const reason = prompt("Reason for rejecting the merge");
    if (reason !== null) jobPost(`api/jobs/${j.id}/merge/reject`, { reason });
  }));
  if (isRole("admin") && j.status === "pending") out.push(btn("Approve", () => jobPost(`api/jobs/${j.id}/approve`)));
  if (isRole("admin") && j.last_run && /changed since approval/.test(j.last_run.message)) out.push(btn("Re-approve", () => jobPost(`api/jobs/${j.id}/approve`)));
  if (canEdit) out.push(btn("Delete", async () => {
    if (!confirm(`Delete job ${j.name}?`)) return;
    const res = await api(`api/jobs/${j.id}`, { method: "DELETE" });
    if (!res.ok) alert((await res.json()).error); else loadJobs();
  }, "danger"));
  return el("div", { cls: "actions" }, ...out);
}

async function loadJobs() {
  if (!jobsAvailable) return;
  const [jr, br] = await Promise.all([api("api/jobs"), api("api/binaries")]);
  if (!jr.ok || !br.ok) return;
  let jobs = await jr.json();
  const bins = await br.json();
  if ($("jobs-mine").checked) jobs = jobs.filter((j) => j.owner === me.user.username);
  const active = jobs.filter((j) => j.state === "active").length;
  const merges = jobs.filter((j) => j.merge_request && j.merge_request.status === "pending").length;
  $("jobs-summary").textContent = `${jobs.length} jobs · ${active} active` + (merges ? ` · ${merges} merge request${merges > 1 ? "s" : ""} waiting` : "");
  $("jobs").tBodies[0].replaceChildren(...jobs.map((j) => {
    const lr = j.last_run;
    const st = j.stats;
    const mr = j.merge_request && j.merge_request.status === "pending";
    const tr = el("tr", { cls: "clickable", title: "Click for runs and output" },
      el("td", {}, el("span", { cls: "site-name", text: j.name }), el("span", { cls: "site-url", text: `${j.owner}${j.description ? " · " + j.description : ""}` })),
      el("td", {}, badge(j.scope === "admin" ? "INFO" : "NONE", j.scope), mr ? el("span", { cls: "msg pending-merge", text: "merge requested" }) : null),
      el("td", {}, badge({ active: "OK", scheduled: "INFO", expired: "NONE", disabled: "NONE", pending: "WARN", rejected: "CRIT" }[j.state] || "NONE", j.state)),
      el("td", {}, el("span", { text: j.binary }), el("span", { cls: "msg", text: (j.args || []).join(" ") })),
      el("td", { text: scheduleText(j) }),
      el("td", { text: whereText(j) }),
      el("td", {}, lr ? badge(lr.status, `exit ${lr.details.exit_code}`) : el("span", { cls: "muted", text: "—" }), lr ? el("span", { cls: "msg", text: ago(lr.time) + " · " + lr.message }) : null),
      el("td", { text: st ? `${st.ok + st.warn}/${st.total}` : "0", title: st ? `${st.ok} ok · ${st.warn} warn · ${st.crit} failed` : "" }),
      el("td", {}, jobButtons(j)),
    );
    tr.addEventListener("click", () => showJob(j));
    return tr;
  }));
  $("bins-summary").textContent = `(${bins.length})`;
  $("bins").tBodies[0].replaceChildren(...bins.map((b) => {
    const del = el("button", { text: "Delete", cls: "danger" });
    del.addEventListener("click", async () => {
      if (!confirm(`Delete ${b.name}?`)) return;
      const res = await api(`api/binaries/${encodeURIComponent(b.name)}`, { method: "DELETE" });
      if (!res.ok) alert((await res.json()).error); else loadJobs();
    });
    return el("tr", {}, el("td", { text: b.name }), el("td", { text: b.kind }), el("td", { text: (b.size / 1024).toFixed(1) + " KB" }),
      el("td", { text: b.sha256.slice(0, 16) + "…", title: b.sha256 }), el("td", { text: b.uploaded_by }),
      el("td", {}, isRole("admin") || b.uploaded_by === me.user.username ? del : null));
  }));
  const f = $("job-form");
  const cur = f.binary.value;
  f.binary.replaceChildren(...bins.map((b) => { const o = document.createElement("option"); o.value = b.name; o.textContent = `${b.name} (${b.kind})`; return o; }));
  if (cur) f.binary.value = cur;
}

async function showJob(j) {
  const res = await api(`api/jobs/${j.id}/runs?limit=10`);
  const runs = res.ok ? await res.json() : [];
  $("detail-title").textContent = `Job ${j.name}`;
  const pairs = [
    ["Scope / state", `${j.scope} · ${j.state}`], ["Owner", j.owner], ["Program", `${j.binary} ${(j.args || []).join(" ")}`],
    ["Pinned SHA-256", j.binary_sha256 || "—"], ["Schedule", scheduleText(j)], ["Where", whereText(j)], ["Timeout", j.timeout],
  ];
  if (j.merge_request) pairs.push(["Merge request", `${j.merge_request.status} — by ${j.merge_request.requested_by} ${fmtTime(j.merge_request.requested_at)}${j.merge_request.note ? ": " + j.merge_request.note : ""}${j.merge_request.decided_by ? ` (decided by ${j.merge_request.decided_by}${j.merge_request.reason ? ": " + j.merge_request.reason : ""})` : ""}`]);
  if (j.reason) pairs.push(["Reason", j.reason]);
  const body = pairs.flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: v })]);
  body.push(el("dt", { text: "Last runs" }), el("dd", {}, ...(runs.length ? runs.slice().reverse().map((r) => el("div", {},
    badge(r.status, `exit ${r.details.exit_code}`), el("span", { text: ` ${fmtTime(r.time)} · ${r.details.trigger} · ${r.latency_ms} ms on ${r.node}` }),
    r.details.stdout ? el("pre", { cls: "out", text: r.details.stdout }) : null,
    r.details.stderr ? el("pre", { cls: "out", text: "stderr: " + r.details.stderr }) : null,
    !r.details.stdout && !r.details.stderr ? el("div", { cls: "muted", text: r.message }) : null)) : [el("span", { cls: "muted", text: "no runs yet" })])));
  $("detail-body").replaceChildren(...body);
  $("detail").showModal();
}

function jobFormVisibility() {
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

$("job-add").addEventListener("click", () => {
  $("job-form").reset();
  $("job-error").textContent = "";
  jobFormVisibility();
  $("job-dialog").showModal();
});
["type", "where", "as_user"].forEach((n) => $("job-form")[n].addEventListener("change", jobFormVisibility));
$("job-cancel").addEventListener("click", () => $("job-dialog").close());
$("jobs-mine").addEventListener("change", loadJobs);
$("job-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const body = {
    name: f.elements["name"].value.trim(), description: f.description.value.trim(), binary: f.binary.value, region: f.region.value,
    args: f.args.value.split("\n").map((a) => a.replace(/\r$/, "")).filter((a) => a !== ""),
    schedule: f.type.value === "startup" ? { type: "startup", run_for: f.run_for.value } : { type: "interval", every: f.every.value, run_for: f.run_for.value },
    timeout: f.timeout.value.trim(), scope: isRole("admin") && !f.as_user.checked ? "admin" : "user",
  };
  if (f.where.value === "remote") body.target = { host: f.host.value.trim(), port: Number(f.port.value), os: f.os.value, credential: f.credential.value };
  const res = await api("api/jobs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!res.ok) { $("job-error").textContent = (await res.json()).error; return; }
  $("job-dialog").close();
  loadJobs();
});

$("bin-upload").addEventListener("click", () => { $("upload-form").reset(); $("upload-error").textContent = ""; $("upload-dialog").showModal(); });
$("upload-cancel").addEventListener("click", () => $("upload-dialog").close());
$("upload-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const file = f.file.files[0];
  if (!file) return;
  const name = f.elements["name"].value.trim() || file.name;
  const res = await api(`api/binaries/${encodeURIComponent(name)}`, { method: "PUT", headers: { "Content-Type": "application/octet-stream" }, body: file });
  if (!res.ok) { $("upload-error").textContent = (await res.json()).error; return; }
  $("upload-dialog").close();
  loadJobs();
});

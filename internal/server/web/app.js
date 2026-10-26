// Dashboard client. Uses textContent only (never innerHTML) so data from
// monitored sites can never inject markup. Served under a strict CSP.
"use strict";

const CHECKS = ["availability", "tls", "cert", "headers", "api"];
const $ = (id) => document.getElementById(id);
let last = null;
let me = null;          // {user, csrf_token, node_name}
let cfgETag = "";
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
  const rank = { OK: 0, WARN: 1, CRIT: 2 };
  let w = null;
  for (const r of Object.values(site.results)) if (w === null || rank[r.status] > rank[w]) w = r.status;
  return w;
}

function renderKPIs(d) {
  const sites = d.sites;
  const counts = { OK: 0, WARN: 0, CRIT: 0 };
  let certs30 = 0;
  for (const s of sites) {
    const w = worst(s);
    if (w) counts[w]++;
    const c = s.results.cert;
    if (c && c.details && Number(c.details.days_left) <= 30) certs30++;
  }
  const k = (label, value, cls) => el("div", { cls: "kpi " + (cls || "") }, el("div", { cls: "v", text: String(value) }), el("div", { cls: "l", text: label }));
  $("kpis").replaceChildren(
    k("Websites", sites.length),
    k("Healthy", counts.OK, "ok"),
    k("Warning", counts.WARN, counts.WARN ? "warn" : ""),
    k("Critical", counts.CRIT, counts.CRIT ? "crit" : ""),
    k("Certs ≤ 30 days", certs30, certs30 ? "warn" : ""),
    k("Checks run", d.scheduler.completed),
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
      el("td", { text: s.samples ? s.uptime.toFixed(1) + "%" : "—" }),
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

function showDetail(site, r) {
  $("detail-title").textContent = `${site.name} · ${r.check}`;
  const pairs = [
    ["Status", r.status], ["Message", r.message], ["Checked by", `${r.node} (${r.node_region})`],
    ["When", `${fmtTime(r.time)} (${ago(r.time)})`], ["Latency", r.latency_ms + " ms"],
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
  renderEvents(d);
  $("updated").textContent = "updated " + new Date().toLocaleTimeString();
}

async function refresh() {
  try {
    const res = await api("api/status");
    if (!res.ok) throw new Error("HTTP " + res.status);
    render(await res.json());
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
  const canEdit = me.config_api && (u.role === "operator" || u.role === "admin");
  $("config-section").hidden = !canEdit;
  if (canEdit) loadConfig();
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

// ---------- sites.yaml editor (operator/admin) ----------

function cfgResult(text, ok) {
  const r = $("cfg-result");
  r.textContent = text;
  r.className = "cfg-result " + (ok ? "ok" : "bad");
}

async function loadConfig() {
  const res = await api("api/config/sites");
  $("cfg-text").value = await res.text();
  cfgETag = res.headers.get("ETag") || "";
  $("cfg-meta").textContent = "loaded " + new Date().toLocaleTimeString();
  cfgResult("", true);
}

$("cfg-load").addEventListener("click", loadConfig);
$("cfg-validate").addEventListener("click", async () => {
  const res = await api("api/config/sites/validate", { method: "POST", body: $("cfg-text").value, headers: { "Content-Type": "application/yaml" } });
  const body = await res.json();
  cfgResult(res.ok ? `valid: version ${body.version}, ${body.sites} sites` : body.error, res.ok);
});
$("cfg-save").addEventListener("click", async () => {
  const res = await api("api/config/sites", { method: "PUT", body: $("cfg-text").value, headers: { "Content-Type": "application/yaml", "If-Match": cfgETag } });
  const body = await res.json();
  if (!res.ok) { cfgResult((res.status === 412 ? "Conflict: " : "") + body.error, false); return; }
  cfgETag = res.headers.get("ETag") || cfgETag;
  const d = body.diff || {};
  const parts = [];
  if (d.added && d.added.length) parts.push("added " + d.added.join(", "));
  if (d.removed && d.removed.length) parts.push("removed " + d.removed.join(", "));
  if (d.changed && d.changed.length) parts.push("changed " + d.changed.join(", "));
  const peer = body.peer_synced === undefined ? "" : body.peer_synced ? " · synced to peer" : " · peer not reachable (will auto-sync): " + body.peer_error;
  cfgResult(body.changed ? `saved version ${body.version}: ${parts.join("; ") || "no site changes"}${peer}` : "no changes", true);
  refresh();
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

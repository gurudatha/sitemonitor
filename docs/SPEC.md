# SiteMonitor — Complete Specification

This document is the **single source of requirements** for SiteMonitor. It is
written so that Claude (or an engineer) can regenerate an equivalent system
from scratch, from any account, without access to the original conversation.

- Part A is the **history**: every request, in order, with the decisions and
  clarifications that shaped the result.
- Part B is the **consolidated specification**: what the system must do, with
  exact names, defaults, schemas, endpoints and behaviours.
- Part C lists the **acceptance tests**.
- Part D is a **regeneration prompt** to paste into a new Claude session.

The code in this repository (latest commit at the time of writing:
`026e407`) is the reference implementation. A regeneration that follows Part B
is behaviour-equivalent; it will not be byte-identical (Go code style, bcrypt
salts and generated certificates differ).

---

## Part A — Request history and decisions

Requests are numbered in the order they were made. "Decision" records how an
ambiguity was resolved; follow these decisions when regenerating.

| # | Request (summary of the user's words) | Decisions / clarifications |
|---|---|---|
| 1 | "A bunch of websites need to be checked periodically. What checks can be done as a DevOps/infrastructure task? List items to handle periodically." Guidelines: single monolith spawning **time-bound** requests; list of websites; checking; **constraints — not too much load**; basic monitoring: **SSL/TLS 1.0/1.1 disabled, TLS 1.2/1.3 enabled**, **certificate expiring within 30 days**, **servers down**, **API sanity calls** if APIs are given; a **dashboard on the latest secure web platform**. | The user wrote "tls1.2/1.3 is disabled"; interpreted as **TLS 1.2/1.3 enabled** (disabling them would block all clients). Delivered a checklist (availability, TLS, cert, headers, API, DNS, hygiene) and a design. Platform choice: **Go** (memory-safe, single static binary) + embedded HTML/JS dashboard under a strict CSP. |
| 2 | "There are **two locations** this will run. Make the two servers stay **in sync**, **ping each other** to know if they are working; if one is not working, the other **automatically monitors the other region's systems**. Regions are **IDC** and **FC**; each server's region must be noted in a **.yaml file**." | Read "if they are working it should switch" as "if the *peer* is not working". Added a **witness** (third location) to prevent split-brain, automatic **handback**, and an optional cross-region verification of "site down". |
| 3 | "Write the code with docker-compose." | Single binary with subcommands; compose demo with IDC monitor, FC monitor, witness, cert generator and fake target websites. |
| 4 | "Create documentation: Description, ReadMe, Design, Compilation steps, Deployment." | `README.md`, `docs/DESIGN.md`, `docs/COMPILATION.md`, `docs/DEPLOYMENT.md`. |
| 5 | "Does the site have an API to check the status? What are the memory requirements?" | Documented `/api/status`, `/api/history`, `/metrics`, `/healthz`; measured memory and added `history_per_check` + sizing table. |
| 6 | "How is data stored? How much data per server per year?" | Documented storage and retention; recommended Prometheus for long-term history (later superseded by the database, request 18). |
| 7 | "**Size limits**, **name of the server and peer server**, **goroutine limits and others configurable in YAML**. **APIs to update the YAML** from outside, **with authentication**. A **module** that can use **Active Directory or username/password**, configurable in YAML so a **POC uses passwords and deployment uses AD**." | `limits:` section, `node.name`/`peer.name`, config API with If-Match/backups/peer sync, `internal/auth` with `local` and `ldap` providers, roles viewer/operator/admin. |
| 8 | "Commit and push to a git repo" → repo `gurudatha/sitemonitor` (private, created by the user). | Pushed `main`. |
| 9 | Windows/WSL Docker issues; "use Go 1.24.13 — what would it lose?" then "revert to Go 1.26". | Final: **Go 1.26**, build image `golang:1.26-alpine`. |
| 10 | "Remove 'Configuration — sites.yaml' editor from the dashboard. If the same event is seen from a server, store the oldest event and date; if it clears and reappears, note both." | Editor removed (API kept). **Incidents** introduced (one record per problem key; oldest date kept; CLEARED / REAPPEARED transitions). |
| 11 | "Options for sending mail periodically / on critical alert." | Advisory only (go-mail recommended); **not implemented**. |
| 12 | "**Metrics must be updated on every request success or failure.** **Dashboard values and metrics must be read from a database**, not calculated dynamically." | Embedded **SQLite** (pure Go). Every result updates counters, current state and hourly/daily buckets in one transaction. Dashboard/metrics only read stored values. |
| 13 | "Add a feature: users add a **binary**, pass **arguments**, run commands **periodically**. Create a new **abstract structure** for the periodic feature. (1) All user binaries in **one directory** (Python, Perl, PowerShell, any executable). (2) Users choose the **interval** (1, 2, 5, 10, 15, 20, 30, 60 min, 1h, 2h, 4h, 6h) and **for how long** (max **one day**). Option to **run a command when the server comes up**, with a **placeholder for a cgo program** to be written later. (3) **User monitoring requests**, if approved by an admin, can be **merged into the admin's periodic activities**. (4) Run a command by **logging into a system (Windows with Cygwin and Linux)** with **IP and port**: **push a program, run it, get the return value**. Also **APIs for all features, callable by agents**." | `internal/jobs` with one `Job` abstraction; SSH remote execution; cgo startup hook behind a build tag; API tokens + OpenAPI. |
| 13a | Mid-request correction: "**User jobs don't need admin approval to run.** Only **merging into admin's periodic activity** requires approval." | User jobs run immediately (≤ 24 h window). Merge request → admin approve/reject. Optional `jobs.require_approval` (default **false**) for stricter deployments. |
| 14 | "The core is periodic running (admin or user). It should be a **separate module**. What designs make it lightweight?" → "**Go ahead with option A**." | `internal/periodic`: min-heap + single timer, per-class pools, Sync API, no catch-up/overlap, shedding. Checks and jobs are providers. |
| 15 | "How do I create a user job with an exe, arguments and intervals?" | Explained UI/API steps; Windows `.exe` runs only on remote Windows (Cygwin) targets. |
| 16 | "**Don't put all features on one page — use tabs.** Separate section for **User Jobs**: **Add job + Upload program on one page**, **running jobs on another**, **old results on another**; **clicking a job opens its own page** with full details. Separate sections for **Clusters, Websites, Certificate expiry, Incidents, Events**. **Landing page = only a minimal dashboard**; clicking websites/healthy/warning/... tiles lists **short entries** for those items (filtered events)." | Hash-routed single-page app with tabs and job detail pages. |
| 17 | "Add an **Info tab** with **extensive examples of argument formatting for special characters for different languages**." Plus: "Display all the **tiles in a single line**. If nothing is selected show: (1) **all critical errors**, charts; (2) **events per day for a month** (selectable **per hour for a day**)." | Info tab with verified argument reference; dashboard overview with critical errors + two charts (events by severity, failed requests), 7/30/90 days, drill-down per hour in the viewer's time zone. Charts read new hourly counters. |
| 18 | "Write all requests and descriptions into a **spec** so Claude can regenerate the exact code from any account. Is similar documentation already included? Give a downloadable link." | This document (`docs/SPEC.md`). |

---

## Part B — Consolidated specification

### B1. Product summary

SiteMonitor is a **single Go binary** that, on two servers in two regions
(**IDC** and **FC**), periodically checks websites/APIs and runs user/admin
**jobs**, with automatic **failover** between the servers, a **witness** to
prevent split-brain, an embedded **SQLite** database for results and
statistics, **incidents** instead of alert storms, **authentication** (local
users or Active Directory), a **configuration API**, a **tabbed web
dashboard**, **Prometheus metrics**, and a complete **API for agents**
(bearer tokens, OpenAPI).

Non-goals: load testing, crawling, vulnerability scanning, e-mail sending
(advisory only), SSO/MFA (put an OIDC proxy in front).

### B2. Technology and dependencies

| Item | Choice |
|---|---|
| Language | Go **1.26** (`go.mod`: `go 1.26.0`) |
| Modules | `gopkg.in/yaml.v3`, `golang.org/x/crypto` (bcrypt, ssh, ssh/knownhosts), `github.com/go-ldap/ldap/v3`, `modernc.org/sqlite` (pure Go; keeps `CGO_ENABLED=0`). Test only: `github.com/jimlambrt/gldap` (in-process LDAP server) |
| Web UI | Plain HTML/CSS/JS embedded with `go:embed`; **no** frontend build, **no** external scripts (strict CSP); SVG charts drawn in JS |
| Container | Multi-stage Dockerfile: build `golang:1.26-alpine`; runtime targets `runtime` (default, `alpine:3.20` + `python3 perl bash ca-certificates tzdata`, user uid/gid **65532** `nonroot`) and `runtime-minimal` (`gcr.io/distroless/static-debian12:nonroot`) |
| Binary build | `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o monitor ./cmd/monitor`; optional `CGO_ENABLED=1 -tags sitemonitor_cgo` for the native startup hook |

### B3. Repository layout

```
cmd/monitor/main.go           subcommands: run | witness | gen-certs | demo-target | demo-ssh | hash-password
internal/config/              node.yaml + sites.yaml types, defaults, validation, ${VAR} expansion
internal/checks/              availability, tls, cert, headers, api checks; Result type
internal/periodic/            the periodic engine (min-heap, timer, per-class pools)
internal/scheduler/           check provider: sites.yaml -> engine tasks; per-host limits; RunNow
internal/jobs/                jobs: model, repo, binaries, local runner, SSH runner, service, sync
internal/startup/             native startup hook: hook.go, hook_stub.go (!sitemonitor_cgo), hook_cgo.go, startup_hook.h, startup_hook.c (placeholder)
internal/cluster/             heartbeats, state machine, witness client, replication, peer helpers
internal/witness/             lease server
internal/alert/               evaluator (flap control, peer verify, incidents seeding), notifier (log + webhook)
internal/store/               SQLite: results, stats, events, incidents, tokens, activity counters, legacy import
internal/auth/                manager (sessions, CSRF, lockout, basic cache, bearer), local + ldap providers
internal/configstore/         validated/atomic/backed-up edits of sites.yaml and node.yaml
internal/server/              route table + OpenAPI, dashboard/config/jobs/peer APIs, metrics, charts, web/
internal/server/web/          index.html, app.css, app.js
internal/certs/               private CA + mTLS cert generation; TLS configs; ClientCN
internal/demotarget/          demo websites (HTTPS) and demo SSH server
config/idc/, config/fc/       node.yaml + sites.yaml per server (demo)
docs/                         API.md AUTHENTICATION.md COMPILATION.md DEPLOYMENT.md DESIGN.md JOBS.md SPEC.md + screenshots
Dockerfile, docker-compose.yml, .env.example, .gitignore, README.md
```

### B4. Configuration

All YAML is decoded with **unknown keys rejected** (`KnownFields(true)`).
`${VAR}` references are expanded from the environment **only in the
`${NAME}` form** (a custom expander, so bcrypt hashes like `$2a$12$…` are not
mangled). Secrets are always `${VAR}` references.

#### B4.1 `node.yaml` (one per server)

| Key | Default | Rules / meaning |
|---|---|---|
| `node.id` | — (required) | identity; equals the mTLS certificate CN |
| `node.name` | = id | display name |
| `node.region` | — | `IDC` or `FC` |
| `node.peer_listen` | `:8443` | mTLS peer listener |
| `node.dashboard_listen` | `:8080` | HTTPS dashboard/API listener |
| `node.data_dir` | `/data` | database, ownership.yaml, jobs dirs |
| `peer.id` / `peer.name` / `peer.region` / `peer.url` | — / = id / other region / — | peer.region must differ from node.region; peer.id ≠ node.id |
| `peer.heartbeat_interval` / `heartbeat_timeout` | 5s / 2s | timeout ≤ interval |
| `peer.suspect_after` / `down_after` | 3 / 6 | missed beats; down ≥ suspect |
| `peer.handback_stable_for` | 2m (demo 1m) | |
| `witness.url` / `required_for_takeover` / `lease_ttl` | "" / false / 20s | empty url = no witness |
| `tls.cert` / `tls.key` / `tls.ca` | — | required |
| `monitor.sites_file` / `failures_to_alert` | — / 2 | |
| `limits.workers` | 20 | 1..1000; check pool workers |
| `limits.per_host_limit` | 2 | 1..20 |
| `limits.queue_size` | 500 | ≥ workers; check pool wait queue |
| `limits.verify_concurrency` | 4 | concurrent `/peer/verify` served |
| `limits.max_sites` | 2000 | |
| `limits.history_per_check` | 200 | raw results kept per site/check (1..100000) |
| `limits.max_events` | 500 | |
| `limits.max_response_body_kb` / `max_redirects` | 1024 / 5 | applied to checks |
| `limits.replication_queue` / `replication_batch` / `replication_interval` | 20000 / 1000 / 2s | batch 1..queue |
| `limits.max_replication_body_mb` / `max_api_body_kb` / `max_config_size_kb` | 32 / 1024 / 1024 | |
| `limits.snapshot_interval` | 5m | deprecated alias, default for `db_maintenance_interval` |
| `limits.db_maintenance_interval` | = snapshot_interval | retention clean-up |
| `limits.hourly_stats_retention` / `daily_stats_retention` | 168h / 9600h | ≥ 25h / ≥ 31 days |
| `limits.http_read_timeout` / `http_write_timeout` | 30s / 60s | |
| `auth.providers` | `[local]` | ordered list of `local`, `ldap` |
| `auth.session_ttl` / `max_sessions` / `basic_cache_ttl` | 8h / 1000 / 1m | |
| `auth.max_failed_logins` / `lockout` | 5 / 5m | per user+IP |
| `auth.local.users[]` | — | `{username, password_hash (bcrypt, must start "$2"), role, display_name}`; required if provider local |
| `auth.ldap.*` | see B9 | url, start_tls, ca_file, insecure_skip_verify, bind_dn, bind_password, base_dn, user_filter (default `(&(objectClass=user)(sAMAccountName={username}))`, must contain `{username}`), display_attr (`displayName`), nested_groups, role_groups{role: [DNs]}, default_role, timeout (5s). `ldap://` without `start_tls` is refused |
| `config_api.enabled` / `sync_to_peer` / `auto_pull_newer` / `backups` | true / true / true / 10 | |
| `jobs.enabled` | false (true in shipped config) | |
| `jobs.binaries_dir` / `work_dir` | `<data_dir>/jobs/bin` / `<data_dir>/jobs/work` | |
| `jobs.max_binary_size_mb` | 50 | 1..1024 |
| `jobs.max_concurrent` | 4 | 1..100; job pool workers (queue = 4×) |
| `jobs.default_timeout` / `max_timeout` | 5m / 30m | default ≤ max |
| `jobs.max_output_kb` | 64 | per stream |
| `jobs.user_max_run_for` | 24h | 1s..24h |
| `jobs.require_approval` | false | true = user jobs wait for an admin |
| `jobs.interpreters` | `.py: [python3]`, `.pl: [perl]`, `.ps1: [pwsh, -NoLogo, -NoProfile, -NonInteractive, -File]`, `.sh: [sh]`, `.bash: [bash]` | local runs |
| `jobs.exit_codes` | `{ok: [0], warn: [1]}` | others = CRIT |
| `jobs.startup_hook` | false (true in shipped config) | |
| `jobs.remote.known_hosts` / `connect_timeout` / `remote_dir` | `<data_dir>/jobs/known_hosts` / 10s / `/tmp` | |
| `jobs.remote.credentials` | — | `name: {user, password, private_key, passphrase}`; user required; secret may be empty at load (fails at run time) |

#### B4.2 `sites.yaml` (identical on both servers)

```yaml
version: "…"
defaults: { timeout: 15s, user_agent: "SiteMonitor/1.0", cert_warn_days: 30, cert_crit_days: 7,
            intervals: { availability: 1m, api: 5m, cert: 6h, tls: 24h, headers: 24h } }
sites:
  - name, region (IDC|FC), url (http/https), owner, criticality (high|medium|low, default medium),
    checks [availability|tls|cert|headers|api] (default availability,tls,cert),
    timeout, intervals{check: dur}, tls_verify (bool, default true),
    expect {status, contains, max_latency_ms},
    api [{name, method (GET), path, body, headers{} (${VAR} allowed), expect {status (200), json_path ($.a.b), equals, exists}}]
```
Validation: unique names, valid region/url/checks/criticality, `max_sites`.
The file's SHA-256 is its hash (compared between peers).

### B5. Checks (`internal/checks`)

Each check gets a context with a deadline (`site.timeout`; TLS check 2×) and
returns `Result{ID, Site, Region, Check, Node, NodeRegion, Status OK|WARN|CRIT, Message, LatencyMs, Details map, Time}`.

| Check | Behaviour |
|---|---|
| availability | GET with httptrace (dns/connect/tls/ttfb ms), follow ≤ `max_redirects`, read ≤ `max_response_body_kb`, keep-alive disabled. CRIT on error/unexpected status (expect.status, else ≥400)/missing `contains`; WARN if latency > `max_latency_ms` |
| tls | One handshake per version pinned (TLS1.0–1.3, all suites offered); one TLS≤1.2 handshake offering only weak suites (Go insecure list + static-RSA + CBC). CRIT if 1.0/1.1 enabled or neither 1.2 nor 1.3; WARN if 1.3 or 1.2 missing or a weak suite accepted. SSLv3 reported "not testable" |
| cert | Handshake, leaf cert: days_left, subject, issuer, not_after, sha256, OCSP stapled; chain+hostname verified against system roots. CRIT expired/≤ crit days/chain invalid (if tls_verify); WARN ≤ warn days |
| headers | WARN on missing HSTS (https), CSP, X-Content-Type-Options, Referrer-Policy, frame protection; version leaks in Server/X-Powered-By; cookies without Secure/HttpOnly |
| api | Each call in order; status + JSON-path assertions; CRIT if any fails |

### B6. Periodic engine (`internal/periodic`) — the core module

- `Task{Key, Class, Every, First, Shed bool, Run func(ctx)}`; `PoolConfig{Workers, Queue}`.
- `New(map[class]PoolConfig)`, `Upsert(task)`, `Remove(key)`, `Sync(class, []Task)`,
  `Next(key)`, `Running(key)`, `Stats(class) ClassStats{Tasks, Running, Waiting, Workers, QueueCap, Completed, Dropped, Skipped, Shed, LastDone}`,
  `Do(ctx, class, fn)` (ad-hoc run through the pool, blocking), `Run(ctx)`, `Tick(now)` and `Wait()` (tests).
- **Min-heap** by next run time; **one timer** sleeps until the earliest task; no work while idle (benchmark: ~34 ns, 0 allocs with 100k tasks).
- `Sync`: tasks still present with the same `Every` **keep their next run time**; changed period → reschedule from `First`; missing → removed.
- Dispatch: next = next+Every; if still ≤ now → now+Every (**no catch-up**). If the task is running → **skip** (no overlap). If `Shed` and the pool is saturated (all workers busy and waiting ≥ 80% of queue) → postpone 3 intervals. If the pool is full and the queue is full → **drop**, retry after min(Every, 10s). Otherwise start a goroutine that waits for a pool slot.
- Classes used: `check` (limits.workers / queue_size) and `job` (jobs.max_concurrent / 4×).
- Providers declare tasks with `Reconcile()`: on start, config change, job change, peer merge, **ownership change** (cluster callback), and a 30 s safety ticker. A new task's first run is random within `min(interval, 2m)` (checks) or 30 s (jobs) but **never before last stored result + interval** (`store.LastResultTime`).

### B7. Two-region cluster (`internal/cluster`, `internal/witness`)

- **Heartbeat** every `heartbeat_interval`: `POST /peer/heartbeat` (mTLS) carrying `{node_id, node_name, region, epoch, mode, owned_regions, config_version, config_hash, config_mod_time, jobs_hash, worker_saturation, last_check_completed, self_healthy, time}`; the peer replies with its own. Self-healthy = scheduler completed a check within 3× shortest interval + 30 s (or has no tasks).
- **Peer status**: 0 missed = HEALTHY; ≥ suspect_after = SUSPECT; ≥ down_after = DOWN.
- **Ownership**: home region owned unless the healthy peer claims it; with a witness, owned iff the home **lease** is granted (if the witness is unreachable, keep the heartbeat rule). **Takeover** when the peer is DOWN: without a witness immediately; with a witness only if the peer-region lease is granted (blocked if unreachable and `required_for_takeover`). **Handback** when the peer has been HEALTHY for `handback_stable_for` (release lease). A restarted server whose region is held by the peer stays **STANDBY**. Modes: NORMAL, TAKEOVER, STANDBY. Epoch increments on every change and the higher epoch is adopted. State is written to `<data_dir>/ownership.yaml` (node, node_name, peer, peer_name, region, epoch, mode, owned_regions, peer_status, last_peer_heartbeat, updated_at) and restored at start. `OnOwnershipChange` callback fires after changes.
- **Witness**: `POST /v1/lease {region, node, ttl_seconds}` → 200 granted / 409 held; `DELETE /v1/lease?region&node`; `GET /v1/leases`; client cert CN must equal `node`. Leases released on graceful shutdown.
- **Replication**: results and events queued and pushed in batches (`POST /peer/replicate`); bounded queue drops oldest; probe every 15 s while the peer is down. Results/events de-duplicated by ID.
- **Cross-region verify**: before alerting "availability CRIT", ask the peer (`POST /peer/verify`, bounded by `verify_concurrency`): if the peer sees it up → WARN "suspect local network issue"; verify results are stored and replicated.
- **sites.yaml sync**: config API changes pushed (`PUT /peer/config/sites`); on hash mismatch for 3 beats the server with the **older** mtime pulls (`GET /peer/config/sites`); the origin mtime is set on the synced file.
- **Jobs sync**: jobs, binary metadata and API tokens; last-writer-wins by version with tombstones; push via `POST /peer/jobs/changed` (peer pulls `GET /peer/jobs/state`), and a pull when `jobs_hash` differs for 2 beats; binary contents fetched via `GET /peer/jobs/binaries/{name}` and verified by SHA-256.
- Peer listener requires the client cert CN = `peer.id`.

### B8. Storage (`internal/store`, SQLite `<data_dir>/sitemonitor.db`)

Pragmas: WAL, `busy_timeout(5000)`, `synchronous(NORMAL)`, `temp_store(MEMORY)` (read-only root filesystem). Writes serialised by a mutex; 8 connections.

| Table | Content / rule |
|---|---|
| `results` | raw results (id PK, site, check_name, region, node, node_region, status, message, latency_ms, details JSON, time_ms); newest `history_per_check` kept per site/check |
| `check_state` | per site/check: current result (only replaced by a newer result) + lifetime counters total/ok/warn/crit, latency_sum/max, consecutive_failures, last_success_ms, last_failure_ms, first_seen_ms, cert_days_left |
| `check_hourly`, `check_daily` | per site/check buckets (total/ok/warn/crit/latency_sum/max) |
| `events` | id PK, time_ms, node, kind, severity, site, check_name, key, message; newest `max_events` kept |
| `incidents` | key PK, status, severity, first_seen_ms, cleared_at_ms, data JSON |
| `event_hourly`, `result_hourly` | global chart counters: events per UTC hour by severity; requests/failures per UTC hour; kept `daily_stats_retention` |
| `api_tokens` | id, name, role, hash (sha256 of secret), created_by, created_ms, expires_ms, version, deleted |
| `jobs`, `job_binaries` | owned by `internal/jobs` (see B11) |
| `meta` | key/value (e.g. `activity_backfill`) |

Rules:
- `AddResult` (one transaction): insert-or-ignore the result (duplicate → no change); update lifetime counters (always), current state (only if newest), **failure streak independent of arrival order** (failure newer than last success → +1; newest success → count of CRIT results after it), hourly/daily buckets, `result_hourly`.
- WARN counts as a successful request; CRIT is a failure.
- `Stats(now)`, `Summary(sites, certWarnDays, now)`, `WorstBySite()`, `Latest()`, `History()`, `RecentResults(check, n)`, `Events(n)`, `Incidents(n)`, `IncidentLevel(key)`, `OpenIncidentCounts()`, `LastResultTime(site, check)`, `Activity(start, step, n)`, `Maintain(now)`.
- `Activity`: each UTC hour belongs to the bucket containing its **start** (ceil bounds), so days and hours add up in half-hour time zones.
- One-time **legacy import** of `results.json` (renamed `.imported`) and a one-time **backfill** of the activity counters.

### B9. Incidents and alerts (`internal/alert`, `internal/store`)

- Problem-type events carry a **key**: `site:<site>|<check>`, `peer:<node>-><peer>`, `witness:<node>`, `failover:<region>`, `standby:<node>`, `config-mismatch`, `takeover-blocked:<node>`.
- Incident rules: first report → OPEN; same problem while open → **not stored, not notified**, occurrences++, `last_seen`, `seen_by`, `first_seen` moves to the **oldest** report; severity change → stored + SEVERITY transition; clear (OK) → stored, CLEARED, annotated "first seen …, open for …"; duplicate clear → suppressed; problem after clear → stored as `REAPPEARED: … (first seen …, cleared …, reappearance #n)`. History ≤ 50 transitions (first kept); ≤ 2000 incidents (oldest cleared evicted).
- Evaluator: CRIT only after `failures_to_alert` consecutive failures; WARN immediately; events only on level change; state seeded from the open incident level (restart/takeover continue the incident).
- Notifier: JSON log + optional `ALERT_WEBHOOK_URL` (`{"text": …}`).

### B10. Authentication (`internal/auth`)

- Providers tried in order; `ErrNoRole` stops the chain; other errors fall through (AD down → local break-glass).
- **local**: bcrypt (cost 12); dummy hash compare for unknown users. `monitor hash-password` reads a password (≥ 12 chars) from stdin.
- **ldap**: TLS only; service-account bind → search (escaped filter, exactly one entry) → **bind as the user** (password check) → role from `memberOf` (case-insensitive) or, with `nested_groups`, `(member:1.2.840.113556.1.4.1941:=<userDN>)` on each mapped group; highest role wins; empty password rejected.
- Roles: viewer < operator < admin.
- Sessions: `POST /api/login` (JSON only, same-origin) → cookie `sm_session` (HttpOnly, Secure, SameSite=Strict) + CSRF token; writes with a cookie need `X-CSRF-Token`; cross-origin writes rejected (Origin check). HTTP Basic accepted (successful logins cached `basic_cache_ttl`); `WWW-Authenticate` omitted when `X-Requested-With` is present.
- **Bearer API tokens** (`Authorization: Bearer smt_…`): created by admins, only SHA-256 stored, optional TTL, revocable, replicated.
- Lockout after `max_failed_logins` per user+IP for `lockout` (HTTP 429, AUTH event).

### B11. Jobs (`internal/jobs`)

**Model** — `Job{ID, Name (immutable, ^[a-z0-9][a-z0-9._-]{0,62}$), Description, Scope user|admin, Status approved|disabled|pending|rejected, Owner, Region IDC|FC, Binary, Args[] (≤32, ≤1024 chars, no NUL), Schedule{Type interval|startup, Every (one of 1m 2m 5m 10m 15m 20m 30m 60m 1h 2h 4h 6h), RunFor, StartAt, EndsAt}, Timeout, Target{Host, Port (22), OS linux|windows-cygwin, Credential}, ApprovedBy/At, BinarySHA, MergedBy/At, MergeRequest{Status pending|approved|rejected, RequestedBy/At, Note, DecidedBy/At, Reason}, Reason, CreatedAt, UpdatedAt, UpdatedBy, Version, Deleted}`.
State shown: active / scheduled / expired / disabled / pending / rejected.

**Workflow**
- Operator creates a **user job** → **active immediately** (pinned SHA, window StartAt=now, EndsAt=now+RunFor; RunFor default = user max 24h; > 24h rejected).
- Admin creates an **admin activity** (no window limit) unless `scope: user`.
- Owner/admin may edit (window restarts), disable/enable, delete, run now; others forbidden.
- **Merge request** by the owner → admin **approve merge** (scope admin, no end unless `run_for` given, MergeRequest approved) or **reject merge** (stays a user job). Admins may merge any user job directly.
- `jobs.require_approval: true` makes user jobs pending until `approve` (and `reject` exists).
- Binary replaced: the uploader's own user jobs are re-pinned automatically; other pinned jobs (admin activities) run as CRIT "changed since approval" until an admin re-approves.

**Binaries** — names `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$` (no `..`), atomic write mode 0750, size limit, kind from extension (python, perl, powershell, shell, windows-batch, executable). Only the uploader or an admin may replace; the uploader may delete only if unused; admins any.

**Execution** (through the engine's `job` pool; a job never runs twice at once)
- Before running: file SHA-256 must equal the pinned SHA.
- **Local**: `exec` **without a shell**, interpreter by extension, fresh work dir (`work_dir/<run id>`, deleted after), env only `PATH`, `HOME`, `TMPDIR`, `LANG=C.UTF-8`, `SITEMONITOR_JOB`, `SITEMONITOR_RUN_ID`, `SITEMONITOR_NODE`; own process group killed on timeout; stdout/stderr capped with a truncation note.
- **Remote (SSH)**: verify host key with `known_hosts` (no TOFU); auth by key and/or password from the named credential; push program with `umask 077 && cat > '<remote_dir>/sitemonitor-<run>-<name>' && chmod 700 …`; run; collect exit status; `rm -f` afterwards. Every argument single-quoted for the remote shell (`'` → `'\''`). Commands: Cygwin `.ps1` → `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$(cygpath -w '<p>')"`; Cygwin `.bat/.cmd` → `cmd.exe /c "$(cygpath -w '<p>')"`; `.ps1` → `pwsh …-File`; `.py` → python3; `.pl` → perl; `.sh` → sh; `.bash` → bash; else direct.
- Result stored as a check result: site `job:<name>`, check `job`, details `run_id, trigger, binary, scope, owner, target, sha256, exit_code, duration_ms, stdout, stderr`; status by `exit_codes`; message `exit N in D: <first line>`.
- **Scheduling**: interval jobs of owned regions are engine tasks (re-read before each run; skip if no longer active/owned); **startup jobs** run once after ownership is known; the jobs follow region failover.
- **Startup hook**: `int sitemonitor_on_startup(const char *node, const char *region)` (`startup_hook.h`); `startup_hook.c` is a **placeholder** gated by `//go:build sitemonitor_cgo`; stub returns `ErrNotCompiled`; result as a STARTUP event.
- Demo `demo-ssh` subcommand: SSH server (password auth) executing `/bin/sh -c`, writes its host key to a shared `known_hosts`.

### B12. HTTP API

All dashboard routes are declared once in a **route table** (method, path,
role, tag, summary, body, query, returns, handler) that both registers
handlers and generates **`GET /api/openapi.json`** (OpenAPI 3.1, `x-required-role`,
security schemes bearer/basic/session). Errors: 401, 403, 404, 409, 412
(If-Match), 413, 422, 429.

| Method & path | Role | Purpose |
|---|---|---|
| GET /healthz · GET /api/openapi.json · GET /api/auth/info · POST /api/login · POST /api/logout | public | |
| GET /api/me | viewer | user, csrf_token, node, node_name |
| GET /api/status | viewer | cluster, scheduler stats, config, **summary**, sites (status, results, stats), events (100), incidents (100) |
| GET /api/summary · /api/stats · /api/history?site&check&limit · /api/incidents?status · /api/events?limit · /api/limits · /metrics | viewer | |
| GET /api/charts/activity?days=N&tz=M · ?day=YYYY-MM-DD&tz=M | viewer | buckets `{start, label, events{CRIT,WARN,OK}, requests, failures}` per local day / hour |
| GET/POST /api/tokens · DELETE /api/tokens/{id} | admin | secret returned once |
| GET /api/config/sites · /api/config/sites/{name} | viewer | YAML + ETag |
| GET /api/config/backups · POST /api/config/sites/validate · PUT /api/config/sites · PUT/DELETE /api/config/sites/{name} | operator | validated, If-Match, backup, atomic, applied, synced, audited; per-site edits keep comments (yaml.Node) |
| GET /api/config/node · POST /api/config/node/validate · PUT /api/config/node[?restart=true] · POST /api/admin/restart | admin | identity fields immutable (409); `${VAR}` never expanded in GET |
| GET /api/jobs/options · GET /api/jobs[?scope&owner&merge=pending] · GET /api/jobs/runs[?limit&status&job] · GET /api/jobs/{id} · GET /api/jobs/{id}/runs | viewer | |
| POST /api/jobs · PUT/DELETE /api/jobs/{id} · POST /api/jobs/{id}/run[?wait=true] · /disable · /enable · /merge-request | operator | owner or admin |
| POST /api/jobs/{id}/merge · /merge/reject · /approve · /reject | admin | |
| GET /api/binaries · GET /api/binaries/{name} | viewer | |
| PUT /api/binaries/{name} (raw body) · GET /api/binaries/{name}/content · DELETE /api/binaries/{name} | operator | |

Peer (mTLS, CN = peer.id): `POST /peer/heartbeat`, `POST /peer/replicate`, `POST /peer/verify`, `GET|PUT /peer/config/sites`, `GET /peer/jobs/state`, `GET /peer/jobs/binaries/{name}`, `POST /peer/jobs/changed`.

### B13. Prometheus metrics (`/metrics`, read from the database)

`monitor_peer_up`, `monitor_peer_heartbeat_age_seconds`, `monitor_ownership_epoch`, `monitor_owns_region{target_region}`, `monitor_config_hash_match`, `monitor_replication_lag_seconds`, `monitor_witness_reachable`, `monitor_queue_depth`, `monitor_worker_saturation`, `monitor_checks_completed_total`, `monitor_checks_dropped_total`, `monitor_open_incidents{severity}`, `site_check_status`, `site_check_latency_ms`, `site_check_requests_total{result=ok|warn|crit}`, `site_check_latency_ms_sum`, `site_check_latency_ms_max`, `site_check_consecutive_failures`, `site_check_last_success_timestamp_seconds`, `site_check_last_failure_timestamp_seconds`, `site_check_uptime_ratio{window=24h|30d}`, `site_cert_days_left`. Labels include node, node_name, region, site, site_region, check, checked_from. Each metric family's samples are contiguous.

### B14. Web dashboard

- Strict CSP (`default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`), HSTS, nosniff, X-Frame-Options DENY, no-referrer, COOP, no-store. **textContent only, never innerHTML.** Light and dark mode via CSS variables.
- Login page (shows server name and providers); header with mode badge, user and Sign out; refresh every 5 s (paused while typing in a form).
- **Hash routes**: `#/` dashboard, `#/websites`, `#/certificates`, `#/incidents`, `#/events`, `#/jobs`, `#/jobs/add`, `#/jobs/history`, `#/jobs/<id>`, `#/clusters`, `#/info`; filters in the query (`#/websites?status=CRIT`, `#/events?sev=CRIT`, `#/incidents?status=OPEN`, `#/?tile=critical`, `#/info?s=<section>`). Tabs in one line; Incidents shows the open count; User jobs shows the active count (hidden if jobs disabled).
- **Dashboard**: 9 KPI tiles **on one line** (horizontal scroll on narrow screens): Websites, Healthy, Warning, Critical, Certs ≤ N days, Availability 24h, Requests 24h, Failed 24h, Open incidents — all from `summary`. Clicking a tile toggles a **selection panel**: short table of matching items (≤ 25), "Open full list →" (pre-filtered tab), and **related recent events**. With **no tile selected**: **Critical errors** (open CRIT incidents; click → history) and **Activity** charts: "Events by severity" (stacked CRIT/WARN/OK) and "Critical errors: failed requests", range 7/30/90 days per day; **click a day or pick a date → per hour** (local time zone), "← back to days"; tooltip, legend with totals, recessive grid, 2 px gaps, rounded tops, "Show as table". Status colours `#d03b3b` / `#fab219` / `#0ca30c`, never colour alone.
- **Websites**: filters region/status (incl. pending); per-check cells (click → detail dialog with stored counters); uptime 24h; TLS compliance matrix. **Certificates**, **Incidents** (filter; click → timeline), **Events** (severity, kind, text search), **Clusters** (this server + peer cards).
- **User jobs**: sub-tabs **Running jobs** (active/scheduled/pending; filters scope, only mine, include expired/disabled; row → job page), **Add job & programs** (add-job form and upload form side by side + programs list; after adding, open the job's page), **Results history** (`/api/jobs/runs` with result/job filters + finished jobs). **Job page**: actions by role (Run now, Disable/Enable, Request merge, Approve/Reject merge, Merge, Approve, Re-approve program, Delete), definition, statistics, runs with stdout/stderr.
- **Info**: table of contents; getting started; schedules/limits/exit codes/environment; how arguments are passed (one line = one argument, no shell, no added quotes); **special-characters table** (spaces, quotes, `$HOME`, `$(…)`, backticks, Windows/UNC paths, wildcards, `; & | < >`, `%`, `!`, `#`, option forms, leading dash, negative numbers, JSON, regex, URL, Unicode, padding, empty arg, newline) with Windows batch/powershell.exe notes; per-language sections (Python argparse, Perl Getopt::Long, sh/bash quoting, PowerShell `-File` binding, Windows batch `%~1` and characters to avoid, `.exe`/Linux executables, Cygwin path note); JSON escaping for API calls with bash (here-doc file), PowerShell (`ConvertTo-Json`) and Python examples; common pitfalls; API for agents; about this server.

### B15. Packaging and demo

- `docker-compose.yml`: `certs` (gen-certs, one-shot), `witness`, `monitor-idc-01` (8081→8080), `monitor-fc-01` (8082→8080), `target-idc-1` (365 d), `target-idc-2` (20 d, legacy TLS), `target-fc-1` (365 d, 9441→8443, `POST /admin/toggle`), `target-fc-2` (5 d, no TLS 1.3), `target-ssh` (demo-ssh, tmpfs /tmp). Monitors: read-only root, `cap_drop: ALL`, `no-new-privileges`, `mem_limit 256m`, `GOMEMLIMIT 200MiB`, volumes `./config/<region>:/etc/monitor` (writable), `certs:/certs:ro`, `<region>-data:/data`, `jobs-shared:/shared:ro`. Env: `ORDERS_API_TOKEN`, `ALERT_WEBHOOK_URL`, `LDAP_BIND_PASSWORD`, `DEMO_SSH_PASSWORD`.
- Demo logins (local provider; change before real use): `admin / Admin-Demo-Pass-1`, `operator / Operator-Demo-Pass-1`, `viewer / Viewer-Demo-Pass-1`.
- Demo `sites.yaml` per server: idc-portal (healthy), idc-legacy-app (legacy TLS, cert 20 d), fc-orders-api (API calls `/health`, `/api/v1/items` with `Bearer ${ORDERS_API_TOKEN}`), fc-reports (cert 5 d, no TLS 1.3); demo intervals availability 30s, api 1m, cert 2m, tls 5m, headers 5m; `tls_verify: false` for demo targets.
- `gen-certs`: ECDSA P-256 CA + node certs (SAN name, localhost, 127.0.0.1; EKU server+client), idempotent.

### B16. Documentation set

`README.md` (description, quick start, layout, docs index), `docs/DESIGN.md`, `docs/API.md`, `docs/AUTHENTICATION.md`, `docs/JOBS.md`, `docs/COMPILATION.md`, `docs/DEPLOYMENT.md` (sizing, network, certificates, deployment with compose/systemd/Kubernetes, runbook, troubleshooting), `docs/SPEC.md` (this file), screenshots.

---

## Part C — Acceptance tests

Unit tests (must pass with `go test ./...`):

| Package | Test asserts |
|---|---|
| config | shipped configs valid (names, limits, bcrypt hash intact); `${VAR}` only; `ldap://` without StartTLS rejected |
| auth | local login, unknown user, empty password, lockout per user+IP, sessions; **LDAP** against in-process `gldap` directory: role mapping, wrong password, no role, unknown user, filter injection; LDAP down → local fallback |
| configstore | validation, unknown keys, If-Match conflict, size limit, backups kept, per-site edits keep comments, max_sites, delete |
| store | incident lifecycle (dup suppressed, oldest first_seen, clear, reappear, severity, restart); counters on every success/failure; duplicates; late results; order-independent streak; summary; retention; legacy import; activity buckets by local day/hour in UTC+05:30 (hours add up to days) |
| periodic | order/rhythm; no catch-up; no overlap; pools bounded and isolated; drop + retry; Sync keeps schedule / removes / reschedules on period change; shedding; real timer loop; Do; benchmark |
| jobs | validation (intervals, 24 h limit, traversal, names, target); user job active immediately; merge request/approve/reject permissions; local run: arguments not shell-interpreted, exit 0/1/2 → OK/WARN/CRIT, output cap, timeout kill; replaced binary; scheduling + restart rhythm + failover + disable removes task; peer sync incl. tampered binary rejected and tombstones; **remote SSH** against in-process server (push, run, exit code, quoting, cleanup, host-key mismatch, missing known_hosts); Cygwin command lines; **26 special-character arguments** reach Python, Perl and sh unchanged locally and over SSH |

End-to-end scenarios (docker-compose): both servers NORMAL; stop FC → IDC TAKEOVER in ~30 s with FC sites/jobs; start FC → STANDBY → HANDBACK; site down → incident once, no duplicate after restart/takeover, CLEARED, REAPPEARED; counters identical on both servers; config API add/delete applies instantly and syncs; node.yaml change + restart; agent flow with bearer tokens (upload, create, run, remote SSH, merge, OpenAPI); dashboard tabs, tiles, charts, drill-down, job pages, Info tab — no browser errors.

---

## Part D — Regeneration prompt

Paste the following into a new Claude Code session (any account), with this
file attached or in the repository:

> Build the system specified in `docs/SPEC.md` exactly: Go 1.26 single binary
> `monitor` with the subcommands, packages, configuration keys and defaults,
> database tables, API routes and roles, web dashboard tabs and behaviours,
> Dockerfile targets and docker-compose services described there. Follow the
> decisions in Part A. Implement in this order: config → checks → store →
> periodic engine → scheduler → alert/incidents → certs/witness/cluster →
> auth → configstore → jobs (+ startup hook stub and cgo placeholder) →
> server (route table, OpenAPI, metrics, charts) → web UI → demo targets →
> Dockerfile/compose → docs. Write every unit test in Part C and make
> `go vet ./...`, `gofmt -l .` and `go test ./...` clean. Then run the
> end-to-end scenarios with docker compose and fix anything that fails.
> Keep the security rules (no shell for local jobs, pinned SHA-256, host-key
> verification, CSP, textContent only, secrets only via `${VAR}`).

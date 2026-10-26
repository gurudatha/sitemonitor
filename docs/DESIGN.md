# Design

## 1. Goals and non-goals

**Goals**
- Check many websites and APIs on a schedule: availability, TLS protocol
  compliance, certificate expiry, security headers, and API sanity.
- Keep the load on monitored sites negligible.
- Run in two regions (IDC and FC), each server responsible for its own
  region, with automatic failover and handback between them.
- Show everything on a secure dashboard and alert on state changes.

**Non-goals**
- Load testing, crawling, or vulnerability scanning beyond lightweight
  hygiene checks.
- Long-term analytics. The server keeps a fixed rolling window of
  recent results; for yearly history, scrape `/metrics` into Prometheus
  (see section 10).

## 2. Architecture

All of these components run in **one process** (a monolith):

```
                     ┌────────────────────────── monitor (one process) ──────────────────────────┐
 sites.yaml ──reload─►  Scheduler ──► Job queue ──► Worker pool (N) ──► Checks (deadline-bound) │
 node.yaml           │   (jitter,      (bounded)     per-host semaphore    availability/tls/     │
                     │    ownership                                        cert/headers/api      │
                     │    filter)                                               │                │
                     │      ▲                                                   ▼                │
                     │      │ owned regions              Store ◄──── result ──► Evaluator ──► Notifier (log/webhook)
                     │  Cluster manager ◄─ heartbeat ─►  (memory +              (flap control,   │
                     │  (state machine, witness lease,    snapshot)              peer verify)    │
                     │   replication queue)                  │                                   │
                     │      │                               ▼                                    │
                     │  Peer API :8443 (mTLS)        Dashboard :8080 (HTTPS + auth, /metrics)    │
                     └──────┼─────────────────────────────────────────────────────────────────────┘
                            │ mTLS                                           │
                    peer server (other region)                    witness (third location)
```

| Component | File | Responsibility |
|---|---|---|
| Config | `internal/config` | Load and validate `node.yaml` and `sites.yaml`; hash the sites file |
| Scheduler | `internal/scheduler` | Decide what is due, enqueue jobs, run workers, apply per-host limits and deadlines, hot-reload `sites.yaml` |
| Checks | `internal/checks` | Each check takes a `context` with a deadline and returns a `Result` |
| Evaluator / Notifier | `internal/alert` | Turn results into state-change events; verify with the peer before paging; post to a webhook |
| Store | `internal/store` | Recent history per site/check and an event log; de-duplicates replicated items; snapshots to disk every 30 s |
| Cluster manager | `internal/cluster` | Heartbeats, peer status, takeover and handback, witness leases, `ownership.yaml`, replication |
| Witness | `internal/witness` | Grants time-limited leases per region (the split-brain tie-breaker) |
| Server | `internal/server` | Peer API, dashboard API and UI, Prometheus metrics |

### How a check runs

1. Once a second, the scheduler builds the set of jobs
   (`site × check`) for the **regions this server currently owns**.
2. A new job gets a random first run within `min(interval, 2 min)`, so
   startup and takeover never cause a burst of traffic. Every later run
   is scheduled at `interval ± 10 %`.
3. A due job goes into a bounded queue. If the queue is full, the run is
   skipped and counted in `monitor_checks_dropped_total`.
4. A worker acquires the per-host semaphore (default 2) and runs the
   check with `context.WithTimeout(site.timeout)`. The TLS check gets
   twice the timeout because it makes several handshakes.
5. The result is stored locally, queued for replication to the peer,
   and passed to the evaluator.

## 3. Checks

| Check | Implementation | Status rules |
|---|---|---|
| **availability** | `GET url` with `httptrace` timings; follows up to 5 redirects; reads at most 1 MB | CRIT: connection error, unexpected status, or content not found. WARN: latency above `max_latency_ms` |
| **tls** | One handshake per version, pinned with `MinVersion = MaxVersion` (TLS 1.0–1.3), offering all cipher suites; plus one TLS ≤ 1.2 handshake offering **only** weak suites (static RSA, CBC, 3DES, RC4) | CRIT: TLS 1.0/1.1 enabled, or neither 1.2 nor 1.3 enabled. WARN: 1.3 or 1.2 missing, or a weak cipher accepted |
| **cert** | TLS handshake, then read the leaf certificate; verify the chain and hostname against the system trust store | CRIT: expired, ≤ `cert_crit_days`, or chain invalid (when `tls_verify` is true). WARN: ≤ `cert_warn_days` |
| **headers** | `GET url`, then inspect the response headers and cookies | WARN: missing HSTS, CSP, X-Content-Type-Options, Referrer-Policy or frame protection; `Server`/`X-Powered-By` reveal a version; cookies lack `Secure`/`HttpOnly` |
| **api** | Configured calls run in order; header and body values support `${ENV}` | CRIT: any call fails its status or JSON-path assertion |

**SSLv3 cannot be tested.** Go's `crypto/tls` has no SSLv3 client, so
the result reports SSLv3 as "not testable". In practice, servers that
have disabled TLS 1.0 have also disabled SSLv3.

## 4. Load limits

| Control | Default | Where |
|---|---|---|
| Global concurrency | 20 workers | `limits.workers` |
| Concurrent connections per host | 2 | `limits.per_host_limit` |
| Per-check deadline | 15 s (30 s for `tls`) | `defaults.timeout` / `site.timeout` |
| Interval jitter | ±10 % | scheduler |
| Startup / takeover spreading | random within `min(interval, 2 min)` | scheduler |
| Response body cap | 1 MB | checks |
| Keep-alive | disabled; no idle sockets left open on targets | checks |
| Shedding under saturation | `criticality: low` sites run at 3× their interval when the queue is > 80 % full or all workers are busy | scheduler |
| Identification | Custom `User-Agent` with a contact address | `defaults.user_agent` |
| API safety | Configure **read-only or idempotent** calls only; use test accounts | `sites.yaml` |

Recommended production intervals: availability 1–5 min, API 5–15 min,
certificate 6–12 h, TLS and headers daily. At 200 sites with a 1-minute
availability check, that is about 3.3 requests per second **in total**,
and one request per minute per site.

## 5. Two-region sync and failover

### 5.1 Roles

- Every server has a **home region** (`node.region: IDC|FC`) and exactly
  one **peer** in the other region. Both are set in `node.yaml`.
- Every site has a **region** (`sites[].region: IDC|FC`) in `sites.yaml`.
- A server checks a site only when it currently **owns** the site's
  region.

### 5.2 Heartbeat

Every `heartbeat_interval` (5 s), each server sends
`POST /peer/heartbeat` to its peer over mutual TLS. The peer answers
with its own heartbeat, so one call exchanges state in both directions.

```json
{ "node_id": "monitor-idc-01", "region": "IDC", "epoch": 6, "mode": "NORMAL",
  "owned_regions": ["IDC"], "config_version": "2026-10-26.1", "config_hash": "9f2c…",
  "worker_saturation": 0.05, "last_check_completed": "…", "self_healthy": true }
```

A peer counts as **working** only if it answers within 2 s **and**
reports `self_healthy`. A server is self-healthy if it has completed a
check recently: within 3× the shortest interval plus 30 s. A server that
is running but no longer performing checks therefore counts as down.

| Consecutive missed beats | Peer status |
|---|---|
| 0 | HEALTHY |
| ≥ `suspect_after` (3, which is 15 s) | SUSPECT (alert only; no ownership change) |
| ≥ `down_after` (6, which is 30 s) | DOWN (start a takeover) |

### 5.3 State machine

```
            peer healthy                         peer DOWN + witness grants peer-region lease
 ┌────────────────────────┐   ───────────────────────────────────────────────────────►   ┌──────────────────────┐
 │ NORMAL                 │                                                               │ TAKEOVER             │
 │ owns: home region      │   ◄───────────────────────────────────────────────────────    │ owns: home + peer    │
 └────────────────────────┘     peer HEALTHY for handback_stable_for → release lease      └──────────────────────┘
            ▲
            │ peer hands back (lease free / peer stops claiming our region)
 ┌────────────────────────┐
 │ STANDBY                │   ← a server that restarts while the peer still owns its region
 │ owns: nothing          │
 └────────────────────────┘
```

**Takeover** (example: IDC takes over FC)
1. FC misses 6 heartbeats.
2. IDC asks the witness for the **FC lease**. The witness grants it only
   if FC has not renewed the lease within `lease_ttl` (20 s).
3. IDC increments its `epoch`, adds FC to its owned regions, writes
   `ownership.yaml` and emits a `FAILOVER` event.
4. The scheduler picks up FC's sites with spread-out start times.
   Results show `checked_from = IDC`, which the dashboard marks as
   "(failover)".

**Handback**
1. FC restarts. Its home-lease request is refused because IDC holds the
   lease, so FC enters **STANDBY** and sends heartbeats with
   `owned_regions: []`.
2. Once FC has been healthy for `handback_stable_for`, IDC releases the
   FC lease, removes FC from its owned regions, increments the epoch and
   emits `HANDBACK`.
3. On its next heartbeat, FC acquires its home lease and starts
   monitoring FC again (event: "resumed monitoring home region").

The handback gap is at most one heartbeat interval. New jobs are
spread out, so the first checks after handback start within the first
2 minutes.

### 5.4 Split-brain protection (witness)

The witness runs at a **third location**. It is a small in-memory lease
table: `region → (holder, expiry)`. Every server renews the lease for
each region it owns on every heartbeat.

| Situation | Outcome |
|---|---|
| FC is truly down | FC stops renewing; the lease expires; IDC acquires it and takes over |
| The IDC–FC link breaks, but both servers reach the witness | IDC sees FC as DOWN, but the witness still shows FC renewing, so IDC's request is refused (409) and it does **not** take over. A `FAILOVER … blocked` event explains why |
| The witness is unreachable, with `required_for_takeover: true` | No takeover. Both servers keep monitoring their home regions (safe default) |
| The witness is unreachable, with `required_for_takeover: false` | Takeover happens on heartbeat failure alone. Both servers may briefly check the same region; this is harmless because checks are read-only |
| No witness configured (`url: ""`) | Ownership is decided by heartbeats only; a server does not take its home region while a healthy peer still claims it |

Any server that holds the lease for a region may act on it. The `epoch`
(the higher one wins and is adopted from the peer) orders ownership
changes, so the timeline in the dashboard is unambiguous.

### 5.5 What is kept in sync

| Data | Mechanism | Conflicts |
|---|---|---|
| `sites.yaml` | Distributed by Git or CI to both servers; its SHA-256 is sent in every heartbeat | Mismatch produces a `CONFIG WARN` event and `monitor_config_hash_match = 0` |
| Check results | Queued, then pushed in batches every 2 s (`POST /peer/replicate`); the queue is bounded at 20k and drops the oldest | Append-only; de-duplicated by result ID (`node-start-seq`) |
| Events (alerts, failovers) | Same batch mechanism | De-duplicated by event ID |
| Ownership | Heartbeat (`owned_regions`, `epoch`) plus witness leases | Lease holder wins; higher epoch is adopted |
| Catch-up after an outage | The sender keeps its queue while the peer is down (retries every 15 s) and flushes once the peer is back | — |

Both servers hold the full picture, so **either dashboard shows
everything**.

## 6. Alerting

- **Flap control:** a site/check is marked CRIT only after
  `failures_to_alert` (2) consecutive CRIT results. WARN is reported
  immediately.
- **Only changes are reported:** `ALERT` fires on OK→WARN/CRIT,
  `RECOVERY` on back-to-OK.
- **Cross-region verification:** before paging for an availability
  failure, the server calls `POST /peer/verify` and the peer runs the
  same check from its region.
  - If the peer also sees the site down: CRIT, "confirmed DOWN from FC
    too".
  - If the peer sees it up: WARN, "unreachable from IDC but UP from FC:
    suspect local network issue".
- **Delivery:** every event is logged as JSON on stdout and, if
  `ALERT_WEBHOOK_URL` is set, posted as `{"text": …}`. This format works
  with Slack, Teams workflows and Mattermost.
- **Cluster events:** `PEER` (HEALTHY, SUSPECT or DOWN), `FAILOVER`,
  `HANDBACK`, `CONFIG` (reload or mismatch), and witness reachability.

## 7. Security

| Area | Measure |
|---|---|
| Server-to-server traffic | Mutual TLS, **TLS 1.3 only**, private CA. The peer API also requires the client certificate's CN to equal `peer.id`; the witness requires the CN to equal the requesting `node` |
| Users | `internal/auth`: local bcrypt users (POC) or Active Directory over LDAPS/StartTLS (production), selected by `auth.providers`. Roles are viewer, operator and admin. Sessions use HttpOnly + Secure + SameSite=Strict cookies, with CSRF tokens on writes and cross-origin writes rejected; lockout is per user+IP. See [AUTHENTICATION.md](AUTHENTICATION.md) |
| Configuration API | Writes require the operator or admin role; changes are validated, size-limited, protected against conflicts with `If-Match`, backed up, written atomically and audited with the user name. Identity fields can't be changed remotely |
| Browser hardening | CSP `default-src 'none'; script-src 'self'; style-src 'self'; frame-ancestors 'none'`; HSTS; nosniff; `X-Frame-Options: DENY`; COOP; `no-store`. The UI uses `textContent` only, never `innerHTML`, so data from monitored sites cannot inject markup |
| Secrets | Not stored in YAML. `${ENV}` expansion is used for API tokens; the dashboard credentials and webhook URL come from the environment |
| Input limits | Request bodies are capped (heartbeat 64 KB, replication 32 MB); read, write and header timeouts are set on every listener |
| Container | Distroless static image, no shell, uid 65532, `read_only: true`, `cap_drop: ALL`, `no-new-privileges` |
| Language | Go is memory-safe; the only dependency is `gopkg.in/yaml.v3` |

## 8. Observability

The dashboard shows:
- KPI tiles
- A cluster panel for this server and its peer: mode, owned regions,
  epoch, heartbeat age, config match, replication lag and witness status
- A table of sites with their region and where they were checked from
- A certificate expiry board, sorted by days remaining
- A TLS compliance matrix
- An event log
- Click any cell for the full details of that result

`GET /metrics` (viewer role; Prometheus can use Basic auth) exposes these Prometheus metrics:

| Metric | Meaning |
|---|---|
| `monitor_peer_up` | 1 if the peer is HEALTHY |
| `monitor_peer_heartbeat_age_seconds` | Seconds since the last successful heartbeat |
| `monitor_ownership_epoch` | Increases on every failover or handback |
| `monitor_owns_region{target_region}` | 1 if this server monitors that region |
| `monitor_config_hash_match` | 1 if `sites.yaml` is identical on both servers |
| `monitor_replication_lag_seconds` | Age of the oldest unreplicated item |
| `monitor_witness_reachable` | 1 if the witness answered |
| `monitor_queue_depth`, `monitor_worker_saturation` | Scheduler pressure |
| `monitor_checks_completed_total`, `monitor_checks_dropped_total` | Throughput, and runs skipped because the queue was full |
| `site_check_status{site,site_region,check,checked_from}` | 0 = OK, 1 = WARN, 2 = CRIT |
| `site_check_latency_ms` | Latency of the latest check |
| `site_cert_days_left{site}` | Days until the certificate expires |

`GET /healthz` (no auth) returns 503 when the server is not completing
checks. Use it for load balancer or orchestrator health checks.

## 9. Configuration, limits and the configuration API

### 9.1 Configurable limits (`node.yaml` → `limits:`)

Every goroutine pool, queue and buffer is bounded and set in YAML.
Changes take effect after a restart, which you can trigger with
`PUT /api/config/node?restart=true`.

| Key | Default | Bounds |
|---|---|---|
| `workers` | 20 | Goroutines running checks (1–1000) |
| `per_host_limit` | 2 | Concurrent connections to one site (1–20) |
| `queue_size` | 500 | Pending jobs (≥ workers); overflow is skipped and counted |
| `verify_concurrency` | 4 | Concurrent cross-region verify requests served |
| `max_sites` | 2000 | Larger `sites.yaml` files are rejected |
| `history_per_check` | 200 | Results kept per site/check (memory) |
| `max_events` | 500 | Events kept |
| `max_response_body_kb` | 1024 | Bytes read from a monitored site |
| `max_redirects` | 5 | Redirects followed |
| `replication_queue` / `replication_batch` / `replication_interval` | 20000 / 1000 / 2s | Peer replication |
| `max_replication_body_mb` | 32 | Largest batch accepted from the peer |
| `max_api_body_kb` / `max_config_size_kb` | 1024 / 1024 | API request and config file size |
| `snapshot_interval` | 30s | `results.json` write period |
| `http_read_timeout` / `http_write_timeout` | 30s / 60s | Server timeouts |

Other bounded goroutines:
- Heartbeat: one, with `peer.heartbeat_timeout`.
- Replication: one.
- Snapshot: one.
- Auth housekeeping: one.
- Webhook sends: one per event, with a 10 s timeout.

### 9.2 Server and peer names

- `node.id` and `peer.id` are identities. They must match the
  certificate CN.
- `node.name` and `peer.name` are human-friendly display names. They
  appear in the dashboard, events, `/api/limits`, `ownership.yaml` and
  the `node_name` metrics label.

### 9.3 Configuration API flow

```
PUT /api/config/sites ──► auth (operator) ──► size limit ──► If-Match check ──► parse + validate + max_sites
     ──► backup old file ──► atomic write (temp + rename) ──► scheduler.SetSites (immediate)
     ──► CONFIG event (user, diff) ──► push to peer (PUT /peer/config/sites, mTLS)
                                         └─ peer down? ─► peer pulls the newer file on return
                                                          (heartbeat carries config hash + mtime)
```

The two copies of `sites.yaml` converge automatically:
1. Each heartbeat carries the config hash and the file's modification
   time.
2. After 3 mismatched beats, the server with the **older** file fetches
   the newer one from its peer (`config_api.auto_pull_newer`).
3. Synced files get the origin's modification time, so the servers
   don't ping-pong.

## 10. Data storage and retention

### 10.1 What each server stores

| Data | Where | Format | Size |
|---|---|---|---|
| Check results (all sites, both regions, because of replication) | Memory, plus `/data/results.json` snapshot every 30 s and on shutdown (atomic write-and-rename) | JSON, about **0.4 KB per result** on disk, 0.6 KB in memory | **Fixed**: `sites × checks × history_per_check × 0.4 KB` |
| Events (alerts, failovers, config changes) | Same snapshot | JSON | Last 500 events, about 150 KB |
| Ownership state | `/data/ownership.yaml` | YAML | < 1 KB |
| Configuration | `node.yaml`, `sites.yaml` (read-only) | YAML | KB |

Results are a **rolling window**: each site/check keeps its newest
`history_per_check` results (default 200), and older ones are discarded.
**Disk and memory usage therefore do not grow over time.** A year from
now the server stores the same amount as after the first day.

| Websites (4 checks each) | `/data` on each server (constant) |
|---|---|
| 100 | ~32 MB |
| 500 | ~160 MB |
| 1,000 | ~320 MB |

How much time the window covers depends on each check's interval. With
the production defaults and 200 results:

| Check | Interval | Window covered |
|---|---|---|
| availability | 1 min | ~3.3 hours |
| api | 5 min | ~17 hours |
| cert | 6 h | ~50 days |
| tls / headers | 24 h | ~200 days |

The built-in store is therefore for **current state and recent
history** (the dashboard), **not** for yearly reporting.

### 10.2 Keeping a year of history

A year of every result, per site with all 5 checks at production
intervals:

```
availability 525,600 + api 105,120 + cert 1,460 + tls 365 + headers 365  ≈ 633,000 results/site/year
```

| Approach | Per site per year | 100 sites | 500 sites | 1,000 sites | Notes |
|---|---|---|---|---|---|
| Every raw result (JSON files or a PostgreSQL table with indexes) | ~250 MB | ~25 GB | ~125 GB | ~250 GB | Full detail; needs a database and partitioning. Doubles if both servers keep a copy |
| **Prometheus** scraping `/metrics` every 60 s (~11 series per site, ~1.5 bytes per compressed sample) | ~9 MB | ~1 GB | ~4.5 GB | ~9 GB | **Recommended.** Status, latency and certificate days kept as time series; PromQL for SLO and uptime reports |
| Hourly roll-ups (min, avg, max, uptime % per site/check/hour) | ~4 MB | ~0.4 GB | ~2 GB | ~4 GB | Enough for yearly SLA reports |

Recommendation: keep the built-in rolling window for the dashboard, and
let Prometheus (or Thanos or Mimir for multi-year storage) hold the
yearly history. Set Prometheus retention to `--storage.tsdb.retention.time=400d`.
Scrape both monitors; deduplicate in queries with `max by (site, check)`.
If raw per-result detail must be kept for audit, add a PostgreSQL or
TimescaleDB sink partitioned by month, and size it from the first row
of the table above.

## 11. Known limitations and next steps

| Limitation | Suggested next step |
|---|---|
| History is in memory with a JSON snapshot, limited to the last 200 results per check | Add PostgreSQL or TimescaleDB storage, or rely on Prometheus for long-term data |
| The witness keeps leases in memory; after a witness restart, holders re-acquire within one heartbeat | Acceptable as designed; alternatively, back it with etcd or Consul |
| Evaluator state (consecutive failure counts) is not replicated; after a takeover, an already-failing site may alert once more | De-duplicate alerts in the notification tool, or replicate evaluator state |
| Login is username/password (local or AD); no SSO/MFA | Put the dashboard behind an OIDC proxy (oauth2-proxy, Entra ID) for SSO + MFA; or add an OIDC provider to `internal/auth` |
| Sessions are in memory; a restart logs users out | Acceptable; or add a shared session store |
| Per-site API edits re-format the YAML (comments are kept, spacing and inline-comment placement may change) | Use the whole-file `PUT` when exact formatting matters |
| No DNS, WHOIS, CAA or SPF/DMARC checks yet | Add them as new check types in `internal/checks` (same interface) |
| Exactly two servers (one per region) | The design supports N regions in principle; the peer configuration would become a list |

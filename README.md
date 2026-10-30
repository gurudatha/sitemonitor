# SiteMonitor

A two-region website monitoring service written in Go. It runs as one
binary on one server in **IDC** and one in **FC**. Each server checks the
websites in its own region. The two servers send each other heartbeats
and stay in sync. If one server goes down, the other **automatically takes
over** its region and hands it back once the server has recovered.

## Description

Many websites and APIs need to be checked on a schedule. SiteMonitor
runs these checks without putting load on the sites:

| Check | What it verifies | Default interval |
|---|---|---|
| `availability` | Server is up, HTTP status, expected content, latency (DNS / connect / TLS / TTFB) | 1 min |
| `tls` | TLS 1.0 / 1.1 are **disabled**, TLS 1.2 / 1.3 are **enabled**, no weak cipher suites | 24 h |
| `cert` | Certificate expiry (WARN ≤ 30 days, CRIT ≤ 7 days), chain and hostname validity | 6 h |
| `headers` | HSTS, CSP, X-Content-Type-Options, frame protection, Referrer-Policy, version leaks, cookie flags | 24 h |
| `api` | Read-only API sanity calls with status and JSON-field assertions | 5 min |

Key properties:

- **One monolith** sends **time-bound** requests. Every check has a hard
  deadline and runs on a bounded worker pool, with at most 2 concurrent
  connections per host.
- **Low load.** Intervals are jittered and first runs are spread out.
  Connections are not kept alive, response bodies are capped at 1 MB,
  and low-priority sites are slowed down when the server is saturated.
- **Two regions (IDC and FC).** The two servers exchange heartbeats over
  mutual TLS. A witness lease prevents split-brain, failover takes about
  30 seconds, and handback is automatic. Results and events are
  replicated to both servers.
- **Stored metrics.** Every request, success or failure, updates the
  check's counters (OK/WARN/CRIT, latency, failure streak, last
  success/failure) and its hourly and daily totals in an embedded
  SQLite database. The dashboard and `/metrics` only read those stored
  values, and a year of daily uptime is kept.
- **Periodic jobs.** Users and admins upload their own programs
  (Python, Perl, PowerShell, shell or any executable) into one directory
  and run them on an interval (1 min to 6 h) or when the server comes
  up. Jobs run locally or on remote Linux / Windows (Cygwin) servers over
  SSH: the program is pushed, run, and its return value collected.
  - User jobs run immediately, for at most 24 hours.
  - Merging a user job into the admin's periodic activities needs admin
    approval.
  - A placeholder for a native (cgo) startup hook is included.
- **Every feature has an API**, usable by agents with bearer tokens and
  described at `/api/openapi.json`.
- **Incidents, not alert storms.** Repeats of the same problem are
  collapsed into one incident that keeps the first date. Clearing and
  re-appearance are both recorded.
- **Fewer false alarms.** An alert fires only after N consecutive
  failures. Before reporting a site as down, the server asks its peer
  in the other region to check the same site.
- **Login and roles.** Users authenticate against local accounts (for
  a POC) or **Active Directory** (for production), chosen in
  `node.yaml`. Roles are viewer, operator and admin, with sessions,
  CSRF protection, lockout and an audit trail.
- **Configuration API.** `sites.yaml` and `node.yaml` can be changed
  over authenticated HTTPS, with validation, conflict detection,
  backups, immediate apply and automatic sync to the peer server.
- **Everything is configurable in YAML.** Server and peer names,
  goroutine and worker pools, queues, size limits, timeouts.
- **Dashboard.
  Includes a cluster panel, certificate expiry board, TLS compliance
  matrix and event log, plus Prometheus `/metrics`.

## Quick start (local demo)

You need Docker with Compose v2.

```bash
cp .env.example .env
sudo chown -R 65532:65532 config/idc config/fc   # the config API writes here (container uid 65532)
docker compose up -d --build
```

| URL | What |
|---|---|
| https://localhost:8081 | IDC server dashboard |
| https://localhost:8082 | FC server dashboard |

Demo logins (local provider; **change them before real use**):

| User | Password | Role |
|---|---|---|
| admin | Admin-Demo-Pass-1 | admin |
| operator | Operator-Demo-Pass-1 | operator |
| viewer | Viewer-Demo-Pass-1 | viewer |

The demo uses a self-signed CA,
so accept the browser warning.

The demo starts four fake websites, each with known problems so every
check has something to show:

| Site | Region | Expected findings |
|---|---|---|
| idc-portal | IDC | All OK |
| idc-legacy-app | IDC | TLS 1.0/1.1 enabled (CRIT), cert expires in 20 days (WARN), missing headers |
| fc-orders-api | FC | All OK, including 2 API sanity calls |
| fc-reports | FC | Cert expires in 5 days (CRIT), TLS 1.3 disabled (WARN) |

**Try a failover:**

```bash
docker compose stop monitor-fc-01    # ~30s later IDC shows TAKEOVER and monitors FC sites
docker compose start monitor-fc-01   # FC starts in STANDBY; handback after 1 min
```

**Simulate a site outage:**

```bash
curl -k -X POST https://localhost:9441/admin/toggle   # fc-orders-api returns 503
```

## Repository layout

```
cmd/monitor/            single binary: run | witness | gen-certs | demo-target | demo-ssh | hash-password
internal/config/        node.yaml + sites.yaml loading and validation
internal/checks/        availability, tls, cert, headers, api checks
internal/scheduler/     jittered scheduling, bounded worker pool, per-host limits
internal/cluster/       heartbeats, failover/handback, witness leases, replication
internal/witness/       lease service (split-brain tie-breaker)
internal/alert/         flap control, cross-region verification, webhook notifier
internal/store/         SQLite database: results, per-check counters, hourly/daily stats, events, incidents
internal/server/        peer API (mTLS), dashboard + config API, /metrics, embedded web UI
internal/auth/          login: local bcrypt users or Active Directory (LDAP), sessions, roles
internal/configstore/   validated, atomic, backed-up edits of sites.yaml / node.yaml
internal/jobs/          periodic jobs: programs, schedules, local + SSH execution, merge workflow, peer sync
internal/startup/       native (cgo) startup hook placeholder + pure-Go stub
internal/certs/         private CA + mTLS certificate generation
internal/demotarget/    fake websites for the demo
config/idc/, config/fc/ node.yaml + sites.yaml for each server (writable by the config API)
docs/                   design, compilation and deployment guides
```

## Documentation

| Document | Contents |
|---|---|
| [docs/DESIGN.md](docs/DESIGN.md) | Architecture, checks, load limits, two-region sync and failover, security, metrics |
| [docs/COMPILATION.md](docs/COMPILATION.md) | Prerequisites, building the binary and the container image, offline builds |
| [docs/JOBS.md](docs/JOBS.md) | Periodic jobs: programs, schedules, remote servers, merge workflow, startup hook, safety |
| [docs/API.md](docs/API.md) | Every HTTP endpoint: login, status, configuration API, roles, examples |
| [docs/AUTHENTICATION.md](docs/AUTHENTICATION.md) | Local users (POC) vs Active Directory (production), roles, lockout, audit |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | Production deployment across IDC, FC and the witness; certificates, configuration, operations runbook, troubleshooting |

## Configuration in brief

- **`node.yaml`** is different on every server. It holds:
  - the server's identity (`id`, display `name`, `region: IDC|FC`)
  - the peer's `id`, `name` and address
  - witness and mTLS settings
  - `limits:`: worker goroutines, per-host connections, queues, size
    limits, timeouts
  - `auth:`: local users or Active Directory
  - `config_api:` settings
- **`sites.yaml`** is identical on both servers. It lists the websites,
  each tagged with `region: IDC|FC`, and sets the check intervals. It
  reloads automatically when the file changes.
- **`ownership.yaml`** is written by the service in its data directory.
  It records the current mode, epoch and owned regions, and is not meant
  to be edited by hand.

Secrets such as API tokens and the AD bind password are passed as
environment variables and referenced as `${VAR}`. The API never returns
them expanded.

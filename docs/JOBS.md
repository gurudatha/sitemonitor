# Periodic jobs

Users and admins can run their **own programs** on a schedule:
- **What runs:** any program from the shared binaries directory: Python,
  Perl, PowerShell, shell, or a native executable.
- **When it runs:** on a fixed interval, or once when the server comes up.
- **Where it runs:** locally on the monitor server, or **on a remote Linux
  or Windows (Cygwin) server** over SSH.

Each run is stored like a check result. Runs therefore get the same
features as website checks:
- stored counters and uptime
- incidents when a job keeps failing
- Prometheus metrics
- replication to the peer server
- failover: a job runs on whichever server currently owns its region

## 1. Concepts

| Term | Meaning |
|---|---|
| **Program** (binary) | A file in the binaries directory (`jobs.binaries_dir`, default `/data/jobs/bin`). Every user's programs go into this one directory. The interpreter is chosen by extension: `.py` → `python3`, `.pl` → `perl`, `.ps1` → `pwsh`, `.sh` → `sh`, `.bash` → `bash`; anything else is executed directly. |
| **Job** | One periodic activity: program + arguments + schedule + where to run. |
| **User job** | Added by an operator. **Runs immediately**, with no approval, for a limited window: at most **24 hours** (`jobs.user_max_run_for`). |
| **Admin periodic activity** | A job with scope `admin`. Created by an admin, or a user job **merged** by an admin. No window limit. |
| **Merge request** | A user asks for their job to become an admin periodic activity. **This is the only step that needs admin approval.** The job keeps running as a user job while the request is pending. |
| **Schedule** | `interval`: every `1m, 2m, 5m, 10m, 15m, 20m, 30m, 60m, 1h, 2h, 4h or 6h`, optionally limited by `run_for` (for example `8h`). `startup`: once, **when the server comes up**. |
| **Region** | The job runs on the server that currently owns this region (IDC or FC). If that server is down, the peer runs it. |
| **Target** | Omit it to run on the monitor server. Otherwise `{host, port, os: linux \| windows-cygwin, credential}`. |

### Lifecycle

```
                  operator adds job                    admin adds job
                         │                                   │
                         ▼                                   ▼
             ┌──────────────────────┐             ┌──────────────────────┐
             │ USER JOB  (active)   │  merge      │ ADMIN ACTIVITY       │
             │ runs now, ≤ 24 h     │ ──request──►│ (active, no end)     │
             │ window, then expired │  + admin    │                      │
             └──────────────────────┘  approval   └──────────────────────┘
                 │      ▲    │ merge rejected → stays a user job
           disable│      │enable
                 ▼      │
             ┌──────────────────────┐
             │ disabled             │
             └──────────────────────┘
```

| Action | Who |
|---|---|
| Upload a program | operator, admin |
| Replace a program | the uploader, or an admin |
| Add a user job / edit / disable / delete / run now **own** user job | operator (the owner), admin |
| Request a merge of **own** user job | operator (the owner) |
| Approve or reject a merge | **admin** |
| Add, edit or delete admin activities | admin |
| See jobs, runs and programs | viewer and above |

Editing a job restarts its run window. A user job's window is still at
most 24 hours.

## 2. Safety

| Risk | Protection |
|---|---|
| Shell injection through arguments | **No shell** for local runs: arguments are passed to the program unchanged (`exec`). For remote runs every argument is single-quoted. Tested with `$(touch /tmp/pwned)` and quotes |
| A program changed after it was approved | The program's **SHA-256 is pinned** in the job. A different file **does not run**; the run is CRIT, "changed since approval". A user's re-upload updates the pin of their own user jobs; admin activities need `POST /api/jobs/{id}/approve` |
| Path tricks | Program names are limited to `[A-Za-z0-9._-]`, with no `/` and no `..` |
| Runaway programs | Timeout per job (≤ `jobs.max_timeout`, and never longer than the interval); the **whole process group** is killed. At most `jobs.max_concurrent` jobs run at once, and a job never overlaps itself |
| Large output | stdout and stderr are capped at `jobs.max_output_kb` each |
| Leaking the monitor's secrets | Programs get a minimal environment: `PATH`, `HOME`/`TMPDIR` (a fresh scratch directory, deleted after the run), `LANG`, and `SITEMONITOR_JOB`, `SITEMONITOR_RUN_ID`, `SITEMONITOR_NODE`. They run as the unprivileged service user (uid 65532) |
| Remote servers | **Host keys are always verified** against `jobs.remote.known_hosts` (no trust-on-first-use). Credentials live only in `node.yaml` (secrets from `${ENV}`); jobs reference them **by name**, so no password goes through the API |
| Who did what | Every create, change, merge request, merge decision, upload and token change is a `JOB` or `AUTH` event, and every run records its trigger (`schedule every 5m`, `server startup`, `manual by alice`) |

Operators can run their own programs on the monitor servers without an
admin's approval, as requested. Give the operator role only to people
you would let run code on those servers. Set `jobs.require_approval:
true` if every user job should wait for an admin, or `jobs.enabled:
false` to turn the feature off.

## 3. Results, exit codes and incidents

| Exit code | Status |
|---|---|
| In `jobs.exit_codes.ok` (default `0`) | OK |
| In `jobs.exit_codes.warn` (default `1`) | WARN |
| Anything else, timeout, connection or start failure | CRIT |

This follows the usual Nagios plugin convention, so existing check
scripts work unchanged.

Each run is stored as a result of site **`job:<name>`**, check **`job`**,
with these details: `exit_code`, `stdout`, `stderr`, `duration_ms`,
`trigger`, `target`, `binary`, `sha256`, `run_id`.

Because of that:
- `GET /api/stats` and `/metrics` include jobs, for example
  `site_check_requests_total{site="job:disk-check",check="job",result="crit"}`.
- Two failed runs in a row open an **incident**, and the next successful
  run clears it.

## 4. Remote servers

The steps for each run:
1. **Connect** with SSH. The host key must match `known_hosts`.
2. **Push** the program to `<remote_dir>/sitemonitor-<run id>-<name>`
   (mode 0700).
3. **Run** it with the job's arguments. stdout, stderr and the
   **return value** (exit code) are collected.
4. **Delete** the pushed file, even if the run failed.

| OS | How the program is started on the remote server |
|---|---|
| Linux | `.py` → `python3`, `.pl` → `perl`, `.sh` → `sh`, `.ps1` → `pwsh -File`, others executed directly |
| Windows with Cygwin (sshd from Cygwin) | `.ps1` → `powershell.exe -ExecutionPolicy Bypass -File "$(cygpath -w …)"`, `.bat`/`.cmd` → `cmd.exe /c`, `.exe` and others directly, `.py`/`.pl`/`.sh` via Cygwin's interpreters |

Setting up a remote server:
1. Create a service account on the server, preferably with key-based
   login.
2. Add its host key to the known_hosts file on **both** monitors:
   ```bash
   ssh-keyscan -p 22 server.example.com >> /data/jobs/known_hosts
   ```
3. Add the credential to `node.yaml` on both monitors, then restart:
   ```yaml
   jobs:
     remote:
       credentials:
         linux-svc:  { user: svcmon, private_key: /certs/jobs_ed25519 }
         win-cygwin: { user: monitor, password: "${WIN_CYGWIN_PASSWORD}" }
   ```
4. Create jobs with `target: {host, port, os, credential: linux-svc}`.

## 5. Startup jobs and the native startup hook

When a server comes up, once it knows which regions it owns, it does two
things:
1. It calls the **native startup hook**, if one is compiled in and
   `jobs.startup_hook: true`.
2. It runs every active job with `schedule.type: startup` whose region it
   owns.

**The native hook is a placeholder for a C program that will be added
later.**

| File | Purpose |
|---|---|
| `internal/startup/startup_hook.h` | The interface: `int sitemonitor_on_startup(const char *node, const char *region);` — return 0 for success |
| `internal/startup/startup_hook.c` | **Placeholder**: replace its body with the real program |
| `internal/startup/hook_cgo.go` | The cgo binding, used with `-tags sitemonitor_cgo` |
| `internal/startup/hook_stub.go` | The default build: no C compiler needed; logs "startup hook not compiled in" |

Building with the hook needs a C toolchain and an image with libc:

```bash
CGO_ENABLED=1 go build -tags sitemonitor_cgo -o monitor ./cmd/monitor
```

In Docker, use a `golang:1.26` (Debian) build stage and a
`debian:bookworm-slim` or `distroless/base` runtime. The default static
build can't load C code.

The result is a `STARTUP` event:
- OK if the hook returns 0
- WARN with the return code otherwise

## 6. Dashboard and API

In the dashboard, the **User jobs** tab has three pages:
- **Running jobs**: the active jobs. Click a job to open its page.
- **Add job & programs**: add a job and upload programs.
- **Results history**: past runs of all jobs, and finished jobs.

A job's page shows its actions, its definition, statistics and the
output of every run.

### API (for people and agents)

Agents should use an **API token** (admin: `POST /api/tokens`) and send
`Authorization: Bearer <token>`. The complete, machine-readable
description is at **`GET /api/openapi.json`**.

```bash
B=https://monitor-idc-01:8080; T="Authorization: Bearer $TOKEN"

# upload a program and add a user job (runs right away, for 8 hours)
curl -s -H "$T" -X PUT --data-binary @disk.sh $B/api/binaries/disk.sh
curl -s -H "$T" -X POST -H 'Content-Type: application/json' $B/api/jobs -d '{
  "name": "disk-check", "binary": "disk.sh", "args": ["--warn", "80"],
  "schedule": {"type": "interval", "every": "15m", "run_for": "8h"}, "region": "IDC"}'

# remote job on a Windows server with Cygwin
curl -s -H "$T" -X POST -H 'Content-Type: application/json' $B/api/jobs -d '{
  "name": "win-services", "binary": "services.ps1",
  "schedule": {"type": "interval", "every": "30m", "run_for": "24h"},
  "target": {"host": "10.20.0.15", "port": 22, "os": "windows-cygwin", "credential": "win-cygwin"}}'

# run now and wait for the result; read past runs
curl -s -H "$T" -X POST "$B/api/jobs/$ID/run?wait=true"
curl -s -H "$T" "$B/api/jobs/$ID/runs?limit=10"

# ask for the merge; an admin approves it
curl -s -H "$T" -X POST -H 'Content-Type: application/json' -d '{"note":"needed by all teams"}' $B/api/jobs/$ID/merge-request
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" -X POST $B/api/jobs/$ID/merge
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" "$B/api/jobs?merge=pending"   # the admin's to-do list
```

| Endpoint | Role | Purpose |
|---|---|---|
| `GET /api/jobs/options` | viewer | Allowed intervals, run_for choices, OS, credential names, regions |
| `GET /api/jobs[?scope=&owner=&merge=pending]` | viewer | Jobs with state, last run and counters |
| `POST /api/jobs` | operator | Add a job |
| `GET /api/jobs/{id}` | viewer | Get one job |
| `PUT /api/jobs/{id}` | operator | Change a job (owner or admin) |
| `DELETE /api/jobs/{id}` | operator | Delete a job (owner or admin) |
| `POST /api/jobs/{id}/run[?wait=true]` | operator | Run now |
| `GET /api/jobs/{id}/runs` | viewer | Run history with output |
| `POST /api/jobs/{id}/disable`, `/enable` | operator | Pause or resume |
| `POST /api/jobs/{id}/merge-request` | operator (owner) | Ask for a merge |
| `POST /api/jobs/{id}/merge` | admin | Approve the merge |
| `POST /api/jobs/{id}/merge/reject` | admin | Decline the merge |
| `POST /api/jobs/{id}/approve` | admin | Re-approve after a program change (or approve, in require_approval mode) |
| `GET /api/binaries` | viewer | List programs |
| `PUT /api/binaries/{name}` | operator | Upload a program |
| `GET /api/binaries/{name}`, `/content` | viewer / operator | Metadata, download |
| `DELETE /api/binaries/{name}` | operator | Delete a program |

## 7. Two servers

- **Definitions are shared.** Job definitions, program files (verified by
  SHA-256) and API tokens are kept identical on both servers. A change on
  one server is pushed to the other right away. The heartbeat also
  compares a hash of all of it and re-syncs if they ever differ. The
  newer record wins, and deletions are kept as tombstones so they
  propagate.
- **Each job runs on one server only:** the one that owns the job's
  region. After a failover, the other server takes over that region's
  jobs.
- **Scheduling:** jobs are tasks of the shared periodic engine (class
  `job`, `jobs.max_concurrent` workers), separate from the website-check
  pool. See DESIGN.md §2.1.
- **No double runs:** the next run is based on the last stored run,
  whichever server made it, so a restart or failover doesn't run a job
  early.

## 8. Configuration (`node.yaml` → `jobs:`)

| Key | Default | Meaning |
|---|---|---|
| `enabled` | false (true in the shipped config) | Turn the feature on |
| `binaries_dir` / `work_dir` | `/data/jobs/bin` / `/data/jobs/work` | Program directory / per-run scratch directories |
| `max_binary_size_mb` | 50 | Largest upload |
| `max_concurrent` | 4 | Jobs running at the same time on one server |
| `default_timeout` / `max_timeout` | 5m / 30m | Run time limits |
| `max_output_kb` | 64 | stdout and stderr kept per run |
| `user_max_run_for` | 24h | Longest window for a user job |
| `require_approval` | false | true = user jobs wait for an admin before running |
| `startup_hook` | false (true in the shipped config) | Call the native startup hook |
| `exit_codes` | `{ok: [0], warn: [1]}` | Exit-code mapping |
| `interpreters` | see section 1 | Extension → command, for local runs |
| `remote.known_hosts` | `/data/jobs/known_hosts` | Host keys of remote servers |
| `remote.connect_timeout` | 10s | |
| `remote.remote_dir` | `/tmp` | Where programs are pushed |
| `remote.credentials` | — | `name: {user, password \| private_key, passphrase}` |

**Image:** the default Docker image (`runtime` stage) is Alpine with
`python3`, `perl`, `bash` and `sh`. For local `.ps1` jobs, base the
runtime stage on `mcr.microsoft.com/powershell`. Remote PowerShell on
Windows needs nothing extra. Deployments without jobs can use
`--target runtime-minimal` (distroless, no shell).

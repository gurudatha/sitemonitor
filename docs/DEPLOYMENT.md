# Deployment

## 1. Production topology

```
   IDC (data center)                     FC (second site)                 Third location
┌───────────────────────┐   mTLS :8443  ┌───────────────────────┐        ┌──────────────┐
│ monitor-idc-01        │◄────────────►│ monitor-fc-01         │        │ witness      │
│ node.yaml region: IDC │              │ node.yaml region: FC  │        │ :9443 (mTLS) │
│ :8080 dashboard       │              │ :8080 dashboard       │        └──────▲───────┘
└──────────┬────────────┘              └──────────┬────────────┘               │
           └───────────── lease renew every 5s (mTLS) ─────────────────────────┘
```

| Component | Count | Placement |
|---|---|---|
| Monitor server | 2 | One in IDC, one in FC |
| Witness | 1 | A **third** failure domain: a cloud VM, another site, or a small container elsewhere. If it shares a site with IDC or FC, it can't break ties when that site is cut off |

## 2. Server sizing (memory, CPU, disk)

All checks run in one Go process, so memory is mostly the stored result
history. Measured values:

| Measurement | Value |
|---|---|
| Idle process (demo, 4 sites) | ~9 MB RSS |
| Witness | ~4 MB RSS |
| One stored result in memory | ~0.6 KB |
| One site with 4 checks × 200 results | ~0.5 MB |
| 100 sites (80k results) | ~48 MB heap |
| 500 sites (400k results) | ~240 MB heap |
| 1,000 sites (800k results) | ~480 MB heap |

**Both servers hold the history for all sites** because results are
replicated. While the 30-second disk snapshot is being written, memory
briefly needs up to about 1.8× the heap. During a takeover, the extra
work is goroutines and sockets (a few MB), not more history.

**Formula:**

```
RAM ≈ 50 MB + sites × checks_per_site × history_per_check × 0.6 KB × 2 (snapshot headroom)
```

**Recommended sizes** (per server; both servers sized the same so either
can carry both regions):

| Websites | `history_per_check` | RAM limit | `GOMEMLIMIT` | vCPU | Disk (`/data`) |
|---|---|---|---|---|---|
| ≤ 100 | 200 | 256 MB | 200MiB | 0.5 | 100 MB |
| ≤ 500 | 200 | 1 GB | 800MiB | 1 | 500 MB |
| ≤ 1,000 | 200 | 2 GB | 1600MiB | 1–2 | 1 GB |
| ≤ 1,000 | 50 | 512 MB | 400MiB | 1–2 | 300 MB |

- Set `GOMEMLIMIT` to about 80 % of the container or VM memory limit, so
  the Go garbage collector works harder before it hits that limit.
- To reduce memory, lower `limits.history_per_check` in `node.yaml`.
  Long-term history belongs in Prometheus (see section 7).
- CPU usage is very low; the demo idles at about 0.05 %. The TLS check
  is the most expensive because each run makes 5 handshakes, which is
  why it runs daily by default.
- Network usage is roughly `sites × requests/min × ~10 KB`. For
  example, 500 sites at one check per minute is about 85 KB/s.

## 3. Network and firewall

| From | To | Port | Purpose |
|---|---|---|---|
| monitor-idc-01 | monitor-fc-01 | 8443/tcp | Heartbeat, replication, cross-region verification (mTLS) |
| monitor-fc-01 | monitor-idc-01 | 8443/tcp | Same, in the other direction |
| Both monitors | witness | 9443/tcp | Lease acquire, renew and release (mTLS) |
| Both monitors | Monitored sites | 443/80 (as configured) | Checks |
| Both monitors | Webhook (Slack, Teams, …) | 443/tcp | Alerts |
| Operators, Prometheus | Both monitors | 8080/tcp | Dashboard, `/api/*`, `/metrics` |

Port 8443 accepts **only** clients presenting the peer's certificate.
Don't expose it beyond the two monitors and the witness.

## 4. Certificates (mTLS)

**Option A: the built-in generator** (for small installations). Run
this once on a secure machine:

```bash
./monitor gen-certs -out ./certs -names monitor-idc-01,monitor-fc-01,witness -days 365
```

Distribute the files:

| Host | Files |
|---|---|
| IDC server | `ca.crt`, `monitor-idc-01.crt`, `monitor-idc-01.key` |
| FC server | `ca.crt`, `monitor-fc-01.crt`, `monitor-fc-01.key` |
| Witness | `ca.crt`, `witness.crt`, `witness.key` |

Store `ca.key` **offline**. The servers don't need it.

The certificate SANs are the node name, `localhost` and `127.0.0.1`. If
the servers reach each other by FQDN (for example
`monitor-fc-01.fc.example.com`), use Option B, or name the nodes after
their FQDNs.

**Option B: corporate PKI or Vault PKI** (recommended). Issue one
certificate per node with:
- `CN = node id` (must equal `node.id` and `peer.id` in `node.yaml`)
- SAN = the DNS name used in `peer.url` and `witness.url`
- EKU = serverAuth **and** clientAuth

**Rotation:** replace the files and restart one server at a time. The
peer will report it SUSPECT for a few seconds, which is far below the
30-second takeover threshold. Monitor certificate expiry by adding the
monitors themselves to `sites.yaml`.

## 5. Configuration per server

Each server has its own **writable** config directory, mounted at
`/etc/monitor`. It holds `node.yaml`, `sites.yaml` and the `backups/`
folder the config API creates. The container user (uid 65532) must own
it: `sudo chown -R 65532:65532 /etc/sitemonitor/config`.

1. **`node.yaml`** is unique per server; the region and the names are
   set here. Start from `config/idc/node.yaml` and `config/fc/node.yaml`,
   and change at least:

```yaml
node:   { id: monitor-idc-01, name: "IDC Monitor - Mumbai DC", region: IDC }
peer:   { id: monitor-fc-01, name: "FC Monitor - Chennai", region: FC,
          url: https://monitor-fc-01.fc.example.com:8443,
          handback_stable_for: 2m }                  # demo uses 1m; production 2–5m
witness: { url: https://witness.example.net:9443, required_for_takeover: true }
limits:  { workers: 20, per_host_limit: 2, queue_size: 500, history_per_check: 200 }   # see sizing table
auth:    { providers: [ldap, local] }                # see docs/AUTHENTICATION.md
```

2. **`sites.yaml`** must be **identical** on both servers. Keep it in
   Git and deploy it to both servers from CI. Use production intervals:

```yaml
defaults:
  intervals: { availability: 1m, api: 5m, cert: 6h, tls: 24h, headers: 24h }
```

   Remove `tls_verify: false`; it exists only for the self-signed demo
   targets. The file is re-read automatically within 10 s of a change.
   An invalid file is rejected, and the previous configuration stays
   active.

3. **Environment variables** (from a secrets manager or a `.env` file
   with mode 600):

| Variable | Required | Purpose |
|---|---|---|
| `LDAP_BIND_PASSWORD` | when `auth.providers` includes `ldap` | AD service-account password (`auth.ldap.bind_password: "${LDAP_BIND_PASSWORD}"`) |
| `ALERT_WEBHOOK_URL` | no | Slack-compatible webhook for events |
| `GOMEMLIMIT` | recommended | See sizing |
| Any `${VAR}` used in `sites.yaml` | as used | API tokens for sanity calls |

## 6. Deploying

### 6.1 Docker Compose, one host per site (recommended for VMs)

Create a compose file on each monitor host containing only that host's
service. For example, on the IDC host:

```yaml
services:
  monitor:
    image: registry.example.com/ops/sitemonitor:1.0.0
    restart: unless-stopped
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    mem_limit: 1g
    env_file: /etc/sitemonitor/monitor.env            # LDAP_BIND_PASSWORD, ALERT_WEBHOOK_URL, GOMEMLIMIT, API tokens
    volumes:
      - /etc/sitemonitor/config:/etc/monitor              # node.yaml + sites.yaml; writable (config API), owned by 65532
      - /etc/sitemonitor/certs:/certs:ro                # mTLS certs (+ ad-ca.crt for Active Directory)
      - /var/lib/sitemonitor:/data                    # chown 65532:65532
    ports: ["8080:8080", "8443:8443"]
```

On the witness host:

```yaml
services:
  witness:
    image: registry.example.com/ops/sitemonitor:1.0.0
    command: ["witness", "-addr", ":9443", "-cert", "/certs/witness.crt", "-key", "/certs/witness.key", "-ca", "/certs/ca.crt"]
    restart: unless-stopped
    read_only: true
    cap_drop: [ALL]
    mem_limit: 64m
    volumes: ["/etc/sitemonitor/certs:/certs:ro"]
    ports: ["9443:9443"]
```

Then start it:

```bash
sudo install -d -o 65532 -g 65532 /var/lib/sitemonitor
docker compose up -d && docker compose logs -f
```

### 6.2 systemd (no containers)

Put the binary at `/usr/local/bin/monitor`, create a `monitor` system
user, and create `/etc/systemd/system/sitemonitor.service`:

```ini
[Unit]
Description=SiteMonitor
After=network-online.target
Wants=network-online.target

[Service]
User=monitor
EnvironmentFile=/etc/sitemonitor/monitor.env
ExecStart=/usr/local/bin/monitor run -config /etc/sitemonitor/config/node.yaml
Restart=always
RestartSec=5
MemoryMax=1G
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/sitemonitor /etc/sitemonitor/config
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now sitemonitor
```

### 6.3 Kubernetes

Run one Deployment with `replicas: 1` **per region cluster**. Keep it at
1 replica, because the peer model expects exactly one server per region.
Mount `node.yaml` and `sites.yaml` from ConfigMaps, the certificates and
environment from Secrets, and `/data` from a small PVC. Set:
- `livenessProbe` and `readinessProbe` to `GET /healthz` on port 8080
  with scheme HTTPS
- `resources.limits.memory` from the sizing table
- the `GOMEMLIMIT` environment variable

Expose port 8443 to the other cluster through an internal load balancer.

### 6.4 Rollout order and upgrades

1. Deploy the witness, then confirm `GET /healthz` returns `ok`.
2. Deploy IDC. It logs `started: monitoring home region IDC`, with peer
   DOWN.
   - If the witness is required, IDC **will** take over FC sites after
     30 s, because FC isn't up yet. That's expected.
3. Deploy FC. It starts in STANDBY. IDC hands FC back after
   `handback_stable_for`.
4. **Upgrades:** do one server at a time. Wait until both show
   `NORMAL · peer HEALTHY` before upgrading the second. A graceful stop
   (SIGTERM) releases the server's witness leases, finishes in-flight
   checks, and saves a snapshot.

## 7. Monitoring the monitor

### 7.1 HTTP API (on the dashboard port 8080)

All endpoints except `/healthz`, `/api/auth/info` and `/api/login`
require authentication: a session cookie from the dashboard, or HTTP
Basic for scripts. The full reference, including the configuration
API, is in [API.md](API.md).

| Endpoint | Returns |
|---|---|
| `GET /api/status` | Full JSON: cluster state (mode, owned regions, epoch, peer status, heartbeat age, config match, replication lag, witness), scheduler stats, config version, every site with its latest result per check and uptime %, and the last 100 events |
| `GET /api/history?site=<name>&check=<check>` | Last 100 results for one site/check |
| `GET /metrics` | Prometheus metrics (see [DESIGN.md §8](DESIGN.md#8-observability)) |
| `GET /healthz` | No auth. `ok`, or 503 if the server is not completing checks |

Examples:

```bash
curl -s -u viewer:$PASS https://monitor-idc-01:8080/api/status | jq '.cluster | {node_name, mode, owned_regions, peer_name, peer_status}'
curl -s -u viewer:$PASS https://monitor-idc-01:8080/api/status | jq '.sites[] | {name, cert: .results.cert.details.days_left}'
curl -s -u viewer:$PASS "https://monitor-idc-01:8080/api/history?site=fc-orders-api&check=availability" | jq length
curl -s -u viewer:$PASS https://monitor-idc-01:8080/api/limits | jq '{node, peer, limits}'
curl -s -u operator:$PASS -X PUT -H 'Content-Type: application/json' \
  -d '{"region":"FC","url":"https://shop.example.com"}' https://monitor-idc-01:8080/api/config/sites/shop
```

Add `--cacert ca.crt` if you use the private CA, or `-k` for a quick
test.

### 7.2 Prometheus

Scrape configuration:

```yaml
scrape_configs:
  - job_name: sitemonitor
    scheme: https
    tls_config: { ca_file: /etc/prometheus/sitemonitor-ca.crt }
    basic_auth: { username: prometheus, password_file: /etc/prometheus/sitemonitor.pass }   # a viewer-role account
    static_configs:
      - targets: ["monitor-idc-01:8080", "monitor-fc-01:8080"]
```

Suggested alert rules:

```yaml
- alert: MonitorPeerDown
  expr: monitor_peer_up == 0
  for: 1m
- alert: MonitorRegionUnmonitored                       # nobody owns a region
  expr: max by (target_region) (monitor_owns_region) == 0
  for: 2m
- alert: MonitorConfigMismatch
  expr: monitor_config_hash_match == 0
  for: 10m
- alert: MonitorReplicationLag
  expr: monitor_replication_lag_seconds > 300
- alert: CertificateExpiringSoon
  expr: site_cert_days_left <= 30
```

### 7.3 Dead-man's switch

If **both** monitors die, nothing inside the system can alert you.
Point an external heartbeat service (for example Healthchecks.io or
Cronitor), or your Prometheus, at both `/healthz` endpoints.

## 8. Operations runbook

| Task | Steps |
|---|---|
| **Add or change a site** | Either: (a) `PUT /api/config/sites/{name}` or a whole-file `PUT` with an operator account (see [API.md](API.md)); or (b) edit `sites.yaml` on disk, or roll it out from Git/CI. The API applies the change immediately and syncs it to the peer. File edits are picked up within 10 s, and the server with the older file pulls the newer one automatically |
| **Change limits, names or auth** | `GET /api/config/node`, edit, `PUT /api/config/node?restart=true` (admin), on each server. Or edit the file and restart. Identity fields (`id`, `region`, `tls`) only on disk |
| **Undo a config change** | `GET /api/config/backups?file=sites` lists versions; copy one from `<config dir>/backups/` back, or `PUT` its content |
| **Planned maintenance on one server** | `docker compose stop` (or `systemctl stop`). Its leases are released on shutdown, so the peer takes over after about 30 s. Start it again; handback follows `handback_stable_for` later |
| **Failover test** (quarterly) | Stop FC. Within 45 s, IDC shows `TAKEOVER`, a `FAILOVER` event appears, and the FC sites show "Checked from IDC (failover)". Start FC: it shows STANDBY, then `HANDBACK`, and both return to `NORMAL` |
| **Witness down** | Both servers keep monitoring their own regions; takeover is blocked and a `FAILOVER … blocked` event explains why. Restore the witness; no other action is needed |
| **Network split between IDC and FC** | Each server keeps its own region, because the witness refuses takeover. The event reads "peer unreachable … still holds the lease (network split?)". Fix the link |
| **Server stuck in STANDBY** | The peer still owns its region. Check the peer's dashboard: it hands back after `handback_stable_for` once it sees this server as HEALTHY. Look at `last_error` on both servers (certificate CN mismatch or a firewall are the usual causes) |
| **Reset ownership state** | Stop the server, delete `/data/ownership.yaml`, start it again. It re-decides ownership from heartbeats and leases |
| **Backup** | Optional. `/data/results.json` is a cache that is rebuilt from new checks. Back up `node.yaml`, `sites.yaml`, and the certificates or keys |

## 9. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `peer … is DOWN: … certificate signed by unknown authority` | The two servers use different `ca.crt` files | Distribute the same CA to both |
| `peer … DOWN: … HTTP 403 forbidden` | The client certificate's CN doesn't equal `peer.id` on the other server | Reissue the certificate, or fix `peer.id` |
| `unexpected peer identity` | `peer.url` points at the wrong server | Fix `peer.url` |
| `witness unreachable` | Firewall, DNS, or the witness is down | Check port 9443 and the witness's `/healthz` |
| `takeover … blocked: … still holds the … lease` | The servers can't reach each other, but the witness still sees the peer renewing | This is correct split-brain behavior. Fix the IDC↔FC link |
| Checks show `timed out` | The site is slow or the timeout is too tight | Raise `timeout` for that site |
| `monitor_checks_dropped_total` increasing | The queue is full | Raise `workers` or `queue_size`, or lengthen intervals |
| Container OOM-killed | History too large for the memory limit | Use the sizing table: lower `history_per_check` or raise the memory limit |
| `auth: provider local needs at least one auth.local.users entry` / `password_hash must be a bcrypt hash` | `auth:` section incomplete | Add users with `monitor hash-password`, or switch to `ldap` |
| Config API returns `config directory must be writable` | Config mounted read-only or owned by root | Mount the directory (not single files) read-write; `chown -R 65532:65532` |
| Login fails for AD users | See [AUTHENTICATION.md §2.5](AUTHENTICATION.md#25-troubleshooting) | |
| HTTP 429 on login | User+IP locked after `max_failed_logins` failures | Wait `auth.lockout`, or restart the server to clear |

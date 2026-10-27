# HTTP API reference

All endpoints are served on the **dashboard port** (default `:8080`, HTTPS)
of each server. Either server can be used, because results and
`sites.yaml` are kept in sync.

## Authentication

There are two ways to authenticate. Both go through the configured
providers (`auth.providers`: local users or Active Directory). See
[AUTHENTICATION.md](AUTHENTICATION.md).

| Method | For | How |
|---|---|---|
| **Session cookie** | Browser / dashboard | `POST /api/login` → `sm_session` cookie (HttpOnly, Secure, SameSite=Strict). Every write must also send `X-CSRF-Token: <csrf_token>` from the login or `/api/me` response |
| **HTTP Basic** | Scripts, CI, Prometheus | `curl -u user:password …`. Successful logins are cached for `auth.basic_cache_ttl` (default 1 min), so automation doesn't cause an AD bind on every call |

**Roles.** Each role includes the permissions of the roles before it.

| Role | Can |
|---|---|
| `viewer` | Read status, history, metrics, limits and `sites.yaml` |
| `operator` | + validate and change `sites.yaml` (whole file or one site), list backups |
| `admin` | + read, validate and change `node.yaml`, restart the server |

**Errors**

| Code | Meaning |
|---|---|
| 401 | Not logged in, or wrong credentials |
| 403 | Role too low, user not in any mapped AD group, missing CSRF token, or cross-origin write |
| 429 | Locked out: `max_failed_logins` failures from this user and IP, for `lockout` |

## Public endpoints

| Method & path | Description |
|---|---|
| `GET /healthz` | `ok`, or 503 if the server is not completing checks. For load balancers and orchestrators |
| `GET /api/auth/info` | `{providers, node, node_name}`, used by the login page |
| `POST /api/login` | Body `{"username","password"}`, with `Content-Type: application/json`. Returns `{user, csrf_token}` and sets the session cookie |
| `POST /api/logout` | Ends the session |

## Read endpoints (viewer)

| Method & path | Description |
|---|---|
| `GET /api/me` | Current user `{username, display_name, role, provider}`, `csrf_token`, server name |
| `GET /api/status` | Cluster state (server and peer **id + name**, mode, owned regions, epoch, heartbeat, config match, replication, witness), scheduler stats, config version, every site with its latest result per check and uptime, last 100 events, up to 100 incidents |
| `GET /api/history?site=<name>&check=<check>` | Last 100 results of one site/check |
| `GET /api/limits` | Effective `node`/`peer` identity and names, all `limits`, auth summary |
| `GET /api/incidents[?status=open\|cleared]` | Grouped problems (see DESIGN.md §6.1): `first_seen` (oldest report), `last_seen`, `occurrences`, `cleared`, `reappeared`, `seen_by`, and the `history` of RAISED / SEVERITY / CLEARED / REAPPEARED transitions. Open incidents come first (oldest first), then cleared ones (most recent first). Up to 500 |
| `GET /metrics` | Prometheus text format (see DESIGN.md §8) |
| `GET /api/config/sites` | Raw `sites.yaml`. The `ETag` header is the file's SHA-256 hash |
| `GET /api/config/sites/{name}` | One site as YAML, exactly as written in the file |

## sites.yaml endpoints (operator)

Every change goes through these steps:
1. **Size check:** the body is limited to `limits.max_config_size_kb`.
2. **Validation:** unknown keys, bad regions, duplicates and
   `limits.max_sites` are all rejected.
3. **Backup:** the previous file is saved as
   `<config dir>/backups/sites.yaml.<UTC timestamp>`. The last
   `config_api.backups` copies are kept.
4. **Atomic replace:** the new file is written to a temporary file,
   then renamed into place.
5. **Applied immediately:** the scheduler picks it up with no restart.
6. **Pushed to the peer** (if `config_api.sync_to_peer`). If the peer is
   down, it pulls the newer file automatically when it comes back
   (`config_api.auto_pull_newer`).
7. **Recorded:** a `CONFIG` event names the user and lists what changed.

To protect against concurrent edits, send `If-Match: "<ETag>"` from your
last GET. If someone changed the file in the meantime, you get **412**
and nothing is written.

| Method & path | Body | Description |
|---|---|---|
| `POST /api/config/sites/validate` | Full `sites.yaml` | Dry run: `{valid, version, sites, hash}` or 422 `{error}` |
| `PUT /api/config/sites` | Full `sites.yaml` | Replace the whole file (comments are kept exactly as sent) |
| `PUT /api/config/sites/{name}` | One site, YAML **or JSON** | Add or replace that site. Comments elsewhere in the file are kept; `version` is set to the current UTC time |
| `DELETE /api/config/sites/{name}` | — | Remove that site |
| `GET /api/config/backups?file=sites\|node` | — | List saved versions, newest first |

**Response:**

```json
{"changed": true, "version": "2026-10-26T12:00:26Z", "sites": 5, "hash": "3d42…",
 "diff": {"added": ["idc-health"], "removed": null, "changed": null},
 "peer_synced": true}
```

When the peer couldn't be reached, the response has
`"peer_synced": false` and a `"peer_error"` field.

## node.yaml endpoints (admin)

| Method & path | Body | Description |
|---|---|---|
| `GET /api/config/node` | — | Raw `node.yaml`. `${VAR}` references are returned **unexpanded**, so secrets are never exposed |
| `POST /api/config/node/validate` | Full `node.yaml` | Dry run |
| `PUT /api/config/node[?restart=true]` | Full `node.yaml` | Validate, back up and replace. Returns `restart_required: true`. With `?restart=true`, the server restarts gracefully and its supervisor (Docker or systemd) starts it again |
| `POST /api/admin/restart` | — | Graceful restart: finishes in-flight checks, releases witness leases, saves the snapshot, exits |

`node.id`, `node.region`, `peer.id`, `peer.region` and `tls.*` can't be
changed through the API (409); they are tied to certificates and
cluster identity. Unknown keys are rejected (422), so a typo like
`wrokers:` can't be silently ignored.

## Examples

```bash
B=https://monitor-idc-01:8080; OP="-u operator:$OPERATOR_PASSWORD"   # add --cacert ca.crt (or -k for a demo)

# Add or update one site (JSON or YAML)
curl -s $OP -X PUT -H 'Content-Type: application/json' $B/api/config/sites/shop \
  -d '{"region":"FC","url":"https://shop.example.com","owner":"web","checks":["availability","tls","cert"]}'

# Safe whole-file edit: download, edit, upload with If-Match
ETAG=$(curl -s $OP -D - -o sites.yaml $B/api/config/sites | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
vi sites.yaml
curl -s $OP -X POST --data-binary @sites.yaml $B/api/config/sites/validate
curl -s $OP -X PUT -H "If-Match: $ETAG" --data-binary @sites.yaml $B/api/config/sites

# Remove a site
curl -s $OP -X DELETE $B/api/config/sites/shop

# Admin: lower the worker pool and restart
curl -s -u admin:$ADMIN_PASSWORD $B/api/config/node -o node.yaml
sed -i 's/^  workers: .*/  workers: 10/' node.yaml
curl -s -u admin:$ADMIN_PASSWORD -X PUT --data-binary @node.yaml "$B/api/config/node?restart=true"
```

## Peer-only endpoints (port 8443, mTLS, peer certificate only)

These are listed for completeness. They are used between the two
servers and can't be called with a user account.

| Method & path | Purpose |
|---|---|
| `POST /peer/heartbeat` | Exchange state every `heartbeat_interval` |
| `POST /peer/replicate` | Results and events batch, up to `limits.max_replication_body_mb` |
| `POST /peer/verify` | Re-run a check from the other region; at most `limits.verify_concurrency` at a time |
| `GET` / `PUT /peer/config/sites` | Pull or push `sites.yaml` |

# Authentication: local users (POC) and Active Directory (production)

Authentication is a separate module (`internal/auth`), selected in
`node.yaml`:

```yaml
auth:
  providers: [local]         # POC: username/password from node.yaml
# providers: [ldap]          # production: Active Directory only
# providers: [ldap, local]   # production + local break-glass admin when AD is unreachable
```

Providers are tried **in order**:
- If one rejects the password, the next one is tried. This is how a
  local break-glass account works when AD is down.
- If AD **authenticates** the user but the user is in no mapped group,
  access is denied (403) and no other provider is tried.

The dashboard login page, session cookies, Basic auth for scripts,
roles, lockout and audit events all behave the same whichever provider
is used.

## Roles

| Role | Permissions |
|---|---|
| `viewer` | Dashboard, status, history, metrics, read `sites.yaml` |
| `operator` | viewer + change `sites.yaml` (dashboard editor or API) |
| `admin` | operator + read and change `node.yaml`, restart |

## 1. POC: local users

1. Create a bcrypt hash for each user:
   ```bash
   echo 'a-long-password-here' | docker run --rm -i sitemonitor:latest hash-password
   # or: ./monitor hash-password   (prompts on stdin)
   ```
2. Add the users to `node.yaml` on **both** servers:
   ```yaml
   auth:
     providers: [local]
     local:
       users:
         - {username: alice, password_hash: "$2a$12$…", role: admin, display_name: "Alice (Ops)"}
         - {username: noc,   password_hash: "$2a$12$…", role: viewer}
   ```
3. Restart the server, or `PUT /api/config/node?restart=true`.

Passwords are stored only as bcrypt hashes (cost 12) and must be at
least 12 characters. `${VAR}` expansion leaves `$2a$…` hashes intact.
**Change the demo users** shipped in `config/*/node.yaml` before
exposing a server.

## 2. Production: Active Directory

### 2.1 What you need from the AD team

| Item | Example |
|---|---|
| LDAPS endpoint (or LDAP with StartTLS) | `ldaps://dc01.corp.example.com:636`; a load-balanced name such as `ldaps://corp.example.com:636` is better |
| AD root/issuing CA certificate (PEM) | `/certs/ad-ca.crt` |
| A read-only **service account** to search users | `CN=svc-sitemonitor,OU=Service Accounts,DC=corp,DC=example,DC=com` |
| Base DN for user search | `DC=corp,DC=example,DC=com` |
| Security groups for each role | `SiteMonitor-Admins`, `SiteMonitor-Operators`, `SiteMonitor-Viewers` |

### 2.2 node.yaml

```yaml
auth:
  providers: [ldap, local]          # keep one local admin for emergencies
  ldap:
    url: ldaps://corp.example.com:636
    ca_file: /certs/ad-ca.crt
    bind_dn: "CN=svc-sitemonitor,OU=Service Accounts,DC=corp,DC=example,DC=com"
    bind_password: "${LDAP_BIND_PASSWORD}"      # from the environment / secret store
    base_dn: "DC=corp,DC=example,DC=com"
    user_filter: "(&(objectClass=user)(sAMAccountName={username}))"
    # UPN login instead (alice@corp.example.com):
    # user_filter: "(&(objectClass=user)(userPrincipalName={username}))"
    display_attr: displayName
    nested_groups: true             # members of sub-groups count too
    timeout: 5s
    role_groups:
      admin:    ["CN=SiteMonitor-Admins,OU=Groups,DC=corp,DC=example,DC=com"]
      operator: ["CN=SiteMonitor-Operators,OU=Groups,DC=corp,DC=example,DC=com"]
      viewer:   ["CN=SiteMonitor-Viewers,OU=Groups,DC=corp,DC=example,DC=com"]
    default_role: ""                # "" = deny users in none of the groups; or "viewer"
  local:
    users:
      - {username: breakglass, password_hash: "$2a$12$…", role: admin}
```

Then:
- Set `LDAP_BIND_PASSWORD` in the server's environment file.
- Mount the AD CA certificate at `ca_file`.
- Restart the server.

### 2.3 How a login is checked

1. The server opens a TLS connection to AD. `ldaps://` is used, or
   `ldap://` + StartTLS. **Plain `ldap://` is refused** in config
   validation, because passwords would cross the network in clear text.
2. It binds as the service account and searches `base_dn` with
   `user_filter`. The username is LDAP-escaped, so filter injection
   such as `alice)(cn=*` doesn't work. Exactly one entry must match.
3. It **binds as that user's DN with the entered password**. This is the
   actual password check, done by AD, so AD's own lockout, password
   expiry and disabled-account rules apply.
4. It maps the user's groups to a role. The highest role wins.
   - The user's `memberOf` values are compared case-insensitively with
     `role_groups`.
   - With `nested_groups: true`, AD's `LDAP_MATCHING_RULE_IN_CHAIN`
     (`1.2.840.113556.1.4.1941`) is also queried, so membership through
     sub-groups counts.
5. An empty password is rejected before contacting AD. LDAP treats an
   empty-password bind as an anonymous bind that succeeds.

### 2.4 Moving from POC to AD

1. Do the POC with `providers: [local]`.
2. Add the `ldap:` block and switch to `providers: [ldap, local]` on
   **one** server. Log in with an AD account and check the role shown in
   the top-right corner.
3. Roll out to the second server.
4. Optionally remove all local users except a break-glass admin. Store
   its password in your vault.

### 2.5 Troubleshooting

| Symptom (server log or UI) | Cause |
|---|---|
| `ldap connect: … certificate signed by unknown authority` | `ca_file` missing or wrong |
| `ldap service-account bind failed` | Wrong `bind_dn` / `LDAP_BIND_PASSWORD`, or the account is locked |
| Login → "invalid username or password" for a valid user | `user_filter` doesn't find the user: check `base_dn` and the attribute (`sAMAccountName` or `userPrincipalName`) |
| Login → 403 "not a member of any group" | User authenticated but isn't in a `role_groups` group. Check the exact DN, or enable `nested_groups` |
| AD down | With `[ldap, local]` the local break-glass user still works; AD users can't log in until it is back |

Test from the server host:

```bash
ldapsearch -H ldaps://corp.example.com -D "$BIND_DN" -w "$LDAP_BIND_PASSWORD" \
  -b "DC=corp,DC=example,DC=com" "(sAMAccountName=alice)" memberOf
```

## 3. Sessions, lockout and audit

| Setting | Default | Meaning |
|---|---|---|
| `session_ttl` | 8h | Dashboard session lifetime |
| `max_sessions` | 1000 | Oldest session is evicted beyond this |
| `basic_cache_ttl` | 1m | Successful Basic-auth logins (scripts) are cached |
| `max_failed_logins` / `lockout` | 5 / 5m | After 5 failures from one **user + IP**, that pair is locked for 5 min (HTTP 429) and an `AUTH` event is raised. A client already holding cached valid credentials isn't affected, so an attacker can't lock out your integrations |

**Web security:**
- Session cookies are `HttpOnly`, `Secure` and `SameSite=Strict`.
- Writes need an `X-CSRF-Token` header, and cross-origin writes are
  rejected.
- The login endpoint accepts only JSON from the same origin.
- Error messages never reveal whether a username exists.

**Audit trail:**
- Every login (success or failure) is logged as JSON, with the user, IP
  and provider.
- Every configuration change is a `CONFIG` event that names the user,
  e.g. `sites.yaml updated by alice (ldap/operator) via API: added shop`.
- Events are replicated to both servers and sent to the webhook.

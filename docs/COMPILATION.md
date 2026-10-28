# Compilation

## Prerequisites

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.26 or newer (`go.mod` requires 1.26.0) | Building the binary |
| Docker Engine + Compose v2 | 24+ / v2.20+ | Container image, local demo |
| git | any | Getting the source |

Third-party modules: `gopkg.in/yaml.v3` (config), `golang.org/x/crypto` (bcrypt),
`github.com/go-ldap/ldap/v3` (Active Directory) and `modernc.org/sqlite` (embedded
database; pure Go, so `CGO_ENABLED=0` static builds keep working). `github.com/jimlambrt/gldap` is used only by tests. The dashboard
(HTML/CSS/JS) is embedded into the binary with `go:embed`, so there is
no Node.js or frontend build step.

## 1. Build the binary

```bash
git clone <your-repo-url> sitemonitor && cd sitemonitor
go mod download
go vet ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o monitor ./cmd/monitor
./monitor                     # prints: usage: monitor run|witness|gen-certs|demo-target
```

- `CGO_ENABLED=0` produces a fully static binary with no libc
  dependency, so it runs on any Linux host or on distroless.
- `-trimpath -ldflags="-s -w"` makes the build reproducible and
  smaller (about 8 MB).

### Cross-compiling

```bash
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/monitor-linux-arm64 ./cmd/monitor
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/monitor-linux-amd64 ./cmd/monitor
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/monitor.exe ./cmd/monitor
```

## 2. Build the container image

```bash
docker build -t sitemonitor:1.0.0 .
```

The `Dockerfile` has two stages:
1. **build:** `golang:1.26-alpine`. Downloads modules and compiles a
   static binary.
2. **runtime:** `gcr.io/distroless/static-debian12:nonroot`. Contains
   only the binary. There is no shell or package manager, and it runs
   as uid 65532. `/data` and `/certs` are created with the right owner,
   so named volumes are writable.

To push to your registry:

```bash
docker tag sitemonitor:1.0.0 registry.example.com/ops/sitemonitor:1.0.0
docker push registry.example.com/ops/sitemonitor:1.0.0
```

Recommended in CI: sign the image (`cosign sign`), generate an SBOM
(`syft`), and scan it (`trivy image`).

## 3. Offline / air-gapped builds

If the build host can't reach `proxy.golang.org`, vendor the dependency
on a connected machine and commit or copy the `vendor/` directory:

```bash
go mod vendor
CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o monitor ./cmd/monitor
```

For Docker, remove the `RUN go mod download` line and add `-mod=vendor`
to the `go build` line in the `Dockerfile`.

If you are behind a corporate TLS-inspecting proxy, add the proxy's CA
certificate to the build stage. Don't turn off verification.

```dockerfile
COPY corp-ca.crt /usr/local/share/ca-certificates/
RUN apk add --no-cache ca-certificates && update-ca-certificates
```

## 4. Run locally without Docker

```bash
mkdir -p ./run/certs ./run/data-idc ./run/data-fc
./monitor gen-certs -out ./run/certs -names monitor-idc-01,monitor-fc-01,witness
```

Copy `config/idc/` and `config/fc/` to `./run/idc` and `./run/fc`, then
edit each `node.yaml`:
- the cert paths to point at `./run/certs/…`
- `data_dir` to `./run/data-idc` and `./run/data-fc`
- `sites_file` to `./run/idc/sites.yaml` or `./run/fc/sites.yaml`
- the listen ports, so the two servers don't collide
- `peer.url` to `https://localhost:<other peer port>`

Then start both servers:

```bash
./monitor run -config ./run/idc/node.yaml &
./monitor run -config ./run/fc/node.yaml &
# log in with a user from auth.local.users (demo: admin / Admin-Demo-Pass-1)
```

## 5. Verifying a build

```bash
go vet ./...                               # static checks
go test ./...                              # unit tests (auth incl. an in-process LDAP server, configstore, config)
gofmt -l .                                 # must print nothing
docker compose up -d --build               # full two-region demo
curl -sk -u viewer:Viewer-Demo-Pass-1 https://localhost:8081/api/status | head -c 400
```

A healthy start logs `started: monitoring home region IDC` on one server
and `started: monitoring home region FC` on the other. In the dashboard,
both servers show `NORMAL · peer HEALTHY`.

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/monitor ./cmd/monitor \
 && mkdir -p /out/data /out/certs /out/shared

# ---- runtime-minimal: distroless, no shell, non-root ----
# For deployments without periodic jobs (jobs.enabled: false):
#   docker build --target runtime-minimal -t sitemonitor:minimal .
FROM gcr.io/distroless/static-debian12:nonroot AS runtime-minimal
COPY --from=build /out/monitor /usr/local/bin/monitor
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/certs /certs
USER 65532:65532
EXPOSE 8080 8443 9443
ENTRYPOINT ["/usr/local/bin/monitor"]
CMD ["run", "-config", "/etc/monitor/node.yaml"]

# ---- runtime (default): with interpreters for periodic jobs ----
# Python 3, Perl, Bash and POSIX sh. PowerShell (pwsh) is not in Alpine's
# repositories; to run .ps1 jobs locally, base this stage on
# mcr.microsoft.com/powershell instead (remote .ps1 jobs on Windows/Cygwin
# servers need nothing here).
FROM alpine:3.20 AS runtime
RUN apk add --no-cache python3 perl bash ca-certificates tzdata \
 && addgroup -g 65532 nonroot && adduser -D -H -u 65532 -G nonroot -s /sbin/nologin nonroot
COPY --from=build /out/monitor /usr/local/bin/monitor
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/certs /certs
COPY --from=build --chown=65532:65532 /out/shared /shared
USER 65532:65532
EXPOSE 8080 8443 9443
ENTRYPOINT ["/usr/local/bin/monitor"]
CMD ["run", "-config", "/etc/monitor/node.yaml"]

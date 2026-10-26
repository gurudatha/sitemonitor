# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/monitor ./cmd/monitor \
 && mkdir -p /out/data /out/certs

# ---- runtime: distroless, no shell, non-root ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/monitor /usr/local/bin/monitor
# Pre-create volume mount points owned by the nonroot user (uid 65532).
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/certs /certs
USER 65532:65532
EXPOSE 8080 8443 9443
ENTRYPOINT ["/usr/local/bin/monitor"]
CMD ["run", "-config", "/etc/monitor/node.yaml"]

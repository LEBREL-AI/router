# Reproducible image of the Lebrel router: pinned toolchain and base images (by digest), no CGO,
# no build ids, no timestamps. `scripts/reproducible-build.sh` builds it twice and compares.
# The build stage runs on the builder's own architecture and cross-compiles for the target: the
# confidential VMs are x86-64, so the published image is linux/amd64 (scripts/reproducible-build.sh).
FROM --platform=$BUILDPLATFORM golang:1.24.11-alpine@sha256:ef75fa8822a4c0fb53a390548b3dc1c39639339ec3373c58f5441117e1ff46ae AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOOS=$TARGETOS GOARCH=$TARGETARCH
RUN go build -ldflags="-s -w -buildid=" -o /out/lebrel-router ./cmd/router && mkdir -p /out/state

# distroless static: CA certificates for the labs' TLS, no shell, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/lebrel-router /usr/local/bin/lebrel-router
# The routes are part of the measured image: a change of upstream or price changes the hash.
COPY routes.production.json /etc/lebrel-router/routes.json
# Settlements owed to the control plane survive a restart: a volume is mounted here (owned by nonroot).
COPY --from=build --chown=65532:65532 /out/state /var/lib/lebrel-router
ENV ROUTES_FILE=/etc/lebrel-router/routes.json LISTEN_ADDR=:8090 SETTLEMENT_DIR=/var/lib/lebrel-router/settlements
VOLUME ["/var/lib/lebrel-router"]
EXPOSE 8090
USER nonroot
ENTRYPOINT ["/usr/local/bin/lebrel-router"]

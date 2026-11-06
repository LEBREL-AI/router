# Reproducible image of the Lebrel router: pinned toolchain and base images (by digest), no CGO,
# no build ids, no timestamps. `scripts/reproducible-build.sh` builds it twice and compares.
# The build stage runs on the builder's own architecture and cross-compiles for the target: the
# confidential VMs are x86-64, so the published image is linux/amd64 (scripts/reproducible-build.sh).
FROM --platform=$BUILDPLATFORM golang:1.26.3-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY internal ./internal
# The dependencies are vendored (Tinfoil's EHBP, MIT; Go's x/crypto for Let's Encrypt in-process, with
# x/net and x/text, BSD): the build touches no network.
ENV CGO_ENABLED=0 GOFLAGS="-trimpath -mod=vendor" GOOS=$TARGETOS GOARCH=$TARGETARCH GOTOOLCHAIN=local
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
# 8090 plaintext (an edge or gateway that ends TLS itself); 8443 TLS ending in the process (LISTEN_TLS_ADDR).
EXPOSE 8090 8443
USER nonroot
ENTRYPOINT ["/usr/local/bin/lebrel-router"]

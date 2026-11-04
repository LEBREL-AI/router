# syntax=docker/dockerfile:1
# Reproducible image of the Lebrel router: pinned toolchain and base images (by digest), no CGO,
# no build ids, no timestamps. `scripts/reproducible-build.sh` builds it twice and compares.
FROM golang:1.24.11-alpine@sha256:ef75fa8822a4c0fb53a390548b3dc1c39639339ec3373c58f5441117e1ff46ae AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN go build -ldflags="-s -w -buildid=" -o /out/lebrel-router ./cmd/router

# distroless static: CA certificates for the labs' TLS, no shell, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/lebrel-router /usr/local/bin/lebrel-router
# The routes are part of the measured image: a change of upstream or price changes the hash.
COPY routes.production.json /etc/lebrel-router/routes.json
ENV ROUTES_FILE=/etc/lebrel-router/routes.json LISTEN_ADDR=:8090
EXPOSE 8090
USER nonroot
ENTRYPOINT ["/usr/local/bin/lebrel-router"]

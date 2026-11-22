#!/usr/bin/env python3
"""Writes deploy/dstack/docker-compose.relay.yml, the relay's whole measured configuration.

    relay-config.py <instance> <zone> [<instance> <zone> ...] > docker-compose.relay.yml

Each pair is a router of the new app: its instance id (40 hex characters) and its gateway zone
(dstack-pha1-prod9.phala.network). Customers are sent to the first one that may take them; the
others are its backups, in order; the LAST one is the earlier router, which also receives
everything when no check passes.
"""
import re
import sys

IMAGE = "haproxy:3.4.7-alpine@sha256:b9fe316f718d5015c5cfebd8ca019591f3e44656f61d93cb8b59a1028961e530"

HEADER = """# The relay: a confidential VM of the EARLIER app (4d72eae6…, the app whose KMS root may have leaked on
# 6 November 2026) that no longer runs the router. It only passes bytes: every TCP connection that dstack's
# gateway hands it for attested.lebrel.ai (the custom name is still connected to the earlier app, because
# the gateway of the new KMS's zone does not route custom names yet) is piped, unread, to a router's own
# TLS listener in the NEW app (40b35a05…, Phala KMS 2026-11), through that router's gateway hostname.
#
#   client ──TLS (ends in a router, new app)────────────────────────────────────────────────┐
#     └─ gateway (earlier zone, passes the stream by SNI) ─ this relay ─ gateway (new zone) ─ router :443
#
# The relay opens its own TLS connection to the new zone's gateway (the gateway's certificate and name are
# verified; the gateway ends that outer TLS and hands the inner stream to port 443 of the router); what
# travels inside is the client's TLS session with the router, which neither gateway nor this relay can
# read or alter. The relay holds no key and no certificate of the name, and this file passes it no secret:
# a router's signing key, its certificate key and its sealed secrets live in the new app, under the new
# KMS. Let's Encrypt's validation of the name (TLS-ALPN-01) travels the same way and is answered by a
# router.
#
# Which router. Each one is addressed by its own instance (never by the app's hostname, which the gateway
# would hand to any instance of the app), in the order of the servers below: {order}
# The relay asks each on its plaintext listener, through the gateway's hostname for its port 8080
# (`GET /healthz/relay`, cmd/router/relayhealth.go): 200, it may be sent customers; 503, it serves but
# asks to be left (the operator stepped it aside, or its certificate is about to end); 410, it is
# stopping; 409, it holds no certificate for the name and cannot serve; a router of a release before
# 1.4.1 answers 404 and is sent customers as it always was. A connection goes to the first of:
#
#   challenges  when it is Let's Encrypt's validation (told from a client's by the protocol named in its
#               first bytes, read without ending TLS): a router that answers at all, ready or not. The
#               one without a certificate is the one that needs it, and a router answers its peer's
#               challenge as well as its own (cmd/router/tlspair.go).
#   routers     a router that may be sent customers (200, or 404). Its check gives up fast: when the
#               router that serves goes, the other has the customers within seconds.
#   standing    when there is none: a router that serves although it asked to be left (503 too). A
#               router that is stopping, or gone, leaves this list within seconds as well: the other,
#               stepped aside as it may be, has the customers then.
#   always      when no check passes: the earlier router, unchecked, as before there were checks. A
#               check failing for a reason of its own never closes the name.
#
# Port 8404 tells, to whoever asks, how many connections this relay holds, how many routers each list
# above counts and, for each router, whether the first two lists hold it: the operator knows when a
# relay is empty, and what it would do, before touching it.
#
# What the relay does NOT change: which machine receives the name's connections is still decided by the
# earlier zone's gateway, which knows the earlier app's instances by certificates of the earlier KMS. Until
# the name is connected to the new app directly, the name's certificate issuance has to be pinned in DNS
# (a CAA record naming the routers' own ACME accounts), so that receiving its connections is not enough to
# obtain a certificate for it. See RELEASES.md.
#
# The whole configuration is in this file (written by relay-config.py), so dstack measures it (image
# digest included) into the relay's quote. Image: the official haproxy 3.4.7-alpine (docker-library),
# pinned by digest. Nothing is logged about a connection that ends normally; a failed one logs its time,
# duration and byte counts (the address on the other side is the gateway's, never a customer's), and a
# router's check logs when it changes.
"""

CONFIG_HEAD = """global
  log stdout format raw local0 notice
  maxconn 30000
defaults
  mode tcp
  log global
  option tcplog
  option dontlog-normal
  option log-separate-errors
  option clitcpka
  option srvtcpka
  clitcpka-idle 60s
  clitcpka-intvl 15s
  clitcpka-cnt 4
  srvtcpka-idle 60s
  srvtcpka-intvl 15s
  srvtcpka-cnt 4
  retries 1
  option redispatch
  option log-health-checks
  timeout check 2s
  timeout connect 4s
  timeout client 1h
  timeout server 1h
  timeout tunnel 1h
  timeout client-fin 30s
  timeout server-fin 30s
resolvers system
  parse-resolv-conf
  timeout resolve 10s
  hold timeout 24h
  hold other 24h
  hold refused 24h
  hold nx 24h
frontend names
  bind :8443
  tcp-request inspect-delay 5s
  tcp-request content accept if { req.ssl_hello_type 1 }
  use_backend challenges if { req.ssl_alpn acme-tls/1 } { nbsrv(challenges) gt 0 }
  use_backend routers if { nbsrv(routers) gt 0 }
  use_backend standing if { nbsrv(standing) gt 0 }
  default_backend always
frontend state
  bind :8404
  mode http
  maxconn 20
  timeout client 10s
  timeout http-request 5s
  http-request return status 200 content-type "text/plain" lf-string "connections %[fe_conn(names)]\\nrouters %[nbsrv(routers)]\\nstanding %[nbsrv(standing)]\\nchallenges %[nbsrv(challenges)]\\n{each}"
"""

SERVER = "ssl verify required ca-file @system-ca resolvers system init-addr libc,last,none"
CHECK = "  option httpchk\n  http-check send meth GET uri /healthz/relay ver HTTP/1.1 hdr Host relay.check\n"


def names(instance, zone):
    return f"{instance}-443.{zone}", f"{instance}-8080.{zone}"


def backend(name, expect, pace, routers):
    text = f"backend {name}\n" + CHECK + f"  http-check expect rstatus {expect}\n  default-server {SERVER} {pace}\n"
    for index, (instance, zone) in enumerate(routers):
        tls, plain = names(instance, zone)
        # a server is named by the start of its instance id, which is what its check's log lines show
        text += f"  server {instance[:8]} {tls}:443 sni str({tls}) verifyhost {tls} check check-sni {plain}"
        text += " backup\n" if index else "\n"
    return text


def config(routers):
    earlier, _ = names(*routers[-1])
    # one line a router on the state port: its name, then 1 or 0 for each of the first two lists
    each = "".join(f"{instance[:8]} %[srv_is_up(routers/{instance[:8]})] %[srv_is_up(standing/{instance[:8]})]\\n" for instance, _ in routers)
    return (
        CONFIG_HEAD.replace("{each}", each)
        + backend("routers", "^(200|404)", "inter 2s fastinter 1s downinter 2s rise 2 fall 2", routers)
        + backend("standing", "^(200|404|503)", "inter 2s fastinter 1s rise 1 fall 3", routers)
        + backend("challenges", "^[1-5]", "inter 3s rise 1 fall 3", routers)
        + f"backend always\n  server earlier {earlier}:443 {SERVER} sni str({earlier}) verifyhost {earlier}\n"
    )


def compose(routers):
    if len(routers) == 1:
        order = "one router today."
    else:
        order = "the first one that may take customers serves them all;\n# the ones after it are its backups."
    body = "\n".join(("        " + line) if line else "" for line in config(routers).rstrip("\n").split("\n"))
    return (
        HEADER.format(order=order)
        + "services:\n  relay:\n"
        + f"    image: {IMAGE}\n"
        + "    restart: unless-stopped\n    ports:\n      - \"443:8443\"\n      - \"8404:8404\"\n"
        + "    entrypoint: [\"/bin/sh\", \"-c\", \"printf '%s\\\\n' \\\"$$RELAY_CONFIG\\\" > /tmp/relay.cfg && exec haproxy -W -db -f /tmp/relay.cfg\"]\n"
        + "    environment:\n      RELAY_CONFIG: |\n" + body + "\n"
    )


if __name__ == "__main__":
    args = sys.argv[1:]
    if not args or len(args) % 2:
        sys.exit(__doc__)
    pairs = [(args[i], args[i + 1]) for i in range(0, len(args), 2)]
    for instance, zone in pairs:
        if not re.fullmatch(r"[0-9a-f]{40}", instance) or not re.fullmatch(r"[a-z0-9-]+(\.[a-z0-9-]+)+", zone):
            sys.exit(f"not an instance id and its zone: {instance} {zone}\n{__doc__}")
    if len({instance for instance, _ in pairs}) != len(pairs):
        sys.exit("an instance is named twice")
    if len({instance[:8] for instance, _ in pairs}) != len(pairs):
        sys.exit("two instances begin with the same eight characters, which is what a server is named by")
    sys.stdout.write(compose(pairs))

package main

import (
	"crypto/tls"
	"io"
	"log"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestTrustedIngressPrefixesReadAddressesAndCIDRsAndUnmapIPv4(t *testing.T) {
	got, err := prefixes("127.0.0.1, 10.0.0.0/8,::ffff:192.168.0.0/112, 2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("2001:db8::/32")}
	if !slices.Equal(got, want) {
		t.Fatalf("prefixes: got %v, want %v", got, want)
	}
	if _, err := prefixes("nonsense"); err == nil {
		t.Fatal("a word is neither an address nor a CIDR")
	}
	if got, err := prefixes(" , "); err != nil || len(got) != 0 {
		t.Fatalf("an empty list: %v %v", got, err)
	}
}

func TestTheTLSListenerTakesOneCertificateSource(t *testing.T) {
	quiet := log.New(io.Discard, "", 0)
	if _, _, err := tlsSetup("cert.pem", "", nil, "", "", quiet); err == nil {
		t.Fatal("a certificate without its key must be refused")
	}
	if _, _, err := tlsSetup("cert.pem", "key.pem", []string{"attested.lebrel.ai"}, t.TempDir(), "", quiet); err == nil {
		t.Fatal("files and Let's Encrypt exclude each other")
	}
	if config, source, err := tlsSetup("", "", nil, "", "", quiet); err != nil || config != nil || source != "" {
		t.Fatalf("no source is no TLS: %v %v %q", config, err, source)
	}
	if _, _, err := tlsSetup("", "", []string{"attested.lebrel.ai"}, "", "", quiet); err == nil || !strings.Contains(err.Error(), "TLS_ACME_CACHE") {
		t.Fatalf("Let's Encrypt needs a place to persist: %v", err)
	}
	for _, bad := range []string{"*.lebrel.ai", "attested.lebrel.ai:443", "https://attested.lebrel.ai"} {
		if _, _, err := tlsSetup("", "", []string{bad}, t.TempDir(), "", quiet); err == nil {
			t.Fatalf("%q is not a name Let's Encrypt issues for here", bad)
		}
	}
	config, source, err := tlsSetup("", "", []string{"attested.lebrel.ai", "router-tee.lebrel.ai"}, t.TempDir(), "ops@lebrel.ai", quiet)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "Let's Encrypt") || !strings.Contains(source, "attested.lebrel.ai, router-tee.lebrel.ai") {
		t.Fatalf("source %q", source)
	}
	// the challenge is answered on the listener itself, HTTP/2 stays, and old TLS is refused
	if !slices.Contains(config.NextProtos, "acme-tls/1") || !slices.Contains(config.NextProtos, "h2") || config.GetCertificate == nil || config.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS config: protos=%v min=%d", config.NextProtos, config.MinVersion)
	}
	// a name outside the list is refused at the handshake, before any ACME order
	if _, err := config.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example"}); err == nil {
		t.Fatal("a name outside TLS_ACME_HOSTS must be refused")
	}
}

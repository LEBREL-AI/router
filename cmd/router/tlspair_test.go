package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

const (
	pairName   = "attested.example.com"
	otherName  = "old.example.com"
	pairSecret = "the pair's secret, the same on both instances"
)

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// cachePEM is a key and its certificates in autocert's cache form (the key, then the chain).
func cachePEM(t *testing.T, key *ecdsa.PrivateKey, chain ...[]byte) []byte {
	t.Helper()
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	for _, der := range chain {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	return buf.Bytes()
}

func certTemplate(host string, notBefore, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

// certPEM is a key and a self-signed certificate for host, valid from notBefore to notAfter, in
// autocert's cache form.
func certPEM(t *testing.T, host string, notBefore, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := certTemplate(host, notBefore, notAfter)
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return cachePEM(t, key, der)
}

// rsaCertPEM is the same on an RSA key, as autocert keeps one under "<host>+rsa".
func rsaCertPEM(t *testing.T, host string, notBefore, notAfter time.Time) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := certTemplate(host, notBefore, notAfter)
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	return buf.Bytes()
}

// challengePEM is the TLS-ALPN-01 challenge certificate an ACME account makes for host and token, in
// autocert's cache form: what autocert keeps under "<host>+token" while an order is pending.
func challengePEM(t *testing.T, host, token string) []byte {
	t.Helper()
	account, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := (&acme.Client{Key: account}).TLSALPN01ChallengeCert(token, host)
	if err != nil {
		t.Fatal(err)
	}
	return cachePEM(t, cert.PrivateKey.(*ecdsa.PrivateKey), cert.Certificate...)
}

// leafOf is the first certificate of a key and its chain in autocert's cache form.
func leafOf(data []byte) []byte {
	block, _ := pem.Decode(data[bytes.Index(data, []byte("-----BEGIN CERTIFICATE")):])
	return block.Bytes
}

func mustKey(t *testing.T, secret string) []byte {
	t.Helper()
	key, err := pairKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// pairIn is an instance of a pair whose certificates live in dir, its peer at peerURL ("" for an
// instance that does not know its peer). It never reaches Let's Encrypt, and a validation of a name
// is handed to the instance itself unless a test says otherwise.
func pairIn(t *testing.T, dir, peerURL string, notify func(string)) *tlsPair {
	t.Helper()
	pair, err := newTLSPair([]string{pairName, otherName}, []string{pairName}, dir, "", peerURL, pairSecret, nil, quietLogger(), notify)
	if err != nil {
		t.Fatal(err)
	}
	pair.manager.Client.DirectoryURL = "http://127.0.0.1:1/never"
	pair.validation = handedTo(pair)
	pair.register = func(context.Context) (string, error) { return "https://acme.example/acct/1", nil }
	pair.pace.again = 0 // a try that fails is not made a second time, unless a test is about that
	return pair
}

// handedTo is a validation the relay hands to the given instance: what it would present.
func handedTo(pair *tlsPair) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, host string) ([]byte, error) {
		cert, err := pair.GetCertificate(&tls.ClientHelloInfo{ServerName: host, SupportedProtos: []string{acme.ALPNProto}})
		if err != nil {
			return nil, err
		}
		return cert.Certificate[0], nil
	}
}

// plainOf serves an instance's plaintext listener (the peer's lookup among it), counting the lookups.
func plainOf(t *testing.T, pair *tlsPair) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var lookups atomic.Int64
	inner := pair.Handler(http.NotFoundHandler())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == acmeChallengePath {
			lookups.Add(1)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &lookups
}

func peerAt(t *testing.T, url, secret string) *peer {
	t.Helper()
	other, err := newPeer(url, mustKey(t, secret), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	return other
}

func TestAChallengeThisInstanceLacksIsLookedUpAtItsPeer(t *testing.T) {
	ctx := context.Background()
	theirDir := t.TempDir()
	theirs := pairIn(t, theirDir, "http://127.0.0.1:1", nil)
	challenge := challengePEM(t, pairName, "token-of-the-peer")
	disk := autocert.DirCache(theirDir)
	_ = disk.Put(ctx, pairName+challengeKeySuffix, challenge)
	// what must never cross: the peer's certificate with its private key (its account key is on its disk too)
	_ = disk.Put(ctx, pairName, certPEM(t, pairName, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)))
	plain, lookups := plainOf(t, theirs)

	mine := pairIn(t, t.TempDir(), plain.URL, nil)
	presented, err := handedTo(mine)(ctx, pairName)
	if err != nil || !bytes.Equal(presented, leafOf(challenge)) {
		t.Fatalf("a validation handed to this instance was not answered with the peer's challenge: %v", err)
	}
	if lookups.Load() != 1 {
		t.Fatalf("%d lookups for one validation", lookups.Load())
	}
	// its own pending order answers first, and the peer is not asked
	own := challengePEM(t, pairName, "token-of-this-instance")
	_ = mine.disk.Put(ctx, pairName+challengeKeySuffix, own)
	presented, err = handedTo(mine)(ctx, pairName)
	if err != nil || !bytes.Equal(presented, leafOf(own)) || lookups.Load() != 1 {
		t.Fatalf("its own challenge must answer first, without asking the peer: %v (%d lookups)", err, lookups.Load())
	}
	// a name it holds nothing for is never asked of the peer
	if _, err := handedTo(mine)(ctx, "unknown.example.com"); err == nil || lookups.Load() != 1 {
		t.Fatalf("an unknown name: %v (%d lookups)", err, lookups.Load())
	}
}

func TestThePeerAnswersOnlyAChallengeOfItsOwnMaking(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pair := pairIn(t, dir, "http://127.0.0.1:1", nil)
	disk := autocert.DirCache(dir)
	real := certPEM(t, pairName, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	_ = disk.Put(ctx, pairName, real)
	plain, _ := plainOf(t, pair)
	get := func(query string) (int, string) {
		response, err := http.Get(plain.URL + acmeChallengePath + query)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	for _, query := range []string{"?name=" + pairName, "", "?name=unknown.example.com", "?name=" + pairName + "%2Btoken", "?name=acme_account%2Bkey", "?name=..%2F" + pairName} {
		if status, body := get(query); status != http.StatusNotFound || strings.Contains(body, "PRIVATE") {
			t.Fatalf("%q with no order pending: %d %s", query, status, body)
		}
	}
	// a certificate clients would trust, wherever it lies, is never answered as a challenge
	_ = disk.Put(ctx, pairName+challengeKeySuffix, real)
	if status, body := get("?name=" + pairName); status != http.StatusNotFound || strings.Contains(body, "PRIVATE") {
		t.Fatalf("a real certificate was answered as a challenge: %d %s", status, body)
	}
	_ = disk.Put(ctx, pairName+challengeKeySuffix, challengePEM(t, pairName, "token"))
	if status, _ := get("?name=" + pairName); status != http.StatusOK {
		t.Fatalf("a pending challenge: %d", status)
	}
	request, _ := http.NewRequest(http.MethodPost, plain.URL+acmeChallengePath+"?name="+pairName, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", response.StatusCode)
	}
	// an instance that does not know its peer answers nothing
	alone := pairIn(t, dir, "", nil)
	lonely, _ := plainOf(t, alone)
	if response, err := http.Get(lonely.URL + acmeChallengePath + "?name=" + pairName); err != nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("an instance without a peer answered a challenge: %v", err)
	}
}

func writeChallenge(w http.ResponseWriter, name string, data, mac []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"version":1,"name":"`+name+`","pem":"`+base64.StdEncoding.EncodeToString(data)+`","mac":"`+hex.EncodeToString(mac)+`"}`)
}

func TestAChallengeIsTakenOnlyWithThePairsSecretAndOnlyForItsName(t *testing.T) {
	ctx := context.Background()
	key := mustKey(t, pairSecret)
	good := challengePEM(t, pairName, "token")
	real := certPEM(t, pairName, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	forOther := challengePEM(t, otherName, "token")
	answers := map[string]func(w http.ResponseWriter){
		"a challenge signed with another secret": func(w http.ResponseWriter) {
			writeChallenge(w, pairName, good, signChallenge(mustKey(t, "another pair's secret, of another deployment"), pairName, good))
		},
		"a real certificate, signed": func(w http.ResponseWriter) {
			writeChallenge(w, pairName, real, signChallenge(key, pairName, real))
		},
		"a challenge for another name under this one": func(w http.ResponseWriter) {
			writeChallenge(w, pairName, forOther, signChallenge(key, pairName, forOther))
		},
		"an answer that names another name": func(w http.ResponseWriter) {
			writeChallenge(w, otherName, forOther, signChallenge(key, otherName, forOther))
		},
		"a redirect": func(w http.ResponseWriter) {
			w.Header().Set("Location", "http://127.0.0.1:1"+acmeChallengePath+"?name="+pairName)
			w.WriteHeader(http.StatusFound)
		},
		"too much": func(w http.ResponseWriter) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), acmeChallengeMaxBytes+10))
		},
		"an error": func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		"unknown fields": func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"version":1,"name":"`+pairName+`","pem":"","mac":"","extra":1}`)
		},
	}
	for what, answer := range answers {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { answer(w) }))
		if _, err := peerAt(t, server.URL, pairSecret).challenge(ctx, pairName); err == nil {
			t.Fatalf("%s was taken as a challenge", what)
		}
		server.Close()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeChallenge(w, pairName, good, signChallenge(key, pairName, good))
	}))
	defer server.Close()
	cert, err := peerAt(t, server.URL, pairSecret).challenge(ctx, pairName)
	if err != nil || !bytes.Equal(cert.Certificate[0], leafOf(good)) {
		t.Fatalf("the pair's own challenge was refused: %v", err)
	}
}

func TestAPeerWithoutAChallengeIsAskedOnceForEveryoneAndAValidationThatComesRightAfterIsNotAnsweredStale(t *testing.T) {
	key := mustKey(t, pairSecret)
	challenge := challengePEM(t, pairName, "token")
	release := make(chan struct{})
	var lookups atomic.Int64
	var pending atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		had := pending.Load() // what the peer holds when it is asked, whenever its answer arrives
		if lookups.Add(1) == 1 {
			<-release
		}
		if !had {
			http.NotFound(w, r)
			return
		}
		writeChallenge(w, pairName, challenge, signChallenge(key, pairName, challenge))
	}))
	defer server.Close()
	other := peerAt(t, server.URL, pairSecret)
	other.missFor = 300 * time.Millisecond
	var wg sync.WaitGroup
	var served atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := other.challenge(context.Background(), pairName); err == nil {
				served.Add(1)
			}
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for lookups.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // every validation has asked by now, and they share the one lookup
	if got := lookups.Load(); got != 1 {
		t.Fatalf("20 validations at once made %d lookups at the peer, not 1", got)
	}
	// the peer's order begins while that lookup is still out: its answer ("none") is already stale
	pending.Store(true)
	close(release)
	wg.Wait()
	// those who only joined the first lookup asked once more themselves, together, after the pause: one
	// more lookup for all of them, or a few on a machine so busy that one of them wakes after it ended
	if got := lookups.Load(); got < 2 || got > 20 {
		t.Fatalf("%d lookups: the validations that joined a lookup begun before the order must ask once more, together", got)
	}
	if got := served.Load(); got != 19 {
		t.Fatalf("%d of the 19 validations that could ask again were served the peer's challenge", got)
	}
	// and with nothing pending, a validation right after a miss waits for the pause instead of taking the miss
	pending.Store(false)
	if _, err := other.challenge(context.Background(), pairName); err == nil {
		t.Fatal("served although the peer holds nothing")
	}
	asked := lookups.Load()
	pending.Store(true)
	started := time.Now()
	if _, err := other.challenge(context.Background(), pairName); err != nil {
		t.Fatalf("a validation right after a miss was answered with the miss: %v", err)
	}
	if waited := time.Since(started); waited < 200*time.Millisecond || lookups.Load() != asked+1 {
		t.Fatalf("it must wait for the pause (%s) and ask once (%d lookups)", waited, lookups.Load()-asked)
	}
}

func TestALookupDoesNotOutliveTheHandshakeThatWaits(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer server.Close()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := peerAt(t, server.URL, pairSecret).challenge(ctx, pairName); err == nil {
		t.Fatal("a peer that never answers gave a challenge")
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("the handshake waited %s for the peer", waited)
	}
}

func TestThePeersAddressAndThePairsSecretAreCheckedAtStart(t *testing.T) {
	for _, bad := range []string{"", "ftp://peer.example.com", "http://peer.example.com", "http://127.0.0.1.evil.example", "http://localhost@evil.example",
		"https://user@peer.example.com", "https://peer.example.com/path", "https://peer.example.com?x=1", "https://peer.example.com#x", "https://"} {
		if _, err := peerBase(bad); err == nil {
			t.Fatalf("%q was accepted as the peer's address", bad)
		}
	}
	for good, want := range map[string]string{
		"https://abc-8080.gateway.example.com":  "https://abc-8080.gateway.example.com",
		"https://abc-8080.gateway.example.com/": "https://abc-8080.gateway.example.com",
		" http://127.0.0.1:8090 ":               "http://127.0.0.1:8090",
		"http://localhost:8090":                 "http://localhost:8090",
	} {
		if got, err := peerBase(good); err != nil || got != want {
			t.Fatalf("%q: %q %v", good, got, err)
		}
	}
	if _, err := pairKey("too short"); err == nil {
		t.Fatal("a short secret was accepted")
	}
	one, two := mustKey(t, pairSecret), mustKey(t, pairSecret+" ")
	if !bytes.Equal(one, two) || bytes.Equal(one, mustKey(t, "another pair's secret, of another deployment")) {
		t.Fatal("the same secret must give the same key, another secret another")
	}
	hosts := []string{pairName}
	if _, err := newTLSPair(hosts, hosts, t.TempDir(), "", "https://peer.example.com", "short", nil, quietLogger(), nil); err == nil {
		t.Fatal("a peer without the pair's secret was accepted")
	}
	if _, err := newTLSPair(hosts, hosts, t.TempDir(), "", "http://peer.example.com", pairSecret, nil, quietLogger(), nil); err == nil {
		t.Fatal("a peer over plain http was accepted")
	}
	if _, err := newTLSPair(hosts, nil, t.TempDir(), "", "https://peer.example.com", pairSecret, nil, quietLogger(), nil); err == nil {
		t.Fatal("a pair whose customers' names it cannot obtain was accepted")
	}
	if _, err := newTLSPair(nil, nil, t.TempDir(), "", "https://peer.example.com", pairSecret, nil, quietLogger(), nil); err == nil {
		t.Fatal("a pair without names was accepted")
	}
}

func TestAnInstanceIsReadyWithACertificateForTheNamesItsCustomersCallAndNoOther(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pair := pairIn(t, dir, "https://peer.example.com", nil)
	if pair.look() || pair.Ready() {
		t.Fatal("ready without a certificate")
	}
	disk := autocert.DirCache(dir)
	now := time.Now()
	// the name nobody is served under does not make it ready, and its absence does not keep it from being so
	_ = disk.Put(ctx, otherName, certPEM(t, otherName, now.Add(-time.Hour), now.Add(time.Hour)))
	if pair.look() {
		t.Fatal("ready with a certificate for another name only")
	}
	_ = disk.Put(ctx, pairName, certPEM(t, pairName, now.Add(-2*time.Hour), now.Add(-time.Hour)))
	if pair.look() {
		t.Fatal("ready with an expired certificate")
	}
	_ = disk.Put(ctx, pairName, certPEM(t, otherName, now.Add(-time.Hour), now.Add(time.Hour)))
	if pair.look() {
		t.Fatal("ready with a certificate for another name under this one")
	}
	_ = disk.Put(ctx, pairName, certPEM(t, pairName, now.Add(-time.Hour), now.Add(time.Hour)))
	_ = disk.Delete(ctx, otherName)
	if !pair.look() || !pair.Ready() {
		t.Fatal("not ready with a valid certificate for the served name")
	}
	// minutes before the certificate ends it stops calling itself ready, and goes on serving with it
	pair.now = func() time.Time { return now.Add(time.Hour - readyMargin/2) }
	if pair.look() || pair.Ready() {
		t.Fatal("ready with a certificate about to end: the relay must move its customers before")
	}
	if !pair.holds(pairName) {
		t.Fatal("a certificate still valid is no longer served")
	}
	// and it stops serving it the moment it ends, without waiting for the next look
	pair.now = func() time.Time { return now.Add(2 * time.Hour) }
	if pair.holds(pairName) {
		t.Fatal("an expired certificate is still served")
	}
}

// failingDisk is a disk that cannot be read for a while.
type failingDisk struct {
	autocert.Cache
	broken atomic.Bool
}

func (d *failingDisk) Get(ctx context.Context, key string) ([]byte, error) {
	if d.broken.Load() {
		return nil, errors.New("input/output error")
	}
	return d.Cache.Get(ctx, key)
}

func TestADiskThatCannotBeReadForAMomentTakesNoCertificateAway(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	_ = autocert.DirCache(dir).Put(context.Background(), pairName, certPEM(t, pairName, now.Add(-time.Hour), now.Add(24*time.Hour)))
	pair := pairIn(t, dir, "https://peer.example.com", nil)
	disk := &failingDisk{Cache: pair.disk}
	pair.disk = disk
	if !pair.look() {
		t.Fatal("not ready with its certificate on disk")
	}
	disk.broken.Store(true)
	if !pair.look() || !pair.Ready() || !pair.holds(pairName) {
		t.Fatal("a disk that failed once made a serving instance drop its certificate")
	}
	disk.broken.Store(false)
	_ = autocert.DirCache(dir).Delete(context.Background(), pairName)
	if pair.look() || pair.holds(pairName) {
		t.Fatal("a certificate that is gone from the disk is still held")
	}
}

// listening serves config on a local port and counts nothing: every connection is handshaken and dropped.
func listening(t *testing.T, config *tls.Config) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.(*tls.Conn).Handshake()
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// handshake dials addr as a client with config and returns what the server presented.
func handshake(addr string, config *tls.Config) (*x509.Certificate, error) {
	config.InsecureSkipVerify = true
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, config)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0], nil
}

// ordersCounted makes the instance's ACME client call a local server that counts and refuses.
func ordersCounted(t *testing.T, pair *tlsPair) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	directory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(directory.Close)
	pair.manager.Client.DirectoryURL = directory.URL
	return &calls
}

func TestOverRealTLSAValidationIsAnsweredWithThePeersChallengeAndAClientOnlyWithACertificateHeld(t *testing.T) {
	ctx := context.Background()
	// the peer: an order pending, its challenge on its own disk
	peerDir := t.TempDir()
	challenge := challengePEM(t, pairName, "token-of-the-peer")
	_ = autocert.DirCache(peerDir).Put(ctx, pairName+challengeKeySuffix, challenge)
	plain, _ := plainOf(t, pairIn(t, peerDir, "http://127.0.0.1:1", nil))

	// this instance: nothing on disk yet
	dir := t.TempDir()
	pair := pairIn(t, dir, plain.URL, nil)
	orders := ordersCounted(t, pair)
	pair.look()
	addr := listening(t, pair.TLSConfig())

	// Let's Encrypt's validation, handed to this instance by the relay, finds the peer's challenge
	presented, err := handshake(addr, &tls.Config{ServerName: pairName, NextProtos: []string{acme.ALPNProto}})
	if err != nil {
		t.Fatalf("the validation connection was refused: %v", err)
	}
	if !bytes.Equal(presented.Raw, leafOf(challenge)) {
		t.Fatal("the validation connection was not answered with the peer's challenge certificate")
	}
	// a client, while this instance holds no certificate: refused, and no order is started for it
	clients := map[string]*tls.Config{
		"an ordinary client":      {ServerName: pairName, NextProtos: []string{"h2", "http/1.1"}},
		"a TLS 1.3 only client":   {ServerName: pairName, MinVersion: tls.VersionTLS13},
		"a client that takes RSA": {ServerName: pairName, MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}},
	}
	for who, config := range clients {
		if _, err := handshake(addr, config.Clone()); err == nil {
			t.Fatalf("%s was served without a certificate", who)
		}
	}
	if _, err := handshake(addr, &tls.Config{ServerName: otherName}); err == nil {
		t.Fatal("a client of the other name was served without a certificate")
	}
	if _, err := handshake(addr, &tls.Config{ServerName: "unknown.example.com"}); err == nil {
		t.Fatal("an unknown name was served")
	}
	if got := orders.Load(); got != 0 {
		t.Fatalf("clients' handshakes reached Let's Encrypt %d times", got)
	}
	// with its certificate on disk the clients are served it: the one autocert obtains, on its ECDSA
	// key, whatever autocert would have read their hello as, and still without a call to Let's Encrypt
	now := time.Now()
	own := certPEM(t, pairName, now.Add(-time.Hour), now.Add(24*time.Hour))
	_ = autocert.DirCache(dir).Put(ctx, pairName, own)
	if !pair.look() {
		t.Fatal("not ready with its certificate on disk")
	}
	for _, who := range []string{"an ordinary client", "a TLS 1.3 only client"} {
		presented, err = handshake(addr, clients[who].Clone())
		if err != nil {
			t.Fatalf("%s was refused although the certificate is held: %v", who, err)
		}
		if !bytes.Equal(presented.Raw, leafOf(own)) {
			t.Fatalf("%s was not served this instance's own certificate", who)
		}
	}
	// the client that takes nothing but RSA is refused, and still starts no order
	if _, err := handshake(addr, clients["a client that takes RSA"].Clone()); err == nil {
		t.Fatal("a client that takes RSA only was served an ECDSA certificate")
	}
	// unless an earlier release left the RSA certificate on disk: then it is served that one
	rsaOne := rsaCertPEM(t, pairName, now.Add(-time.Hour), now.Add(24*time.Hour))
	_ = autocert.DirCache(dir).Put(ctx, pairName+rsaKeySuffix, rsaOne)
	pair.look()
	presented, err = handshake(addr, clients["a client that takes RSA"].Clone())
	if err != nil || !bytes.Equal(presented.Raw, leafOf(rsaOne)) {
		t.Fatalf("a client that takes RSA only was not served the RSA certificate held: %v", err)
	}
	if presented, err = handshake(addr, clients["an ordinary client"].Clone()); err != nil || !bytes.Equal(presented.Raw, leafOf(own)) {
		t.Fatalf("an ordinary client must go on being served the ECDSA certificate: %v", err)
	}
	if got := orders.Load(); got != 0 {
		t.Fatalf("serving the certificates held reached Let's Encrypt %d times", got)
	}
}

// alerts collects what the operator is told.
type alerts struct {
	mu   sync.Mutex
	told []string
}

func (a *alerts) notify(text string) {
	a.mu.Lock()
	a.told = append(a.told, text)
	a.mu.Unlock()
}

func (a *alerts) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.told...)
}

func TestAnInstanceWithoutACertificateAsksAtItsOwnPaceTellsTheOperatorOnceAndBecomesReady(t *testing.T) {
	dir := t.TempDir()
	told := &alerts{}
	pair := pairIn(t, dir, "https://peer.example.com", told.notify)
	pair.pace.ask, pair.pace.check, pair.pace.probe, pair.pace.verify, pair.pace.alert = 120*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, time.Hour, time.Hour
	var asks atomic.Int64
	var succeed atomic.Bool
	pair.obtain = func(host string, rsa bool) error {
		if host != pairName || rsa {
			t.Errorf("asked for %s (rsa %v): only the certificate customers are served with is asked", host, rsa)
		}
		asks.Add(1)
		if !succeed.Load() {
			return errors.New("acme: validation failed\nsecond line never told")
		}
		now := time.Now()
		return autocert.DirCache(dir).Put(context.Background(), pairName, certPEM(t, pairName, now.Add(-time.Minute), now.Add(90*24*time.Hour)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pair.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	// 300 ms of looking every 5 ms: asked at the start and every 120 ms, not at every look
	if got := asks.Load(); got < 2 || got > 4 {
		t.Fatalf("asked %d times in 300 ms at one every 120 ms", got)
	}
	if pair.Ready() {
		t.Fatal("ready although every order failed")
	}
	if got := told.all(); len(got) != 1 || !strings.Contains(got[0], pairName) || !strings.Contains(got[0], "validation failed") || strings.Contains(got[0], "second line") || !strings.Contains(got[0], "👉") {
		t.Fatalf("the operator is told once, with the reason: %q", got)
	}
	succeed.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for !pair.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !pair.Ready() {
		t.Fatal("not ready after its certificate was obtained")
	}
	if got := told.all(); len(got) != 2 || !strings.Contains(got[1], "ya tiene su certificado") {
		t.Fatalf("the end of the trouble is told: %q", got)
	}
	// ready: nothing more is asked of Let's Encrypt, the certificate is only loaded for its renewal
	settled := asks.Load()
	time.Sleep(200 * time.Millisecond)
	if got := asks.Load() - settled; got > 1 {
		t.Fatalf("a ready instance asked %d more times", got)
	}
}

func TestAChallengeLeftOnDiskByAProcessThatDiedDoesNotHoldTheNextOne(t *testing.T) {
	dir := t.TempDir()
	disk := autocert.DirCache(dir)
	stale := challengePEM(t, pairName, "of an order nobody waits for")
	_ = disk.Put(context.Background(), pairName+challengeKeySuffix, stale)
	_ = disk.Put(context.Background(), otherName+challengeKeySuffix, challengePEM(t, otherName, "another"))
	pair := pairIn(t, dir, "https://peer.example.com", nil)
	pair.pace.check, pair.pace.probe, pair.pace.ask = 5*time.Millisecond, 5*time.Millisecond, time.Hour
	var asks atomic.Int64
	pair.obtain = func(host string, rsa bool) error {
		asks.Add(1)
		now := time.Now()
		return disk.Put(context.Background(), pairName, certPEM(t, pairName, now.Add(-time.Minute), now.Add(90*24*time.Hour)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pair.Start(ctx)
	for _, name := range []string{pairName, otherName} {
		if _, err := disk.Get(context.Background(), name+challengeKeySuffix); !errors.Is(err, autocert.ErrCacheMiss) {
			t.Fatalf("the challenge a dead process left for %s is still on disk (%v)", name, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for !pair.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// asked once (and then loaded once more for its renewal, which is the same call)
	if got := asks.Load(); !pair.Ready() || got < 1 || got > 2 {
		t.Fatalf("ready %v after %d asks: a stale challenge must not keep the instance from asking", pair.Ready(), got)
	}
}

func TestAReadyInstanceLoadsTheCertificatesItServesForRenewalAndOrdersNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	now := time.Now()
	disk := autocert.DirCache(dir)
	_ = disk.Put(ctx, pairName, certPEM(t, pairName, now.Add(-time.Minute), now.Add(90*24*time.Hour)))
	_ = disk.Put(ctx, pairName+rsaKeySuffix, rsaCertPEM(t, pairName, now.Add(-time.Minute), now.Add(90*24*time.Hour)))
	// a certificate held for a name nobody is served under is left alone: its renewal is nobody's business
	_ = disk.Put(ctx, otherName, certPEM(t, otherName, now.Add(-time.Minute), now.Add(90*24*time.Hour)))
	told := &alerts{}
	pair := pairIn(t, dir, "https://peer.example.com", told.notify)
	pair.pace.check, pair.pace.warm = 5*time.Millisecond, 60*time.Millisecond
	var mu sync.Mutex
	loaded := map[string]int{}
	pair.obtain = func(host string, rsa bool) error {
		mu.Lock()
		if rsa {
			host += rsaKeySuffix
		}
		loaded[host]++
		mu.Unlock()
		return nil
	}
	pair.Start(ctx)
	if !pair.Ready() {
		t.Fatal("an instance that starts with its certificate is ready before its listeners open")
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if loaded[pairName] < 2 || loaded[pairName] > 5 || loaded[pairName+rsaKeySuffix] != loaded[pairName] || loaded[otherName] != 0 {
		t.Fatalf("loaded for renewal: %v", loaded)
	}
	if got := told.all(); len(got) != 0 {
		t.Fatalf("nothing to tell the operator: %q", got)
	}
}

func TestACertificateThatShouldHaveBeenRenewedIsToldAndNotAtEveryLook(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	told := &alerts{}
	pair := pairIn(t, dir, "https://peer.example.com", told.notify)
	issued := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	_ = autocert.DirCache(dir).Put(ctx, pairName, certPEM(t, pairName, issued, issued.Add(90*24*time.Hour)))
	at := func(day int) {
		pair.now = func() time.Time { return issued.Add(time.Duration(day) * 24 * time.Hour) }
		pair.look()
		pair.watch()
	}
	at(10) // 80 days left
	at(62) // 28 days left: autocert renews from 30 on, a few days of failures are not told
	if got := told.all(); len(got) != 0 {
		t.Fatalf("told too early: %q", got)
	}
	at(66) // 24 days left
	at(66)
	got := told.all()
	if len(got) != 1 || !strings.Contains(got[0], "caduca en 24 días") || !strings.Contains(got[0], "👉") {
		t.Fatalf("told once when the renewal is overdue: %q", got)
	}
	at(67) // a day later, more than six hours on: told again
	if got := told.all(); len(got) != 2 {
		t.Fatalf("told again after the pause: %q", got)
	}
	// renewed: nothing more, and a later trouble is told afresh
	renewed := issued.Add(67 * 24 * time.Hour)
	_ = autocert.DirCache(dir).Put(ctx, pairName, certPEM(t, pairName, renewed, renewed.Add(90*24*time.Hour)))
	at(68)
	if got := told.all(); len(got) != 2 {
		t.Fatalf("told after the renewal: %q", got)
	}
}

func TestAnInstanceTriesWhetherAValidationWouldFindItWithAChallengeOfItsOwnMaking(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	disk := autocert.DirCache(dir)
	pair := pairIn(t, dir, "https://peer.example.com", nil)
	key := pairName + challengeKeySuffix
	untouched := func(what string) {
		t.Helper()
		if _, err := disk.Get(ctx, key); !errors.Is(err, autocert.ErrCacheMiss) {
			t.Fatalf("%s: the probe was written where autocert keeps a pending order's challenge (%v)", what, err)
		}
		pair.mu.RLock()
		defer pair.mu.RUnlock()
		if len(pair.probes) != 0 {
			t.Fatalf("%s: the probe was left in memory", what)
		}
	}
	// handed to itself: its own challenge comes back, a certificate its peer would answer and no other
	if err := pair.reaches(ctx, pairName); err != nil {
		t.Fatalf("a validation that finds its challenge: %v", err)
	}
	untouched("found")
	var probed []byte
	pair.validation = func(ctx context.Context, host string) ([]byte, error) {
		pair.mu.RLock()
		probed = pair.probes[host].data
		pair.mu.RUnlock()
		if _, err := challengeFor(host, probed); err != nil {
			t.Fatalf("the probe is not a challenge certificate its peer would take: %v", err)
		}
		return leafOf(probed), nil
	}
	if err := pair.reaches(ctx, pairName); err != nil || probed == nil {
		t.Fatalf("the probe: %v", err)
	}
	// answered by a machine that does not know its challenge (a relay that hands it to nobody of the pair)
	pair.validation = func(context.Context, string) ([]byte, error) {
		return leafOf(challengePEM(t, pairName, "another")), nil
	}
	if err := pair.reaches(ctx, pairName); err == nil {
		t.Fatal("another machine's answer was taken for its own")
	}
	untouched("another machine")
	pair.validation = func(context.Context, string) ([]byte, error) { return nil, errors.New("handshake failure") }
	if err := pair.reaches(ctx, pairName); err == nil {
		t.Fatal("a name that answers no validation was taken as reached")
	}
	untouched("no answer")
	// an order of its own is pending: its challenge answers first and is neither replaced nor removed
	pending := challengePEM(t, pairName, "pending")
	_ = disk.Put(ctx, key, pending)
	pair.validation = handedTo(pair)
	if err := pair.reaches(ctx, pairName); err == nil {
		t.Fatal("the pending order's challenge came back and was taken for the probe")
	}
	if got, _ := disk.Get(ctx, key); !bytes.Equal(got, pending) {
		t.Fatal("the pending order's challenge was touched")
	}
}

func TestAnInstanceTheRelayDoesNotKnowYetWaitsWithoutAskingAndTellsOnlyWhenItLasts(t *testing.T) {
	dir := t.TempDir()
	told := &alerts{}
	pair := pairIn(t, dir, "https://peer.example.com", told.notify)
	pair.pace.check, pair.pace.probe, pair.pace.ask, pair.pace.patience, pair.pace.alert, pair.pace.verify = 5*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond, 250*time.Millisecond, time.Hour, time.Hour
	var asks, probes atomic.Int64
	var known atomic.Bool
	itself := handedTo(pair)
	pair.validation = func(ctx context.Context, host string) ([]byte, error) {
		probes.Add(1)
		if !known.Load() {
			return nil, errors.New("handshake failure")
		}
		return itself(ctx, host)
	}
	pair.obtain = func(host string, rsa bool) error {
		asks.Add(1)
		now := time.Now()
		return autocert.DirCache(dir).Put(context.Background(), pairName, certPEM(t, pairName, now.Add(-time.Minute), now.Add(90*24*time.Hour)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pair.Start(ctx)
	time.Sleep(150 * time.Millisecond)
	if asks.Load() != 0 {
		t.Fatalf("asked Let's Encrypt %d times although no validation would find it", asks.Load())
	}
	if got := probes.Load(); got < 3 || got > 9 {
		t.Fatalf("tried %d times in 150 ms at one every 20 ms", got)
	}
	if got := told.all(); len(got) != 0 {
		t.Fatalf("a new instance waiting for the relay is no trouble yet: %q", got)
	}
	time.Sleep(200 * time.Millisecond)
	got := told.all()
	if len(got) != 1 || !strings.Contains(got[0], "las validaciones del nombre no le llegan") || !strings.Contains(got[0], "👉") {
		t.Fatalf("told once when it lasts: %q", got)
	}
	if asks.Load() != 0 {
		t.Fatal("asked while unreachable")
	}
	// the relay knows it now
	known.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for !pair.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !pair.Ready() || asks.Load() != 1 {
		t.Fatalf("ready %v after %d asks", pair.Ready(), asks.Load())
	}
	// the instance is ready a moment before it tells so
	for len(told.all()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := told.all(); len(got) != 2 || !strings.Contains(got[1], "ya tiene su certificado") {
		t.Fatalf("the end of the trouble is told: %q", got)
	}
}

func TestAnInstanceThatHoldsItsCertificateGoesOnTryingThatItsRenewalCouldBeValidated(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	_ = autocert.DirCache(dir).Put(context.Background(), pairName, certPEM(t, pairName, now.Add(-time.Minute), now.Add(60*24*time.Hour)))
	told := &alerts{}
	pair := pairIn(t, dir, "https://peer.example.com", told.notify)
	pair.pace.check, pair.pace.verify, pair.pace.patience, pair.pace.alert = 5*time.Millisecond, 20*time.Millisecond, 150*time.Millisecond, time.Hour
	var reachable atomic.Bool
	var probes, asks atomic.Int64
	reachable.Store(true)
	itself := handedTo(pair)
	pair.validation = func(ctx context.Context, host string) ([]byte, error) {
		probes.Add(1)
		if !reachable.Load() {
			return nil, errors.New("handshake failure")
		}
		return itself(ctx, host)
	}
	pair.obtain = func(string, bool) error { asks.Add(1); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pair.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	if got := probes.Load(); got < 3 || got > 9 {
		t.Fatalf("tried %d times in 120 ms at one every 20 ms", got)
	}
	if got := told.all(); len(got) != 0 {
		t.Fatalf("nothing to tell while validations reach it: %q", got)
	}
	if at, ok := pair.Detail()["certificates"].(map[string]any)[pairName].(map[string]any)["validation_reached_at"]; !ok || at == "" {
		t.Fatal("the operator is not told when a validation last reached it")
	}
	// the relay, or the peer, stops answering for this instance: it serves on, and tells when it lasts
	reachable.Store(false)
	time.Sleep(300 * time.Millisecond)
	got := told.all()
	if len(got) != 1 || !strings.Contains(got[0], "no se podrá renovar") || !strings.Contains(got[0], "caduca en 59 días") || !strings.Contains(got[0], "👉") {
		t.Fatalf("told once that the renewal could not be validated: %q", got)
	}
	if !pair.Ready() {
		t.Fatal("an instance whose certificate is valid stopped being ready")
	}
	reachable.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for len(told.all()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := told.all(); len(got) != 2 || !strings.Contains(got[1], "vuelven a llegar") {
		t.Fatalf("the end of the trouble is told: %q", got)
	}
	if asks.Load() == 0 {
		t.Fatal("the certificate was never loaded for its renewal")
	}
}

func TestOverRealTLSAnInstanceFindsItsOwnProbeThroughItsPeer(t *testing.T) {
	ctx := context.Background()
	// the new instance: its plaintext listener is where the first looks its challenges up
	second := pairIn(t, t.TempDir(), "http://127.0.0.1:1", nil)
	plain, lookups := plainOf(t, second)
	// the instance the relay hands validations to: it holds nothing for the name, and its peer is elsewhere
	first := pairIn(t, t.TempDir(), "http://127.0.0.1:1", nil)
	addr := listening(t, first.TLSConfig())
	second.validation = func(ctx context.Context, host string) ([]byte, error) { return validationAt(ctx, addr, host) }
	if err := second.reaches(ctx, pairName); err == nil {
		t.Fatal("the probe came back from an instance that does not know this one")
	}
	// now the first knows the second
	first.peer = peerAt(t, plain.URL, pairSecret)
	first.peer.missFor = 0
	if err := second.reaches(ctx, pairName); err != nil {
		t.Fatalf("the probe did not come back through the peer: %v", err)
	}
	if lookups.Load() != 1 {
		t.Fatalf("%d lookups for one probe", lookups.Load())
	}
	// with another secret between them the probe is not taken
	first.peer = peerAt(t, plain.URL, "another pair's secret, of another deployment")
	first.peer.missFor = 0
	if err := second.reaches(ctx, pairName); err == nil {
		t.Fatal("the probe came back through a peer that does not share the pair's secret")
	}
}

func TestAnInstanceThatDoesNotKnowItsPeerSaysSoAndServesOn(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	_ = autocert.DirCache(dir).Put(context.Background(), pairName, certPEM(t, pairName, now.Add(-time.Minute), now.Add(60*24*time.Hour)))
	told := &alerts{}
	pair := pairIn(t, dir, "", told.notify)
	pair.pace.check, pair.pace.alert = 5*time.Millisecond, time.Hour
	pair.obtain = func(string, bool) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pair.Start(ctx)
	time.Sleep(60 * time.Millisecond)
	if !pair.Ready() {
		t.Fatal("an instance with its certificate is not ready because it does not know its peer")
	}
	got := told.all()
	if len(got) != 1 || !strings.Contains(got[0], "TLS_ACME_PEER") || !strings.Contains(got[0], "👉") {
		t.Fatalf("the operator is told once: %q", got)
	}
	if pair.Detail()["paired"] != false {
		t.Fatal("the status says it is paired")
	}
	if _, err := pair.GetCertificate(&tls.ClientHelloInfo{ServerName: pairName, SupportedProtos: []string{acme.ALPNProto}}); err == nil {
		t.Fatal("a validation was answered with nothing to answer it with")
	}
}

func TestTheAccountsAddressIsLearnedAtStartAndToldToTheOperator(t *testing.T) {
	dir := t.TempDir()
	pair := pairIn(t, dir, "https://peer.example.com", nil)
	pair.pace.check, pair.pace.account = 5*time.Millisecond, 40*time.Millisecond
	pair.validation = func(context.Context, string) ([]byte, error) { return nil, errors.New("not yet") }
	var tries atomic.Int64
	pair.register = func(context.Context) (string, error) {
		if tries.Add(1) < 3 {
			return "", errors.New("the authority does not answer")
		}
		return "https://acme.example/acct/42", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pair.Start(ctx)
	time.Sleep(60 * time.Millisecond)
	if _, known := pair.Detail()["acme_account"]; known || tries.Load() > 2 {
		t.Fatalf("known %v after %d tries in 60 ms at one every 40 ms", known, tries.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for pair.Detail()["acme_account"] == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := pair.Detail()["acme_account"]; got != "https://acme.example/acct/42" {
		t.Fatalf("the account's address: %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if tries.Load() != 3 {
		t.Fatalf("asked %d times: once known it is not asked again", tries.Load())
	}
	// the key is autocert's own file, in autocert's form, and the same one after a restart
	first, err := os.ReadFile(filepath.Join(dir, accountKeyName))
	if err != nil || !bytes.Contains(first, []byte("EC PRIVATE KEY")) {
		t.Fatalf("the account key on disk: %v", err)
	}
	again := pairIn(t, dir, "https://peer.example.com", nil)
	second, _ := os.ReadFile(filepath.Join(dir, accountKeyName))
	if !bytes.Equal(first, second) {
		t.Fatal("a restart made another account key")
	}
	if again.manager.Client.Key.Public().(*ecdsa.PublicKey).X.Cmp(pair.manager.Client.Key.Public().(*ecdsa.PublicKey).X) != 0 {
		t.Fatal("autocert is not given the account key that is on disk")
	}
}

func TestTheServedNamesAreThoseCustomersCall(t *testing.T) {
	hosts := []string{"a.example.com", "b.example.com"}
	if got := servedNames(hosts, nil); len(got) != 2 {
		t.Fatalf("no direct names: every name is served, got %v", got)
	}
	if got := servedNames(hosts, []string{"b.example.com", "c.example.com"}); len(got) != 1 || got[0] != "b.example.com" {
		t.Fatalf("got %v", got)
	}
	if got := servedNames(hosts, []string{"c.example.com"}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestATryThatFailsIsMadeOnceMoreSoTwoInstancesTryingAtOnceDoNotFailEachOther(t *testing.T) {
	// The peer is trying at the same moment: a validation of the name is answered with ITS probe, so
	// this instance's first try sees a challenge that is not its own. A moment later the peer is done.
	pair := pairIn(t, t.TempDir(), "https://peer.example.com", nil)
	pair.pace.again = 5 * time.Millisecond
	itself := handedTo(pair)
	var tries atomic.Int64
	pair.validation = func(ctx context.Context, host string) ([]byte, error) {
		if tries.Add(1) == 1 {
			return []byte("the peer's own probe"), nil
		}
		return itself(ctx, host)
	}
	started := time.Now()
	if err := pair.reachesTwice(context.Background(), pairName); err != nil {
		t.Fatalf("the second try: %v", err)
	}
	if waited := time.Since(started); tries.Load() != 2 || waited < 5*time.Millisecond || waited > time.Second {
		t.Fatalf("%d tries in %s: one more, a moment later", tries.Load(), waited)
	}
	// a name that does not answer fails after the two, and no more
	tries.Store(0)
	pair.validation = func(context.Context, string) ([]byte, error) {
		tries.Add(1)
		return nil, errors.New("handshake failure")
	}
	if err := pair.reachesTwice(context.Background(), pairName); err == nil || tries.Load() != 2 {
		t.Fatalf("a name that does not answer: %v after %d tries", err, tries.Load())
	}
	// without a second try configured there is none
	pair.pace.again = 0
	tries.Store(0)
	if err := pair.reachesTwice(context.Background(), pairName); err == nil || tries.Load() != 1 {
		t.Fatalf("no second try configured: %v after %d tries", err, tries.Load())
	}
	// and the wait ends with the instance
	pair.pace.again = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	started = time.Now()
	if err := pair.reachesTwice(ctx, pairName); err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("a stopping instance waited %s for its second try (%v)", time.Since(started), err)
	}
	// the two of a pair do not try at the same moments: each adds its own part of a sixth to the hour
	if pair.skew < 0 || pair.skew >= 1 {
		t.Fatalf("skew %v", pair.skew)
	}
	if defaultPace.again <= 0 || defaultPace.again > 5*time.Second {
		t.Fatalf("the second try of a real instance comes after %s", defaultPace.again)
	}
}

func TestWhyThePeerCouldNotBeAskedIsToldOnTheSwitch(t *testing.T) {
	// the peer answers with a challenge signed with another secret: the two were not handed the same one
	other, err := pairKey("another secret, as long as it has to be, but not the pair's")
	if err != nil {
		t.Fatal(err)
	}
	var answer atomic.Value
	answer.Store("wrong")
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answer.Load() == "none" {
			http.Error(w, "no challenge for that name", http.StatusNotFound)
			return
		}
		data := challengePEM(t, pairName, "a-token-of-the-test")
		_ = json.NewEncoder(w).Encode(challengeAnswer{Version: acmeChallengeVersion, Name: pairName, PEM: base64.StdEncoding.EncodeToString(data), MAC: hex.EncodeToString(signChallenge(other, pairName, data))})
	}))
	defer peerServer.Close()
	pair := pairIn(t, t.TempDir(), "https://peer.example.com", nil)
	pair.peer.base, pair.peer.client = peerServer.URL, peerServer.Client()
	pair.peer.missFor = time.Millisecond
	if _, err := pair.peer.challenge(context.Background(), pairName); err == nil {
		t.Fatal("a challenge under another secret was taken")
	}
	detail := pair.Detail()
	if text, _ := detail["peer_last_error"].(string); text == "" || detail["peer_last_error_at"] == nil {
		t.Fatalf("the operator is not told why the peer could not be asked: %v", detail)
	}
	// a peer that answers properly, even that it has nothing, clears nothing by saying nothing; a good answer does
	time.Sleep(3 * time.Millisecond)
	answer.Store("none")
	_, _ = pair.peer.challenge(context.Background(), pairName)
	if _, still := pair.Detail()["peer_last_error"]; !still {
		t.Fatal("the last trouble was forgotten before the peer answered a challenge well")
	}
}

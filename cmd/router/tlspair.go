package main

// Two routers behind one name (a primary and its backup, the relay in front choosing by their
// health), each with ITS OWN certificate. Every instance obtains and renews its certificate from
// Let's Encrypt as a single router always did (autocert, the TLS-ALPN-01 challenge answered on its
// TLS listener), under its own ACME account, and no private key of the name ever leaves the VM that
// made it.
//
// What two instances behind one name need is that Let's Encrypt's validation connection, which the
// relay hands to either of them, finds the challenge of the instance that asked. So an instance
// asked for a challenge it does not hold looks it up at its peer (TLS_ACME_PEER, the peer's
// plaintext listener behind the gateway): the peer answers the challenge certificate of its own
// pending order, authenticated with the pair's secret (TLS_ACME_PEER_SECRET, the same on both), and
// this instance presents it. A challenge certificate is no secret of the name: an instance shows it
// to whoever connects during a validation, it holds a digest of the order's key authorization and a
// throwaway key of its own, and presenting it achieves one thing only, that the peer's order (for
// the peer's own account and the peer's own key) is validated. The pair's secret is handed in, not
// derived by the VM's KMS: two machines must hold the same one whenever each of them booted.
//
// A client is served the certificate this VM holds and nothing else: a client's handshake never
// asks Let's Encrypt for anything. An instance without a valid certificate for every name its
// customers call tells the relay so (relayhealth.go) and is sent nobody. It asks for a missing
// certificate itself, at most every 20 minutes (Let's Encrypt refuses a name after five failed
// validations in an hour), and only once it has seen that a validation of the name would find it:
// it calls the name as Let's Encrypt would, with a challenge of its own making that Let's Encrypt
// knows nothing of, and asks only when that challenge is what comes back. An instance the relay
// does not hand validations to yet (a new one, before the relay knows it) waits without spending a
// failed order. An instance that holds its certificate goes on trying the same once an hour, so a
// renewal that could not be validated is known weeks before the certificate ends.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

const (
	acmeChallengePath    = "/.well-known/lebrel-acme-challenge"
	acmeChallengeVersion = 1
	// The most a peer's answer may be: one throwaway key and one small certificate, in base64.
	acmeChallengeMaxBytes = 32 * 1024
	// autocert keeps a pending order's challenge certificate in its cache under the name and this
	// suffix, an RSA certificate under the name and "+rsa", and its account key under accountKeyName.
	challengeKeySuffix = "+token"
	rsaKeySuffix       = "+rsa"
	accountKeyName     = "acme_account+key"
	// An instance stops calling itself ready this long before its certificate ends: the relay moves
	// its customers while it still serves them.
	readyMargin = 10 * time.Minute
	// A peer that had no challenge is not asked again for this long; a validation that comes
	// meanwhile waits for it to pass.
	peerLookupTimeout = 4 * time.Second
	peerMissFor       = time.Second
)

// pace is how often an instance of a pair does each thing; a test runs them in milliseconds.
type pace struct {
	ask        time.Duration // an instance without a certificate asks Let's Encrypt at most this often, per name
	check      time.Duration // how often it looks at what it holds
	warm       time.Duration // how often it loads its certificates into autocert (which schedules their renewal)
	alert      time.Duration // the same trouble is told again at most this often
	askTimeout time.Duration // a little more than autocert's own five minutes, so its reason is what is told
	probe      time.Duration // without a certificate: how often it tries whether a validation would reach it
	verify     time.Duration // with one: how often it tries the same, for the renewal to come
	patience   time.Duration // how long validations may not reach it before the operator is told
	account    time.Duration // how often it tries to learn its ACME account's address until it has it
	again      time.Duration // a try that fails is made once more after this and up to 2.5 times as much again (0: it is not)
}

var defaultPace = pace{ask: 20 * time.Minute, check: 30 * time.Second, warm: 12 * time.Hour, alert: 6 * time.Hour, askTimeout: 5*time.Minute + 30*time.Second,
	probe: time.Minute, verify: time.Hour, patience: 2 * time.Hour, account: 10 * time.Minute, again: 2 * time.Second}

// peer is the other instance of the pair, as this one asks it.
type peer struct {
	base    string // its plaintext base URL
	key     []byte // authenticates a challenge between the two
	client  *http.Client
	logger  *log.Logger
	now     func() time.Time
	missFor time.Duration

	mu        sync.Mutex
	missed    map[string]time.Time // by name: when it last had no challenge
	flights   map[string]*peerLookup
	logged    time.Time // the last failed lookup written to the log
	trouble   string    // why the last lookup failed, when it was not for the peer having no challenge
	troubleAt time.Time
}

// peerLookup is one lookup in flight; every validation asking for the same name waits for it.
type peerLookup struct {
	done chan struct{}
	cert *tls.Certificate
	err  error
}

// errNoChallenge is the peer's ordinary answer: it has no order pending for the name.
var errNoChallenge = errors.New("the peer holds no challenge for the name")

// peerBase checks the peer's base URL: https to a named host, nothing else in it (http only to this
// machine, for development and tests).
func peerBase(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("TLS_ACME_PEER must be the peer's base URL (its plaintext listener behind the gateway): https://host, nothing after it")
	}
	host := parsed.Hostname()
	loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return "", errors.New("TLS_ACME_PEER must use https")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// pairKey is the key the two instances authenticate each other's challenges with, from the secret
// both were handed.
func pairKey(secret string) ([]byte, error) {
	secret = strings.TrimSpace(secret)
	if len(secret) < 32 {
		return nil, errors.New("TLS_ACME_PEER_SECRET must be at least 32 characters, the same on both instances of the pair")
	}
	sum := sha256.Sum256([]byte("lebrel-router acme-pair v1\n" + secret))
	return sum[:], nil
}

func newPeer(base string, key []byte, logger *log.Logger) (*peer, error) {
	base, err := peerBase(base)
	if err != nil {
		return nil, err
	}
	if len(key) < 32 {
		return nil, errors.New("the key between the instances is shorter than 32 bytes")
	}
	return &peer{
		base: base, key: append([]byte(nil), key[:32]...), logger: logger, now: time.Now, missFor: peerMissFor,
		missed: map[string]time.Time{}, flights: map[string]*peerLookup{},
		client: &http.Client{
			Timeout:       peerLookupTimeout,
			Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: peerLookupTimeout},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// challenge is the peer's challenge certificate for name. One lookup at a time per name, shared by
// every validation that asks meanwhile; a peer that had none is left alone for a moment, and a
// validation that comes in that moment waits for it to pass and asks then, so nobody opening
// connections can make this instance call its peer without end, nor make it answer a validation
// with what the peer said before its order began.
func (p *peer) challenge(ctx context.Context, name string) (*tls.Certificate, error) {
	for again := false; ; again = true {
		p.mu.Lock()
		var wait time.Duration
		if at, ok := p.missed[name]; ok {
			wait = p.missFor - p.now().Sub(at)
		}
		flight, joined := p.flights[name], true
		if flight == nil && wait <= 0 {
			flight, joined = &peerLookup{done: make(chan struct{})}, false
			p.flights[name] = flight
			go p.fly(flight, name)
		}
		p.mu.Unlock()
		if flight == nil {
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		select {
		case <-flight.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if flight.err == nil {
			return flight.cert, nil
		}
		// A lookup this validation only joined may have begun before the peer's order did: it asks once more itself.
		if joined && !again {
			continue
		}
		return nil, flight.err
	}
}

// fly runs one lookup; it outlives the validation that started it, and the others waiting get its answer.
func (p *peer) fly(flight *peerLookup, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), peerLookupTimeout)
	cert, err := p.lookup(ctx, name)
	cancel()
	p.mu.Lock()
	flight.cert, flight.err = cert, err
	delete(p.flights, name)
	switch {
	case err == nil:
		delete(p.missed, name)
		p.trouble, p.troubleAt = "", time.Time{}
	case errors.Is(err, errNoChallenge):
		p.missed[name] = p.now()
	default:
		p.missed[name] = p.now()
		// kept for the status: a peer that does not answer, or a secret that is not the same on both,
		// shows here at once and not hours later as validations that do not arrive
		p.trouble, p.troubleAt = brief(err), p.now()
		if p.now().Sub(p.logged) >= time.Minute {
			p.logged = p.now()
			p.logger.Printf("tls pair: the peer's challenge for %s could not be read: %v", name, err)
		}
	}
	p.mu.Unlock()
	close(flight.done)
}

// lastTrouble is why the last lookup at the peer failed, and when; "" when it did not.
func (p *peer) lastTrouble() (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.trouble, p.troubleAt
}

type challengeAnswer struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	PEM     string `json:"pem"`
	MAC     string `json:"mac"`
}

// lookup asks the peer once. What it answers is taken only if it carries the pair's secret and is a
// challenge certificate for the name and nothing else.
func (p *peer) lookup(ctx context.Context, name string) (*tls.Certificate, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+acmeChallengePath+"?name="+url.QueryEscape(name), nil)
	if err != nil {
		return nil, err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, errNoChallenge
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("the peer answered %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, acmeChallengeMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > acmeChallengeMaxBytes {
		return nil, errors.New("the peer's answer is too large")
	}
	var answer challengeAnswer
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil {
		return nil, fmt.Errorf("the peer's answer is not a challenge: %w", err)
	}
	if answer.Version != acmeChallengeVersion || answer.Name != name {
		return nil, errors.New("the peer's answer is for another name or another version")
	}
	data, err := base64.StdEncoding.DecodeString(answer.PEM)
	if err != nil {
		return nil, errors.New("the peer's answer is not base64")
	}
	mac, err := hex.DecodeString(answer.MAC)
	if err != nil || !hmac.Equal(mac, signChallenge(p.key, name, data)) {
		return nil, errors.New("the peer's answer does not carry the pair's secret")
	}
	return challengeFor(name, data)
}

// takes says whether the peer may be sent customers now, and why not.
func (p *peer) takes(ctx context.Context) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, peerLookupTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+relayHealthPath, nil)
	if err != nil {
		return false, err.Error()
	}
	response, err := p.client.Do(request)
	if err != nil {
		return false, "it does not answer"
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	switch response.StatusCode {
	case http.StatusOK, http.StatusNotFound: // 404: a release before the check, which always took them
		return true, ""
	case http.StatusServiceUnavailable:
		return false, "it is stepped aside"
	case http.StatusGone:
		return false, "it is stopping"
	case http.StatusConflict:
		return false, "it holds no certificate"
	}
	return false, fmt.Sprintf("it answers %d", response.StatusCode)
}

// signChallenge authenticates a challenge certificate for a name with the pair's key; the name and
// the format version are bound in.
func signChallenge(key []byte, name string, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "lebrel-router acme-challenge v%d\n%s\n", acmeChallengeVersion, name)
	mac.Write(data)
	return mac.Sum(nil)
}

// challengeFor reads data (a key and one certificate, in autocert's cache form) as a TLS-ALPN-01
// challenge certificate for name, and refuses anything else: one certificate, for that name alone,
// carrying the ACME identifier. A certificate a client would trust for the name is never one.
func challengeFor(name string, data []byte) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, fmt.Errorf("not a key and its certificate: %w", err)
	}
	if len(pair.Certificate) != 1 {
		return nil, errors.New("a chain, not a challenge certificate")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if len(leaf.DNSNames) != 1 || !strings.EqualFold(leaf.DNSNames[0], name) {
		return nil, errors.New("the challenge certificate is not for the name alone")
	}
	for _, extension := range leaf.Extensions {
		if extension.Id.Equal(acmeIdentifier) && extension.Critical {
			pair.Leaf = leaf
			return &pair, nil
		}
	}
	return nil, errors.New("no ACME identifier: not a challenge certificate")
}

// acmeIdentifier is the certificate extension of a TLS-ALPN-01 challenge (RFC 8737, id-pe-acmeIdentifier).
var acmeIdentifier = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 31}

// held is what this VM holds for a name: its certificate (the one autocert obtains, on an ECDSA
// key), and an RSA one where an earlier release obtained it for a client that takes no other.
type held struct {
	cert, rsa           *tls.Certificate
	notBefore, notAfter time.Time
}

// probe is a challenge of this instance's own making, held only while it tries whether a
// validation would reach it.
type probe struct {
	cert *tls.Certificate
	data []byte // the same in autocert's cache form, as the peer is answered
}

// tlsPair is one instance's part of a pair: its own certificates, and its peer's challenges.
type tlsPair struct {
	hosts   []string // every name it may hold a certificate for (TLS_ACME_HOSTS)
	served  []string // the names its customers call: no customer until each has a valid certificate
	disk    autocert.Cache
	manager *autocert.Manager
	peer    *peer  // nil: the instance does not know its peer
	key     []byte // authenticates this instance's challenges to its peer
	logger  *log.Logger
	notify  func(string)
	now     func() time.Time
	pace    pace
	// obtain makes autocert hold host's certificate in memory: it loads the one on disk, which
	// schedules its renewal, or, when there is none, asks Let's Encrypt for it (an order).
	obtain func(host string, rsa bool) error
	// validation calls host as Let's Encrypt's validation would (TLS to its port 443, the ACME
	// protocol alone) and returns the certificate presented.
	validation func(ctx context.Context, host string) ([]byte, error)
	// register tells the address of this instance's ACME account, making the account if there is none.
	register func(ctx context.Context) (string, error)

	mu     sync.RWMutex
	certs  map[string]held
	probes map[string]probe
	ready  atomic.Bool
	// what the operator is told, kept for the status
	account  atomic.Value // string: the ACME account's address
	reached  map[string]time.Time
	lastLook error

	// the loop's own
	asked        map[string]time.Time
	probed       map[string]time.Time
	unreached    map[string]time.Time // by name: since when validations do not reach this instance
	alerted      map[string]time.Time
	accountTried time.Time
	told         bool    // the log has said whether it is ready
	skew         float64 // 0 to 1: how much of a sixth of pace.verify this instance adds to it, so the two of a pair do not try at the same moments
}

// newTLSPair builds an instance's part. served are the names customers call (a subset of hosts);
// peerURL and secret name the other instance and what the two authenticate with (both empty: the
// instance does not know its peer, and says so); authority is a rehearsal's own ACME client, nil
// for Let's Encrypt; notify tells the operator.
func newTLSPair(hosts, served []string, cacheDir, email, peerURL, secret string, authority *acme.Client, logger *log.Logger, notify func(string)) (*tlsPair, error) {
	if len(hosts) == 0 {
		return nil, errors.New("a pair needs TLS_ACME_HOSTS: the names this instance obtains certificates for")
	}
	if _, err := acmeTLS(hosts, cacheDir, email); err != nil {
		return nil, err
	}
	if len(served) == 0 {
		return nil, errors.New("a pair: none of the names customers call (DIRECT_HOSTS) is in TLS_ACME_HOSTS")
	}
	if notify == nil {
		notify = func(string) {}
	}
	disk := autocert.DirCache(cacheDir)
	pair := &tlsPair{
		hosts: hosts, served: served, disk: disk, logger: logger, notify: notify, now: time.Now, pace: defaultPace,
		certs: map[string]held{}, probes: map[string]probe{}, reached: map[string]time.Time{},
		asked: map[string]time.Time{}, probed: map[string]time.Time{}, unreached: map[string]time.Time{}, alerted: map[string]time.Time{},
		skew: mrand.Float64(),
	}
	if strings.TrimSpace(peerURL) != "" {
		key, err := pairKey(secret)
		if err != nil {
			return nil, err
		}
		if pair.peer, err = newPeer(peerURL, key, logger); err != nil {
			return nil, err
		}
		pair.key = key
	}
	// The ACME account's key is autocert's own file; it is read (or made) here so that the account's
	// address can be told before the first order.
	accountKey, err := accountKeyOn(disk)
	if err != nil {
		return nil, fmt.Errorf("the ACME account key: %w", err)
	}
	client := authority
	if client == nil {
		client = &acme.Client{DirectoryURL: autocert.DefaultACMEDirectory}
	}
	client.Key = accountKey
	if client.UserAgent == "" {
		client.UserAgent = "autocert" // what autocert would write at its first use, while this instance may be reading it
	}
	pair.manager = &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(hosts...), Cache: disk, Email: email, Client: client}
	pair.obtain = func(host string, rsa bool) error {
		hello := ecdsaHello(host)
		if rsa {
			hello = rsaHello(host)
		}
		_, err := pair.manager.GetCertificate(hello)
		return err
	}
	pair.validation = func(ctx context.Context, host string) ([]byte, error) {
		return validationAt(ctx, net.JoinHostPort(host, "443"), host)
	}
	pair.register = func(ctx context.Context) (string, error) {
		var contact []string
		if email != "" {
			contact = []string{"mailto:" + email}
		}
		account, err := client.GetReg(ctx, "")
		if errors.Is(err, acme.ErrNoAccount) {
			account, err = client.Register(ctx, &acme.Account{Contact: contact}, acme.AcceptTOS)
		}
		if err != nil {
			return "", err
		}
		return account.URI, nil
	}
	return pair, nil
}

// accountKeyOn is the ACME account key autocert keeps on disk, made as autocert makes it when there
// is none yet.
func accountKeyOn(disk autocert.Cache) (crypto.Signer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := disk.Get(ctx, accountKeyName)
	if errors.Is(err, autocert.ErrCacheMiss) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := disk.Put(ctx, accountKeyName, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || !strings.Contains(block.Type, "PRIVATE") {
		return nil, errors.New("not a private key")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, errors.New("the key cannot sign")
	}
	return signer, nil
}

// validationAt calls host at addr as Let's Encrypt's TLS-ALPN-01 validation does and returns the
// certificate it is shown (a challenge certificate is self-signed: it is compared, never trusted).
func validationAt(ctx context.Context, addr, host string) ([]byte, error) {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{ServerName: host, NextProtos: []string{acme.ALPNProto}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return nil, errors.New("no certificate was presented")
	}
	return state.PeerCertificates[0].Raw, nil
}

// reaches says whether a validation of host would find a challenge of this instance. It makes a
// challenge of its own (one Let's Encrypt knows nothing of, so nothing can be validated with it),
// holds it in memory, calls the name as Let's Encrypt would, and looks at what comes back: its own
// challenge when the relay hands the connection to this instance, or to its peer, which looks it up
// here. An order of this instance that is pending answers first, with its own challenge: then the
// probe is not what comes back, and it is tried again later.
func (p *tlsPair) reaches(ctx context.Context, host string) error {
	account, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	token := make([]byte, 24)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	made, err := (&acme.Client{Key: account}).TLSALPN01ChallengeCert(base64.RawURLEncoding.EncodeToString(token), host)
	if err != nil {
		return err
	}
	signer, ok := made.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || len(made.Certificate) != 1 {
		return errors.New("the probe is not a challenge certificate")
	}
	keyDER, err := x509.MarshalECPrivateKey(signer)
	if err != nil {
		return err
	}
	var data bytes.Buffer
	_ = pem.Encode(&data, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	_ = pem.Encode(&data, &pem.Block{Type: "CERTIFICATE", Bytes: made.Certificate[0]})
	p.mu.Lock()
	p.probes[host] = probe{cert: &made, data: data.Bytes()}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.probes, host)
		p.mu.Unlock()
	}()
	presented, err := p.validation(ctx, host)
	if err != nil {
		return fmt.Errorf("the name did not answer a validation: %w", err)
	}
	if !bytes.Equal(presented, made.Certificate[0]) {
		return errors.New("a validation of the name is answered with a challenge that is not this instance's")
	}
	return nil
}

// reachesTwice is reaches, tried once more a few seconds later when it fails. The peer may have
// been trying the same at that moment: each answers a validation of the name with its own probe
// first, so each would see the other's; without the second try, two instances whose clocks agree
// would find each other failing every time.
func (p *tlsPair) reachesTwice(ctx context.Context, host string) error {
	err := p.reaches(ctx, host)
	if err == nil || p.pace.again <= 0 {
		return err
	}
	select {
	case <-time.After(p.pace.again + time.Duration(mrand.Int63n(int64(5*p.pace.again/2)+1))):
	case <-ctx.Done():
		return err
	}
	return p.reaches(ctx, host)
}

// servedNames are the names of hosts that customers call: those DIRECT_HOSTS lists, or all of them
// when it lists none.
func servedNames(hosts, direct []string) []string {
	if len(direct) == 0 {
		return hosts
	}
	var served []string
	for _, host := range hosts {
		for _, name := range direct {
			if host == name {
				served = append(served, host)
				break
			}
		}
	}
	return served
}

// Ready says whether this instance holds a certificate for every name its customers call that is
// valid now and for a while yet.
func (p *tlsPair) Ready() bool { return p.ready.Load() }

// Serving says whether it holds one that is valid now, be it for minutes: it can still be sent
// customers when nobody else can.
func (p *tlsPair) Serving() bool {
	for _, host := range p.served {
		if !p.holds(host) {
			return false
		}
	}
	return true
}

// holding is what this VM holds for name, if it is valid now.
func (p *tlsPair) holding(name string) (held, bool) {
	p.mu.RLock()
	found, ok := p.certs[name]
	p.mu.RUnlock()
	now := p.now()
	return found, ok && !now.Before(found.notBefore) && now.Before(found.notAfter)
}

func (p *tlsPair) holds(name string) bool {
	_, ok := p.holding(name)
	return ok
}

func (p *tlsPair) isHost(name string) bool {
	for _, host := range p.hosts {
		if host == name {
			return true
		}
	}
	return false
}

// GetCertificate is the TLS listener's source.
//
// A validation connection is answered with a challenge: this VM's own pending order's, this VM's
// own probe, or its peer's. Anyone else is served the certificate this VM holds for the name, and
// without one is refused: a client's handshake never reaches autocert, so it never starts an order.
func (p *tlsPair) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto {
		if !p.isHost(name) {
			return nil, fmt.Errorf("tls pair: no challenge for %q", name)
		}
		if cert, err := p.manager.GetCertificate(hello); err == nil {
			return cert, nil
		}
		p.mu.RLock()
		own, probing := p.probes[name]
		p.mu.RUnlock()
		if probing {
			return own.cert, nil
		}
		if p.peer == nil {
			return nil, fmt.Errorf("tls pair: no challenge for %q", name)
		}
		parent := hello.Context()
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, 2*peerLookupTimeout+2*peerMissFor)
		defer cancel()
		return p.peer.challenge(ctx, name)
	}
	found, ok := p.holding(name)
	if !ok {
		return nil, fmt.Errorf("tls pair: no certificate for %q on this instance", name)
	}
	// The certificate autocert obtains is on an ECDSA key, which every client of today takes; the
	// RSA one, where an earlier release left it, is for the client that takes nothing else.
	if found.rsa != nil && hello.SupportsCertificate(found.cert) != nil && hello.SupportsCertificate(found.rsa) == nil {
		return found.rsa, nil
	}
	return found.cert, nil
}

// TLSConfig is the TLS listener's configuration: autocert's protocols (the ACME challenge among
// them), with this instance's source.
func (p *tlsPair) TLSConfig() *tls.Config {
	config := p.manager.TLSConfig()
	config.GetCertificate = p.GetCertificate
	config.MinVersion = tls.VersionTLS12
	return config
}

// Handler adds the peer's lookup to the plaintext listener.
func (p *tlsPair) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == acmeChallengePath {
			p.serveChallenge(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveChallenge answers, to the peer that was handed a validation connection, the challenge of
// this VM for a name: its pending order's, or its own probe. Only what this VM made: it never asks
// further, and a certificate a client would trust is never answered.
func (p *tlsPair) serveChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("name")))
	if !p.isHost(name) || p.key == nil {
		http.Error(w, "no challenge for that name", http.StatusNotFound)
		return
	}
	data, err := p.disk.Get(r.Context(), name+challengeKeySuffix)
	if err == nil {
		_, err = challengeFor(name, data)
	}
	if err != nil {
		p.mu.RLock()
		own, probing := p.probes[name]
		p.mu.RUnlock()
		if !probing {
			http.Error(w, "no challenge for that name", http.StatusNotFound)
			return
		}
		data = own.data
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(challengeAnswer{
		Version: acmeChallengeVersion, Name: name, PEM: base64.StdEncoding.EncodeToString(data), MAC: hex.EncodeToString(signChallenge(p.key, name, data)),
	})
}

// usable parses a PEM of a private key and its chain and says whether it serves host now.
func usable(host string, data []byte, now time.Time) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return nil, err
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("the certificate for %s is not valid now (%s to %s)", host, leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	cert.Leaf = leaf
	return &cert, nil
}

// look reads what this VM holds for every name and says whether it is ready. A name the disk could
// not be read for keeps what was held: a disk that fails for a moment takes no certificate away.
func (p *tlsPair) look() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	now := p.now()
	p.mu.RLock()
	before := p.certs
	p.mu.RUnlock()
	found := make(map[string]held, len(p.hosts))
	var failed error
	for _, host := range p.hosts {
		data, err := p.disk.Get(ctx, host)
		switch {
		case err == nil:
			cert, err := usable(host, data, now)
			if err != nil {
				continue
			}
			entry := held{cert: cert, notBefore: cert.Leaf.NotBefore, notAfter: cert.Leaf.NotAfter}
			if data, err := p.disk.Get(ctx, host+rsaKeySuffix); err == nil {
				if rsa, err := usable(host, data, now); err == nil {
					entry.rsa = rsa
				}
			}
			found[host] = entry
		case errors.Is(err, autocert.ErrCacheMiss):
		default:
			failed = err
			if previous, ok := before[host]; ok {
				found[host] = previous
			}
		}
	}
	ready := true
	for _, host := range p.served {
		if entry, ok := found[host]; !ok || !now.Before(entry.notAfter.Add(-readyMargin)) {
			ready = false
		}
	}
	p.mu.Lock()
	p.certs = found
	if failed != nil && p.lastLook == nil {
		p.logger.Printf("tls pair: the certificates on disk could not be read, what was held is kept: %v", failed)
	}
	p.lastLook = failed
	p.mu.Unlock()
	if was := p.ready.Swap(ready); was != ready || !p.told {
		p.told = true
		if ready {
			p.logger.Printf("tls pair: ready, holding its own certificate for %s", strings.Join(p.served, ", "))
		} else {
			p.logger.Printf("tls pair: NOT ready: no certificate valid for a while yet for every name of %s", strings.Join(p.served, ", "))
		}
	}
	return ready
}

// Start clears what a process that died left behind (a challenge of an order nobody is waiting
// for any more would be shown to a validation of the peer's), looks at what is on disk (an
// instance that restarts with its certificate is ready before its listeners open) and starts the
// instance's own work. It returns at once; the work stops with ctx.
func (p *tlsPair) Start(ctx context.Context) {
	clearing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	for _, host := range p.hosts {
		_ = p.disk.Delete(clearing, host+challengeKeySuffix)
	}
	cancel()
	p.look()
	if p.peer == nil {
		p.logger.Printf("tls pair: this instance does not know its peer (TLS_ACME_PEER is empty)")
	}
	go p.loop(ctx)
}

func (p *tlsPair) loop(ctx context.Context) {
	var warmed time.Time
	tick := time.NewTicker(p.pace.check)
	defer tick.Stop()
	for {
		p.look()
		p.learnAccount(ctx)
		if p.peer == nil {
			p.alert("unpaired", "este router forma parte de una pareja pero no sabe cuál es el otro (falta TLS_ACME_PEER): si las validaciones del nombre le llegan al otro, su certificado no se podrá renovar. 👉 Pásale la dirección de su pareja.")
		}
		for _, host := range p.served {
			if ctx.Err() != nil {
				return
			}
			if entry, ok := p.holding(host); ok {
				p.verify(ctx, host, entry)
				continue
			}
			if last, ok := p.asked[host]; ok && p.now().Sub(last) < p.pace.ask {
				continue
			}
			if last, ok := p.probed[host]; ok && p.now().Sub(last) < p.pace.probe {
				continue
			}
			p.probed[host] = p.now()
			if err := p.reachesTwice(ctx, host); err != nil {
				p.unreachable(host, err, nil)
				continue
			}
			p.reachedNow(host)
			p.asked[host] = p.now()
			p.ask(ctx, host)
		}
		// autocert renews a certificate only once it has loaded it (a handshake did that on a single
		// router): the instance loads its certificates here, at boot and twice a day, so their
		// renewal is always scheduled, whoever it serves.
		if warmed.IsZero() || p.now().Sub(warmed) >= p.pace.warm {
			warmed = p.now()
			p.warm(ctx)
		}
		p.watch()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// verify tries, for a name whose certificate is held, whether a validation would still find this
// instance: the renewal weeks from now depends on it.
func (p *tlsPair) verify(ctx context.Context, host string, entry held) {
	if last, ok := p.probed[host]; ok && p.now().Sub(last) < p.pace.verify+time.Duration(p.skew*float64(p.pace.verify/6)) {
		return
	}
	p.probed[host] = p.now()
	if err := p.reachesTwice(ctx, host); err != nil {
		p.unreachable(host, err, &entry)
		return
	}
	p.reachedNow(host)
}

func (p *tlsPair) reachedNow(host string) {
	delete(p.unreached, host)
	p.mu.Lock()
	p.reached[host] = p.now()
	p.mu.Unlock()
	if _, told := p.alerted["reach "+host]; told && p.holds(host) {
		delete(p.alerted, "reach "+host)
		p.notify(fmt.Sprintf("las validaciones de %s vuelven a llegar a este router: su certificado se podrá renovar.", host))
	}
}

// unreachable notes that a validation of host would not find this instance. Without a certificate
// nothing is asked of Let's Encrypt (the order would fail and count against the name); with one,
// its renewal would fail. The operator is told only when it has lasted, since a new instance waits
// like this until the relay knows it.
func (p *tlsPair) unreachable(host string, err error, entry *held) {
	since, known := p.unreached[host]
	if !known {
		since = p.now()
		p.unreached[host] = since
		if entry == nil {
			p.logger.Printf("tls pair: no certificate for %s is asked yet: %v (tried again every %s)", host, err, p.pace.probe)
		} else {
			p.logger.Printf("tls pair: a validation of %s would not find this instance now: %v (tried again every %s)", host, err, p.pace.verify)
		}
	}
	if p.now().Sub(since) < p.pace.patience {
		return
	}
	lasted := p.now().Sub(since).Round(time.Minute)
	if entry == nil {
		p.alert("reach "+host, fmt.Sprintf("este router lleva %s sin certificado para %s y no recibe clientes: las validaciones del nombre no le llegan (%s), así que no lo pide. 👉 Mira que la puerta le entregue las validaciones.",
			lasted, host, brief(err)))
		return
	}
	p.alert("reach "+host, fmt.Sprintf("las validaciones de %s no llegan a este router desde hace %s (%s): su certificado caduca en %s y así no se podrá renovar. 👉 Mira que la puerta entregue las validaciones y que su pareja le responda.",
		host, lasted, brief(err), days(entry.notAfter.Sub(p.now()))))
}

// learnAccount learns the address of this instance's ACME account, making the account if there is
// none, so that the name's certificate issuance can be pinned in DNS to the pair's accounts.
func (p *tlsPair) learnAccount(ctx context.Context) {
	if p.account.Load() != nil || (!p.accountTried.IsZero() && p.now().Sub(p.accountTried) < p.pace.account) {
		return
	}
	p.accountTried = p.now()
	asking, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	address, err := p.register(asking)
	if err != nil || address == "" {
		p.logger.Printf("tls pair: the ACME account's address is not known yet: %v", err)
		return
	}
	p.account.Store(address)
	p.logger.Printf("tls pair: ACME account %s", address)
}

// ask makes autocert obtain host's certificate, as a client's first handshake did on a single
// router, and tells the operator how it went.
func (p *tlsPair) ask(ctx context.Context, host string) {
	done := make(chan error, 1)
	go func() { done <- p.obtain(host, false) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(p.pace.askTimeout):
		err = fmt.Errorf("no answer from Let's Encrypt within %s", p.pace.askTimeout)
	case <-ctx.Done():
		return
	}
	p.look()
	if p.holds(host) {
		p.logger.Printf("tls pair: certificate obtained for %s", host)
		_, asked := p.alerted["ask "+host]
		_, unreached := p.alerted["reach "+host]
		if asked || unreached {
			// the trouble was told: so is its end
			delete(p.alerted, "ask "+host)
			delete(p.alerted, "reach "+host)
			p.notify(fmt.Sprintf("este router ya tiene su certificado para %s.", host))
		}
		return
	}
	if err == nil {
		// autocert answers the copy it keeps in memory even when it has expired
		err = errors.New("the certificate held is not valid any more and its renewal has not succeeded")
	}
	p.logger.Printf("tls pair: no certificate for %s yet: %v", host, err)
	p.alert("ask "+host, fmt.Sprintf("este router no tiene certificado para %s y no recibe clientes hasta obtenerlo (%s). Lo vuelve a pedir a Let's Encrypt cada %s. 👉 Si sigue así, mira por qué falla la validación del nombre.",
		host, brief(err), p.pace.ask))
}

// warm loads into autocert, as a handshake did, the certificates customers are served with, which
// schedules their renewal. Only what is valid: nothing here starts an order.
func (p *tlsPair) warm(ctx context.Context) {
	for _, host := range p.served {
		if ctx.Err() != nil {
			return
		}
		entry, ok := p.holding(host)
		if !ok {
			continue
		}
		if err := p.obtain(host, false); err != nil {
			p.logger.Printf("tls pair: the certificate for %s could not be loaded for renewal: %v", host, err)
		}
		if entry.rsa != nil {
			if err := p.obtain(host, true); err != nil {
				p.logger.Printf("tls pair: the RSA certificate for %s could not be loaded for renewal: %v", host, err)
			}
		}
	}
}

// watch tells the operator when a certificate customers are served with should have been renewed
// and has not: autocert renews with a third of its life left (30 days at most), so one that is well
// inside that time is one whose renewal keeps failing.
func (p *tlsPair) watch() {
	now := p.now()
	for _, host := range p.served {
		p.mu.RLock()
		cert, ok := p.certs[host]
		p.mu.RUnlock()
		if !ok {
			continue
		}
		renewFrom := cert.notAfter.Sub(cert.notBefore) / 3
		if renewFrom > 30*24*time.Hour {
			renewFrom = 30 * 24 * time.Hour
		}
		left := cert.notAfter.Sub(now)
		if left >= renewFrom*5/6 {
			delete(p.alerted, "expiry "+host)
			continue
		}
		p.alert("expiry "+host, fmt.Sprintf("el certificado de %s en este router caduca en %s y no se ha renovado: cuando caduque dejará de recibir clientes. 👉 Mira por qué falla la renovación.",
			host, days(left)))
	}
}

// alert tells the operator about a trouble, and again only after a pause while it lasts.
func (p *tlsPair) alert(what, text string) {
	if last, ok := p.alerted[what]; ok && p.now().Sub(last) < p.pace.alert {
		return
	}
	p.alerted[what] = p.now()
	p.notify(text)
}

// Detail is what the operator is told about the pair on the switch: whether it knows its peer,
// the certificates it holds, its ACME account, and when a validation last found it.
func (p *tlsPair) Detail() map[string]any {
	certificates := map[string]any{}
	p.mu.RLock()
	for _, host := range p.hosts {
		entry, ok := p.certs[host]
		if !ok {
			continue
		}
		row := map[string]any{"not_after": entry.notAfter.UTC().Format(time.RFC3339), "served": false, "rsa": entry.rsa != nil}
		if at, ok := p.reached[host]; ok {
			row["validation_reached_at"] = at.UTC().Format(time.RFC3339)
		}
		certificates[host] = row
	}
	p.mu.RUnlock()
	for _, host := range p.served {
		if row, ok := certificates[host].(map[string]any); ok {
			row["served"] = true
		}
	}
	detail := map[string]any{"paired": p.peer != nil, "certificates": certificates}
	if address, ok := p.account.Load().(string); ok {
		detail["acme_account"] = address
	}
	if p.peer != nil {
		if trouble, at := p.peer.lastTrouble(); trouble != "" {
			detail["peer_last_error"], detail["peer_last_error_at"] = trouble, at.UTC().Format(time.RFC3339)
		}
	}
	return detail
}

// brief is an error in a few words for an alert: its first line, cut.
func brief(err error) string {
	text := strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0])
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}

// days says a duration as whole days, or hours under two days, in Spanish for the operator.
func days(d time.Duration) string {
	if d < 48*time.Hour {
		hours := int(d / time.Hour)
		if hours < 0 {
			hours = 0
		}
		return fmt.Sprintf("%d horas", hours)
	}
	return fmt.Sprintf("%d días", int(d/(24*time.Hour)))
}

// ecdsaHello is a client hello that takes the ECDSA certificate autocert obtains by default.
func ecdsaHello(host string) *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{
		ServerName:        host,
		SignatureSchemes:  []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		SupportedCurves:   []tls.CurveID{tls.CurveP256},
		CipherSuites:      []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
	}
}

// rsaHello is a client hello autocert reads as one that takes an RSA certificate only.
func rsaHello(host string) *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{
		ServerName:        host,
		SignatureSchemes:  []tls.SignatureScheme{tls.PSSWithSHA256, tls.PKCS1WithSHA256},
		CipherSuites:      []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
		SupportedVersions: []uint16{tls.VersionTLS12},
	}
}

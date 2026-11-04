// Command loadtest measures how many routed requests per second router instances sustain on this
// machine. It runs, in process, a stand-in for the platform (every call answered after a fixed
// delay, 200 ms by default: what a call from the router's server to lebrel.ai takes), a stand-in for
// a lab (a streamed answer with usage), and, for several instances, a round-robin balancer; it
// starts the router binary it is given as real processes, offers a fixed rate for a while, and
// follows every request until the platform holds its settlement.
//
// A rate is sustained when every request is answered, the settlement backlog stays flat, and the
// 99th percentile of the settlement lag (the platform receiving the settlement, minus the customer
// receiving the last byte) stays under the alert age. The stand-in platform never slows down with
// load: the numbers are the router's own ceiling, not the platform database's.
//
//	go build -o /tmp/router ./cmd/router
//	go run ./cmd/loadtest -router /tmp/router -instances 1 -rates 50,100,200 -duration 30s
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	apiKey     = "leb_live_loadtest_0123456789abcdef_0123456789abcdef0123456789abcdef"
	modelID    = "loadtest/model"
	secret     = "loadtest-control-secret"
	batchPath  = "/api/internal/gateway/reservations/batch"
	alertAge   = 5 * time.Second
	sampleTick = time.Second
)

type config struct {
	router        string
	instances     int
	rates         []float64
	duration      time.Duration
	grace         time.Duration
	linger        time.Duration
	blipAt        time.Duration
	blipFor       time.Duration
	latency       time.Duration
	itemCost      time.Duration
	batch         bool
	ttfb          time.Duration
	chunks        int
	chunkInterval time.Duration
	env           []string
	label         string
	out           string
}

func main() {
	var c config
	var rates, env string
	flag.StringVar(&c.router, "router", "", "router binary to start")
	flag.IntVar(&c.instances, "instances", 1, "router instances behind the balancer")
	flag.StringVar(&rates, "rates", "10,20", "offered requests per second, comma separated, one run each")
	flag.DurationVar(&c.duration, "duration", 30*time.Second, "how long each rate is offered")
	flag.DurationVar(&c.grace, "grace", 30*time.Second, "how long the backlog may take to drain after the load stops")
	flag.DurationVar(&c.linger, "linger", 0, "how long the routers keep running once drained (to watch their alerts)")
	flag.DurationVar(&c.blipAt, "blip-at", 0, "an outage of the platform: its start, counted from the first hold of each run")
	flag.DurationVar(&c.blipFor, "blip-for", 0, "the outage's length: every acceptance and settlement answers 503 ledger_busy meanwhile")
	flag.DurationVar(&c.latency, "latency", 200*time.Millisecond, "the platform's answer time per call")
	flag.DurationVar(&c.itemCost, "item-cost", 2*time.Millisecond, "the platform's extra time per item of a batch")
	flag.BoolVar(&c.batch, "batch", false, "the platform offers the batch endpoint")
	flag.DurationVar(&c.ttfb, "ttfb", 300*time.Millisecond, "the lab's time to its first token")
	flag.IntVar(&c.chunks, "chunks", 20, "tokens the lab streams")
	flag.DurationVar(&c.chunkInterval, "chunk-interval", 50*time.Millisecond, "time between the lab's tokens")
	flag.StringVar(&env, "env", "", "extra router settings, NAME=value separated by spaces")
	flag.StringVar(&c.label, "label", "", "name of this configuration in the results")
	flag.StringVar(&c.out, "out", "", "append the results as JSON lines to this file")
	flag.Parse()
	if c.router == "" {
		log.Fatal("-router is required")
	}
	for _, field := range strings.Split(rates, ",") {
		rate, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
		if err != nil || rate <= 0 {
			log.Fatalf("bad rate %q", field)
		}
		c.rates = append(c.rates, rate)
	}
	c.env = strings.Fields(env)
	if c.label == "" {
		c.label = filepath.Base(c.router)
	}
	if err := runAll(c); err != nil {
		log.Fatal(err)
	}
}

type result struct {
	Label            string         `json:"label"`
	Instances        int            `json:"instances"`
	Rate             float64        `json:"offered_rps"`
	Seconds          float64        `json:"seconds"`
	Sent             int            `json:"sent"`
	Answered         int            `json:"answered"`
	Failed           int            `json:"failed"`
	AnsweredRate     float64        `json:"answered_rps"`
	LatencyP50       float64        `json:"latency_p50_s"`
	LatencyP99       float64        `json:"latency_p99_s"`
	FirstByteP50     float64        `json:"first_byte_p50_s"`
	FirstByteP99     float64        `json:"first_byte_p99_s"`
	LagP50           float64        `json:"settle_lag_p50_s"`
	LagP99           float64        `json:"settle_lag_p99_s"`
	LagMax           float64        `json:"settle_lag_max_s"`
	BacklogAtEnd     int            `json:"backlog_at_end"`
	BacklogPeak      int            `json:"backlog_peak"`
	BacklogSlope     float64        `json:"backlog_slope_per_s"`
	DrainSeconds     float64        `json:"drain_s"`
	Unsettled        int            `json:"unsettled_after_grace"`
	SettledRate      float64        `json:"settled_rps_during_load"`
	PeakPlatformCall int64          `json:"peak_platform_calls_in_flight"`
	SingleSettles    int64          `json:"settle_calls"`
	Blipped          int64          `json:"calls_failed_by_the_outage"`
	BatchCalls       int64          `json:"batch_calls"`
	OpenStreamsPeak  int64          `json:"open_streams_peak"`
	Mismatches       int64          `json:"platform_refusals"`
	RSSPeakMB        float64        `json:"router_rss_peak_mb"`
	Sustained        bool           `json:"sustained"`
	Failures         map[string]int `json:"failures,omitempty"`
	RouterWarnings   []string       `json:"router_warnings,omitempty"`
}

func runAll(c config) error {
	lab, err := startLab(c)
	if err != nil {
		return err
	}
	defer lab.Close()
	platform := newPlatform(c)
	platformServer, err := serve(platform)
	if err != nil {
		return err
	}
	defer platformServer.Close()
	work, err := os.MkdirTemp("", "router-loadtest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	routes := filepath.Join(work, "routes.json")
	if err := writeRoutes(routes, "http://"+lab.Addr().String()+"/v1"); err != nil {
		return err
	}
	fmt.Printf("%-24s %2s %6s %7s %5s %6s %6s %7s %7s %7s %8s %8s %6s %7s %s\n", "config", "n", "rate", "answer", "fail", "open", "p50", "lag p50", "lag p99", "lag max", "backlog", "slope/s", "drain", "rss MB", "sustained")
	var results []result
	for _, rate := range c.rates {
		r, err := runRate(c, rate, platform, "http://"+platformServer.Addr().String(), routes, work)
		if err != nil {
			return err
		}
		results = append(results, r)
		drain := fmt.Sprintf("%.1fs", r.DrainSeconds)
		if r.Unsettled > 0 {
			drain = fmt.Sprintf("+%d", r.Unsettled)
		}
		fmt.Printf("%-24s %2d %6.0f %7d %5d %6d %6.2f %7.2f %7.2f %7.2f %8d %8.1f %6s %7.0f %t\n", c.label, c.instances, rate, r.Answered, r.Failed,
			r.OpenStreamsPeak, r.LatencyP50, r.LagP50, r.LagP99, r.LagMax, r.BacklogAtEnd, r.BacklogSlope, drain, r.RSSPeakMB, r.Sustained)
		if r.Blipped > 0 {
			fmt.Printf("    %d calls answered 503 by the injected outage\n", r.Blipped)
		}
		for kind, count := range r.Failures {
			fmt.Printf("    %d failed: %s\n", count, kind)
		}
		for _, line := range r.RouterWarnings {
			fmt.Printf("    router log %s\n", line)
		}
		if c.out != "" {
			file, err := os.OpenFile(c.out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			encoded, _ := json.Marshal(r)
			_, _ = file.Write(append(encoded, '\n'))
			file.Close()
		}
	}
	best := 0.0
	for _, r := range results {
		if r.Sustained && r.Rate > best {
			best = r.Rate
		}
	}
	fmt.Printf("highest sustained rate for %s x%d: %.0f requests/s\n", c.label, c.instances, best)
	return nil
}

// ---- one rate ----

type request struct {
	started, firstByte, ended time.Time
	id                        string
	ok                        bool
	failure                   string
}

func runRate(c config, rate float64, platform *platform, platformURL, routes, work string) (result, error) {
	platform.reset()
	routers, err := startRouters(c, platformURL, routes, work)
	if err != nil {
		return result{}, err
	}
	defer stopRouters(routers)
	target := routers[0].url
	if len(routers) > 1 {
		balancer, err := startBalancer(routers)
		if err != nil {
			return result{}, err
		}
		defer balancer.Close()
		target = "http://" + balancer.Addr().String()
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConns: 20000, MaxIdleConnsPerHost: 20000, IdleConnTimeout: 90 * time.Second,
		DisableCompression: true}, Timeout: 5 * time.Minute}
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Say twenty words."}],"stream":true,"max_tokens":64}`, modelID)

	var mu sync.Mutex
	var requests []*request
	var wg sync.WaitGroup
	var open, openPeak atomic.Int64
	var rss sampler
	rss.start(routers)
	var backlog []int
	stopSampling := make(chan struct{})
	samplingDone := make(chan struct{})
	ended := map[string]time.Time{}
	go func() {
		defer close(samplingDone)
		ticker := time.NewTicker(sampleTick)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-ticker.C:
				mu.Lock()
				backlog = append(backlog, platform.unsettled(ended))
				mu.Unlock()
			}
		}
	}()
	begin := time.Now()
	total := int(rate * c.duration.Seconds())
	for i := 0; i < total; i++ {
		if wait := time.Until(begin.Add(time.Duration(float64(i) / rate * float64(time.Second)))); wait > 0 {
			time.Sleep(wait)
		}
		r := &request{started: time.Now()}
		mu.Lock()
		requests = append(requests, r)
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := open.Add(1)
			for peak := openPeak.Load(); now > peak && !openPeak.CompareAndSwap(peak, now); peak = openPeak.Load() {
			}
			send(client, target, body, r)
			open.Add(-1)
			if r.id != "" {
				mu.Lock()
				ended[r.id] = r.ended
				mu.Unlock()
			}
		}()
	}
	loadSeconds := time.Since(begin).Seconds()
	settledDuringLoad := platform.settledCount()
	wg.Wait()
	close(stopSampling)
	<-samplingDone
	answersDone := time.Now()
	// the load is over: how long until every answered request is settled?
	drainStart := time.Now()
	deadline := drainStart.Add(c.grace)
	for platform.unsettled(ended) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	drain := time.Since(drainStart).Seconds()
	rss.stop()
	time.Sleep(c.linger)

	r := result{Label: c.label, Instances: c.instances, Rate: rate, Seconds: loadSeconds, Sent: len(requests), DrainSeconds: drain,
		PeakPlatformCall: platform.peak.Load(), Mismatches: platform.refusals.Load(), RSSPeakMB: rss.peakMB(),
		SingleSettles: platform.singles.Load(), BatchCalls: platform.batches.Load(), OpenStreamsPeak: openPeak.Load(), Blipped: platform.blipped.Load()}
	var latencies, firstBytes, lags []float64
	settledAt := platform.settledTimes()
	for _, req := range requests {
		if !req.ok {
			r.Failed++
			if r.Failures == nil {
				r.Failures = map[string]int{}
			}
			r.Failures[req.failure]++
			continue
		}
		r.Answered++
		latencies = append(latencies, req.ended.Sub(req.started).Seconds())
		firstBytes = append(firstBytes, req.firstByte.Sub(req.started).Seconds())
		if at, ok := settledAt[req.id]; ok {
			lags = append(lags, math.Max(0, at.Sub(req.ended).Seconds()))
		} else {
			r.Unsettled++
			// still owed: its lag is at least the time until the measurement stopped
			lags = append(lags, time.Since(req.ended).Seconds())
		}
	}
	r.AnsweredRate = float64(r.Answered) / answersDone.Sub(begin).Seconds()
	r.SettledRate = float64(settledDuringLoad) / loadSeconds
	r.LatencyP50, r.LatencyP99 = percentile(latencies, 50), percentile(latencies, 99)
	r.FirstByteP50, r.FirstByteP99 = percentile(firstBytes, 50), percentile(firstBytes, 99)
	r.LagP50, r.LagP99, r.LagMax = percentile(lags, 50), percentile(lags, 99), percentile(lags, 100)
	if len(backlog) > 0 {
		r.BacklogAtEnd = backlog[len(backlog)-1]
		for _, value := range backlog {
			r.BacklogPeak = max(r.BacklogPeak, value)
		}
		r.BacklogSlope = slope(backlog[len(backlog)/2:])
	}
	r.RouterWarnings = warnings(routers)
	r.Sustained = r.Failed == 0 && r.Unsettled == 0 && r.Mismatches == 0 && r.LagP99 < alertAge.Seconds() && r.BacklogSlope < rate/100+1
	return r, nil
}

func send(client *http.Client, target, body string, r *request) {
	request, _ := http.NewRequest(http.MethodPost, target+"/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		r.ended = time.Now()
		r.failure = "client: " + errorKind(err)
		return
	}
	defer response.Body.Close()
	r.id = response.Header.Get("X-Request-ID")
	reader := bufio.NewReader(response.Body)
	first := true
	done := false
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && first {
			r.firstByte, first = time.Now(), false
		}
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data: [DONE]")) {
			done = true
		}
		if err != nil {
			break
		}
	}
	r.ended = time.Now()
	r.ok = response.StatusCode == 200 && done
	if !r.ok {
		r.failure = fmt.Sprintf("router: HTTP %d, stream complete %t", response.StatusCode, done)
	}
}

// errorKind keeps what a network error says without its addresses, so failures group.
func errorKind(err error) string {
	text := err.Error()
	if i := strings.LastIndex(text, ": "); i >= 0 {
		return text[i+2:]
	}
	return text
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(0, min(index, len(sorted)-1))]
}

// slope is the least-squares growth per sample (one sample a second).
func slope(values []int) float64 {
	n := float64(len(values))
	if n < 2 {
		return 0
	}
	var sx, sy, sxy, sxx float64
	for i, v := range values {
		x, y := float64(i), float64(v)
		sx, sy, sxy, sxx = sx+x, sy+y, sxy+x*y, sxx+x*x
	}
	return (n*sxy - sx*sy) / (n*sxx - sx*sx)
}

// warnings groups the routers' log lines that are not routine, by what they say.
func warnings(routers []*instance) []string {
	counts := map[string]int{}
	for _, router := range routers {
		out := router.logs.(*lockedWriter)
		out.mu.Lock()
		for _, line := range strings.Split(out.w.String(), "\n") {
			if !strings.Contains(line, "failed") && !strings.Contains(line, "refused") && !strings.Contains(line, "owed") && !strings.Contains(line, "ALERT") {
				continue
			}
			if i := strings.Index(line, "request="); i >= 0 {
				if j := strings.Index(line[i:], " "); j > 0 {
					line = line[:i] + line[i+j+1:]
				}
			}
			if len(line) > 20 {
				line = line[20:] // the timestamp
			}
			counts[line]++
		}
		out.mu.Unlock()
	}
	var lines []string
	for line, count := range counts {
		lines = append(lines, fmt.Sprintf("%6d × %s", count, line))
	}
	sort.Slice(lines, func(a, b int) bool { return lines[a] > lines[b] })
	if len(lines) > 6 {
		lines = lines[:6]
	}
	return lines
}

// ---- the router processes ----

type instance struct {
	cmd  *exec.Cmd
	url  string
	logs io.Writer
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func startRouters(c config, platformURL, routes, work string) ([]*instance, error) {
	seed := make([]byte, 32)
	_, _ = rand.Read(seed)
	var routers []*instance
	for i := 0; i < c.instances; i++ {
		port, err := freePort()
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(work, fmt.Sprintf("settlements-%d-%d", time.Now().UnixNano(), i))
		cmd := exec.Command(c.router)
		cmd.Env = append([]string{
			"PATH=" + os.Getenv("PATH"), "ROUTES_FILE=" + routes, fmt.Sprintf("LISTEN_ADDR=127.0.0.1:%d", port),
			"CONTROL_PLANE_URL=" + platformURL, "CONTROL_PLANE_SECRET=" + secret, "SETTLEMENT_DIR=" + dir,
			"RECEIPT_SIGNING_SEED=" + base64.StdEncoding.EncodeToString(seed), fmt.Sprintf("INSTANCE_ID=load-%d", i+1),
			"ENVIRONMENT=development", "LOADTEST_UPSTREAM_KEY=lab-key",
		}, c.env...)
		logs := &lockedWriter{w: &bytes.Buffer{}}
		cmd.Stdout, cmd.Stderr = logs, logs
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		router := &instance{cmd: cmd, url: fmt.Sprintf("http://127.0.0.1:%d", port), logs: logs}
		routers = append(routers, router)
		if err := waitReady(router); err != nil {
			stopRouters(routers)
			return nil, fmt.Errorf("router %d: %w\n%s", i+1, err, logs.w.String())
		}
	}
	return routers, nil
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w.Len() > 4<<20 {
		return len(p), nil
	}
	return l.w.Write(p)
}

func waitReady(router *instance) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(router.url + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("not ready after 15s")
}

func stopRouters(routers []*instance) {
	for _, router := range routers {
		_ = router.cmd.Process.Signal(syscall.SIGTERM)
	}
	for _, router := range routers {
		done := make(chan struct{})
		go func() {
			_ = router.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = router.cmd.Process.Kill()
			<-done
		}
	}
}

// sampler records the routers' resident memory once a second.
type sampler struct {
	stopped chan struct{}
	done    chan struct{}
	peakKB  atomic.Int64
}

func (s *sampler) start(routers []*instance) {
	s.stopped, s.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(sampleTick)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopped:
				return
			case <-ticker.C:
				for _, router := range routers {
					out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(router.cmd.Process.Pid)).Output()
					if err != nil {
						continue
					}
					if kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && kb > s.peakKB.Load() {
						s.peakKB.Store(kb)
					}
				}
			}
		}
	}()
}

func (s *sampler) stop()           { close(s.stopped); <-s.done }
func (s *sampler) peakMB() float64 { return float64(s.peakKB.Load()) / 1024 }

// ---- the balancer ----

type listener struct {
	server *http.Server
	ln     net.Listener
}

func (l *listener) Addr() net.Addr { return l.ln.Addr() }
func (l *listener) Close() error   { return l.server.Close() }

func serve(handler http.Handler) (*listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() { _ = server.Serve(ln) }()
	return &listener{server: server, ln: ln}, nil
}

func startBalancer(routers []*instance) (*listener, error) {
	targets := make([]*url.URL, len(routers))
	for i, router := range routers {
		targets[i], _ = url.Parse(router.url)
	}
	var next atomic.Uint64
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(targets[next.Add(1)%uint64(len(targets))])
		},
		FlushInterval: -1,
		Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 20000, MaxIdleConnsPerHost: 20000, IdleConnTimeout: 90 * time.Second, DisableCompression: true},
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	return serve(proxy)
}

// ---- the lab ----

func startLab(c config) (*listener, error) {
	return serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		time.Sleep(c.ttfb)
		for i := 0; i < c.chunks; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"lab-1\",\"model\":\"lab-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"word \"}}]}\n\n")
			flusher.Flush()
			if i < c.chunks-1 {
				time.Sleep(c.chunkInterval)
			}
		}
		fmt.Fprintf(w, "data: {\"id\":\"lab-1\",\"model\":\"lab-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", c.chunks, 12+c.chunks)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

func writeRoutes(path, labURL string) error {
	routes := map[string]any{"routes": []any{map[string]any{
		"model_id": modelID, "level": 2, "family": "loadtest", "claimed_revision": "lab-model", "data_note": "load test",
		"upstreams": []any{map[string]any{"name": "lab/api", "base_url": labURL, "model": "lab-model", "api_key_env": "LOADTEST_UPSTREAM_KEY",
			"priority": 1, "usage_format": "openai", "data_note": "load test"}},
		"pricing": map[string]any{"input_microusd_per_million_tokens": 157500, "cached_input_microusd_per_million_tokens": 3150,
			"output_microusd_per_million_tokens": 630000, "reservation_bps": 15000, "fee_bps": 2000, "version": "loadtest",
			"effective_at": "2026-10-20T00:00:00.000Z"},
		"max_context_tokens": 100000, "max_output_tokens": 4096, "default_max_output_tokens": 256,
	}}}
	encoded, err := json.MarshalIndent(routes, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}

// ---- the platform ----

type reservation struct {
	requestID string
	accepted  bool
	settled   bool
	outcome   string
	actual    int64
	settledAt time.Time
}

// platform answers the router's control-plane calls the way lebrel.ai does (same paths, same
// idempotency keys, same refusals), each after a fixed delay.
type platform struct {
	c        config
	mu       sync.Mutex
	byID     map[string]*reservation
	byReq    map[string]string
	next     atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
	refusals atomic.Int64
	settled  atomic.Int64
	singles  atomic.Int64
	batches  atomic.Int64
	first    atomic.Int64 // when the run's first hold arrived
	blipped  atomic.Int64
}

// inOutage is true while the injected outage lasts.
func (p *platform) inOutage() bool {
	first := p.first.Load()
	if p.c.blipFor <= 0 || first == 0 {
		return false
	}
	since := time.Since(time.Unix(0, first))
	return since >= p.c.blipAt && since < p.c.blipAt+p.c.blipFor
}

func newPlatform(c config) *platform {
	p := &platform{c: c}
	p.reset()
	return p
}

func (p *platform) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byID, p.byReq = map[string]*reservation{}, map[string]string{}
	p.peak.Store(0)
	p.refusals.Store(0)
	p.settled.Store(0)
	p.singles.Store(0)
	p.batches.Store(0)
	p.first.Store(0)
	p.blipped.Store(0)
}

func (p *platform) settledCount() int64 { return p.settled.Load() }

func (p *platform) settledTimes() map[string]time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	times := make(map[string]time.Time, len(p.byID))
	for _, res := range p.byID {
		if res.settled {
			times[res.requestID] = res.settledAt
		}
	}
	return times
}

// unsettled counts the requests the customer has finished whose settlement the platform lacks.
func (p *platform) unsettled(ended map[string]time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for requestID := range ended {
		id, ok := p.byReq[requestID]
		if !ok || !p.byID[id].settled {
			count++
		}
	}
	return count
}

func (p *platform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		peak := p.peak.Load()
		if now <= peak || p.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	if r.Header.Get("Authorization") != "Bearer "+secret {
		write(w, 401, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "bad secret"}})
		return
	}
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	path := r.URL.Path
	switch {
	case path == "/api/internal/gateway/authenticate":
		time.Sleep(p.c.latency)
		write(w, 200, map[string]any{"accountId": "acct_loadtest_000001", "apiKeyId": "key_loadtest_000001", "concurrency": 1_000_000, "accountConcurrency": 1_000_000})
	case path == "/api/internal/gateway/reservations":
		p.first.CompareAndSwap(0, time.Now().UnixNano())
		time.Sleep(p.c.latency)
		requestID, _ := payload["requestId"].(string)
		if r.Header.Get("Idempotency-Key") != requestID || requestID == "" {
			write(w, 400, map[string]any{"error": map[string]any{"code": "invalid_idempotency_key"}})
			return
		}
		p.mu.Lock()
		id, ok := p.byReq[requestID]
		if !ok {
			id = fmt.Sprintf("res_%012d", p.next.Add(1))
			p.byReq[requestID], p.byID[id] = id, &reservation{requestID: requestID}
		}
		p.mu.Unlock()
		write(w, 201, map[string]any{"id": id, "reservationId": id, "amountMicrousd": payload["amountMicrousd"], "createdAt": time.Now().UTC().Format(time.RFC3339Nano)})
	case (path == batchPath || strings.HasSuffix(path, "/settle") || strings.HasSuffix(path, "/accepted")) && p.inOutage():
		time.Sleep(p.c.latency)
		p.blipped.Add(1)
		write(w, 503, map[string]any{"error": map[string]any{"code": "ledger_busy", "message": "busy"}})
	case path == batchPath && p.c.batch:
		p.batches.Add(1)
		items, _ := payload["items"].([]any)
		time.Sleep(p.c.latency + time.Duration(len(items))*p.c.itemCost)
		results := make([]any, 0, len(items))
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			key, _ := item["key"].(string)
			id, _ := item["reservationId"].(string)
			kind, _ := item["kind"].(string)
			status, answer := p.apply(id, kind, item)
			entry := map[string]any{"key": key, "status": status}
			if status >= 300 {
				entry["error"] = answer["error"]
			}
			results = append(results, entry)
		}
		write(w, 200, map[string]any{"results": results})
	case strings.HasPrefix(path, "/api/internal/gateway/reservations/"):
		time.Sleep(p.c.latency)
		parts := strings.Split(strings.TrimPrefix(path, "/api/internal/gateway/reservations/"), "/")
		if len(parts) != 2 || r.Header.Get("Idempotency-Key") != parts[0]+":"+parts[1] {
			write(w, 400, map[string]any{"error": map[string]any{"code": "invalid_idempotency_key"}})
			return
		}
		if parts[1] == "settle" {
			p.singles.Add(1)
		}
		status, answer := p.apply(parts[0], parts[1], payload)
		write(w, status, answer)
	default:
		write(w, 404, map[string]any{"error": map[string]any{"code": "not_found"}})
	}
}

func (p *platform) apply(id, kind string, payload map[string]any) (int, map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.byID[id]
	if !ok {
		p.refusals.Add(1)
		return 404, map[string]any{"error": map[string]any{"code": "reservation_not_found", "message": "Credit reservation not found"}}
	}
	switch kind {
	case "accepted":
		if res.settled {
			p.refusals.Add(1)
			return 409, map[string]any{"error": map[string]any{"code": "reservation_not_open", "message": "Reservation cannot accept new upstream evidence"}}
		}
		res.accepted = true
		return 200, map[string]any{"reservationId": id, "upstreamAccepted": true}
	case "settle":
		outcome, _ := payload["outcome"].(string)
		actual, _ := payload["actualMicrousd"].(float64)
		if (outcome == "upstream_rejected") == res.accepted {
			p.refusals.Add(1)
			return 409, map[string]any{"error": map[string]any{"code": "settlement_outcome_mismatch", "message": "Settlement outcome does not match upstream admission evidence"}}
		}
		if res.settled {
			if res.outcome == outcome && res.actual == int64(actual) {
				return 200, map[string]any{"reservationId": id, "actualMicrousd": res.actual}
			}
			p.refusals.Add(1)
			return 409, map[string]any{"error": map[string]any{"code": "settlement_idempotency_conflict", "settled": map[string]any{"outcome": res.outcome, "actualMicrousd": res.actual}}}
		}
		res.settled, res.outcome, res.actual, res.settledAt = true, outcome, int64(actual), time.Now()
		p.settled.Add(1)
		return 200, map[string]any{"reservationId": id, "actualMicrousd": res.actual}
	default:
		return 404, map[string]any{"error": map[string]any{"code": "not_found"}}
	}
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxDownloadDur   = 30 * time.Second
	maxUploadBytes   = 256 << 20
	perClientStreams = 16  // concurrent transfers per IPv4 address or IPv6 /64
	globalStreams    = 512 // concurrent transfers for the whole server
	resultTTL        = 10 * time.Minute
	maxResults       = 20000
)

type tcpStat struct {
	RTTus        uint32
	TotalRetrans uint32
	MSS          uint32
	DataSegsOut  uint32
	HaveSegs     bool
}

type ctxKey int

const connKey ctxKey = 0

// connFromCtx returns the request's transport connection: a net.Conn for
// HTTP/1.1 and HTTP/2, a *quic.Conn for HTTP/3.
func connFromCtx(ctx context.Context) any { return ctx.Value(connKey) }

type app struct {
	cfg     config
	geo     *geoDB
	origins map[string]bool
	junk    []byte
	bufPool sync.Pool

	limMu    sync.Mutex
	visitors map[netip.Prefix]*visitor
	inflight int

	resMu   sync.Mutex
	results map[string]*dlResult
}

type dlResult struct {
	created    time.Time
	conns      map[any]*connAgg
	rttSamples []float64 // ms, server-side smoothed RTT sampled during the transfer
	protos     map[string]bool
}

func newApp(cfg config, geo *geoDB) *app {
	a := &app{
		cfg:      cfg,
		geo:      geo,
		origins:  map[string]bool{},
		junk:     make([]byte, 8<<20),
		visitors: map[netip.Prefix]*visitor{},
		results:  map[string]*dlResult{},
	}
	if _, err := rand.Read(a.junk); err != nil {
		panic(err)
	}
	a.bufPool.New = func() any { b := make([]byte, 1<<20); return &b }
	for _, o := range cfg.origins() {
		a.origins[o] = true
	}
	go a.gcResults()
	return a
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", a.handleConfig)
	mux.HandleFunc("/api/info", a.handleInfo)
	mux.HandleFunc("/api/ping", a.handlePing)
	mux.HandleFunc("/api/download", a.handleDownload)
	mux.HandleFunc("/api/upload", a.handleUpload)
	mux.HandleFunc("/api/result", a.handleResult)
	mux.Handle("/", staticHandler(a.cfg))
	return a.withHeaders(mux)
}

// ---------------------------------------------------------------- middleware

func (a *app) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if origin := r.Header.Get("Origin"); origin != "" && (a.cfg.dev || a.origins[origin]) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Timing-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type")
				h.Set("Access-Control-Max-Age", "7200")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("X-Proto", r.Proto) // HTTP/1.1, HTTP/2.0 or HTTP/3.0: drives the protocol lights
			h.Set("Access-Control-Expose-Headers", "X-Proto")
		}
		want := a.cfg.hostProto(r.Host)
		// A browser may reuse a connection opened for another of our names (same IP,
		// same certificate). 421 makes it retry on a connection of its own, so the
		// protocol chosen on the page is the one actually measured.
		if (want == "h1" && r.ProtoMajor != 1) || (want == "h2" && r.ProtoMajor == 3) {
			http.Error(w, "misdirected request: wrong protocol for this hostname", http.StatusMisdirectedRequest)
			return
		}
		if alt := a.cfg.altSvc(); alt != "" && want == "h3" && r.TLS != nil && r.ProtoMajor < 3 {
			h.Set("Alt-Svc", alt) // tells the browser HTTP/3 is available on UDP
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) clientIP(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	ip := ap.Addr().Unmap()
	if a.cfg.trustProxy && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if p, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
				return p.Unmap()
			}
		}
	}
	return ip
}

// visitor is one IPv4 address or IPv6 /64 with transfers in progress. Its speed
// caps are shared by all of its parallel streams.
type visitor struct {
	streams  int
	down, up *bucket
}

// acquire enforces per-visitor and global limits on concurrent transfers and
// returns the visitor's shared speed caps.
func (a *app) acquire(ip netip.Addr) (*visitor, func(), bool) {
	bits := 32
	if ip.Is6() {
		bits = 64
	}
	key, _ := ip.Prefix(bits)
	a.limMu.Lock()
	defer a.limMu.Unlock()
	v := a.visitors[key]
	if a.inflight >= globalStreams || (v != nil && v.streams >= perClientStreams) {
		return nil, nil, false
	}
	if v == nil {
		v = &visitor{down: newBucket(a.cfg.maxDown), up: newBucket(a.cfg.maxUp)}
		a.visitors[key] = v
	}
	a.inflight++
	v.streams++
	return v, func() {
		a.limMu.Lock()
		a.inflight--
		if v.streams--; v.streams <= 0 {
			delete(a.visitors, key)
		}
		a.limMu.Unlock()
	}, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- handlers

func (a *app) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"name":      a.cfg.name,
		"location":  a.cfg.location,
		"limits":    map[string]int64{"down": a.cfg.maxDown, "up": a.cfg.maxUp},
		"protos":    a.cfg.protos(),
		"default":   a.cfg.defProto,
		"endpoints": a.cfg.endpoints(),
	})
}

func (a *app) handleInfo(w http.ResponseWriter, r *http.Request) {
	ip := a.clientIP(r)
	family := "IPv6"
	if ip.Is4() {
		family = "IPv4"
	}
	writeJSON(w, struct {
		IP     string `json:"ip"`
		Family string `json:"family"`
		GeoInfo
	}{ip.String(), family, a.geo.Lookup(ip)})
}

func (a *app) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

var validID = func(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (a *app) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	vis, release, ok := a.acquire(a.clientIP(r))
	if !ok {
		http.Error(w, "too many concurrent tests", http.StatusTooManyRequests)
		return
	}
	defer release()

	dur := 15 * time.Second
	if ms, err := strconv.Atoi(r.URL.Query().Get("ms")); err == nil && ms > 0 {
		dur = min(time.Duration(ms)*time.Millisecond, maxDownloadDur)
	}
	id := r.URL.Query().Get("id")

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Encoding", "identity")
	w.WriteHeader(http.StatusOK)

	conn := connFromCtx(r.Context())
	st0, haveStats := snapshot(conn)

	// The browser ends the test by aborting the stream. On HTTP/1.1 net/http then
	// closes the socket, so the counters can't be read afterwards: keep the latest
	// snapshot (every 100 ms) and the byte count at that moment.
	var sent, sentAtLast int64
	var rtts []float64
	last := st0
	deadline := time.Now().Add(dur)
	nextSample := time.Now().Add(100 * time.Millisecond)
	chunk := vis.down.chunk(1 << 20)
	off := 0
	for {
		now := time.Now()
		if now.After(deadline) {
			break
		}
		if haveStats && now.After(nextSample) {
			if st, ok := snapshot(conn); ok {
				last, sentAtLast = st, sent
				if st.rtt > 0 {
					rtts = append(rtts, float64(st.rtt.Microseconds())/1000)
				}
			}
			nextSample = now.Add(100 * time.Millisecond)
		}
		if err := vis.down.wait(r.Context(), chunk); err != nil {
			break
		}
		n, err := w.Write(a.junk[off : off+chunk])
		sent += int64(n)
		if err != nil {
			break
		}
		off = (off + chunk) % (len(a.junk) - chunk)
	}

	if haveStats && validID(id) {
		if st, ok := snapshot(conn); ok {
			last, sentAtLast = st, sent
		}
		a.recordDownload(id, r.Proto, conn, sentAtLast, st0, last, rtts)
	}
}

func (a *app) recordDownload(id, proto string, conn any, sent int64, first, last connSnap, rtts []float64) {
	a.resMu.Lock()
	defer a.resMu.Unlock()
	res := a.results[id]
	if res == nil {
		if len(a.results) >= maxResults {
			return
		}
		res = &dlResult{created: time.Now(), conns: map[any]*connAgg{}, protos: map[string]bool{}}
		a.results[id] = res
	}
	res.protos[proto] = true
	res.rttSamples = append(res.rttSamples, rtts...)
	ag := res.conns[conn]
	if ag == nil {
		ag = &connAgg{first: first, last: last}
		res.conns[conn] = ag
	}
	if first.at.Before(ag.first.at) {
		ag.first = first
	}
	if last.at.After(ag.last.at) {
		ag.last = last
	}
	ag.bytes += sent
}

func (a *app) handleResult(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	a.resMu.Lock()
	res := a.results[id]
	var out map[string]any
	if res != nil {
		var bytes int64
		var lost, sent uint64
		quic := false
		for _, ag := range res.conns {
			l, s := ag.lostAndSent()
			lost, sent, bytes = lost+l, sent+s, bytes+ag.bytes
			quic = quic || ag.last.quic
		}
		protos := make([]string, 0, len(res.protos))
		for p := range res.protos {
			protos = append(protos, p)
		}
		sort.Strings(protos)
		out = map[string]any{"bytes": bytes, "connections": len(res.conns), "protocols": protos}
		if sent > 0 {
			out["loss_pct"] = 100 * float64(lost) / float64(sent)
			out["loss_source"] = map[bool]string{true: "quic", false: "tcp"}[quic]
		}
		if n := len(res.rttSamples); n > 0 {
			s := append([]float64(nil), res.rttSamples...)
			sort.Float64s(s)
			out["loaded_rtt_ms"] = s[n/2]
		}
	}
	a.resMu.Unlock()
	if out == nil {
		http.Error(w, "unknown test id", http.StatusNotFound)
		return
	}
	writeJSON(w, out)
}

func (a *app) gcResults() {
	for range time.Tick(time.Minute) {
		cutoff := time.Now().Add(-resultTTL)
		a.resMu.Lock()
		for id, r := range a.results {
			if r.created.Before(cutoff) {
				delete(a.results, id)
			}
		}
		a.resMu.Unlock()
	}
}

func (a *app) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	vis, release, ok := a.acquire(a.clientIP(r))
	if !ok {
		http.Error(w, "too many concurrent tests", http.StatusTooManyRequests)
		return
	}
	defer release()
	bp := a.bufPool.Get().(*[]byte)
	defer a.bufPool.Put(bp)
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if vis.up != nil {
		body = &limitedReader{ctx: r.Context(), r: body, b: vis.up, chunk: vis.up.chunk(len(*bp))}
	}
	n, err := io.CopyBuffer(io.Discard, body, *bp)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		http.Error(w, "upload larger than 256 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	writeJSON(w, map[string]int64{"bytes": n})
}

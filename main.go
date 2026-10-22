// SPDX-License-Identifier: GPL-3.0-or-later

// all-for-speed — dual-stack browser speed test server.
// https://git.marwan.ma/MARWAN/all-for-speed
// Serves HTTP/1.1 and HTTP/2 on TCP and HTTP/3 on UDP, all from one binary.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"html"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

//go:embed web
var webFS embed.FS

type config struct {
	domain     string
	name       string
	location   string
	httpsAddr  string
	httpAddr   string
	plainAddr  string
	certFile   string
	keyFile    string
	acmeRoot   string
	geoCity    string
	geoASN     string
	h3         bool
	h2         bool
	defProto   string
	trustProxy bool
	dev        bool
}

// httpsPort is "" for the standard port 443, otherwise ":8443" etc.
func (c config) httpsPort() string {
	if p := portOf(c.httpsAddr); p != ":443" {
		return p
	}
	return ""
}

// Every protocol has its own pair of single-stack test hostnames, so the page can
// choose the protocol instead of leaving it to the browser:
//
//	v4.<domain>    v6.<domain>     HTTP/3 (advertised with Alt-Svc; HTTP/2 until QUIC is up)
//	v4-h2.<domain> v6-h2.<domain>  HTTP/2 only (no Alt-Svc)
//	v4-h1.<domain> v6-h1.<domain>  HTTP/1.1 only (TLS offers nothing else)
var protoSuffix = map[string]string{"h3": "", "h2": "-h2", "h1": "-h1"}

func (c config) protos() []string {
	if c.dev || c.httpsAddr == "" {
		return []string{"h1"} // plain HTTP has no ALPN, so HTTP/1.1 only
	}
	p := []string{"h1"}
	if c.h2 {
		p = append(p, "h2")
	}
	if c.h3 {
		p = append(p, "h3")
	}
	return p
}

func (c config) host(fam, proto string) string { return fam + protoSuffix[proto] + "." + c.domain }

func (c config) base(fam, proto string) string {
	if c.dev {
		if fam == "v4" {
			return "http://127.0.0.1" + portOf(c.plainAddr)
		}
		return "http://[::1]" + portOf(c.plainAddr)
	}
	return "https://" + c.host(fam, proto) + c.httpsPort()
}

// endpoints is sent to the page: protocol -> family -> base URL.
func (c config) endpoints() map[string]map[string]string {
	e := map[string]map[string]string{}
	for _, p := range c.protos() {
		e[p] = map[string]string{"v4": c.base("v4", p), "v6": c.base("v6", p)}
	}
	return e
}

// hostProto returns "h1", "h2" or "h3" for a test hostname, "" for anything else.
func (c config) hostProto(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for p := range protoSuffix {
		if host == c.host("v4", p) || host == c.host("v6", p) {
			return p
		}
	}
	return ""
}

func (c config) altSvc() string {
	if !c.h3 || c.httpsAddr == "" {
		return ""
	}
	return `h3="` + portOf(c.httpsAddr) + `"; ma=86400`
}

// hosts lists every name the certificate must cover.
func (c config) hosts() []string {
	h := []string{c.domain}
	for _, p := range []string{"h3", "h2", "h1"} {
		h = append(h, c.host("v4", p), c.host("v6", p))
	}
	return h
}

// origins the page may be loaded from (for CORS).
func (c config) origins() []string {
	o := []string{"https://" + c.domain + c.httpsPort()}
	if c.dev {
		o = append(o, "http://localhost"+portOf(c.plainAddr))
	}
	return o
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ""
}

func main() {
	var cfg config
	flag.StringVar(&cfg.domain, "domain", "", "main hostname, e.g. speedtest.example.org (required); v4[-h1|-h2].<domain> must be A-only and v6[-h1|-h2].<domain> AAAA-only")
	flag.StringVar(&cfg.name, "name", "Speed test", "site name shown in the page header, browser tab and home-screen icon")
	flag.StringVar(&cfg.location, "location", "", "server location shown on the page, e.g. \"Paris, France\" (hidden when empty)")
	flag.StringVar(&cfg.httpsAddr, "https", ":443", "HTTPS listen address, TCP and UDP (empty to disable)")
	flag.StringVar(&cfg.httpAddr, "http", ":80", "HTTP listen address for ACME challenges and redirects (empty to disable)")
	flag.StringVar(&cfg.plainAddr, "plain", "", "extra plain-HTTP listener serving the full app, e.g. 127.0.0.1:8080 behind a reverse proxy or for -dev")
	flag.StringVar(&cfg.certFile, "cert", "/etc/speedtest/tls/fullchain.pem", "TLS certificate chain (reloaded when it changes)")
	flag.StringVar(&cfg.keyFile, "key", "/etc/speedtest/tls/privkey.pem", "TLS private key")
	flag.StringVar(&cfg.acmeRoot, "acme-webroot", "/var/lib/speedtest/acme", "certbot --webroot directory, served on port 80")
	flag.StringVar(&cfg.geoCity, "geo-city", "/var/lib/speedtest/geo/city.mmdb", "city database (DB-IP Lite or GeoLite2 City, .mmdb)")
	flag.StringVar(&cfg.geoASN, "geo-asn", "/var/lib/speedtest/geo/asn.mmdb", "ASN database (DB-IP Lite or GeoLite2 ASN, .mmdb)")
	flag.BoolVar(&cfg.h3, "h3", true, "serve HTTP/3 on UDP and advertise it with Alt-Svc")
	flag.BoolVar(&cfg.h2, "h2", true, "offer HTTP/2 on TCP (otherwise HTTP/1.1 only)")
	flag.StringVar(&cfg.defProto, "default-proto", "h1", "protocol selected when the page opens: h1, h2 or h3")
	flag.BoolVar(&cfg.trustProxy, "trust-proxy", false, "trust X-Forwarded-For from loopback peers")
	flag.BoolVar(&cfg.dev, "dev", false, "local development: allow any origin, use 127.0.0.1 / [::1] on the -plain port as the v4/v6 endpoints")
	flag.Parse()

	if cfg.dev && cfg.plainAddr == "" {
		cfg.plainAddr = ":8080"
	}
	if cfg.dev && cfg.domain == "" {
		cfg.domain = "localhost"
	}
	if cfg.domain == "" {
		log.Fatal("-domain is required, e.g. -domain speedtest.example.org")
	}
	if strings.TrimSpace(cfg.name) == "" {
		cfg.name = "Speed test"
	}
	if !slices.Contains(cfg.protos(), cfg.defProto) {
		cfg.defProto = cfg.protos()[0]
	}

	geo := newGeoDB(cfg.geoCity, cfg.geoASN)
	go geo.Watch(time.Hour)
	a := newApp(cfg, geo)
	handler := a.routes()

	connCtx := func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, connKey, c)
	}
	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler:           h,
			ConnContext:       connCtx,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       90 * time.Second, // bounds one upload request
			WriteTimeout:      60 * time.Second, // bounds one download request (max 30s)
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    16 << 10,
			// Large HTTP/2 receive windows: the defaults (1 MiB) would cap uploads
			// at roughly 1 MiB per round trip for the whole connection.
			HTTP2: &http.HTTP2Config{
				MaxConcurrentStreams:          100,
				MaxReceiveBufferPerConnection: 64 << 20,
				MaxReceiveBufferPerStream:     16 << 20,
			},
		}
	}

	type stopper interface{ Shutdown(context.Context) error }
	var servers []stopper
	errc := make(chan error, 4)
	var h3srv *http3.Server

	if cfg.httpsAddr != "" {
		cr := &certReloader{cert: cfg.certFile, key: cfg.keyFile}

		srv := newServer(handler)
		// The HTTP/1.1 hostnames offer only http/1.1 during the TLS handshake.
		h1TLS := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: cr.get, NextProtos: []string{"http/1.1"}}
		srv.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: cr.get,
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				if cfg.hostProto(hello.ServerName) == "h1" {
					return h1TLS, nil
				}
				return nil, nil
			},
		}
		if !cfg.h2 {
			srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
		}
		ln, err := net.Listen("tcp", cfg.httpsAddr)
		if err != nil {
			log.Fatal(err)
		}
		servers = append(servers, srv)
		go func() { errc <- srv.ServeTLS(ln, "", "") }()

		if cfg.h3 {
			h3srv = &http3.Server{
				Addr:      cfg.httpsAddr,
				Handler:   handler,
				TLSConfig: http3.ConfigureTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: cr.get}),
				QUICConfig: &quic.Config{
					MaxIncomingStreams:             200,
					InitialStreamReceiveWindow:     2 << 20,
					MaxStreamReceiveWindow:         32 << 20,
					InitialConnectionReceiveWindow: 4 << 20,
					MaxConnectionReceiveWindow:     96 << 20,
					MaxIdleTimeout:                 60 * time.Second,
				},
				IdleTimeout:    120 * time.Second,
				MaxHeaderBytes: 16 << 10,
				ConnContext: func(ctx context.Context, c *quic.Conn) context.Context {
					return context.WithValue(ctx, connKey, c)
				},
			}
			servers = append(servers, h3srv)
			go func() { errc <- h3srv.ListenAndServe() }()
		}
		protos := "HTTP/1.1"
		if cfg.h2 {
			protos += ", HTTP/2"
		}
		if cfg.h3 {
			protos += ", HTTP/3 (udp)"
		}
		log.Printf("https on %s [%s] for %s", cfg.httpsAddr, protos, strings.Join(cfg.hosts(), ", "))
	}

	if cfg.httpAddr != "" {
		srv := newServer(acmeAndRedirect(cfg))
		srv.Addr = cfg.httpAddr
		servers = append(servers, srv)
		go func() { errc <- srv.ListenAndServe() }()
		log.Printf("http on %s (ACME + redirect)", cfg.httpAddr)
	}

	if cfg.plainAddr != "" {
		srv := newServer(handler)
		srv.Addr = cfg.plainAddr
		servers = append(servers, srv)
		go func() { errc <- srv.ListenAndServe() }()
		log.Printf("plain http on %s", cfg.plainAddr)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, quic.ErrServerClosed) {
				log.Fatal(err)
			}
		case s := <-sig:
			if s == syscall.SIGHUP {
				log.Print("SIGHUP: reloading geo databases")
				geo.Reload()
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for _, srv := range servers {
				_ = srv.Shutdown(ctx)
			}
			cancel()
			return
		}
	}
}

// acmeAndRedirect serves HTTP-01 challenges and sends everything else to HTTPS.
func acmeAndRedirect(cfg config) http.Handler {
	known := map[string]bool{}
	for _, h := range cfg.hosts() {
		known[h] = true
	}
	// certbot --webroot writes to <webroot>/.well-known/acme-challenge/<token>
	acme := http.FileServer(http.Dir(cfg.acmeRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
			if strings.HasSuffix(r.URL.Path, "/") { // no directory listings
				http.NotFound(w, r)
				return
			}
			acme.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !known[host] {
			host = cfg.domain
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}

// certReloader re-reads the certificate when the files change (certbot renewals),
// checking at most once a minute. Until a certificate exists, HTTPS handshakes fail
// but the server keeps running so port 80 can answer the first ACME challenge.
type certReloader struct {
	cert, key string
	mu        sync.Mutex
	cur       *tls.Certificate
	mod       time.Time
	checked   time.Time
}

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil && time.Since(c.checked) < time.Minute {
		return c.cur, nil
	}
	c.checked = time.Now()
	st, err := os.Stat(c.cert)
	if err != nil {
		if c.cur != nil {
			return c.cur, nil
		}
		return nil, err
	}
	if c.cur == nil || !st.ModTime().Equal(c.mod) {
		kp, err := tls.LoadX509KeyPair(c.cert, c.key)
		if err != nil {
			log.Printf("tls: %v", err)
			if c.cur != nil {
				return c.cur, nil
			}
			return nil, err
		}
		c.cur, c.mod = &kp, st.ModTime()
		log.Printf("tls: loaded certificate %s", c.cert)
	}
	return c.cur, nil
}

// render fills the site name into a page template ({{NAME}}).
func render(file string, esc func(string) string, name string) []byte {
	b, err := webFS.ReadFile("web/" + file)
	if err != nil {
		panic(err)
	}
	return bytes.ReplaceAll(b, []byte("{{NAME}}"), []byte(esc(name)))
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func staticHandler(cfg config) http.Handler {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	page := render("index.html", html.EscapeString, cfg.name)
	manifest := render("manifest.webmanifest", jsonString, cfg.name)
	connect := []string{"'self'"}
	for _, p := range cfg.protos() {
		connect = append(connect, cfg.base("v4", p), cfg.base("v6", p))
	}
	if cfg.dev { // CSP has no syntax for IPv6 literals such as http://[::1]:8080
		connect = []string{"'self'", "http:", "https:"}
	}
	csp := "default-src 'self'; connect-src " + strings.Join(connect, " ") +
		"; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		switch r.URL.Path {
		case "/", "/index.html":
			h.Set("Cache-Control", "no-cache")
			h.Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
		case "/manifest.webmanifest":
			h.Set("Cache-Control", "no-cache")
			h.Set("Content-Type", "application/manifest+json")
			_, _ = w.Write(manifest)
		default:
			h.Set("Cache-Control", "public, max-age=3600")
			files.ServeHTTP(w, r)
		}
	})
}

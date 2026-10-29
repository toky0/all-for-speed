// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The HTTP-to-HTTPS redirect must never send a visitor to another host, whatever
// the request line looks like.
func TestRedirectStaysOnOurHosts(t *testing.T) {
	cfg := config{domain: "speedtest.example.org"}
	h := acmeAndRedirect(cfg)
	cases := map[string]string{ // request target -> expected Location
		"/x?y=1":                                 "https://speedtest.example.org/x?y=1",
		"/a%2Fb":                                 "https://speedtest.example.org/a%2Fb",
		"//evil.com/x":                           "https://speedtest.example.org//evil.com/x",
		"http:@evil.com/":                        "https://speedtest.example.org/",
		"http://speedtest.example.org@evil.com/": "https://speedtest.example.org/",
		"http://evil.com/path?q=1":               "https://speedtest.example.org/path?q=1",
		"*":                                      "https://speedtest.example.org/",
	}
	for target, want := range cases {
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader("GET " + target + " HTTP/1.1\r\nHost: speedtest.example.org\r\n\r\n")))
		if err != nil {
			continue // rejected before it reaches us: fine
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		got := rec.Header().Get("Location")
		u, err := url.Parse(got)
		if err != nil || u.Hostname() != "speedtest.example.org" || u.User != nil {
			t.Errorf("%q redirected off our host: %q", target, got)
		} else if got != want {
			t.Errorf("%q -> %q, want %q", target, got, want)
		}
	}
	// a known test hostname is kept; an unknown one falls back to the page name
	for host, want := range map[string]string{"v6-h1.speedtest.example.org": "https://v6-h1.speedtest.example.org/", "evil.com": "https://speedtest.example.org/"} {
		req := httptest.NewRequest("GET", "http://"+host+"/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("Host %s -> %q, want %q", host, got, want)
		}
	}
}

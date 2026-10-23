// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bucket is a token bucket counted in bytes. One bucket is shared by all the
// streams of one visitor, so six parallel connections together stay under the cap.
// A nil *bucket means "no limit"; every method is safe to call on nil.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	tokens float64 // may go negative: that's time owed
	last   time.Time
}

func newBucket(bitsPerSec int64) *bucket {
	if bitsPerSec <= 0 {
		return nil
	}
	return &bucket{rate: float64(bitsPerSec) / 8, last: time.Now()}
}

// wait accounts for n bytes and sleeps as long as needed to stay at the rate.
// Bursts are capped at 50 ms worth of data.
func (b *bucket) wait(ctx context.Context, n int) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	now := time.Now()
	b.tokens = math.Min(b.tokens+now.Sub(b.last).Seconds()*b.rate, b.rate*0.05)
	b.last = now
	b.tokens -= float64(n)
	var d time.Duration
	if b.tokens < 0 {
		d = time.Duration(-b.tokens / b.rate * float64(time.Second))
	}
	b.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// chunk picks an I/O size giving about 50 updates a second at the capped rate,
// so throttled transfers flow smoothly instead of in big steps.
func (b *bucket) chunk(max int) int {
	if b == nil {
		return max
	}
	return int(math.Max(16<<10, math.Min(float64(max), b.rate/50)))
}

// limitedReader throttles an upload body. Reading slower makes TCP or QUIC flow
// control push back on the browser, which then sends at the capped rate.
type limitedReader struct {
	ctx   context.Context
	r     io.Reader
	b     *bucket
	chunk int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if len(p) > l.chunk {
		p = p[:l.chunk]
	}
	n, err := l.r.Read(p)
	if n > 0 {
		if werr := l.b.wait(l.ctx, n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}

// parseRate reads a speed in bits per second: "0" or "" (no limit), "500M",
// "2.5G", "800k", "1Gbps", "100 Mbit/s". Suffixes are decimal (1G = 10^9).
func parseRate(s string) (int64, error) {
	t := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	for _, suf := range []string{"bit/s", "bps", "b/s", "bit"} {
		t = strings.TrimSuffix(t, suf)
	}
	if t == "" || t == "0" {
		return 0, nil
	}
	mult := 1.0
	switch t[len(t)-1] {
	case 'k':
		mult = 1e3
	case 'm':
		mult = 1e6
	case 'g':
		mult = 1e9
	case 't':
		mult = 1e12
	}
	if mult != 1 {
		t = t[:len(t)-1]
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("invalid speed %q (examples: 500M, 2.5G, 0 for no limit)", s)
	}
	bps := v * mult
	if bps > 0 && bps < 64e3 {
		return 0, fmt.Errorf("speed %q is below the 64k minimum", s)
	}
	return int64(bps), nil
}

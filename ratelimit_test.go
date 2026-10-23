// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	ok := map[string]int64{
		"": 0, "0": 0, "500M": 500e6, "2.5G": 2.5e9, "800k": 800e3, "1Gbps": 1e9,
		"100 Mbit/s": 100e6, "10g": 10e9, "250mbps": 250e6, "1000000": 1e6,
	}
	for in, want := range ok {
		got, err := parseRate(in)
		if err != nil || got != want {
			t.Errorf("parseRate(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"fast", "-5M", "1.2.3G", "10", "M"} {
		if _, err := parseRate(in); err == nil {
			t.Errorf("parseRate(%q) accepted", in)
		}
	}
}

// Six goroutines sharing one 80 Mbit/s bucket should move about 10 MB/s together.
func TestBucketSharedRate(t *testing.T) {
	b := newBucket(80e6)
	var mu sync.Mutex
	var total int
	ctx := context.Background()
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Since(start) < 1500*time.Millisecond {
				n := b.chunk(1 << 20)
				_ = b.wait(ctx, n)
				mu.Lock()
				total += n
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	rate := float64(total) / time.Since(start).Seconds()
	if rate < 9e6 || rate > 11.5e6 {
		t.Errorf("shared rate %.2f MB/s, want about 10", rate/1e6)
	}
}

func TestLimitedReader(t *testing.T) {
	b := newBucket(40e6) // 5 MB/s
	src := bytes.NewReader(make([]byte, 5<<20))
	start := time.Now()
	n, err := io.Copy(io.Discard, &limitedReader{ctx: context.Background(), r: src, b: b, chunk: b.chunk(1 << 20)})
	el := time.Since(start).Seconds()
	if err != nil || n != 5<<20 {
		t.Fatalf("copied %d, %v", n, err)
	}
	if el < 0.9 || el > 1.3 {
		t.Errorf("5 MiB at 5 MB/s took %.2fs, want about 1.05s", el)
	}
}

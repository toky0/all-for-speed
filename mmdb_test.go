// SPDX-License-Identifier: GPL-3.0-or-later

package main

// Compares the built-in reader with the official MaxMind reader.
//   MMDB_TESTDATA=<dir with .mmdb files> MMDB_EXPECTED=<expected.json> go test -run TestMMDB
// expected.json is produced by tools/gen_expected.py (maxminddb Python package).

import (
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = normalize(e)
		}
		return m
	case []any:
		a := make([]any, len(x))
		for i, e := range x {
			a[i] = normalize(e)
		}
		return a
	case []byte:
		return map[string]any{"$bytes": base64.StdEncoding.EncodeToString(x)}
	}
	return v
}

func TestMMDB(t *testing.T) {
	dir, exp := os.Getenv("MMDB_TESTDATA"), os.Getenv("MMDB_EXPECTED")
	if dir == "" || exp == "" {
		t.Skip("MMDB_TESTDATA / MMDB_EXPECTED not set")
	}
	raw, err := os.ReadFile(exp)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name, cases := range want {
		db, err := openMMDB(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: open: %v", name, err)
			continue
		}
		for ipStr, w := range cases {
			got, err := db.Lookup(netip.MustParseAddr(ipStr))
			if err != nil {
				t.Errorf("%s %s: %v", name, ipStr, err)
				continue
			}
			// round-trip through JSON so number types compare equal
			b, _ := json.Marshal(normalize(got))
			var g any
			_ = json.Unmarshal(b, &g)
			if !reflect.DeepEqual(g, w) && !hasBig(w) {
				t.Errorf("%s %s:\n got  %s\n want %v", name, ipStr, b, w)
			}
			checked++
		}
	}
	t.Logf("%d lookups matched across %d databases", checked, len(want))
}

// Python marks integers above 2^64 as {"$big": ...}; JSON floats can't compare them exactly.
func hasBig(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["$big"]; ok {
			return true
		}
		for _, e := range x {
			if hasBig(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasBig(e) {
				return true
			}
		}
	}
	return false
}

func TestMMDBBadData(t *testing.T) {
	dir := os.Getenv("MMDB_BADDATA")
	if dir == "" {
		t.Skip("MMDB_BADDATA not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.mmdb"))
	for _, f := range files {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", filepath.Base(f), r)
				}
			}()
			db, err := openMMDB(f)
			if err != nil {
				return
			}
			for _, ip := range []string{"1.1.1.1", "::1", "2001:db8::1", "255.255.255.255", "::"} {
				_, _ = db.Lookup(netip.MustParseAddr(ip))
			}
		}()
	}
	t.Logf("%d corrupt files handled without panic", len(files))
}

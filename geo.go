// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"log"
	"net/netip"
	"os"
	"sync"
	"time"
)

type GeoInfo struct {
	ASN         uint   `json:"asn,omitempty"`
	Org         string `json:"org,omitempty"`
	City        string `json:"city,omitempty"`
	Region      string `json:"region,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
}

type geoFile struct {
	path string
	mod  time.Time
	db   *mmdb
}

// geoDB holds the city and ASN databases and reloads them when the files change
// (e.g. after the monthly update timer replaces them).
type geoDB struct {
	mu   sync.RWMutex
	city geoFile
	asn  geoFile
}

func newGeoDB(cityPath, asnPath string) *geoDB {
	g := &geoDB{city: geoFile{path: cityPath}, asn: geoFile{path: asnPath}}
	g.Reload()
	return g
}

func (g *geoDB) Reload() {
	for _, f := range []*geoFile{&g.city, &g.asn} {
		if f.path == "" {
			continue
		}
		st, err := os.Stat(f.path)
		if err != nil {
			g.mu.RLock()
			had := f.db != nil
			g.mu.RUnlock()
			if !had {
				log.Printf("geo: %s not available: %v", f.path, err)
			}
			continue
		}
		g.mu.RLock()
		same := f.db != nil && st.ModTime().Equal(f.mod)
		g.mu.RUnlock()
		if same {
			continue
		}
		db, err := openMMDB(f.path)
		if err != nil {
			log.Printf("geo: loading %s: %v", f.path, err)
			continue
		}
		g.mu.Lock()
		f.db, f.mod = db, st.ModTime()
		g.mu.Unlock()
		log.Printf("geo: loaded %s (%s)", f.path, db.dbType)
	}
}

func (g *geoDB) Watch(every time.Duration) {
	for range time.Tick(every) {
		g.Reload()
	}
}

func (g *geoDB) Lookup(ip netip.Addr) GeoInfo {
	var info GeoInfo
	g.mu.RLock()
	city, asn := g.city.db, g.asn.db
	g.mu.RUnlock()

	if city != nil {
		if rec, err := city.Lookup(ip); err == nil && rec != nil {
			info.City, _ = dig(rec, "city", "names", "en").(string)
			info.Region, _ = dig(rec, "subdivisions", 0, "names", "en").(string)
			info.Country, _ = dig(rec, "country", "names", "en").(string)
			info.CountryCode, _ = dig(rec, "country", "iso_code").(string)
		}
	}
	if asn != nil {
		if rec, err := asn.Lookup(ip); err == nil && rec != nil {
			info.ASN = asUint(dig(rec, "autonomous_system_number"))
			info.Org, _ = dig(rec, "autonomous_system_organization").(string)
		}
	}
	return info
}

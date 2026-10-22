// SPDX-License-Identifier: GPL-3.0-or-later

package main

// Minimal, dependency-free reader for MaxMind DB (.mmdb) files.
// Works with DB-IP Lite and MaxMind GeoLite2 City / ASN databases.
// Spec: https://maxmind.github.io/MaxMind-DB/

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"os"
)

var (
	mmdbMetaMarker = []byte("\xab\xcd\xefMaxMind.com")
	errMMDBCorrupt = errors.New("mmdb: corrupt database")
)

type mmdb struct {
	tree       []byte
	data       []byte
	nodeCount  uint
	recordSize uint
	ipVersion  uint
	ipv4Start  uint
	dbType     string
}

func openMMDB(path string) (*mmdb, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndex(buf, mmdbMetaMarker)
	if i < 0 {
		return nil, fmt.Errorf("mmdb: %s: metadata marker not found", path)
	}
	metaRaw, _, err := mmdbDecoder{b: buf[i+len(mmdbMetaMarker):]}.decode(0, 0)
	if err != nil {
		return nil, fmt.Errorf("mmdb: metadata: %w", err)
	}
	meta, ok := metaRaw.(map[string]any)
	if !ok {
		return nil, errMMDBCorrupt
	}
	db := &mmdb{
		nodeCount:  asUint(meta["node_count"]),
		recordSize: asUint(meta["record_size"]),
		ipVersion:  asUint(meta["ip_version"]),
	}
	db.dbType, _ = meta["database_type"].(string)
	switch db.recordSize {
	case 24, 28, 32:
	default:
		return nil, fmt.Errorf("mmdb: unsupported record size %d", db.recordSize)
	}
	treeSize := db.nodeCount * db.recordSize / 4
	if treeSize+16 > uint(i) {
		return nil, errMMDBCorrupt
	}
	db.tree = buf[:treeSize]
	db.data = buf[treeSize+16 : i]

	if db.ipVersion == 6 {
		node := uint(0)
		for k := 0; k < 96 && node < db.nodeCount; k++ {
			node = db.record(node, 0)
		}
		db.ipv4Start = node
	}
	return db, nil
}

func (db *mmdb) record(node, bit uint) uint {
	b := db.tree
	switch db.recordSize {
	case 24:
		o := node*6 + bit*3
		return uint(b[o])<<16 | uint(b[o+1])<<8 | uint(b[o+2])
	case 28:
		o := node * 7
		if bit == 0 {
			return uint(b[o+3]&0xF0)<<20 | uint(b[o])<<16 | uint(b[o+1])<<8 | uint(b[o+2])
		}
		return uint(b[o+3]&0x0F)<<24 | uint(b[o+4])<<16 | uint(b[o+5])<<8 | uint(b[o+6])
	default: // 32
		o := node*8 + bit*4
		return uint(binary.BigEndian.Uint32(b[o : o+4]))
	}
}

// Lookup returns the decoded record for ip, or nil if the address is not in the database.
func (db *mmdb) Lookup(ip netip.Addr) (any, error) {
	ip = ip.Unmap()
	var addr []byte
	node := uint(0)
	if ip.Is4() {
		if db.ipVersion == 6 {
			node = db.ipv4Start
		}
		a := ip.As4()
		addr = a[:]
	} else {
		if db.ipVersion == 4 {
			return nil, nil
		}
		a := ip.As16()
		addr = a[:]
	}
	for i := 0; i < len(addr)*8 && node < db.nodeCount; i++ {
		bit := uint(addr[i>>3]>>(7-uint(i&7))) & 1
		node = db.record(node, bit)
	}
	switch {
	case node == db.nodeCount:
		return nil, nil
	case node < db.nodeCount:
		return nil, errMMDBCorrupt
	}
	off := node - db.nodeCount - 16
	v, _, err := mmdbDecoder{b: db.data}.decode(off, 0)
	return v, err
}

type mmdbDecoder struct{ b []byte }

func (d mmdbDecoder) need(off, n uint) error {
	if off+n > uint(len(d.b)) || off+n < off {
		return errMMDBCorrupt
	}
	return nil
}

func (d mmdbDecoder) uintN(off, n uint) uint64 {
	var v uint64
	for _, c := range d.b[off : off+n] {
		v = v<<8 | uint64(c)
	}
	return v
}

func (d mmdbDecoder) decode(off uint, depth int) (any, uint, error) {
	if depth > 32 {
		return nil, 0, errMMDBCorrupt
	}
	if err := d.need(off, 1); err != nil {
		return nil, 0, err
	}
	ctrl := d.b[off]
	off++
	typ := uint(ctrl >> 5)

	if typ == 1 { // pointer
		ss := uint(ctrl>>3) & 3
		vvv := uint(ctrl & 7)
		var p uint
		switch ss {
		case 0:
			if err := d.need(off, 1); err != nil {
				return nil, 0, err
			}
			p = vvv<<8 | uint(d.b[off])
			off++
		case 1:
			if err := d.need(off, 2); err != nil {
				return nil, 0, err
			}
			p = (vvv<<16 | uint(d.uintN(off, 2))) + 2048
			off += 2
		case 2:
			if err := d.need(off, 3); err != nil {
				return nil, 0, err
			}
			p = (vvv<<24 | uint(d.uintN(off, 3))) + 526336
			off += 3
		case 3:
			if err := d.need(off, 4); err != nil {
				return nil, 0, err
			}
			p = uint(d.uintN(off, 4))
			off += 4
		}
		v, _, err := d.decode(p, depth+1)
		return v, off, err
	}

	if typ == 0 { // extended type
		if err := d.need(off, 1); err != nil {
			return nil, 0, err
		}
		typ = 7 + uint(d.b[off])
		off++
	}

	size := uint(ctrl & 0x1f)
	switch size {
	case 29:
		if err := d.need(off, 1); err != nil {
			return nil, 0, err
		}
		size = 29 + uint(d.b[off])
		off++
	case 30:
		if err := d.need(off, 2); err != nil {
			return nil, 0, err
		}
		size = 285 + uint(d.uintN(off, 2))
		off += 2
	case 31:
		if err := d.need(off, 3); err != nil {
			return nil, 0, err
		}
		size = 65821 + uint(d.uintN(off, 3))
		off += 3
	}

	switch typ {
	case 2: // UTF-8 string
		if err := d.need(off, size); err != nil {
			return nil, 0, err
		}
		return string(d.b[off : off+size]), off + size, nil
	case 3: // double
		if size != 8 || d.need(off, 8) != nil {
			return nil, 0, errMMDBCorrupt
		}
		return math.Float64frombits(d.uintN(off, 8)), off + 8, nil
	case 4: // bytes
		if err := d.need(off, size); err != nil {
			return nil, 0, err
		}
		return append([]byte(nil), d.b[off:off+size]...), off + size, nil
	case 5, 6, 9: // uint16, uint32, uint64
		if size > 8 || d.need(off, size) != nil {
			return nil, 0, errMMDBCorrupt
		}
		return d.uintN(off, size), off + size, nil
	case 10: // uint128
		if size > 16 || d.need(off, size) != nil {
			return nil, 0, errMMDBCorrupt
		}
		return new(big.Int).SetBytes(d.b[off : off+size]), off + size, nil
	case 8: // int32
		if size > 4 || d.need(off, size) != nil {
			return nil, 0, errMMDBCorrupt
		}
		v := uint32(d.uintN(off, size))
		if size > 0 && size < 4 && v&(1<<(size*8-1)) != 0 {
			v |= ^uint32(0) << (size * 8)
		}
		return int64(int32(v)), off + size, nil
	case 7: // map
		m := make(map[string]any, size)
		for k := uint(0); k < size; k++ {
			key, next, err := d.decode(off, depth+1)
			if err != nil {
				return nil, 0, err
			}
			ks, ok := key.(string)
			if !ok {
				return nil, 0, errMMDBCorrupt
			}
			val, next2, err := d.decode(next, depth+1)
			if err != nil {
				return nil, 0, err
			}
			m[ks] = val
			off = next2
		}
		return m, off, nil
	case 11: // array
		a := make([]any, 0, min(size, 1024))
		for k := uint(0); k < size; k++ {
			val, next, err := d.decode(off, depth+1)
			if err != nil {
				return nil, 0, err
			}
			a = append(a, val)
			off = next
		}
		return a, off, nil
	case 14: // boolean (value stored in size)
		return size != 0, off, nil
	case 15: // float
		if size != 4 || d.need(off, 4) != nil {
			return nil, 0, errMMDBCorrupt
		}
		return float64(math.Float32frombits(uint32(d.uintN(off, 4)))), off + 4, nil
	}
	return nil, 0, fmt.Errorf("mmdb: unsupported data type %d", typ)
}

func asUint(v any) uint {
	switch x := v.(type) {
	case uint64:
		return uint(x)
	case int64:
		if x >= 0 {
			return uint(x)
		}
	}
	return 0
}

// dig walks nested maps/arrays: dig(rec, "city", "names", "en"), dig(rec, "subdivisions", 0, "names", "en").
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

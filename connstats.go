// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

// connSnap is a point-in-time reading of a connection's transport counters.
//
//	TCP  (HTTP/1.1, HTTP/2): lost = segments retransmitted, sent = data segments sent
//	QUIC (HTTP/3):           lost = packets declared lost,   sent = packets sent
type connSnap struct {
	at       time.Time
	rtt      time.Duration
	lost     uint64
	sent     uint64
	haveSent bool // false on old kernels without tcpi_data_segs_out
	mss      uint32
	quic     bool
}

func snapshot(c any) (connSnap, bool) {
	switch x := c.(type) {
	case *quic.Conn:
		s := x.ConnectionStats()
		return connSnap{at: time.Now(), rtt: s.SmoothedRTT, lost: s.PacketsLost,
			sent: s.PacketsSent, haveSent: true, quic: true}, true
	case net.Conn:
		st, ok := tcpStats(x)
		if !ok {
			return connSnap{}, false
		}
		return connSnap{
			at:       time.Now(),
			rtt:      time.Duration(st.RTTus) * time.Microsecond,
			lost:     uint64(st.TotalRetrans),
			sent:     uint64(st.DataSegsOut),
			haveSent: st.HaveSegs,
			mss:      st.MSS,
		}, true
	}
	return connSnap{}, false
}

// connAgg collects every download stream of one test that ran on one connection.
// With HTTP/2 and HTTP/3 several streams share a connection, so counters are taken
// once per connection (earliest snapshot to latest), never summed per stream.
type connAgg struct {
	first, last connSnap
	bytes       int64
}

func (a *connAgg) lostAndSent() (lost, sent uint64) {
	if a.last.lost >= a.first.lost {
		lost = a.last.lost - a.first.lost
	}
	if a.last.haveSent {
		if a.last.sent >= a.first.sent {
			sent = a.last.sent - a.first.sent
		}
		return lost, sent
	}
	mss := uint64(a.last.mss) // old kernels: estimate segments from bytes
	if mss == 0 {
		mss = 1448
	}
	return lost, uint64(a.bytes) / mss
}

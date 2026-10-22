// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux && (amd64 || arm64)

package main

import (
	"crypto/tls"
	"encoding/binary"
	"net"
	"syscall"
	"unsafe"
)

// Offsets in the kernel's struct tcp_info (include/uapi/linux/tcp.h). The layout is
// append-only, so these hold for every kernel that reports at least that many bytes.
const (
	tiSndMss       = 16
	tiRTT          = 68  // smoothed RTT, µs
	tiTotalRetrans = 100 // segments retransmitted over the connection's life
	tiDataSegsOut  = 156 // data segments sent, Linux 4.6+
)

// tcpStats reads TCP_INFO for the connection behind an HTTP/1.1 or HTTP/2 request.
func tcpStats(c net.Conn) (tcpStat, bool) {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return tcpStat{}, false
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return tcpStat{}, false
	}
	var buf [256]byte
	l := uint32(len(buf))
	var serr syscall.Errno
	cerr := raw.Control(func(fd uintptr) {
		_, _, serr = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd,
			syscall.SOL_TCP, syscall.TCP_INFO,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&l)), 0)
	})
	if cerr != nil || serr != 0 || l < tiTotalRetrans+4 {
		return tcpStat{}, false
	}
	u32 := func(off int) uint32 { return binary.NativeEndian.Uint32(buf[off : off+4]) }
	st := tcpStat{
		RTTus:        u32(tiRTT),
		TotalRetrans: u32(tiTotalRetrans),
		MSS:          u32(tiSndMss),
	}
	if l >= tiDataSegsOut+4 {
		st.DataSegsOut, st.HaveSegs = u32(tiDataSegsOut), true
	}
	return st, true
}

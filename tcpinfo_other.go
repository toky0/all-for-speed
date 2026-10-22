// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !(linux && (amd64 || arm64))

package main

import "net"

func tcpStats(net.Conn) (tcpStat, bool) { return tcpStat{}, false }

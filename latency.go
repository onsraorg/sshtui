package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type latencyResult struct {
	RTT    time.Duration
	Method string
	Err    error
	At     time.Time
}

type latencyState struct {
	Probing bool
	HasData bool
	Result  latencyResult
}

var icmpSeq atomic.Uint64

func probeServerLatency(parent context.Context, host string, port int) latencyResult {
	ctx, cancel := context.WithTimeout(parent, 1800*time.Millisecond)
	defer cancel()

	addr, err := resolveTargetIP(ctx, host)
	if err != nil {
		return latencyResult{Err: err, At: time.Now()}
	}

	icmpCtx, icmpCancel := context.WithTimeout(ctx, 600*time.Millisecond)
	rtt, icmpErr := probeICMPAddr(icmpCtx, addr)
	icmpCancel()
	if icmpErr == nil {
		return latencyResult{RTT: rtt, Method: "ICMP", At: time.Now()}
	}
	if ctx.Err() != nil {
		return latencyResult{Err: ctx.Err(), At: time.Now()}
	}

	tcpCtx, tcpCancel := context.WithTimeout(ctx, 1100*time.Millisecond)
	defer tcpCancel()
	rtt, tcpErr := probeTCPAddr(tcpCtx, addr, port)
	if tcpErr == nil {
		return latencyResult{RTT: rtt, Method: "TCP", At: time.Now()}
	}
	return latencyResult{Err: tcpErr, At: time.Now()}
}

func resolveTargetIP(ctx context.Context, host string) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil && addr.IsValid() {
		return addr, nil
	}
	dnsCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(dnsCtx, "ip", host)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if a.IsValid() {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("未解析到有效地址: %s", host)
}

func probeICMPAddr(ctx context.Context, addr netip.Addr) (time.Duration, error) {
	addr = addr.Unmap()
	domain, proto := unix.AF_INET, unix.IPPROTO_ICMP
	reqType, repType := byte(8), byte(0)
	if addr.Is6() {
		domain, proto = unix.AF_INET6, unix.IPPROTO_ICMPV6
		reqType, repType = 128, 129
	}

	fd, err := unix.Socket(domain, unix.SOCK_DGRAM, proto)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(fd), "sshtui-icmp")
	conn, err := net.FilePacketConn(f)
	_ = f.Close()
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	seq := icmpSeq.Add(1)
	var pkt [16]byte
	pkt[0] = reqType
	pkt[1] = 0
	binary.BigEndian.PutUint16(pkt[4:6], uint16(os.Getpid()))
	binary.BigEndian.PutUint16(pkt[6:8], uint16(seq))
	binary.BigEndian.PutUint64(pkt[8:16], uint64(time.Now().UnixNano())^seq)
	binary.BigEndian.PutUint16(pkt[2:4], icmpChecksum(pkt[:]))

	dst := &net.UDPAddr{IP: net.IP(addr.AsSlice()), Zone: addr.Zone()}
	start := time.Now()
	if _, err := conn.WriteTo(pkt[:], dst); err != nil {
		return 0, err
	}

	var buf [128]byte
	for {
		n, peer, err := conn.ReadFrom(buf[:])
		if err != nil {
			return 0, err
		}
		rtt := time.Since(start)
		if udpPeer, ok := peer.(*net.UDPAddr); ok {
			if peerAddr, ok := netip.AddrFromSlice(udpPeer.IP); ok && peerAddr.Unmap() != addr {
				continue
			}
		}
		reply := buf[:n]
		// macOS SOCK_DGRAM IPPROTO_ICMP includes the outer IPv4 header on read.
		if addr.Is4() && len(reply) >= 20 && reply[0]>>4 == 4 {
			ihl := int(reply[0]&0x0f) * 4
			if ihl >= 20 && len(reply) >= ihl+16 {
				reply = reply[ihl:]
			}
		}
		if len(reply) >= 16 && reply[0] == repType && reply[1] == 0 && bytes.Equal(reply[8:16], pkt[8:16]) {
			return rtt, nil
		}
	}
}

func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func probeTCPAddr(ctx context.Context, addr netip.Addr, port int) (time.Duration, error) {
	target := net.JoinHostPort(addr.String(), strconv.Itoa(port))
	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return 0, err
	}
	rtt := time.Since(start)
	_ = conn.Close()
	return rtt, nil
}

func formatLatencyMs(d time.Duration) string {
	if d < time.Millisecond {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", max(1, d.Round(time.Millisecond).Milliseconds()))
}

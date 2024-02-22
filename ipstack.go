package relraw

import (
	"fmt"
	"math/rand"
	"net/netip"

	"github.com/lysShub/relraw/internal/config/ipstack"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// build ip header
type IPStack struct {
	option    ipstack.Options
	network   tcpip.NetworkProtocolNumber
	transport tcpip.TransportProtocolNumber

	// init ip header
	in, out []byte

	// pseudo header checksum without totalLen
	psoSum1 uint16
}

// UpdateChecksum update tcp/udp checksum, the old
// checksum is without-pseudo-checksum
func UpdateChecksum(o *ipstack.Options) {
	o.Checksum = ipstack.UpdateChecksumWithoutPseudo
}

// ReCalcChecksum re-calculate tcp/udp checksum
func ReCalcChecksum(o *ipstack.Options) {
	o.Checksum = ipstack.ReCalcChecksum
}

// NotCalcChecksum not change tcp/udp checksum
func NotCalcChecksum(o *ipstack.Options) {
	o.Checksum = ipstack.NotCalcChecksum
}

// NotCalcIPChecksum not set ip4 checksum
func NotCalcIPChecksum(o *ipstack.Options) {
	o.CalcIPChecksum = false
}

func NewIPStack(laddr, raddr netip.Addr, proto tcpip.TransportProtocolNumber, opts ...ipstack.Option) (*IPStack, error) {

	switch proto {
	case header.TCPProtocolNumber, header.UDPProtocolNumber:
	default:
		return nil, fmt.Errorf("not support transport protocol number %d", proto)
	}

	var s = &IPStack{
		option:    ipstack.Default,
		transport: proto,
	}
	for _, opt := range opts {
		opt(&s.option)
	}

	if laddr.Is4() {
		s.network = header.IPv4ProtocolNumber
		s.in, s.psoSum1 = initHdr(raddr, laddr, proto)
		s.out, s.psoSum1 = initHdr(laddr, raddr, proto)
	} else {
		s.network = header.IPv6ProtocolNumber
		s.in, s.psoSum1 = initHdr6(raddr, laddr, proto)
		s.out, s.psoSum1 = initHdr6(laddr, raddr, proto)
	}
	return s, nil
}

func initHdr(src, dst netip.Addr, proto tcpip.TransportProtocolNumber) ([]byte, uint16) {
	f := &header.IPv4Fields{
		TOS:            0,
		TotalLength:    0, // dynamic
		ID:             0, // dynamic
		Flags:          0,
		FragmentOffset: 0,
		TTL:            64,
		Protocol:       uint8(proto),
		Checksum:       0,
		SrcAddr:        tcpip.AddrFrom4(src.As4()),
		DstAddr:        tcpip.AddrFrom4(dst.As4()),
		Options:        nil,
	}

	b := header.IPv4(make([]byte, header.IPv4MinimumSize))
	b.Encode(f)
	return []byte(b), header.PseudoHeaderChecksum(proto, f.SrcAddr, f.DstAddr, 0)
}

func initHdr6(src, dst netip.Addr, proto tcpip.TransportProtocolNumber) ([]byte, uint16) {
	f := &header.IPv6Fields{
		TrafficClass:      0,
		FlowLabel:         0,
		PayloadLength:     0, // dynamic
		TransportProtocol: proto,
		HopLimit:          128,
		SrcAddr:           tcpip.AddrFrom16(src.As16()),
		DstAddr:           tcpip.AddrFrom16(dst.As16()),
	}

	b := header.IPv6(make([]byte, header.IPv6MinimumSize))
	b.Encode(f)
	return []byte(b), header.PseudoHeaderChecksum(proto, f.SrcAddr, f.DstAddr, 0)
}

func (i *IPStack) Size() int {
	if i.network == header.IPv4ProtocolNumber {
		return header.IPv4MinimumSize
	} else {
		return header.IPv6MinimumSize
	}
}

func (i *IPStack) AttachInbound(p *Packet) {
	p.Attach(i.in)
	i.calcTransportChecksum(p.Data())
}

func (i *IPStack) AttachOutbound(p *Packet) {
	p.Attach(i.out)
	i.calcTransportChecksum(p.Data())
}

func (i *IPStack) calcTransportChecksum(ip []byte) {
	psosum, p := i.attach(ip)

	switch i.transport {
	case header.TCPProtocolNumber:
		tcphdr := header.TCP(p)
		var sum uint16
		switch i.option.Checksum {
		case ipstack.UpdateChecksumWithoutPseudo:
			sum = ^tcphdr.Checksum()
		case ipstack.ReCalcChecksum:
			tcphdr.SetChecksum(0)
			sum = checksum.Checksum(tcphdr, 0)
		case ipstack.NotCalcChecksum:
			return
		default:
			panic("")
		}
		tcphdr.SetChecksum(^checksum.Combine(psosum, sum))
	case header.UDPProtocolNumber:
		udphdr := header.UDP(p)
		var sum uint16
		switch i.option.Checksum {
		case ipstack.UpdateChecksumWithoutPseudo:
			sum = ^udphdr.Checksum()
		case ipstack.ReCalcChecksum:
			udphdr.SetChecksum(0)
			sum = checksum.Checksum(udphdr, 0)
		case ipstack.NotCalcChecksum:
			return
		default:
			panic("")
		}
		udphdr.SetChecksum(^checksum.Combine(psosum, sum))
	}
}

func (i *IPStack) attach(ip []byte) (uint16, []byte) {
	if i.network == header.IPv4ProtocolNumber {
		iphdr := header.IPv4(ip)
		iphdr.SetTotalLength(uint16(len(iphdr)))
		iphdr.SetID(uint16(rand.Uint32()))
		if i.option.CalcIPChecksum {
			iphdr.SetChecksum(^iphdr.CalculateChecksum())
		}

		var psosum uint16
		switch i.option.Checksum {
		case ipstack.ReCalcChecksum, ipstack.UpdateChecksumWithoutPseudo:
			psosum = checksum.Combine(i.psoSum1, uint16(len(iphdr.Payload())))
		case ipstack.NotCalcChecksum:
		default:
			panic("")
		}
		return psosum, iphdr.Payload()
	} else {
		iphdr := header.IPv6(ip)
		n := uint16(len(ip) - header.IPv6MinimumSize)
		iphdr.SetPayloadLength(n)

		var psosum uint16
		switch i.option.Checksum {
		case ipstack.ReCalcChecksum, ipstack.UpdateChecksumWithoutPseudo:
			psosum = checksum.Combine(i.psoSum1, n)
		case ipstack.NotCalcChecksum:
		default:
			panic("")
		}
		return psosum, iphdr.Payload()
	}
}
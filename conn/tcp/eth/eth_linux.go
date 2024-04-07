//go:build linux
// +build linux

package eth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/lysShub/sockit/conn"
	iconn "github.com/lysShub/sockit/conn/internal"
	itcp "github.com/lysShub/sockit/conn/tcp/internal"
	"github.com/lysShub/sockit/helper"
	"github.com/lysShub/sockit/helper/bpf"
	"github.com/lysShub/sockit/helper/ipstack"
	"github.com/lysShub/sockit/packet"
	"github.com/lysShub/sockit/route"
	"github.com/lysShub/sockit/socket/eth"
	"github.com/lysShub/sockit/test"
	"github.com/lysShub/sockit/test/debug"
	"github.com/mdlayher/arp"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

type Listener struct {
	addr netip.AddrPort
	cfg  *conn.Config

	tcp *net.TCPListener

	raw *net.IPConn

	// AddrPort:ISN
	conns map[netip.AddrPort]uint32

	closedConns   []itcp.ClosedTCPInfo
	closedConnsMu sync.RWMutex
}

var _ conn.Listener = (*Listener)(nil)

func ListenEth(laddr netip.AddrPort, opts ...conn.Option) (*Listener, error) {
	var l = &Listener{
		cfg:   conn.Options(opts...),
		conns: make(map[netip.AddrPort]uint32, 16),
	}

	var err error
	l.tcp, l.addr, err = iconn.ListenLocal(laddr, l.cfg.UsedPort)
	if err != nil {
		l.Close()
		return nil, err
	}

	l.raw, err = net.ListenIP(
		"ip:tcp",
		&net.IPAddr{IP: l.addr.Addr().AsSlice(), Zone: laddr.Addr().Zone()},
	)
	if err != nil {
		l.Close()
		return nil, err
	}

	raw, err := l.raw.SyscallConn()
	if err != nil {
		l.Close()
		return nil, err
	}

	if err = bpf.SetRawBPF(
		raw,
		bpf.FilterDstPortAndSynFlag(l.addr.Port()),
	); err != nil {
		l.Close()
		return nil, err
	}

	return l, nil
}

func (l *Listener) Close() error {
	var err error
	if l.tcp != nil {
		if e := l.tcp.Close(); e != nil {
			err = e
		}
	}
	if l.raw != nil {
		if e := l.raw.Close(); e != nil {
			err = e
		}
	}
	return err
}

func (l *Listener) Addr() netip.AddrPort {
	return l.addr
}

// todo: not support private proto that not start with tcp SYN flag
func (l *Listener) Accept() (conn.RawConn, error) {
	var min, max = itcp.TcpSynSizeRange(l.addr.Addr().Is4())

	var ip = make([]byte, max)
	for {
		n, err := l.raw.Read(ip[:max])
		if err != nil {
			return nil, err
		} else if n < min {
			return nil, fmt.Errorf("recved invalid ip packet, bytes %d", n)
		}
		l.purgeDeleted()

		var raddr netip.AddrPort
		var isn uint32
		switch header.IPVersion(ip) {
		case 4:
			iphdr := header.IPv4(ip[:n])
			tcphdr := header.TCP(iphdr.Payload())
			raddr = netip.AddrPortFrom(netip.AddrFrom4(iphdr.SourceAddress().As4()), tcphdr.SourcePort())
			isn = tcphdr.SequenceNumber()
		case 6:
			iphdr := header.IPv6(ip[:n])
			tcphdr := header.TCP(iphdr.Payload())
			raddr = netip.AddrPortFrom(netip.AddrFrom4(iphdr.SourceAddress().As4()), tcphdr.SourcePort())
			isn = tcphdr.SequenceNumber()
		default:
			continue
		}

		newConn := false
		old, ok := l.conns[raddr]
		if !ok || (ok && old != isn) {
			l.conns[raddr] = isn
			newConn = true
		}

		if newConn {
			c := newConnect(
				l.addr, raddr, isn,
				l.deleteConn, l.cfg.NotTrunc, l.cfg.CtxPeriod,
			)
			return c, c.init(l.cfg)
		}
	}
}

func (l *Listener) purgeDeleted() {
	l.closedConnsMu.Lock()
	defer l.closedConnsMu.Unlock()

	for i := len(l.closedConns) - 1; i >= 0; i-- {
		c := l.closedConns[i]

		if time.Since(c.DeleteAt) > time.Minute {
			isn, ok := l.conns[c.Raddr]
			if ok && isn == c.ISN {
				delete(l.conns, c.Raddr)
			}

			l.closedConns = l.closedConns[:i-1]
		} else {
			break
		}
	}
}

func (l *Listener) deleteConn(raddr netip.AddrPort, isn uint32) error {
	if l == nil {
		return nil
	}
	l.closedConnsMu.Lock()
	defer l.closedConnsMu.Unlock()

	l.closedConns = append(
		l.closedConns,
		itcp.ClosedTCPInfo{
			DeleteAt: time.Now(),
			Raddr:    raddr,
			ISN:      isn,
		},
	)

	// desc
	sort.Slice(l.closedConns, func(i, j int) bool {
		it := l.closedConns[i].DeleteAt
		jt := l.closedConns[i].DeleteAt
		return it.After(jt)
	})
	return nil
}

type Conn struct {
	laddr, raddr netip.AddrPort
	isn          uint32

	// todo: set buff 0
	tcp *net.TCPListener

	raw     *eth.Conn
	ipstack *ipstack.IPStack
	gateway net.HardwareAddr

	ctxPeriod     time.Duration
	completeCheck bool
	closeFn       itcp.CloseCallback
}

var _ conn.RawConn = (*Conn)(nil)

func Connect(laddr, raddr netip.AddrPort, opts ...conn.Option) (*Conn, error) {
	cfg := conn.Options(opts...)
	var c = newConnect(
		laddr, raddr, 0,
		nil, cfg.NotTrunc, cfg.CtxPeriod,
	)

	var err error
	c.tcp, c.laddr, err = iconn.ListenLocal(laddr, cfg.UsedPort)
	if err != nil {
		c.Close()
		return nil, err
	}

	return c, c.init(cfg)
}

func newConnect(laddr, raddr netip.AddrPort, isn uint32, closeCall itcp.CloseCallback, complete bool, ctxPeriod time.Duration) *Conn {
	return &Conn{
		laddr:         laddr,
		raddr:         raddr,
		isn:           isn,
		closeFn:       closeCall,
		completeCheck: complete,
		ctxPeriod:     ctxPeriod,
	}
}

func (c *Conn) init(cfg *conn.Config) (err error) {
	defer func() {
		if err != nil {
			c.Close()
		}
	}()

	entry, err := route.GetBestInterface(c.raddr.Addr())
	if err != nil {
		return err
	}

	// set gateway mac address
	var ifi *net.Interface
	if !entry.Next.IsValid() {
		// is on loopback
		return errors.New("not support loopback connect")

		lo, err := helper.LoopbackInterface()
		if err != nil {
			return err
		}
		ifi, err = net.InterfaceByName(lo)
		if err != nil {
			return errors.WithStack(err)
		}
		c.gateway = net.HardwareAddr(make([]byte, 6))
	} else {
		if debug.Debug() {
			require.Equal(test.T(), c.laddr.Addr(), entry.Addr)
		}
		ifi, err = net.InterfaceByIndex(int(entry.Interface))
		if err != nil {
			return errors.WithStack(err)
		}

		// get gatway hardware address
		if client, err := arp.Dial(c.raw.Interface()); err != nil {
			return errors.WithStack(err)
		} else {
			defer client.Close()
			if err = client.SetDeadline(time.Now().Add(time.Second * 3)); err != nil {
				return errors.WithStack(err)
			}

			c.gateway, err = client.Resolve(entry.Next)
			if err != nil {
				return errors.WithStack(err)
			}
		}
	}

	// create eth conn and set bpf filter
	c.raw, err = eth.Listen("eth:ip4", ifi)
	if err != nil {
		return err
	}
	if err := bpf.SetRawBPF(
		c.raw.SyscallConn(),
		bpf.FilterEndpoint(header.TCPProtocolNumber, c.raddr, c.laddr),
	); err != nil {
		return err
	}

	if c.ipstack, err = ipstack.New(
		c.laddr.Addr(), c.raddr.Addr(),
		header.TCPProtocolNumber, cfg.IPStack.Unmarshal(),
	); err != nil {
		return err
	}
	return nil
}

func (c *Conn) Close() (err error) {
	if c.tcp != nil {
		if e := c.tcp.Close(); e != nil {
			err = e
		}
	}
	if c.raw != nil {
		if e := c.raw.Close(); e != nil {
			err = e
		}
	}
	if c.closeFn != nil {
		if e := c.closeFn(c.raddr, c.isn); e != nil {
			err = e
		}
	}
	return err
}

func (c *Conn) Read(ctx context.Context, p *packet.Packet) (err error) {
	b := p.Data()
	b = b[:cap(b)]

	var n int
	for {
		err = c.raw.SetReadDeadline(time.Now().Add(c.ctxPeriod))
		if err != nil {
			return err
		}

		n, _, err = c.raw.Recvfrom(b, 0)
		if err == nil {
			break
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				continue
			}
		} else {
			return err
		}
	}
	p.SetLen(n)
	if debug.Debug() {

		iphdr := header.IPv4(p.Data())
		tcphdr := header.TCP(iphdr.Payload())
		fmt.Printf(
			"recv %s:%d-->%s:%d	%s\n",
			iphdr.SourceAddress(), tcphdr.SourcePort(),
			iphdr.DestinationAddress(), tcphdr.DestinationPort(),
			tcphdr.Flags(),
		)
		fmt.Println(iphdr)
		fmt.Println()

		test.ValidIP(test.T(), p.Data())
	}
	switch header.IPVersion(b) {
	case 4:
		if !iconn.CompleteCheck(true, p.Data()) {
			return errors.WithStack(io.ErrShortBuffer) // todo: Temporary
		}
		p.SetHead(p.Head() + int(header.IPv4(b).HeaderLength()))
	case 6:
		if !iconn.CompleteCheck(false, p.Data()) {
			return errors.WithStack(io.ErrShortBuffer)
		}
		p.SetHead(p.Head() + header.IPv6MinimumSize)
	}
	return nil
}

func (c *Conn) Write(ctx context.Context, p *packet.Packet) (err error) {
	c.ipstack.AttachOutbound(p)
	if debug.Debug() {
		iphdr := header.IPv4(p.Data())
		tcphdr := header.TCP(iphdr.Payload())
		fmt.Printf(
			"send %s:%d-->%s:%d	%s\n",
			iphdr.SourceAddress(), tcphdr.SourcePort(),
			iphdr.DestinationAddress(), tcphdr.DestinationPort(),
			tcphdr.Flags(),
		)

		test.ValidIP(test.T(), p.Data())
	}

	err = c.raw.Sendto(p.Data(), 0, c.gateway)
	return err
}

func (c *Conn) Inject(ctx context.Context, p *packet.Packet) (err error) {
	return errors.New("todo: not support, need test")

	// c.ipstack.AttachInbound(p)
	// if debug.Debug() {
	// 	test.ValidIP(test.T(), p.Data())
	// }
	// // p.Attach(c.outEthdr[:])
	// _, err = c.raw.Write(p.Data())
	// return err
}

func (c *Conn) LocalAddr() netip.AddrPort  { return c.laddr }
func (c *Conn) RemoteAddr() netip.AddrPort { return c.raddr }
func (c *Conn) Raw() *eth.Conn             { return c.raw }
package netif

import (
	"log/slog"
	"net/netip"

	"github.com/soypat/seqs/stacks"
)

type AddrMethod uint8

const (
	_ AddrMethod = iota
	AddrMethodDHCP
	AddrMethodManual
)

const queueSize = 2
const maxRetriesBeforeDropping = 1

type Engine struct {
	nic      InterfaceEthPoller
	dnssv    []netip.Addr
	gw       netip.Addr
	prefix   netip.Prefix
	dhcpc    stacks.DHCPClient
	s        stacks.PortStack
	log      *slog.Logger
	queue    [queueSize][]byte
	qlen     [queueSize]int
	qretries [queueSize]int
}

type EngineConfig struct {
	MaxOpenPortsUDP uint16
	MaxOpenPortsTCP uint16
	// AddrMethod selects the mode in which the stack address is chosen or obtained.
	AddrMethod AddrMethod
	// Netmask is the bit prefix length of addresses on the physical network.
	// It is used if AddrMethod is set to Manual.
	Netmask uint8
	// Address is used to request a specific DHCP address
	// or set the address of the stack manually.
	Address netip.Addr
	// Gateway manually sets the IP address of the router
	// that can send packets out of the physical network and omits getting
	// the Gateway over DHCP.
	Gateway netip.Addr
	// DNSServers manually sets the DNS servers and omits getting them over DHCP.
	DNSServers []netip.Addr
	Logger     *slog.Logger
}

func NewEngine(nic InterfaceEthPoller, cfg EngineConfig) *Engine {
	if nic == nil {
		panic("nic is nil")
	}
	mac, err := nic.HardwareAddr6()
	if err != nil {
		println("mac get failed")
		panic(err)
	}
	mtu := nic.MTU()
	if mtu < 536 {
		panic("small MTU")
	}
	s := stacks.NewPortStack(stacks.PortStackConfig{
		MaxOpenPortsUDP: int(cfg.MaxOpenPortsUDP),
		MaxOpenPortsTCP: int(cfg.MaxOpenPortsTCP),
		Logger:          cfg.Logger,
		MAC:             mac,
		MTU:             uint16(mtu),
	})

	e := &Engine{
		nic: nic,
		s:   *s,
	}
	queuebuf := make([]byte, mtu*queueSize)
	// var queue [queueSize][]byte
	for i := range e.queue {
		e.queue[i] = queuebuf[i*mtu : (i+1)*mtu]
	}

	return e
}

func (e *Engine) markPktSent(i int) {
	slice := e.queue[i]
	for j := range slice {
		// Once thoroughly tested we can remove this memset.
		slice[j] = 0
	}
	e.qlen[i] = 0
	e.qretries[i] = 0
}

// persistentNICLoop is called once on Engine creation and runs throughout the engine's life (for now forever).
func (e *Engine) poll() (gotPacket, sentPacket bool, err error) {
	nic := e.nic
	stack := e.s

	// Poll for incoming packets.
	var errGot, errSent error
	// RXPOLL:
	gotPacket, errGot = nic.PollOne()

	for i := range e.queue {
		if e.qretries[i] != 0 {
			continue // Packet currently queued for retransmission.
		}

		buf := e.queue[i]
		e.qlen[i], errSent = stack.HandleEth(buf)
		if errSent != nil || e.qlen[i] == 0 {
			// Failure handling or no packet to send.
			break
		}
	}
	stallTx := e.qlen == [queueSize]int{}
	if !stallTx {
		// We have work to do, send out packet.
		// Send queued packets.
		for i := range e.queue {
			n := e.qlen[i]
			if n <= 0 {
				continue
			}
			errSent = nic.SendEth(e.queue[i][:n])
			if err != nil {
				// Queue packet for retransmission.
				e.qretries[i]++
				if e.qretries[i] > maxRetriesBeforeDropping {
					e.markPktSent(i)
					slog.Error("nic:drop-pkt", slog.String("err", err.Error()))
				}
			} else {
				e.markPktSent(i)
			}
		}
	}
	sentPacket = !stallTx
	if errGot != nil {
		err = errGot
	} else {
		err = errSent
	}
	return gotPacket, sentPacket, err
}

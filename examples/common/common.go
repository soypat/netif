package common

import (
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"github.com/soypat/netif"
	"github.com/soypat/seqs/eth/dhcp"
	"github.com/soypat/seqs/eth/dns"
	"github.com/soypat/seqs/stacks"
)

type SetupConfig struct {
	// DHCP requested hostname.
	Hostname string
	// DHCP requested IP address. On failing to find DHCP server is used as static IP.
	RequestedIP string
	Logger      *slog.Logger
	// Number of UDP ports to open for the stack. (we'll actually open one more than this for DHCP)
	UDPPorts uint16
	// Number of TCP ports to open for the stack.
	TCPPorts uint16
	// DHCPTimeout is the maximum time to wait for DHCP to complete.
	DHCPTimeout time.Duration
}

func SetupWithDHCP(dev netif.InterfaceEthPoller, cfg SetupConfig) (*stacks.DHCPClient, *stacks.PortStack, error) {
	if cfg.DHCPTimeout <= 0 {
		cfg.DHCPTimeout = 8 * time.Second
	}
	cfg.UDPPorts++ // Add extra UDP port for DHCP client.
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
			Level: slog.Level(127), // Make temporary logger that does no logging.
		}))
	}
	var err error
	var reqAddr netip.Addr
	if cfg.RequestedIP != "" {
		reqAddr, err = netip.ParseAddr(cfg.RequestedIP)
		if err != nil {
			return nil, nil, err
		}
	}
	hw6, err := dev.HardwareAddr6()
	if err != nil {
		return nil, nil, err
	}
	stack := stacks.NewPortStack(stacks.PortStackConfig{
		MAC:             hw6,
		MaxOpenPortsUDP: int(cfg.UDPPorts),
		MaxOpenPortsTCP: int(cfg.TCPPorts),
		MTU:             uint16(dev.MTU()),
		Logger:          logger,
	})

	dev.RecvEthHandle(stack.RecvEth)

	// Begin asynchronous packet handling.
	go nicLoop(dev, stack)

	// Perform DHCP request.
	dhcpClient := stacks.NewDHCPClient(stack, dhcp.DefaultClientPort)
	err = dhcpClient.BeginRequest(stacks.DHCPRequestConfig{
		RequestedAddr: reqAddr,
		Xid:           uint32(time.Now().Nanosecond()),
		Hostname:      cfg.Hostname,
	})
	if err != nil {
		return nil, stack, err
	}
	i := 0
	deadline := time.Now().Add(cfg.DHCPTimeout)
	for !dhcpClient.IsDone() {
		i++
		logger.Info("DHCP ongoing...")
		time.Sleep(time.Second / 2)
		if time.Until(deadline) <= 0 {
			if !reqAddr.IsValid() {
				return dhcpClient, stack, errors.New("DHCP did not complete and no static IP was requested")
			}
			logger.Info("DHCP did not complete, assigning static IP", slog.String("ip", cfg.RequestedIP))
			stack.SetAddr(reqAddr)
			return dhcpClient, stack, nil
		}
	}
	var primaryDNS netip.Addr
	dnsServers := dhcpClient.DNSServers()
	if len(dnsServers) > 0 {
		primaryDNS = dnsServers[0]
	}
	ip := dhcpClient.Offer()
	logger.Info("DHCP complete",
		slog.Uint64("cidrbits", uint64(dhcpClient.CIDRBits())),
		slog.String("ourIP", ip.String()),
		slog.String("dns", primaryDNS.String()),
		slog.String("broadcast", dhcpClient.BroadcastAddr().String()),
		slog.String("gateway", dhcpClient.Gateway().String()),
		slog.String("router", dhcpClient.Router().String()),
		slog.String("dhcp", dhcpClient.DHCPServer().String()),
		slog.String("hostname", string(dhcpClient.Hostname())),
		slog.Duration("lease", dhcpClient.IPLeaseTime()),
		slog.Duration("renewal", dhcpClient.RenewalTime()),
		slog.Duration("rebinding", dhcpClient.RebindingTime()),
	)

	stack.SetAddr(ip) // It's important to set the IP address after DHCP completes.
	return dhcpClient, stack, nil
}

// ResolveHardwareAddr obtains the hardware address of the given IP address.
func ResolveHardwareAddr(stack *stacks.PortStack, ip netip.Addr, timeout time.Duration) ([6]byte, error) {
	if !ip.IsValid() {
		return [6]byte{}, errors.New("invalid ip")
	}
	arpc := stack.ARP()
	arpc.Abort() // Remove any previous ARP requests.
	err := arpc.BeginResolve(ip)
	if err != nil {
		return [6]byte{}, err
	}
	deadline := time.Now().Add(timeout)
	time.Sleep(4 * time.Millisecond)
	for !arpc.IsDone() && time.Until(deadline) > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if !arpc.IsDone() {
		return [6]byte{}, errors.New("arp timed out")
	}
	_, hw, err := arpc.ResultAs6()
	return hw, err
}

type Resolver struct {
	stack     *stacks.PortStack
	dns       *stacks.DNSClient
	dhcp      *stacks.DHCPClient
	dnsaddr   netip.Addr
	dnshwaddr [6]byte
}

func NewResolver(stack *stacks.PortStack, dhcp *stacks.DHCPClient) (*Resolver, error) {
	dnsc := stacks.NewDNSClient(stack, dns.ClientPort)
	dnsaddrs := dhcp.DNSServers()
	if len(dnsaddrs) > 0 && !dnsaddrs[0].IsValid() {
		return nil, errors.New("dns addr obtained via DHCP not valid")
	}
	return &Resolver{
		stack:   stack,
		dhcp:    dhcp,
		dns:     dnsc,
		dnsaddr: dnsaddrs[0],
	}, nil
}

func (r *Resolver) LookupNetIP(host string) ([]netip.Addr, error) {
	name, err := dns.NewName(host)
	if err != nil {
		return nil, err
	}
	err = r.updateDNSHWAddr()
	if err != nil {
		return nil, err
	}

	err = r.dns.StartResolve(r.dnsConfig(name))
	if err != nil {
		return nil, err
	}
	time.Sleep(5 * time.Millisecond)
	retries := 100

	for retries > 0 {
		done, _ := r.dns.IsDone()
		if done {
			break
		}
		retries--
		time.Sleep(20 * time.Millisecond)
	}
	done, rcode := r.dns.IsDone()
	if !done && retries == 0 {
		return nil, errors.New("dns lookup timed out")
	} else if rcode != dns.RCodeSuccess {
		return nil, errors.New("dns lookup failed:" + rcode.String())
	}
	answers := r.dns.Answers()
	if len(answers) == 0 {
		return nil, errors.New("no dns answers")
	}
	var addrs []netip.Addr
	for i := range answers {
		data := answers[i].RawData()
		if len(data) == 4 {
			addrs = append(addrs, netip.AddrFrom4([4]byte(data)))
		}
	}
	if len(addrs) == 0 {
		return nil, errors.New("no ipv4 dns answers")
	}
	return addrs, nil
}

func (r *Resolver) updateDNSHWAddr() (err error) {
	r.dnshwaddr, err = ResolveHardwareAddr(r.stack, r.dnsaddr, 4*time.Second)
	return err
}

func (r *Resolver) dnsConfig(name dns.Name) stacks.DNSResolveConfig {
	return stacks.DNSResolveConfig{
		Questions: []dns.Question{
			{
				Name:  name,
				Type:  dns.TypeA,
				Class: dns.ClassINET,
			},
		},
		DNSAddr:         r.dnsaddr,
		DNSHWAddr:       r.dnshwaddr,
		EnableRecursion: true,
	}
}

func nicLoop(dev netif.InterfaceEthPoller, Stack *stacks.PortStack) {
	// Maximum number of packets to queue before sending them.
	const (
		queueSize                = 3
		maxRetriesBeforeDropping = 3
	)
	mtu := dev.MTU()
	queuebuf := make([]byte, mtu*queueSize)
	var queue [queueSize][]byte
	for i := range queue {
		queue[i] = queuebuf[i*mtu : (i+1)*mtu]
	}
	var lenBuf [queueSize]int
	var retries [queueSize]int
	markSent := func(i int) {
		for j := range queue[i] {
			queue[i][j] = 0
		}
		lenBuf[i] = 0
		retries[i] = 0
	}
	for {
		stallRx := true
		// Poll for incoming packets.
		for i := 0; i < 1; i++ {
			gotPacket, err := dev.PollOne()
			if err != nil {
				slog.Error("nic:PollOne", slog.String("err", err.Error()))
			}
			if !gotPacket {
				break
			}
			stallRx = false
		}

		// Queue packets to be sent.
		for i := range queue {
			if retries[i] != 0 {
				continue // Packet currently queued for retransmission.
			}
			var err error
			buf := queue[i][:]
			lenBuf[i], err = Stack.HandleEth(buf[:])
			if err != nil {
				slog.Error("nic:HandleEth", slog.String("err", err.Error()))
				lenBuf[i] = 0
				continue
			}
			if lenBuf[i] == 0 {
				break
			}
		}
		stallTx := lenBuf == [queueSize]int{}
		if stallTx {
			if stallRx {
				// Avoid busy waiting when both Rx and Tx stall.
				time.Sleep(51 * time.Millisecond)
			}
			continue
		}

		// Send queued packets.
		for i := range queue {
			n := lenBuf[i]
			if n <= 0 {
				continue
			}
			err := dev.SendEth(queue[i][:n])
			if err != nil {
				// Queue packet for retransmission.
				retries[i]++
				if retries[i] > maxRetriesBeforeDropping {
					markSent(i)
					slog.Error("nic:drop-pkt", slog.String("err", err.Error()))
				}
			} else {
				markSent(i)
			}
		}
	}
}

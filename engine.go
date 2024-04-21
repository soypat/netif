package netif

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/soypat/seqs"
	"github.com/soypat/seqs/eth/dhcp"
	"github.com/soypat/seqs/eth/dns"
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
	nic        InterfaceEthPoller
	dnssv      []netip.Addr
	router     netip.Addr
	routerHW   [6]byte
	method     AddrMethod
	netmask    uint8
	prngstate  uint32
	tcpbufsize int
	prefix     netip.Prefix
	hostname   string
	dhcpStart  time.Time
	dhcpc      stacks.DHCPClient
	s          stacks.PortStack
	log        *slog.Logger
	tcpconns   []engineTCPConn
	queue      [queueSize][]byte
	qlen       [queueSize]int
	qretries   [queueSize]int
	cache      lruCache
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
	// Router manually sets the IP address of the default gateway, which allows
	// sending packets out of the physical network and omits getting
	// the Router over DHCP.
	Router netip.Addr
	// DNSServers manually sets the DNS servers and omits getting them over DHCP.
	DNSServers []netip.Addr
	Logger     *slog.Logger
	// Hostname is the hostname to request over DHCP.
	Hostname string
	// TCPBufferSize is the size of the buffer for TCP connections for both
	// send and receive buffers, in bytes. If zero a sensible value is used.
	TCPBuffersize int
}

type engineTCPConn struct {
	mu          sync.Mutex
	initialized bool
	conn        *stacks.TCPConn
}

// NewEngine returns a networking engine that uses the given network interface controller.
// Engine facilitates:
//   - DHCP handling for address, router, DNS servers, and other network parameters.
//   - DNS resolution.
//   - TCP connection handling.
//   - ARP resolution (handled automatically)
func NewEngine(nic InterfaceEthPoller, cfg EngineConfig) (*Engine, error) {
	if nic == nil {
		panic("nic is nil")
	} else if cfg.AddrMethod != AddrMethodManual && cfg.AddrMethod != AddrMethodDHCP {
		return nil, errors.New("invalid address method")
	} else if cfg.TCPBuffersize < 0 || cfg.TCPBuffersize > 65535 {
		return nil, errors.New("invalid tcp buffer size")
	} else if cfg.AddrMethod == AddrMethodManual && !cfg.Address.IsValid() {
		return nil, errors.New("invalid address")
	}
	mac, err := nic.HardwareAddr6()
	if err != nil {
		return nil, err
	}
	mtu := nic.MTU()
	if mtu < 536 {
		return nil, errors.New("mtu too small")
	}
	if cfg.AddrMethod != AddrMethodManual {
		cfg.MaxOpenPortsUDP += 1 // Add extra UDP port for DHCP client.
	}
	if cfg.TCPBuffersize == 0 {
		// 40 is the length of a Ethernet+IP+TCP header, no options.
		cfg.TCPBuffersize = 2 * (mtu - 40)
	}
	s := stacks.NewPortStack(stacks.PortStackConfig{
		MaxOpenPortsUDP: int(cfg.MaxOpenPortsUDP),
		MaxOpenPortsTCP: int(cfg.MaxOpenPortsTCP),
		Logger:          cfg.Logger,
		MAC:             mac,
		MTU:             uint16(mtu),
	})
	e := &Engine{
		nic:        nic,
		method:     cfg.AddrMethod,
		s:          *s,
		dnssv:      cfg.DNSServers,
		router:     cfg.Router,
		log:        cfg.Logger,
		netmask:    cfg.Netmask,
		hostname:   cfg.Hostname,
		tcpconns:   make([]engineTCPConn, cfg.MaxOpenPortsTCP),
		tcpbufsize: cfg.TCPBuffersize,
		prngstate:  uint32(time.Now().UnixNano()),
	}
	nic.RecvEthHandle(e.s.RecvEth)

	queuebuf := make([]byte, mtu*queueSize)
	for i := range e.queue {
		e.queue[i] = queuebuf[i*mtu : (i+1)*mtu]
	}

	switch cfg.AddrMethod {
	case AddrMethodManual:
		s.SetAddr(cfg.Address)
	case AddrMethodDHCP:
		err = e.startDHCP(stacks.DHCPRequestConfig{
			Hostname:      e.hostname,
			RequestedAddr: cfg.Address,
			Xid:           e.prng32(),
		})
		if err != nil {
			return nil, err
		}
	}

	return e, nil
}

// WaitForDHCP waits for DHCP to complete. This should always be called after creating the engine with AddrMethodDHCP.
func (e *Engine) WaitForDHCP(timeout time.Duration) error {
	if e.method != AddrMethodDHCP {
		return errors.New("not using DHCP")
	}
	err := e.waitDHCP(timeout)
	if err != nil {
		return err
	}
	e.consolidateResultsDHCP()
	return nil
}

// Interface returns the underlying network interface controller.
func (e *Engine) Interface() InterfaceEthPoller {
	return e.nic
}

// Addr returns the IP address of the stack.
func (e *Engine) Addr() netip.Addr {
	return e.s.Addr()
}

// DialTCP creates a new TCP connection to the remote address raddr.
func (e *Engine) DialTCP(localport uint16, establishDeadline time.Time, raddr netip.AddrPort) (net.Conn, error) {
	if len(e.tcpconns) == 0 {
		return nil, errors.New("no tcp connections available")
	} else if e.Addr().BitLen() != raddr.Addr().BitLen() {
		return nil, errors.New("network must be the same as remote address network (v4/v6)")
	} else if !raddr.IsValid() {
		return nil, errors.New("invalid remote address")
	}

	// Early check for available TCP connections.
	econn := e.lockFreeTCPConn()
	if econn == nil {
		return nil, errors.New("no tcp connections available")
	}
	defer econn.mu.Unlock()

	// TCP connection(s) available, resolve address if necessary.
	addr := raddr.Addr()
	hw, err := e.ResolveHardwareAddr(addr, 1*time.Second)
	if err != nil {
		return nil, err
	}
	conn := econn.conn
	if !econn.initialized {
		conn, err = stacks.NewTCPConn(&e.s, stacks.TCPConnConfig{
			TxBufSize: uint16(e.tcpbufsize),
			RxBufSize: uint16(e.tcpbufsize),
		})
		if err != nil {
			return nil, err
		}
		econn.conn = conn
		econn.initialized = true
	}

	err = conn.OpenDialTCP(localport, hw, raddr, seqs.Value(e.prng32()))
	if err != nil {
		return nil, err
	}
	backoff := e.backoff()
	deadlineDisabled := establishDeadline.IsZero()
	for (deadlineDisabled || time.Since(establishDeadline) < 0) && conn.State().IsPreestablished() {
		backoff.Miss()
	}
	if conn.State().IsPreestablished() {
		conn.Close()
		return nil, errors.New("tcp dial timed out")
	}
	return conn, nil
}

func (e *Engine) lockFreeTCPConn() *engineTCPConn {
	for i := range e.tcpconns {
		if e.tcpconns[i].conn == nil || e.tcpconns[i].conn.State().IsClosed() {
			if e.tcpconns[i].mu.TryLock() {
				return &e.tcpconns[i]
			}
		}
	}
	return nil
}

// ResolveHardwareAddr resolves the hardware address of an IP address:
//   - If the IP address is in the cache, it is returned.
//   - If the IP address is not on the local network, the router's hardware address is returned.
//   - If the IP address is on the local network, an ARP request is sent and the resulting hardware address is returned.
func (e *Engine) ResolveHardwareAddr(ip netip.Addr, timeout time.Duration) ([6]byte, error) {
	if !ip.IsValid() {
		return [6]byte{}, errors.New("invalid ip")
	}
	// Check cache.
	if entry := e.cache.getByAddr(ip); entry != nil {
		return entry.hw, nil
	}

	// Check if address is not on the local network.
	if !e.prefix.IsValid() {
		return [6]byte{}, errors.New("netmask/prefix undefined")
	}
	if !e.prefix.Contains(ip) {
		// IP address is not on the local network, return the router's hardware address.
		if err := e.ensureRouterAddr(); err != nil {
			return [6]byte{}, err
		}
		return e.routerHW, nil
	}

	arpc := e.s.ARP()
	arpc.Abort() // Remove any previous ARP requests.
	err := arpc.BeginResolve(ip)
	if err != nil {
		return [6]byte{}, err
	}

	deadline := time.Now().Add(timeout)
	backoff := e.backoff()
	for !arpc.IsDone() && time.Until(deadline) > 0 {
		backoff.Miss()
	}
	if !arpc.IsDone() {
		return [6]byte{}, errors.New("arp timed out")
	}
	_, hw, err := arpc.ResultAs6()
	e.cache.enter("", ip, hw)
	return hw, err
}

// HandlePoll polls the network interface for incoming packets and sends queued packets.
func (e *Engine) HandlePoll() (received, sent int, err error) {
	return e.poll()
}

func (e *Engine) startDHCP(cfg stacks.DHCPRequestConfig) error {
	if !e.dhcpc.Offer().IsValid() {
		// Initialize DHCP client if not already done.
		dhcpClient := stacks.NewDHCPClient(&e.s, dhcp.DefaultClientPort)
		e.dhcpc = *dhcpClient
	}
	// Perform DHCP request.
	dhcpc := &e.dhcpc
	err := dhcpc.BeginRequest(cfg)
	return err
}

// doDHCP shows use of DHCP methods together to fulfill a DHCP request.
func (e *Engine) doDHCP(timeout time.Duration, cfg stacks.DHCPRequestConfig) error {
	err := e.startDHCP(cfg)
	if err != nil {
		return err
	}
	err = e.waitDHCP(timeout)
	if err != nil {
		return err
	}
	e.consolidateResultsDHCP()
	return nil
}

func (e *Engine) waitDHCP(timeout time.Duration) error {
	if e.method != AddrMethodDHCP {
		return errors.New("not using DHCP")
	}
	dhcpc := &e.dhcpc
	defer dhcpc.Abort()

	e.dhcpStart = time.Now()
	deadline := e.dhcpStart.Add(timeout)
	backoff := e.backoff()
	for !dhcpc.IsDone() && time.Since(deadline) < 0 {
		backoff.Miss()
	}
	// close port.
	if !dhcpc.IsDone() {
		e.dhcpStart = time.Time{}
		return errors.New("DHCP did not complete")
	}
	return nil
}

func (e *Engine) consolidateResultsDHCP() {
	var err error
	dhcpc := &e.dhcpc
	offer := dhcpc.Offer()
	if offer.IsValid() && (e.method == AddrMethodDHCP || !e.s.Addr().IsValid()) {
		e.s.SetAddr(offer)
	}
	router := dhcpc.Router()
	if router.IsValid() && (e.method == AddrMethodDHCP && router.IsValid() || !e.router.IsValid()) {
		e.router = router
	}
	if e.method == AddrMethodDHCP || len(e.dnssv) == 0 {
		e.dnssv = dhcpc.DNSServers()
	}
	cidr := dhcpc.CIDRBits()
	cidrValid := cidr > 0 && cidr < 32
	if cidrValid && (e.method == AddrMethodDHCP || !e.prefix.IsValid()) {
		e.prefix, err = e.router.Prefix(int(cidr))
		if err != nil {
			e.prefix = netip.Prefix{}
		}
	}
	if e.log != nil {
		e.log.LogAttrs(context.Background(), slog.LevelInfo,
			"dhcp-complete",
			slog.String("our-ip", e.s.Addr().String()),
			slog.String("dns", e.dnssv[0].String()),
			slog.String("router", e.router.String()),
			slog.String("prefix", e.prefix.String()),
			slog.String("broadcast", dhcpc.BroadcastAddr().String()),
		)
	}
}

func (e *Engine) ensureRouterAddr() error {
	if e.router.IsValid() && e.routerHW != [6]byte{} {
		return nil
	} else if e.method == AddrMethodManual {
		return errors.New("gateway not set")
	}

	gw, err := e.ResolveHardwareAddr(e.router, 1*time.Second)
	if err != nil {
		return err
	}
	e.routerHW = gw
	return nil
}

func (e *Engine) ensureDNSAddr() error {
	if len(e.dnssv) == 0 {
		return errors.New("no dns servers")
	}
	return nil
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
func (e *Engine) poll() (received, sent int, err error) {
	nic := e.nic
	stack := &e.s

	// Poll for incoming packets.
	var errGot, errSent error
	// RXPOLL:
	gotPacket, errGot := nic.PollOne()
	if gotPacket {
		received++
	}
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
				sent++
			}
		}
	}
	if errGot != nil {
		err = errGot
	} else {
		err = errSent
	}
	return received, sent, err
}

// NewResolver returns a DNS client that uses the Engine's ARP and network stack.
func (e *Engine) NewResolver(localport uint16, timeout time.Duration) Resolver {
	d := stacks.NewDNSClient(&e.s, localport)
	return &engineResolver{
		e:       e,
		dns:     d,
		timeout: timeout,
	}
}

type engineResolver struct {
	e       *Engine
	dns     *stacks.DNSClient
	timeout time.Duration
}

// LookupNetIP resolves the IP addresses of a hostname. It implements the [Resolver] interface.
func (r *engineResolver) LookupNetIP(host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	return r.appendLookupNetIP(host, addrs)
}

// appendLookupNetIP is a helper function that appends the resolved IP addresses to the given slice to let user avoid allocations.
func (r *engineResolver) appendLookupNetIP(host string, dst []netip.Addr) ([]netip.Addr, error) {
	name, err := dns.NewName(host)
	if err != nil {
		return dst, err
	}
	err = r.e.ensureRouterAddr()
	if err != nil {
		return dst, err
	}
	err = r.e.ensureDNSAddr()
	if err != nil {
		return dst, err
	}
	err = r.dns.StartResolve(r.dnsConfig(name))
	if err != nil {
		return dst, err
	}
	defer r.dns.Abort()

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
		return dst, errors.New("dns lookup timed out")
	} else if rcode != dns.RCodeSuccess {
		return dst, errors.New("dns lookup failed:" + rcode.String())
	}
	answers := r.dns.Answers()
	if len(answers) == 0 {
		return dst, errors.New("no dns answers")
	}
	var foundIPv4 bool
	for i := range answers {
		data := answers[i].RawData()
		if len(data) == 4 {
			foundIPv4 = true
			dst = append(dst, netip.AddrFrom4([4]byte(data)))
		} else if len(data) == 16 {
			dst = append(dst, netip.AddrFrom16([16]byte(data)))
		}
	}
	if !foundIPv4 {
		return dst, errors.New("no ipv4 dns answers")
	}
	return dst, nil
}

func (r *engineResolver) dnsConfig(name dns.Name) stacks.DNSResolveConfig {
	return stacks.DNSResolveConfig{
		Questions: []dns.Question{
			{
				Name:  name,
				Type:  dns.TypeA,
				Class: dns.ClassINET,
			},
		},
		DNSAddr:         r.e.dnssv[0],
		DNSHWAddr:       r.e.routerHW,
		EnableRecursion: true,
	}
}

func (e *Engine) backoff() exponentialBackoff {
	return newBackoff()
}

func newBackoff() exponentialBackoff {
	return exponentialBackoff{
		MaxWait: 500 * time.Millisecond,
	}
}

// exponentialBackoff implements a [Exponential Backoff]
// delay algorithm to prevent saturation network or processor
// with failing tasks. An exponentialBackoff with a non-zero MaxWait is ready for use.
//
// [Exponential Backoff]: https://en.wikipedia.org/wiki/Exponential_backoff
type exponentialBackoff struct {
	// Wait defines the amount of time that Miss will wait on next call.
	Wait time.Duration
	// Maximum allowable value for Wait.
	MaxWait time.Duration
	// StartWait is the value that Wait takes after a call to Hit.
	StartWait time.Duration
	// ExpMinusOne is the shift performed on Wait minus one, so the zero value performs a shift of 1.
	ExpMinusOne uint32
}

// Hit sets eb.Wait to the StartWait value.
func (eb *exponentialBackoff) Hit() {
	if eb.MaxWait == 0 {
		panic("MaxWait cannot be zero")
	}
	eb.Wait = eb.StartWait
}

// Miss sleeps for eb.Wait and increases eb.Wait exponentially.
func (eb *exponentialBackoff) Miss() {
	const k = 1
	wait := eb.Wait
	maxWait := eb.MaxWait
	exp := eb.ExpMinusOne + 1
	if maxWait == 0 {
		panic("MaxWait cannot be zero")
	}
	time.Sleep(wait)
	wait |= time.Duration(k)
	wait <<= exp
	if wait > maxWait {
		wait = maxWait
	}
	eb.Wait = wait
}

type lruCache struct {
	entries [8]lruEntry
}

type lruEntry struct {
	// name is a index. not guaranteed to be present.
	name string
	// addr is the IP address.
	addr netip.Addr
	// hw is the hardware address.
	hw [6]byte
	// entered is set when the entry is created. If zero the entry is not in use.
	entered time.Time
}

func (c *lruCache) getByName(name string) *lruEntry {
	for i := range c.entries {
		if c.entries[i].name == name {
			return &c.entries[i]
		}
	}
	return nil
}

func (c *lruCache) getByAddr(addr netip.Addr) *lruEntry {
	for i := range c.entries {
		if c.entries[i].addr.Compare(addr) == 0 {
			return &c.entries[i]
		}
	}
	return nil
}

func (c *lruCache) enter(name string, addr netip.Addr, hw [6]byte) {
	var oldest *lruEntry = &c.entries[0]
	for i := 1; i < len(c.entries) && !oldest.entered.IsZero(); i++ {
		runnerup := &c.entries[i]
		if runnerup.entered.Before(oldest.entered) {
			oldest = runnerup
		}
	}
	oldest.name = name
	oldest.addr = addr
	oldest.hw = hw
	oldest.entered = time.Now()
}

func (e *Engine) prng32() uint32 {
	/* Algorithm "xor" from p. 4 of Marsaglia, "Xorshift RNGs" */
	p := e.prngstate
	p ^= p << 13
	p ^= p >> 17
	p ^= p << 5
	e.prngstate = p
	return p
}

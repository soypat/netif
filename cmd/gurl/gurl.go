package main

import (
	"flag"
	"io"
	"log"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/soypat/netif"
	"github.com/soypat/netif/examples/common"
	"github.com/soypat/seqs"
	"github.com/soypat/seqs/eth/dns"
	"github.com/soypat/seqs/httpx"
	"github.com/soypat/seqs/stacks"
)

const connTimeout = 5 * time.Second

const ourHost = "gurl"
const dnsTimeout = 4 * time.Second

func main() {
	var (
		flagInterface   string
		serverPort      uint16
		flagLogLevel    int
		flagRequestedIP string
	)
	// Create device interface.
	iface, err := netif.DefaultInterface()
	if err != nil {
		iface, err = net.InterfaceByIndex(1)
		if err != nil {
			log.Fatal("no interfaces found:", err)
		}
	}
	flag.StringVar(&flagInterface, "i", iface.Name, "Interface to use")
	flag.StringVar(&flagRequestedIP, "d", "", "IP address to request by DHCP.")
	flag.IntVar(&flagLogLevel, "l", int(slog.LevelInfo), "Log level")
	flag.Parse()
	if flag.NArg() > 1 {
		log.Fatal("too many arguments")
	}

	// Parse URL and validate it.
	argURL := flag.Arg(0)
	if argURL == "" {
		log.Fatal("URL is required")
	}
	URL, err := url.Parse(argURL)
	if err != nil {
		log.Fatal(err)
	}
	svHostname := URL.Host
	if newhost, sport, ok := strings.Cut(svHostname, ":"); ok {
		svHostname = newhost
		p, err := strconv.Atoi(sport)
		if err != nil || p < 1 || p > 65535 {
			log.Fatal("invalid port w/ parse err:", err)
		}
		serverPort = uint16(p)
	}
	if serverPort == 0 {
		serverPort = 80 // Sensible default if not present.
	}

	// Create structured logger.
	fp, _ := os.Create("http-client.log")
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(fp, os.Stdout), &slog.HandlerOptions{
		Level: slog.Level(flagLogLevel),
	}))

	slog.Info("url", slog.String("url", URL.String()), slog.String("uri", URL.RequestURI()), slog.String("host", svHostname), slog.Uint64("port", uint64(serverPort)))

	// Check whether we need to resolve hostname.
	_, dnsErr := dns.NewName(svHostname)
	serverAddr, ipErr := netip.ParseAddr(svHostname)
	if dnsErr != nil && ipErr != nil {
		log.Fatal("invalid hostname", dnsErr, ipErr)
	}

	// OK, all pre-processing is done, lets open the socket and start the client.
	// This performs a DHCP setup to acquire network data.
	ethsock, err := netif.NewEthSocket(flagInterface)
	if err != nil {
		log.Fatal("ethernet socket:" + err.Error())
	}
	dhcpc, stack, err := common.SetupWithDHCP(ethsock, common.SetupConfig{
		Hostname:    ourHost,
		Logger:      logger,
		TCPPorts:    1, // For HTTP over TCP.
		UDPPorts:    1, // For DNS.
		RequestedIP: flagRequestedIP,
		DHCPTimeout: 1500 * time.Millisecond,
	})
	if err != nil {
		panic("setup DHCP:" + err.Error())
	}

	if ipErr != nil {
		// We have a hostname we must resolve.
		resolver, err := common.NewResolver(stack, dhcpc)
		if err != nil {
			panic("resolver create:" + err.Error())
		}
		addrs, err := resolver.LookupNetIP(URL.Host)
		if err != nil {
			panic("DNS lookup failed:" + err.Error())
		}
		serverAddr = addrs[0]
	}

	// We then resolve the hardware address of the server. O
	dstHwaddr, err := common.ResolveHardwareAddr(stack, serverAddr, dnsTimeout)
	if err != nil {
		dstHwaddr, err = common.ResolveHardwareAddr(stack, dhcpc.Router(), dnsTimeout)
		if err != nil {
			panic("router hwaddr resolving:" + err.Error())
		}
	}

	// Start TCP server.
	conn, err := stacks.NewTCPConn(stack, stacks.TCPConnConfig{
		TxBufSize: tcpBufSize(iface.MTU),
		RxBufSize: tcpBufSize(iface.MTU),
	})
	if err != nil {
		panic("conn create:" + err.Error())
	}

	closeConn := func(err string) {
		slog.Error("tcpconn:closing", slog.String("err", err))
		conn.Close()
		for !conn.State().IsClosed() {
			slog.Info("closeConn:waiting", slog.String("state", conn.State().String()))
			time.Sleep(1000 * time.Millisecond)
		}
		time.Sleep(5 * time.Second)
	}

	// Create the HTTP request data.
	var req httpx.RequestHeader
	req.SetRequestURI(URL.RequestURI())
	req.SetMethod("GET")
	req.SetHost(svHostname)
	reqbytes := req.Header()

	logger.Info("tcp:ready",
		slog.String("clientaddr", stack.Addr().String()),
		slog.String("serveraddr", serverAddr.String()),
	)
	rxBuf := make([]byte, iface.MTU*8)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	retries := 5
	for retries > 0 {
		retries--
		ourPort := uint16(rng.Intn(0xffff-1025) + 1024)
		slog.Info("dialing", slog.String("serveraddr", serverAddr.String()), slog.Uint64("our-port", uint64(ourPort)))

		// Make sure to timeout the connection if it takes too long.
		conn.SetDeadline(time.Now().Add(connTimeout))
		err = conn.OpenDialTCP(ourPort, dstHwaddr, netip.AddrPortFrom(serverAddr, serverPort), seqs.Value(rng.Intn(0xffff_ffff-2)+1))
		if err != nil {
			closeConn("opening TCP: " + err.Error())
			continue
		}
		retries := 50
		for conn.State() != seqs.StateEstablished && retries > 0 {
			time.Sleep(100 * time.Millisecond)
			retries--
		}
		conn.SetDeadline(time.Time{}) // Disable the deadline.
		if retries == 0 {
			closeConn("tcp establish retry limit exceeded")
			continue
		}

		// Send the request.
		_, err = conn.Write(reqbytes)
		if err != nil {
			closeConn("writing request: " + err.Error())
			continue
		}
		// time.Sleep(500 * time.Millisecond)
		conn.SetDeadline(time.Now().Add(connTimeout))
		n, err := conn.Read(rxBuf)
		if n == 0 && err != nil {
			closeConn("reading response: " + err.Error())
			continue
		} else if n == 0 {
			closeConn("no response")
			continue
		}
		logger.Info("response", slog.String("response", string(rxBuf[:n])))
		os.Stdout.Write(rxBuf[:n])
		closeConn("done")
		return
	}
	os.Stderr.Write([]byte("failed to connect to server\n"))
	os.Exit(1)
}

func tcpBufSize(mtu int) uint16 {
	return uint16(mtu - 40)
}

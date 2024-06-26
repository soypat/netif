package main

import (
	"flag"
	"io"
	"log"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/soypat/netif"
	"github.com/soypat/seqs"
	"github.com/soypat/seqs/eth/dns"
	"github.com/soypat/seqs/httpx"
	"github.com/soypat/seqs/stacks"
)

const connTimeout = 5 * time.Second

const ourHost = "gurl"
const dnsTimeout = 4 * time.Second
const arpTimeout = 1 * time.Second

func main() {
	var (
		flagInterface   string
		serverPort      uint16
		flagLogLevel    int
		flagRequestedIP string
		flagDNS         string
		flagLocalport   int
	)
	// Create device interface.
	iface, err := netif.DefaultInterface()
	if err != nil {
		iface, err = net.InterfaceByIndex(1)
		if err != nil {
			log.Fatal("no interfaces found:", err)
		}
	}
	flag.IntVar(&flagLocalport, "hostport", 8877, "Outgoing TCP port.")
	flag.StringVar(&flagDNS, "dns", "", "DNS IP to seek.")
	flag.StringVar(&flagInterface, "i", iface.Name, "Interface to use")
	flag.StringVar(&flagRequestedIP, "hostip", "", "IP address to request by DHCP.")
	flag.IntVar(&flagLogLevel, "l", int(slog.LevelInfo), "Log level")
	flag.Parse()
	if flag.NArg() > 1 {
		log.Fatal("too many arguments")
	} else if flagLocalport < 1 || flagLocalport > math.MaxUint16 {
		log.Fatal("invalid port ", flagLocalport)
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
	var DNSServers []netip.Addr
	if flagDNS != "" {
		ip, err := netip.ParseAddr(flagDNS)
		if err != nil {
			log.Fatal("failed to parse dns flag:", err.Error())
		}
		DNSServers = append(DNSServers, ip)
	}
	engine, err := netif.NewEngine(ethsock, netif.EngineConfig{
		MaxOpenPortsUDP: 1,
		MaxOpenPortsTCP: 1,
		Logger:          logger,
		Hostname:        ourHost,
		AddrMethod:      netif.AddrMethodDHCP,
		DNSServers:      DNSServers,
	})
	if err != nil {
		log.Fatal("netif.Engine create:" + err.Error())
	}

	go func() {
		stalled := 0
		for {
			rx, tx, err := engine.HandlePoll()
			if err != nil {
				log.Println("engine poll:" + err.Error())
			}
			if rx == 0 && tx == 0 {
				// Exponential backoff.
				stalled += 1
				sleep := time.Duration(1) << stalled
				if sleep > 1*time.Second {
					sleep = 1 * time.Second
				}
				time.Sleep(sleep)
			} else {
				stalled = 0
			}
		}
	}()

	err = engine.WaitForDHCP(5 * time.Second)
	if err != nil {
		log.Fatal("dhcp:" + err.Error())
	}

	if ipErr != nil {
		// We have a hostname we must resolve.
		resolver := engine.NewResolver(dns.ClientPort, dnsTimeout)
		addrs, err := resolver.LookupNetIP(URL.Host)
		if err != nil {
			panic("DNS lookup failed:" + err.Error())
		}
		serverAddr = addrs[0]
	}

	// Create the HTTP request data.
	var req httpx.RequestHeader
	req.SetRequestURI(URL.RequestURI())
	req.SetMethod("GET")
	req.SetHost(svHostname)
	reqbytes := req.Header()
	// Reserve the underlying Linux socket so that
	// the kernel does not reset the connection automatically.

	// Bind TCP socket so underlying OS does not reset the connection.
	// https://widu.tumblr.com/post/43624355124/suppressing-tcp-rst-on-raw-sockets
	ourPort := uint16(flagLocalport)
	err = bindTCPPort(netip.AddrPortFrom(engine.Addr(), ourPort))
	if err != nil {
		log.Fatal(err)
	}

	rxBuf := make([]byte, iface.MTU*8)
	retries := 5

	for retries > 0 {
		retries--
		slog.Info("dialing", slog.String("serveraddr", serverAddr.String()), slog.Uint64("our-port", uint64(ourPort)))
		netconn, err := engine.DialTCP(ourPort, time.Now().Add(connTimeout), netip.AddrPortFrom(serverAddr, serverPort))
		if err != nil {
			panic("conn create:" + err.Error())
		}
		conn := netconn.(*stacks.TCPConn)
		if conn.State() != seqs.StateEstablished {
			panic("conn state:" + conn.State().String())
		}
		// Send the request.
		_, err = conn.Write(reqbytes)
		if err != nil {
			log.Println("writing request:" + err.Error())
			continue
		}
		// time.Sleep(500 * time.Millisecond)
		conn.SetDeadline(time.Now().Add(connTimeout))
		n, err := conn.Read(rxBuf)
		if n == 0 && err != nil {
			log.Println("reading response:" + err.Error())
			continue
		} else if n == 0 {
			log.Println("no response:" + err.Error())
			continue
		}
		logger.Info("response", slog.String("response", string(rxBuf[:n])))
		os.Stdout.Write(rxBuf[:n])
		return
	}
	os.Stderr.Write([]byte("failed to connect to server\n"))
	os.Exit(1)
}

// This does not seem to be working.
func bindTCPPort(addr netip.AddrPort) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return err
	}
	err = syscall.Bind(fd, &syscall.SockaddrInet4{
		Port: int(addr.Port()),
		Addr: addr.Addr().As4(),
	})
	if err != nil {
		return err
	}
	return syscall.Listen(fd, 1)
}

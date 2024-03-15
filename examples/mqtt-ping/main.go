package main

import (
	"flag"
	"io"
	"log"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"os/user"
	"time"

	mqtt "github.com/soypat/natiu-mqtt"
	"github.com/soypat/netif"
	"github.com/soypat/netif/examples/common"
	"github.com/soypat/seqs"
	"github.com/soypat/seqs/eth/dns"
	"github.com/soypat/seqs/stacks"
)

var (
	pubFlags, _ = mqtt.NewPublishFlags(mqtt.QoS0, false, false)
	pubVar      = mqtt.VariablesPublish{
		TopicName:        []byte("tinygo-pico-test"),
		PacketIdentifier: 0x12,
	}
	mqttCfg = mqtt.ClientConfig{
		Decoder: mqtt.DecoderNoAlloc{UserBuffer: make([]byte, 4096)},
		OnPub: func(pubHead mqtt.Header, varPub mqtt.VariablesPublish, r io.Reader) error {
			b, _ := io.ReadAll(r)
			slog.Info("received message",
				slog.String("topic", string(varPub.TopicName)),
				slog.String("msg", string(b)),
			)
			return nil
		},
	}
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug - 1,
	})))
	u, _ := user.Current()
	if u.Name == "" {
		u.Name = "nobody"
	}
	iface, err := netif.DefaultInterface()
	if err != nil {
		iface, err = net.InterfaceByIndex(1)
		if err != nil {
			log.Fatal("no interfaces found:", err)
		}
	}
	var (
		flagNetInterfaceName = iface.Name
		flagServerHostname   = "test.mosquitto.org"
		flagServerPort       = 1883
		flagOurPort          = 0
		flagMQTTTopic        = "test"
		flagClientID         = u.Name
		flagPassword         = ""
		flagRequestedIP      string
	)
	flag.StringVar(&flagNetInterfaceName, "i", flagNetInterfaceName, "Network interface name. e.g. eth0, wlan0, wlp7s0, enp044s etc.")
	flag.StringVar(&flagRequestedIP, "d", "", "IP address to request by DHCP, or assigned if DHCP unavailable")
	flag.StringVar(&flagServerHostname, "server", flagServerHostname, "MQTT server hostname")
	flag.IntVar(&flagServerPort, "port", flagServerPort, "MQTT server TCP port")
	flag.IntVar(&flagOurPort, "ourport", flagOurPort, "Our MQTT TCP port")
	flag.StringVar(&flagMQTTTopic, "topic", flagMQTTTopic, "MQTT topic to subscribe to")
	flag.StringVar(&flagClientID, "clientid", flagClientID, "MQTT client ID")
	flag.StringVar(&flagPassword, "password", flagPassword, "MQTT password")
	flag.Parse()
	dev, err := netif.NewEthSocket(flagNetInterfaceName)
	if err != nil {
		log.Fatal("NewEthSocket:", err)
	}
	defer dev.Close()

	dhcpc, stack, err := common.SetupWithDHCP(dev, common.SetupConfig{
		Hostname:    "mqtt-pinger",
		Logger:      slog.Default(),
		UDPPorts:    1, // DNS
		TCPPorts:    1, // TCP stuff.
		DHCPTimeout: time.Second,
		RequestedIP: flagRequestedIP,
	})
	if err != nil {
		log.Fatal("SetupWithDHCP:", err)
	}

	// Determine Server IP address and MAC address of server (or router if DNS is used).
	_, domainErr := dns.NewName(flagServerHostname)
	serverAddr, ipErr := netip.ParseAddr(flagServerHostname)
	if domainErr != nil && ipErr != nil {
		log.Fatal("invalid hostname", flagServerHostname)
	}
	isDomain := domainErr == nil && ipErr != nil
	var hwAddr [6]byte
	if isDomain {
		slog.Info("resolving via DNS")
		// Is a domain we must resolve via DNS.
		hwAddr, err = common.ResolveHardwareAddr(stack, dhcpc.Router(), 5*time.Second)
		if err != nil {
			log.Fatal("router hwaddr resolving:", err.Error())
		}
		resolver, err := common.NewResolver(stack, dhcpc)
		if err != nil {
			log.Fatal("resolver create:", err.Error())
		}
		addrs, err := resolver.LookupNetIP(flagServerHostname)
		if err != nil {
			log.Fatal("DNS lookup failed:", err.Error())
		}
		serverAddr = addrs[0]
	} else {
		slog.Info("resolving using only ARP")
		hwAddr, err = common.ResolveHardwareAddr(stack, serverAddr, 5*time.Second)
		if err != nil {
			log.Fatal("router hwaddr resolving:", err.Error())
		}
	}

	// Start TCP server.
	const socketBuf = 16 * 1024
	socket, err := stacks.NewTCPConn(stack, stacks.TCPConnConfig{TxBufSize: socketBuf, RxBufSize: socketBuf})
	if err != nil {
		panic("socket create:" + err.Error())
	}
	var varconn mqtt.VariablesConnect
	varconn.SetDefaultMQTT([]byte(flagClientID))
	if flagPassword != "" {
		varconn.Password = []byte(flagPassword)
	}

	client := mqtt.NewClient(mqttCfg)
	closeConn := func() {
		slog.Info("socket:close-connection", slog.String("state", socket.State().String()))
		socket.FlushOutputBuffer()
		socket.Close()
		for !socket.State().IsClosed() {
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(time.Second)
	}

	// Connection loop for TCP+MQTT.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		slog.Info("socket:listen")
		ourPort := uint16(flagOurPort)
		if ourPort == 0 {
			// If flag not set generate random port.
			ourPort = uint16(rng.Intn(0xffff-1024) + 1024)
		}
		err = socket.OpenDialTCP(ourPort, hwAddr, netip.AddrPortFrom(serverAddr, uint16(flagServerPort)), seqs.Value(rng.Intn(0xffff_ffff))+1)
		if err != nil {
			panic("socket dial:" + err.Error())
		}
		retries := 50
		for socket.State() != seqs.StateEstablished && retries > 0 {
			time.Sleep(100 * time.Millisecond)
			retries--
		}
		if retries == 0 {
			slog.Info("socket:no-establish")
			closeConn()
			continue
		}

		// We start MQTT connect with a deadline on the socket.
		slog.Info("mqtt:start-connecting")
		socket.SetDeadline(time.Now().Add(5 * time.Second))
		err = client.StartConnect(socket, &varconn)
		if err != nil {
			slog.Error("mqtt:start-connect-failed", slog.String("reason", err.Error()))
			closeConn()
			continue
		}
		retries = 50
		for retries > 0 && !client.IsConnected() {
			err = client.HandleNext()
			if err != nil {
				slog.Error("mqtt:handle-next-failed", slog.String("reason", err.Error()))
				closeConn()
			}
			time.Sleep(100 * time.Millisecond)
			retries--
		}
		if !client.IsConnected() {
			slog.Error("mqtt:connect-failed", slog.Any("reason", client.Err()))
			closeConn()
			continue
		}

		err = client.StartPing()
		if err != nil {
			slog.Error("mqtt:ping-failed", slog.Any("reason", err))
			closeConn()
			continue
		}
		start := client.LastTx()
		for client.AwaitingPingresp() {
			if socket.BufferedInput() <= 0 {
				slog.Info("mqtt:wait-for-response")
				time.Sleep(time.Second)
				continue
			}
			err = client.HandleNext()
			if err != nil {
				slog.Error("mqtt:ping-failed", slog.Any("reason", err))
				closeConn()
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		end := client.LastRx()
		slog.Info("mqtt:ping", slog.Duration("ping-time", end.Sub(start)))
		closeConn()
		break
	}
}

// htons converts a short (uint16) from host-to-network byte order.
func htons(i uint16) uint16 {
	return (i<<8)&0xff00 | i>>8
}

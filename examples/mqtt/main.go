package main

import (
	"flag"
	"io"
	"log"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/user"
	"time"

	mqtt "github.com/soypat/natiu-mqtt"
	"github.com/soypat/netif"
	"github.com/soypat/netif/examples/common"
	"github.com/soypat/seqs"
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
		Level: slog.LevelDebug - 4,
	})))
	u, _ := user.Current()
	if u.Name == "" {
		u.Name = "nobody"
	}
	iface, err := net.InterfaceByIndex(1)
	if err != nil {
		iface = &net.Interface{Name: "eth0"}
	}
	var (
		flagNetInterfaceName = iface.Name
		flagServerHostname   = "test.mosquitto.org"
		flagServerPort       = 1883
		flagOurPort          = 1883
		flagMQTTTopic        = "test"
		flagClientID         = u.Name
	)
	flag.StringVar(&flagNetInterfaceName, "i", flagNetInterfaceName, "Network interface name. e.g. eth0, wlan0, wlp7s0, enp044s etc.")
	flag.StringVar(&flagServerHostname, "server", flagServerHostname, "MQTT server hostname")
	flag.IntVar(&flagServerPort, "port", flagServerPort, "MQTT server TCP port")
	flag.IntVar(&flagOurPort, "ourport", flagOurPort, "Our MQTT TCP port")
	flag.StringVar(&flagMQTTTopic, "topic", flagMQTTTopic, "MQTT topic to subscribe to")
	flag.StringVar(&flagClientID, "clientid", flagClientID, "MQTT client ID")
	flag.Parse()
	dev, err := netif.NewEthSocket(flagNetInterfaceName)
	if err != nil {
		log.Fatal("NewEthSocket:", err)
	}
	defer dev.Close()

	dhcpc, stack, err := common.SetupWithDHCP(dev, common.SetupConfig{
		Hostname: "myhost",
		Logger:   slog.Default(),
		UDPPorts: 1, // DNS
		TCPPorts: 1, // TCP stuff.
	})
	if err != nil {
		log.Fatal("SetupWithDHCP:", err)
	}
	// stack.RecvEth()
	routerhw, err := common.ResolveHardwareAddr(stack, dhcpc.Router())
	if err != nil {
		panic("router hwaddr resolving:" + err.Error())
	}
	resolver, err := common.NewResolver(stack, dhcpc)
	if err != nil {
		panic("resolver create:" + err.Error())
	}
	addrs, err := resolver.LookupNetIP(flagServerHostname)
	if err != nil {
		panic("DNS lookup failed:" + err.Error())
	}
	serverAddr := netip.AddrPortFrom(addrs[0], uint16(flagServerPort))

	// Start TCP server.
	const socketBuf = 16 * 1024
	socket, err := stacks.NewTCPConn(stack, stacks.TCPConnConfig{TxBufSize: socketBuf, RxBufSize: socketBuf})
	if err != nil {
		panic("socket create:" + err.Error())
	}
	var varconn mqtt.VariablesConnect
	varconn.SetDefaultMQTT([]byte(flagClientID))
	client := mqtt.NewClient(mqttCfg)

	closeConn := func() {
		slog.Info("socket:close-connection", slog.String("state", socket.State().String()))
		socket.FlushOutputBuffer()
		socket.Close()
		for !socket.State().IsClosed() {
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Connection loop for TCP+MQTT.
	for {
		slog.Info("socket:listen")
		err = socket.OpenDialTCP(uint16(flagOurPort), routerhw, serverAddr, 0x123456)
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
			time.Sleep(100 * time.Millisecond)
			retries--
		}
		if !client.IsConnected() {
			slog.Error("mqtt:connect-failed", slog.Any("reason", client.Err()))
			closeConn()
			continue
		}

		for client.IsConnected() {
			socket.SetDeadline(time.Now().Add(5 * time.Second))
			pubVar.PacketIdentifier++
			err = client.PublishPayload(pubFlags, pubVar, []byte("hello world"))
			if err != nil {
				slog.Error("mqtt:publish-failed", slog.Any("reason", err))
			}
			slog.Info("published message", slog.Uint64("packetID", uint64(pubVar.PacketIdentifier)))
			time.Sleep(5 * time.Second)
		}
		slog.Error("mqtt:disconnected", slog.Any("reason", client.Err()))
		closeConn()
	}
}

// htons converts a short (uint16) from host-to-network byte order.
func htons(i uint16) uint16 {
	return (i<<8)&0xff00 | i>>8
}

package netif

import (
	"net"
	"net/netip"
)

type Event uint8

// Network events
const (
	// The device's network connection is now UP
	EventNetUp Event = iota
	// The device's network connection is now DOWN
	EventNetDown
)

// Interface is the minimum interface that need be implemented by any network
// device driver and is based on [net.Interface].
type Interface interface {
	// HardwareAddr6 returns the device's 6-byte [MAC address].
	//
	// [MAC address]: https://en.wikipedia.org/wiki/MAC_address
	HardwareAddr6() ([6]byte, error)
	// NetFlags returns the net.Flag values for the interface. It includes state of connection.
	NetFlags() net.Flags
	// MTU returns the maximum transmission unit size.
	MTU() int
	// Notify to register callback for network events. May not be supported for certain devices.
	// NetNotify(cb func(Event)) error
}

// InterfaceEthPoller is implemented by devices that send/receive ethernet packets.
type InterfaceEthPoller interface {
	Interface
	// SendEth sends an Ethernet packet
	SendEth(pkt []byte) error
	// RecvEthHandle sets recieve Ethernet packet callback function
	RecvEthHandle(func(pkt []byte) error)
	// PollOne tries to receive one Ethernet packet and returns true if
	// a packet was received by the stack callback.
	PollOne() (bool, error)
}

// Resolver is the interface for DNS resolution, as implemented by the `net` package.
type Resolver interface {
	// LookupNetIP returns the IP addresses of a host.
	LookupNetIP(host string) ([]netip.Addr, error)
}

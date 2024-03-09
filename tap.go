//go:build linux

package netif

import (
	"errors"
	"net/netip"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Tap implements Unix TAP interface.
type Tap struct {
	fd int
	// ifr is used to configure the TAP interface.
	ifr unix.Ifreq
}

// OpenTap opens an existing TAP device by name. See ./make-tap.sh script.
func OpenTap(name string) (_ *Tap, err error) {
	var tap Tap

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	tap.ifr = *ifr

	tap.fd, err = unix.Open("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}

	const tunFlags = unix.IFF_TAP | // TAP device: operate at layer 2 of the OSI model, so raw ethernet frames.
		unix.IFF_NO_PI | // Prevents the kernel from including a protocol information (PI) header in the packet data.
		unix.IFF_ONE_QUEUE | //  Enables a single packet queue for the interface.
		// unix.IFF_TUN_EXCL | // Request exclusive access to the interface.
		unix.IFF_NOARP // Prevents the interface from participating in ARP exchanges.
		// unix.IFF_PROMISC // Enable promiscuous mode. In promiscuous mode, the interface receives all packets on the network segment, regardless of the destination MAC address.
		// IFF_BROADCAST is inherently set with IFF_TAP.
		// unix.IFF_BROADCAST // When set, the interface can send and receive broadcast packets.

	err = tap.tunsetFlags(tunFlags)
	if err != nil {
		tap.Close()
		return nil, err
	}

	return &tap, nil
}

func (t *Tap) SetAddr(addr netip.Addr) error {
	if !addr.Is4() {
		return errors.New("invalid addr or not ipv4")
	}
	t.clearIfreq()
	t.ifr.SetInet4Addr(addr.AsSlice())
	return unix.IoctlIfreq(t.fd, unix.SIOCSIFADDR, &t.ifr)
}

func (t *Tap) Addr() (netip.Addr, error) {
	t.clearIfreq()
	err := unix.IoctlIfreq(t.fd, unix.SIOCGIFADDR, &t.ifr)
	if err != nil {
		return netip.Addr{}, err
	}
	addr, err := t.ifr.Inet4Addr()
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.AddrFrom4([4]byte(addr)), nil
}

func (t *Tap) tunsetFlags(flags uint16) error {
	t.clearIfreq()
	t.ifr.SetUint16(flags)
	return unix.IoctlIfreq(t.fd, unix.TUNSETIFF, &t.ifr)
}

func (t *Tap) GetMTU() (uint32, error) {
	t.clearIfreq()
	err := unix.IoctlIfreq(t.fd, unix.SIOCGIFMTU, &t.ifr)
	if err != nil {
		return 0, err
	}
	return t.ifr.Uint32(), nil
}

func (t *Tap) SetMTU(mtu uint32) error {
	t.clearIfreq()
	t.ifr.SetUint32(mtu)
	return unix.IoctlIfreq(t.fd, unix.SIOCSIFMTU, &t.ifr)
}

func (t *Tap) Write(data []byte) (int, error) {
	return unix.Write(t.fd, data)
}

func (t *Tap) Read(data []byte) (int, error) {
	return unix.Read(t.fd, data)
}

func (t *Tap) Close() error {
	return unix.Close(t.fd)
}

func (t *Tap) clearIfreq() {
	// Skip over initial 16 bytes which are Name and must not change.
	ifruStart := unsafe.Pointer(uintptr(unsafe.Pointer(&t.ifr)) + 16)
	// Zero next 24 bytes which correspond to ioctl value.
	ifru := (*[24]byte)(ifruStart)
	for i := range ifru {
		ifru[i] = 0
	}
}

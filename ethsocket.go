package netif

import (
	"errors"
	"log"
	"net"
	"syscall"

	"github.com/soypat/seqs/eth"
)

type EthSocket struct {
	fd  int
	sa  syscall.SockaddrLinklayer
	hw6 [6]byte
	// ethernet handler.
	ehandler  func(b []byte) error
	buf       []byte
	netnotify func(Event)
}

func NewEthSocket(interfaceName string) (*EthSocket, error) {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return nil, err
	}
	srcMac := iface.HardwareAddr
	if len(srcMac) != 6 {
		srcMac = []byte{0, 0, 0, 0, 0, 0}
	}

	var ethsock EthSocket
	ethsock.hw6 = [6]byte(srcMac)
	ethsock.sa = syscall.SockaddrLinklayer{
		Ifindex: iface.Index,
		Halen:   uint8(len(ethsock.hw6)),
	}
	ethsock.buf = make([]byte, iface.MTU)
	ethsock.fd, err = syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		return nil, err
	}
	return &ethsock, nil
}

func (e *EthSocket) HardwareAddr6() ([6]byte, error) {
	if e.hw6 == [6]byte{} {
		return e.hw6, errors.New("no hardware address")
	}
	return e.hw6, nil
}

func (e *EthSocket) SendEth(data []byte) error {
	if len(data) < 14 {
		log.Println("ethernet frame too short")
	}
	ehdr := eth.DecodeEthernetHeader(data)
	e.sa.Protocol = ehdr.SizeOrEtherType
	copy(e.sa.Addr[:6], ehdr.Destination[:6])
	return syscall.Sendto(e.fd, data, 0, &e.sa)
}

func (e *EthSocket) RecvEthHandle(fn func(b []byte) error) {
	e.ehandler = fn
}

func (e *EthSocket) NetFlags() net.Flags {
	if e.fd <= 0 {
		return net.Flags(0)
	}
	return net.FlagUp | net.FlagRunning
}

func (e *EthSocket) NetNotify(cb func(Event)) error {
	e.netnotify = cb
	return nil
}

func (e *EthSocket) PollOne() (bool, error) {
	if e.NetFlags()&net.FlagUp == 0 {
		return false, net.ErrClosed
	} else if e.ehandler == nil {
		return false, errors.New("no handler")
	}
	if len(e.buf) == 0 {
		e.buf = make([]byte, 1500)
	}
	for i := range e.buf {
		e.buf[i] = 0
	}
	n, _, err := syscall.Recvfrom(e.fd, e.buf, 0)
	if err != nil || n == 0 {
		return n > 0, err
	}
	err = e.ehandler(e.buf[:n])
	return true, err
}

func (e *EthSocket) SetPollBuffer(buf []byte) {
	e.buf = buf
}

func (e *EthSocket) Close() error {
	fd := e.fd
	e.fd = -1
	if e.netnotify != nil {
		e.netnotify(EventNetDown)
	}
	return syscall.Close(fd)
}

func (e *EthSocket) MTU() int {
	return len(e.buf)
}

// htons converts a short (uint16) from host-to-network byte order.
func htons(i uint16) uint16 {
	return (i<<8)&0xff00 | i>>8
}

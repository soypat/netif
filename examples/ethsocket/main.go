package main

import (
	"log"
	"net"
	"os"
	"syscall"
)

func main() {
	ifname := os.Args[1]
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		log.Fatal("get link by name:", err)
	}

	srcMac := iface.HardwareAddr
	if len(srcMac) == 0 {
		srcMac = []byte{0, 0, 0, 0, 0, 0}
	}
	dstMac := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}

	fd, _ := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
	addr := syscall.SockaddrLinklayer{
		Ifindex: iface.Index,
		Halen:   6, // Ethernet address length is 6 bytes
		Addr: [8]uint8{
			dstMac[0],
			dstMac[1],
			dstMac[2],
			dstMac[3],
			dstMac[4],
			dstMac[5],
		},
	}

	ethHeader := []byte{
		dstMac[0], dstMac[1], dstMac[2], dstMac[3], dstMac[4], dstMac[5],
		srcMac[0], srcMac[1], srcMac[2], srcMac[3], srcMac[4], srcMac[5],
		0x12, 0x34, // your custom ethertype
	}

	// Your custom data
	p := append(ethHeader, []byte("Hello World")...)

	err = syscall.Sendto(fd, p, 0, &addr)
	if err != nil {
		log.Fatal("Sendto:", err)
	}
}

// htons converts a short (uint16) from host-to-network byte order.
func htons(i uint16) uint16 {
	return (i<<8)&0xff00 | i>>8
}

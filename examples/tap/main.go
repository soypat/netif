package main

import (
	"log"

	"github.com/soypat/netif"
)

func main() {
	// Open TAP interface
	tap, err := netif.OpenTap("tap0")
	if err != nil {
		log.Fatal("Error opening TAP interface:", err)
	}
	mtu, err := tap.GetMTU()
	if err != nil {
		log.Println("Error getting MTU:", err)
	} else {
		log.Println("TAP interface MTU:", mtu)
	}

	addr, err := tap.Addr()
	if err != nil {
		log.Println("Error getting address:", err)
	} else {
		log.Println("TAP interface address:", addr)
	}

	_, err = tap.Write([]byte("Hello, TAP interface!"))
	if err != nil {
		log.Fatal("Error writing to TAP interface:", err)
	}

	log.Println("Data sent over TAP interface successfully.")
}

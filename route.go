//go:build linux

package netif

import (
	"errors"
	"net"
	"os/exec"
	"strings"
)

// Default interface returns the default network interface over which traffic is routed.
func DefaultInterface() (*net.Interface, error) {

	routeCmd := exec.Command("route", "-n")
	output, _ := routeCmd.CombinedOutput()

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 8 && fields[0] == "0.0.0.0" {
			iface := fields[7]
			return net.InterfaceByName(iface)
		}
	}

	return nil, errors.New("no default interface found")
}

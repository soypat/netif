//go:build !baremetal

package netif

import (
	"net/netip"
	"testing"
)

func Test(t *testing.T) {
	p, err := netip.ParsePrefix("192.168.1.32/24")
	t.Error(p, p.Masked(), err)
}

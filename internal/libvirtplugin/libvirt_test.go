package libvirtplugin

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/digitalocean/go-libvirt"
)

func TestEnsureRedialsDroppedConnection(t *testing.T) {
	dials := 0
	alive := true
	c := &libvirtClient{
		uri:  &url.URL{Scheme: "test"},
		dial: func(*url.URL) (*libvirt.Libvirt, error) { dials++; return &libvirt.Libvirt{}, nil },
		live: func(*libvirt.Libvirt) bool { return alive },
	}
	for range 2 {
		if err := c.ensure(); err != nil {
			t.Fatal(err)
		}
	}
	if dials != 1 {
		t.Fatalf("dials=%d with a live connection, want 1", dials)
	}
	alive = false
	if err := c.ensure(); err != nil {
		t.Fatal(err)
	}
	if dials != 2 {
		t.Fatalf("dials=%d after the connection dropped, want 2", dials)
	}
}

func TestAddressesOfMACIgnoresOtherInterfaces(t *testing.T) {
	ifaces := []libvirt.DomainInterface{
		{Name: "docker0", Hwaddr: libvirt.OptString{"02:42:aa:bb:cc:dd"}, Addrs: []libvirt.DomainIPAddr{{Addr: "172.17.0.1"}}},
		{Name: "lo", Addrs: []libvirt.DomainIPAddr{{Addr: "127.0.0.1"}}},
		{Name: "enp1s0", Hwaddr: libvirt.OptString{"52:54:00:AB:CD:EF"}, Addrs: []libvirt.DomainIPAddr{{Addr: "fe80::1"}, {Addr: "192.168.122.9"}}},
	}
	got := addressesOfMAC(ifaces, "52:54:00:ab:cd:ef")
	if !slices.Equal(got, []string{"fe80::1", "192.168.122.9"}) {
		t.Fatalf("got %v", got)
	}
	if pickAddress(got) != "192.168.122.9" {
		t.Fatalf("picked %q", pickAddress(got))
	}
	if got := addressesOfMAC(ifaces, ""); len(got) != 0 {
		t.Fatalf("empty MAC matched %v", got)
	}
}

func TestURIPasswordNeverReachesErrors(t *testing.T) {
	const pw = "hunter2secret"
	u, err := url.Parse("qemu+tcp://admin:" + pw + "@host/system")
	if err != nil {
		t.Fatal(err)
	}
	c := &libvirtClient{
		uri:  u,
		dial: func(*url.URL) (*libvirt.Libvirt, error) { return nil, errors.New("refused " + u.String()) },
		live: func(*libvirt.Libvirt) bool { return false },
	}
	if err := c.ensure(); err == nil || strings.Contains(err.Error(), pw) {
		t.Errorf("ensure error = %v, want one without the password", err)
	}
	if _, err := dialLibvirt("qemu+tcp://admin:" + pw + "@ho st:%zz/system"); err == nil || strings.Contains(err.Error(), pw) {
		t.Errorf("dialLibvirt error = %v, want one without the password", err)
	}
}

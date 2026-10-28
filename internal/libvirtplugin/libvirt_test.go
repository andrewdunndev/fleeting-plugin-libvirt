package libvirtplugin

import (
	"net/url"
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

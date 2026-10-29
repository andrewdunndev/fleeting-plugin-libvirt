package libvirtplugin

import (
	"context"
	"errors"
)

// ErrNotFound is returned by a Client when a domain or volume does not exist.
var ErrNotFound = errors.New("not found")

// Base describes the volume that instances are cloned from.
type Base struct {
	Path          string
	Format        string
	CapacityBytes uint64
}

// DomainInfo is what Update needs to know about a domain.
type DomainInfo struct {
	Name string
	// Prefix comes from the plugin's metadata namespace and is
	// empty for domains that do not carry it.
	Prefix string
}

// Client is the slice of libvirt the plugin uses.
type Client interface {
	Close() error
	BaseVolume(ctx context.Context, pool, name string) (Base, error)
	// CreateVolume defines a volume from XML and returns its path.
	CreateVolume(ctx context.Context, pool, name, xml string) (path string, err error)
	DeleteVolume(ctx context.Context, pool, name string) error
	ListVolumes(ctx context.Context, pool string) ([]string, error)
	// CreateDomain defines and starts a transient domain.
	CreateDomain(ctx context.Context, xml string) error
	DestroyDomain(ctx context.Context, name string) error
	// ListDomains returns the domains whose name starts with namePrefix.
	ListDomains(ctx context.Context, namePrefix string) ([]DomainInfo, error)
	DomainRunning(ctx context.Context, name string) (bool, error)
	// GuestAddresses returns the addresses the guest agent reports on the
	// interface carrying the domain's own MAC. An agent that is not up yet is
	// an error, not an empty list.
	GuestAddresses(ctx context.Context, name string) ([]string, error)
}

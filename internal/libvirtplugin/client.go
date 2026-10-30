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
	// LibVersion returns the libvirt version as major*1000000 + minor*1000 + release.
	LibVersion(ctx context.Context) (uint64, error)
	BaseVolume(ctx context.Context, pool, name string) (Base, error)
	// CreateVolume defines a volume from XML and returns its path.
	CreateVolume(ctx context.Context, pool, name, xml string) (path string, err error)
	// CloneVolume defines a volume from XML as a copy of the volume src and
	// returns its path.
	CloneVolume(ctx context.Context, pool, src, name, xml string) (path string, err error)
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
	// LeaseAddresses returns the addresses libvirt's DHCP leases hold for the
	// domain's own MAC, for guests without an agent on a libvirt-managed
	// network.
	LeaseAddresses(ctx context.Context, name string) ([]string, error)
}

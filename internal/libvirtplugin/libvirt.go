package libvirtplugin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/digitalocean/go-libvirt"
)

type libvirtClient struct {
	uri  *url.URL
	dial func(*url.URL) (*libvirt.Libvirt, error)
	live func(*libvirt.Libvirt) bool

	mu sync.Mutex
	l  *libvirt.Libvirt
}

func dialLibvirt(uri string) (Client, error) {
	u, err := url.Parse(uri)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("plugin_config.uri is not a valid URL: %w", err)
	}
	c := &libvirtClient{uri: u, dial: libvirt.ConnectToURI, live: (*libvirt.Libvirt).IsConnected}
	if err := c.ensure(); err != nil {
		return nil, err
	}
	return c, nil
}

// ensure redials when libvirtd has dropped the connection, so a daemon
// restart does not outlive the plugin.
func (c *libvirtClient) ensure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.l != nil && c.live(c.l) {
		return nil
	}
	l, err := c.dial(c.uri)
	if err != nil {
		return fmt.Errorf("connecting to %s: %s; check the socket path and that the user may use it", c.uri.Redacted(), scrub(err.Error(), c.uri))
	}
	c.l = l
	return nil
}

func (c *libvirtClient) cur() *libvirt.Libvirt {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.l
}

func (c *libvirtClient) Close() error { return c.cur().Disconnect() }

func hasCode(err error, codes ...libvirt.ErrorNumber) bool {
	var e libvirt.Error
	if !errors.As(err, &e) {
		return false
	}
	for _, code := range codes {
		if e.Code == uint32(code) {
			return true
		}
	}
	return false
}

func (c *libvirtClient) pool(ctx context.Context, name string) (libvirt.StoragePool, error) {
	if err := ctx.Err(); err != nil {
		return libvirt.StoragePool{}, err
	}
	if err := c.ensure(); err != nil {
		return libvirt.StoragePool{}, err
	}
	p, err := c.cur().StoragePoolLookupByName(name)
	if hasCode(err, libvirt.ErrNoStoragePool) {
		return p, fmt.Errorf("storage pool %q does not exist; check plugin_config.pool_name", name)
	}
	return p, err
}

func (c *libvirtClient) BaseVolume(ctx context.Context, pool, name string) (Base, error) {
	p, err := c.pool(ctx, pool)
	if err != nil {
		return Base{}, err
	}
	v, err := c.cur().StorageVolLookupByName(p, name)
	if hasCode(err, libvirt.ErrNoStorageVol) {
		return Base{}, fmt.Errorf("volume %q in pool %q: %w", name, pool, ErrNotFound)
	}
	if err != nil {
		return Base{}, err
	}
	_, capacity, _, err := c.cur().StorageVolGetInfo(v)
	if err != nil {
		return Base{}, err
	}
	path, err := c.cur().StorageVolGetPath(v)
	if err != nil {
		return Base{}, err
	}
	desc, err := c.cur().StorageVolGetXMLDesc(v, 0)
	if err != nil {
		return Base{}, err
	}
	format, err := parseVolumeFormat(desc)
	if err != nil {
		return Base{}, err
	}
	return Base{Path: path, Format: format, CapacityBytes: capacity}, nil
}

func (c *libvirtClient) CreateVolume(ctx context.Context, pool, _, xml string) (string, error) {
	p, err := c.pool(ctx, pool)
	if err != nil {
		return "", err
	}
	v, err := c.cur().StorageVolCreateXML(p, xml, 0)
	if err != nil {
		return "", err
	}
	path, err := c.cur().StorageVolGetPath(v)
	if err != nil {
		_ = c.cur().StorageVolDelete(v, libvirt.StorageVolDeleteNormal)
		return "", err
	}
	return path, nil
}

func (c *libvirtClient) DeleteVolume(ctx context.Context, pool, name string) error {
	p, err := c.pool(ctx, pool)
	if err != nil {
		return err
	}
	v, err := c.cur().StorageVolLookupByName(p, name)
	if hasCode(err, libvirt.ErrNoStorageVol) {
		return fmt.Errorf("volume %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return err
	}
	err = c.cur().StorageVolDelete(v, libvirt.StorageVolDeleteNormal)
	if hasCode(err, libvirt.ErrNoStorageVol) {
		return fmt.Errorf("volume %q: %w", name, ErrNotFound)
	}
	return err
}

func (c *libvirtClient) ListVolumes(ctx context.Context, pool string) ([]string, error) {
	p, err := c.pool(ctx, pool)
	if err != nil {
		return nil, err
	}
	vols, _, err := c.cur().StoragePoolListAllVolumes(p, 1, 0)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(vols))
	for i, v := range vols {
		names[i] = v.Name
	}
	return names, nil
}

func (c *libvirtClient) CreateDomain(ctx context.Context, xml string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ensure(); err != nil {
		return err
	}
	_, err := c.cur().DomainCreateXML(xml, libvirt.DomainNone)
	return err
}

func (c *libvirtClient) lookup(ctx context.Context, name string) (libvirt.Domain, error) {
	if err := ctx.Err(); err != nil {
		return libvirt.Domain{}, err
	}
	if err := c.ensure(); err != nil {
		return libvirt.Domain{}, err
	}
	d, err := c.cur().DomainLookupByName(name)
	if hasCode(err, libvirt.ErrNoDomain) {
		return d, fmt.Errorf("domain %q: %w", name, ErrNotFound)
	}
	return d, err
}

func (c *libvirtClient) DestroyDomain(ctx context.Context, name string) error {
	d, err := c.lookup(ctx, name)
	if err != nil {
		return err
	}
	err = c.cur().DomainDestroy(d)
	if hasCode(err, libvirt.ErrNoDomain, libvirt.ErrOperationInvalid) {
		return fmt.Errorf("domain %q: %w", name, ErrNotFound)
	}
	return err
}

func (c *libvirtClient) ListDomains(ctx context.Context, namePrefix string) ([]DomainInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.ensure(); err != nil {
		return nil, err
	}
	doms, _, err := c.cur().ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, err
	}
	var out []DomainInfo
	for _, d := range doms {
		if !strings.HasPrefix(d.Name, namePrefix) {
			continue
		}
		desc, err := c.cur().DomainGetXMLDesc(d, 0)
		if hasCode(err, libvirt.ErrNoDomain) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := parseDomainXML(desc)
		if err != nil {
			return nil, err
		}
		info.Name = d.Name
		out = append(out, info)
	}
	return out, nil
}

func (c *libvirtClient) DomainRunning(ctx context.Context, name string) (bool, error) {
	d, err := c.lookup(ctx, name)
	if err != nil {
		return false, err
	}
	state, _, err := c.cur().DomainGetState(d, 0)
	if hasCode(err, libvirt.ErrNoDomain) {
		return false, fmt.Errorf("domain %q: %w", name, ErrNotFound)
	}
	return state == int32(libvirt.DomainRunning), err
}

func (c *libvirtClient) GuestAddresses(ctx context.Context, name string) ([]string, error) {
	return c.addresses(ctx, name, libvirt.DomainInterfaceAddressesSrcAgent)
}

func (c *libvirtClient) LeaseAddresses(ctx context.Context, name string) ([]string, error) {
	return c.addresses(ctx, name, libvirt.DomainInterfaceAddressesSrcLease)
}

func (c *libvirtClient) addresses(ctx context.Context, name string, src libvirt.DomainInterfaceAddressesSource) ([]string, error) {
	d, err := c.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	desc, err := c.cur().DomainGetXMLDesc(d, 0)
	if hasCode(err, libvirt.ErrNoDomain) {
		return nil, fmt.Errorf("domain %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	mac, err := parseDomainMAC(desc)
	if err != nil {
		return nil, err
	}
	ifaces, err := c.cur().DomainInterfaceAddresses(d, uint32(src), 0)
	if hasCode(err, libvirt.ErrNoDomain) {
		return nil, fmt.Errorf("domain %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return addressesOfMAC(ifaces, mac), nil
}

// addressesOfMAC returns the addresses of the guest interface whose hardware
// address is mac, so that bridges and container networks inside the guest
// never supply the connect address.
func addressesOfMAC(ifaces []libvirt.DomainInterface, mac string) []string {
	var addrs []string
	for _, i := range ifaces {
		if len(i.Hwaddr) == 0 || !strings.EqualFold(i.Hwaddr[0], mac) {
			continue
		}
		for _, a := range i.Addrs {
			addrs = append(addrs, a.Addr)
		}
	}
	return addrs
}

// scrub removes the URI password from text that may echo it.
func scrub(s string, u *url.URL) string {
	if pw, ok := u.User.Password(); ok && pw != "" {
		s = strings.ReplaceAll(s, pw, "xxxxx")
	}
	return s
}

func (c *libvirtClient) LibVersion(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := c.ensure(); err != nil {
		return 0, err
	}
	return c.cur().ConnectGetLibVersion()
}

func (c *libvirtClient) CloneVolume(ctx context.Context, pool, src, _, xml string) (string, error) {
	p, err := c.pool(ctx, pool)
	if err != nil {
		return "", err
	}
	from, err := c.cur().StorageVolLookupByName(p, src)
	if hasCode(err, libvirt.ErrNoStorageVol) {
		return "", fmt.Errorf("volume %q in pool %q: %w", src, pool, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	v, err := c.cur().StorageVolCreateXMLFrom(p, xml, from, 0)
	if err != nil {
		return "", err
	}
	path, err := c.cur().StorageVolGetPath(v)
	if err != nil {
		_ = c.cur().StorageVolDelete(v, libvirt.StorageVolDeleteNormal)
		return "", err
	}
	return path, nil
}

func (c *libvirtClient) DomainCaps(ctx context.Context, machine string) (DomainCaps, error) {
	if err := ctx.Err(); err != nil {
		return DomainCaps{}, err
	}
	if err := c.ensure(); err != nil {
		return DomainCaps{}, err
	}
	var m libvirt.OptString
	if machine != "" {
		m = libvirt.OptString{machine}
	}
	s, err := c.cur().ConnectGetDomainCapabilities(nil, nil, m, libvirt.OptString{"kvm"}, 0)
	if err != nil {
		return DomainCaps{}, err
	}
	return parseDomainCaps(s)
}

func (c *libvirtClient) NodeDeviceXML(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := c.ensure(); err != nil {
		return "", err
	}
	return c.cur().NodeDeviceGetXMLDesc(name, 0)
}

func (c *libvirtClient) ActiveDomainsXML(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.ensure(); err != nil {
		return nil, err
	}
	doms, _, err := c.cur().ConnectListAllDomains(1, libvirt.ConnectListDomainsActive)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range doms {
		desc, err := c.cur().DomainGetXMLDesc(d, 0)
		if hasCode(err, libvirt.ErrNoDomain) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, desc)
	}
	return out, nil
}

func (c *libvirtClient) NWFilterExists(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ensure(); err != nil {
		return err
	}
	_, err := c.cur().NwfilterLookupByName(name)
	if hasCode(err, libvirt.ErrNoNwfilter) {
		return ErrNotFound
	}
	return err
}

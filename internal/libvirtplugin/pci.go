package libvirtplugin

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	pciAddressPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	pciBridgeClass    = regexp.MustCompile(`^0x06[0-9a-f]{4}$`)
)

const driverVFIO = "vfio-pci"

// pciNodeName is libvirt's node-device name for a PCI address.
func pciNodeName(addr string) string {
	return "pci_" + strings.NewReplacer(":", "_", ".", "_").Replace(addr)
}

// normalizePCI lower-cases the pool and checks its shape and uniqueness.
func (c *Config) normalizePCI() error {
	if len(c.PCIDevices) == 0 {
		return nil
	}
	if u, err := url.Parse(c.URI); err == nil && strings.HasSuffix(u.Path, "/session") {
		return fmt.Errorf("plugin_config.pci_devices is not supported with the session URI %q: use the system daemon, qemu:///system", c.URI)
	}
	seen := map[string]bool{}
	for i, a := range c.PCIDevices {
		a = strings.ToLower(a)
		if !pciAddressPattern.MatchString(a) {
			return fmt.Errorf("plugin_config.pci_devices entry %q must be a PCI address such as 0000:03:00.0", c.PCIDevices[i])
		}
		if seen[a] {
			return fmt.Errorf("plugin_config.pci_devices lists %s twice", a)
		}
		seen[a] = true
		c.PCIDevices[i] = a
	}
	return nil
}

// checkPCI reads each pool device and its IOMMU group peers and refuses a
// pool the host has not prepared for passthrough.
func (g *InstanceGroup) checkPCI(ctx context.Context, client Client) error {
	pool := map[string]bool{}
	for _, a := range g.PCIDevices {
		pool[a] = true
	}
	groups := map[int]string{}
	peers := map[string]nodeDevice{}
	for _, a := range g.PCIDevices {
		d, err := readNodeDevice(ctx, client, a)
		if err != nil {
			return fmt.Errorf("plugin_config.pci_devices %s: %w; check the address and that the device is present", a, err)
		}
		if d.Driver != driverVFIO {
			return fmt.Errorf("plugin_config.pci_devices %s is bound to %q, not %s; bind it to %s on the host before starting", a, d.Driver, driverVFIO, driverVFIO)
		}
		if d.Group < 0 {
			return fmt.Errorf("plugin_config.pci_devices %s is in no IOMMU group; enable the IOMMU on the host", a)
		}
		if other, ok := groups[d.Group]; ok {
			return fmt.Errorf("plugin_config.pci_devices %s and %s share IOMMU group %d; list one of them", other, a, d.Group)
		}
		groups[d.Group] = a
		for _, p := range d.Peers {
			if p == a {
				continue
			}
			if pool[p] {
				return fmt.Errorf("plugin_config.pci_devices %s and %s share IOMMU group %d; list one of them", a, p, d.Group)
			}
			pd, ok := peers[p]
			if !ok {
				if pd, err = readNodeDevice(ctx, client, p); err != nil {
					return fmt.Errorf("plugin_config.pci_devices %s: IOMMU group peer %s: %w", a, p, err)
				}
				peers[p] = pd
			}
			if pciBridgeClass.MatchString(pd.Class) {
				continue
			}
			if pd.Driver != driverVFIO {
				return fmt.Errorf("plugin_config.pci_devices %s: IOMMU group peer %s is bound to %q, not %s; bind it to %s on the host", a, p, pd.Driver, driverVFIO, driverVFIO)
			}
		}
	}
	return nil
}

func readNodeDevice(ctx context.Context, client Client, addr string) (nodeDevice, error) {
	s, err := client.NodeDeviceXML(ctx, pciNodeName(addr))
	if err != nil {
		return nodeDevice{}, fmt.Errorf("reading node device %s: %w", pciNodeName(addr), err)
	}
	return parseNodeDevice(s)
}

// maxSize is the pool size, lowered by max_size when that is smaller; with no
// pool it is max_size alone.
func (g *InstanceGroup) maxSize() int {
	if len(g.PCIDevices) == 0 {
		return cmp.Or(g.MaxSize, unboundedSize)
	}
	if g.MaxSize > 0 {
		return min(g.MaxSize, len(g.PCIDevices))
	}
	return len(g.PCIDevices)
}

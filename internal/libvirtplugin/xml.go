package libvirtplugin

import (
	"encoding/xml"
	"fmt"
	"maps"
	"slices"
	"strings"
)

type domainSpec struct {
	Name           string
	Title          string
	Prefix         string
	Partition      string
	Network        string
	NWFilter       string // empty attaches no filter
	NWFilterParams map[string]string
	DiskPath       string
	VCPU           int
	MemoryMiB      int
	CredentialKey  string // credential name; empty seeds nothing
	Firmware       string // "efi" selects libvirt firmware autoselection
	SecureBoot     bool
	NVRAMPath      string
	NVRAMFormat    string
	TPM            bool
	PCIAddress     string // lower-case PCI address of the hostdev; empty adds none
	AuthorizedKey  string
}

type typeAttr struct {
	Type string `xml:"type,attr"`
}

type domainDoc struct {
	XMLName    xml.Name     `xml:"domain"`
	Type       string       `xml:"type,attr"`
	Name       string       `xml:"name"`
	Title      string       `xml:"title,omitempty"`
	Metadata   metadataDoc  `xml:"metadata"`
	Memory     memoryDoc    `xml:"memory"`
	VCPU       int          `xml:"vcpu"`
	Resource   *resourceDoc `xml:"resource,omitempty"`
	OS         osDoc        `xml:"os"`
	SysInfo    *sysInfoDoc  `xml:"sysinfo,omitempty"`
	Features   featuresDoc  `xml:"features"`
	CPU        cpuDoc       `xml:"cpu"`
	Clock      clockDoc     `xml:"clock"`
	OnPoweroff string       `xml:"on_poweroff"`
	OnReboot   string       `xml:"on_reboot"`
	OnCrash    string       `xml:"on_crash"`
	Devices    devicesDoc   `xml:"devices"`
}

type metadataDoc struct {
	Instance instanceMeta `xml:"urn:fleeting-plugin-libvirt:v1 instance"`
}

type instanceMeta struct {
	Prefix string `xml:"prefix"`
}

type memoryDoc struct {
	Unit  string `xml:"unit,attr"`
	Value int    `xml:",chardata"`
}

type resourceDoc struct {
	Partition string `xml:"partition"`
}

type osDoc struct {
	Firmware string      `xml:"firmware,attr,omitempty"`
	Type     osTypeDoc   `xml:"type"`
	Features *fwFeatures `xml:"firmware,omitempty"`
	Loader   *loaderDoc  `xml:"loader,omitempty"`
	NVRAM    *nvramDoc   `xml:"nvram,omitempty"`
	SMBIOS   *smbiosDoc  `xml:"smbios,omitempty"`
}

type osTypeDoc struct {
	Machine string `xml:"machine,attr,omitempty"`
	Value   string `xml:",chardata"`
}

type fwFeatures struct {
	Feature []fwFeature `xml:"feature"`
}

type fwFeature struct {
	Enabled string `xml:"enabled,attr"`
	Name    string `xml:"name,attr"`
}

type loaderDoc struct {
	Format string `xml:"format,attr"`
}

type nvramDoc struct {
	Format string `xml:"format,attr"`
	Path   string `xml:",chardata"`
}

type smbiosDoc struct {
	Mode string `xml:"mode,attr"`
}

type sysInfoDoc struct {
	Type       string   `xml:"type,attr"`
	OEMStrings []string `xml:"oemStrings>entry"`
}

type featuresDoc struct {
	ACPI struct{} `xml:"acpi"`
	APIC struct{} `xml:"apic"`
	SMM  *smmDoc  `xml:"smm,omitempty"`
}

type smmDoc struct {
	State string `xml:"state,attr"`
}

type cpuDoc struct {
	Mode  string `xml:"mode,attr"`
	Check string `xml:"check,attr"`
}

type clockDoc struct {
	Offset string     `xml:"offset,attr"`
	Timers []timerDoc `xml:"timer"`
}

type timerDoc struct {
	Name       string `xml:"name,attr"`
	TickPolicy string `xml:"tickpolicy,attr,omitempty"`
	Present    string `xml:"present,attr,omitempty"`
}

type devicesDoc struct {
	Disk      diskDoc      `xml:"disk"`
	Interface interfaceDoc `xml:"interface"`
	Channel   channelDoc   `xml:"channel"`
	Serial    serialDoc    `xml:"serial"`
	Console   consoleDoc   `xml:"console"`
	RNG       rngDoc       `xml:"rng"`
	TPM       *tpmDoc      `xml:"tpm,omitempty"`
	Hostdev   *hostdevDoc  `xml:"hostdev,omitempty"`
}

type diskDoc struct {
	Type   string    `xml:"type,attr"`
	Device string    `xml:"device,attr"`
	Driver driverDoc `xml:"driver"`
	Source struct {
		File string `xml:"file,attr"`
	} `xml:"source"`
	Target struct {
		Dev string `xml:"dev,attr"`
		Bus string `xml:"bus,attr"`
	} `xml:"target"`
}

type driverDoc struct {
	Name string `xml:"name,attr"`
	Type string `xml:"type,attr"`
}

type interfaceDoc struct {
	Type   string `xml:"type,attr"`
	Source struct {
		Network string `xml:"network,attr"`
	} `xml:"source"`
	Model     typeAttr      `xml:"model"`
	FilterRef *filterRefDoc `xml:"filterref"`
}

type filterRefDoc struct {
	Filter     string           `xml:"filter,attr"`
	Parameters []filterParamDoc `xml:"parameter"`
}

type filterParamDoc struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type channelDoc struct {
	Type   string `xml:"type,attr"`
	Target struct {
		Type string `xml:"type,attr"`
		Name string `xml:"name,attr"`
	} `xml:"target"`
}

type serialDoc struct {
	Type   string `xml:"type,attr"`
	Target struct {
		Port int `xml:"port,attr"`
	} `xml:"target"`
}

type consoleDoc struct {
	Type   string `xml:"type,attr"`
	Target struct {
		Type string `xml:"type,attr"`
		Port int    `xml:"port,attr"`
	} `xml:"target"`
}

type rngDoc struct {
	Model   string `xml:"model,attr"`
	Backend struct {
		Model string `xml:"model,attr"`
		Path  string `xml:",chardata"`
	} `xml:"backend"`
}

type tpmDoc struct {
	Model   string `xml:"model,attr"`
	Backend struct {
		Type    string `xml:"type,attr"`
		Version string `xml:"version,attr"`
	} `xml:"backend"`
}

type hostdevDoc struct {
	Mode    string `xml:"mode,attr"`
	Type    string `xml:"type,attr"`
	Managed string `xml:"managed,attr"`
	Source  struct {
		Address pciAddressDoc `xml:"address"`
	} `xml:"source"`
}

type pciAddressDoc struct {
	Domain   string `xml:"domain,attr"`
	Bus      string `xml:"bus,attr"`
	Slot     string `xml:"slot,attr"`
	Function string `xml:"function,attr"`
}

const guestAgentChannel = "org.qemu.guest_agent.0"

func domainXML(s domainSpec) (string, error) {
	d := domainDoc{
		Type:       "kvm",
		Name:       s.Name,
		Title:      s.Title,
		Metadata:   metadataDoc{Instance: instanceMeta{Prefix: s.Prefix}},
		Memory:     memoryDoc{Unit: "MiB", Value: s.MemoryMiB},
		VCPU:       s.VCPU,
		OS:         osDoc{Type: osTypeDoc{Value: "hvm"}},
		CPU:        cpuDoc{Mode: "host-passthrough", Check: "none"},
		OnPoweroff: "destroy",
		OnReboot:   "restart",
		OnCrash:    "destroy",
		Clock: clockDoc{Offset: "utc", Timers: []timerDoc{
			{Name: "rtc", TickPolicy: "catchup"},
			{Name: "pit", TickPolicy: "delay"},
			{Name: "hpet", Present: "no"},
		}},
	}
	if s.Firmware == firmwareEFI {
		d.OS.Firmware = firmwareEFI
		d.OS.Type.Machine = "q35"
		d.OS.Loader = &loaderDoc{Format: "qcow2"}
		d.OS.Features = &fwFeatures{Feature: []fwFeature{{Enabled: "no", Name: "secure-boot"}}}
		d.OS.NVRAM = &nvramDoc{Format: s.NVRAMFormat, Path: s.NVRAMPath}
		if s.SecureBoot {
			d.OS.Features = &fwFeatures{Feature: []fwFeature{
				{Enabled: "yes", Name: "secure-boot"},
				{Enabled: "no", Name: "enrolled-keys"},
			}}
			d.Features.SMM = &smmDoc{State: "on"}
		}
	}
	if s.Partition != "" {
		d.Resource = &resourceDoc{Partition: s.Partition}
	}
	if s.CredentialKey != "" {
		d.OS.SMBIOS = &smbiosDoc{Mode: "sysinfo"}
		d.SysInfo = &sysInfoDoc{Type: "smbios", OEMStrings: []string{
			"io.systemd.credential:" + s.CredentialKey + "=" + s.AuthorizedKey,
		}}
	}
	d.Devices.Disk = diskDoc{Type: "file", Device: "disk", Driver: driverDoc{Name: "qemu", Type: "qcow2"}}
	d.Devices.Disk.Source.File = s.DiskPath
	d.Devices.Disk.Target.Dev, d.Devices.Disk.Target.Bus = "vda", "virtio"
	d.Devices.Interface = interfaceDoc{Type: "network", Model: typeAttr{Type: "virtio"}}
	d.Devices.Interface.Source.Network = s.Network
	if s.NWFilter != "" {
		ref := &filterRefDoc{Filter: s.NWFilter}
		for _, k := range slices.Sorted(maps.Keys(s.NWFilterParams)) {
			ref.Parameters = append(ref.Parameters, filterParamDoc{Name: k, Value: s.NWFilterParams[k]})
		}
		d.Devices.Interface.FilterRef = ref
	}
	d.Devices.Channel.Type = "unix"
	d.Devices.Channel.Target.Type, d.Devices.Channel.Target.Name = "virtio", guestAgentChannel
	d.Devices.Serial.Type = "pty"
	d.Devices.Console.Type = "pty"
	d.Devices.Console.Target.Type = "serial"
	d.Devices.RNG.Model = "virtio"
	d.Devices.RNG.Backend.Model, d.Devices.RNG.Backend.Path = "random", "/dev/urandom"

	if s.TPM {
		d.Devices.TPM = &tpmDoc{Model: "tpm-tis"}
		d.Devices.TPM.Backend.Type, d.Devices.TPM.Backend.Version = "emulator", "2.0"
	}

	if s.PCIAddress != "" {
		h := &hostdevDoc{Mode: "subsystem", Type: "pci", Managed: "no"}
		h.Source.Address = pciAddressDoc{
			Domain: "0x" + s.PCIAddress[0:4], Bus: "0x" + s.PCIAddress[5:7],
			Slot: "0x" + s.PCIAddress[8:10], Function: "0x" + s.PCIAddress[11:],
		}
		d.Devices.Hostdev = h
	}

	b, err := xml.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("building domain XML: %w", err)
	}
	return string(b), nil
}

type backingDoc struct {
	Path   string   `xml:"path"`
	Format typeAttr `xml:"format"`
}

type volumeDoc struct {
	XMLName  xml.Name `xml:"volume"`
	Name     string   `xml:"name"`
	Capacity struct {
		Unit  string `xml:"unit,attr"`
		Value uint64 `xml:",chardata"`
	} `xml:"capacity"`
	Target struct {
		Format typeAttr `xml:"format"`
	} `xml:"target"`
	Backing *backingDoc `xml:"backingStore,omitempty"`
}

// volumeXML describes a qcow2 overlay on base. The base format defaults to
// qcow2 when the pool does not report one.
func volumeXML(name string, base Base, capacity uint64) (string, error) {
	v := volumeDoc{Name: name}
	v.Capacity.Unit, v.Capacity.Value = "bytes", capacity
	v.Target.Format.Type = "qcow2"
	v.Backing = &backingDoc{Path: base.Path}
	v.Backing.Format.Type = base.Format
	if v.Backing.Format.Type == "" {
		v.Backing.Format.Type = "qcow2"
	}
	b, err := xml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("building volume XML: %w", err)
	}
	return string(b), nil
}

// parseDomainXML extracts the identity of a domain from libvirt's XML.
func parseDomainXML(s string) (DomainInfo, error) {
	var d struct {
		Name     string `xml:"name"`
		Metadata struct {
			Instance instanceMeta `xml:"urn:fleeting-plugin-libvirt:v1 instance"`
		} `xml:"metadata"`
	}
	if err := xml.Unmarshal([]byte(s), &d); err != nil {
		return DomainInfo{}, fmt.Errorf("parsing domain XML: %w", err)
	}
	return DomainInfo{Name: d.Name, Prefix: d.Metadata.Instance.Prefix}, nil
}

// parseDomainMAC returns the hardware address of the domain's interface.
func parseDomainMAC(s string) (string, error) {
	var d struct {
		MAC struct {
			Address string `xml:"address,attr"`
		} `xml:"devices>interface>mac"`
	}
	if err := xml.Unmarshal([]byte(s), &d); err != nil {
		return "", fmt.Errorf("parsing domain XML: %w", err)
	}
	return d.MAC.Address, nil
}

// parseVolumeFormat returns target/format@type of a volume's XML.
func parseVolumeFormat(s string) (string, error) {
	var v struct {
		Target struct {
			Format typeAttr `xml:"format"`
		} `xml:"target"`
	}
	if err := xml.Unmarshal([]byte(s), &v); err != nil {
		return "", fmt.Errorf("parsing volume XML: %w", err)
	}
	return v.Target.Format.Type, nil
}

// nvramXML describes a copy of the NVRAM template in its own format.
func nvramXML(name, format string, capacity uint64) (string, error) {
	v := volumeDoc{Name: name}
	v.Capacity.Unit, v.Capacity.Value = "bytes", capacity
	v.Target.Format.Type = format
	b, err := xml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("building NVRAM volume XML: %w", err)
	}
	return string(b), nil
}

// parseDomainCaps reads the TPM and PCI hostdev enumerations from libvirt's
// domain capabilities.
func parseDomainCaps(s string) (DomainCaps, error) {
	type enum struct {
		Name   string   `xml:"name,attr"`
		Values []string `xml:"value"`
	}
	type feature struct {
		Supported string `xml:"supported,attr"`
		Enums     []enum `xml:"enum"`
	}
	var c struct {
		TPM     feature `xml:"devices>tpm"`
		Hostdev feature `xml:"devices>hostdev"`
	}
	if err := xml.Unmarshal([]byte(s), &c); err != nil {
		return DomainCaps{}, fmt.Errorf("parsing domain capabilities: %w", err)
	}
	vals := func(f feature, name string) []string {
		for _, e := range f.Enums {
			if e.Name == name {
				return e.Values
			}
		}
		return nil
	}
	return DomainCaps{
		TPM:         c.TPM.Supported == "yes",
		TPMModels:   vals(c.TPM, "model"),
		TPMBackends: vals(c.TPM, "backendModel"),
		TPMVersions: vals(c.TPM, "backendVersion"),
		PCIHostdev:  c.Hostdev.Supported == "yes" && slices.Contains(vals(c.Hostdev, "subsysType"), "pci"),
		PCIBackends: vals(c.Hostdev, "pciBackend"),
	}, nil
}

// nodeDevice is the part of a PCI node-device description the pool checks use.
type nodeDevice struct {
	Driver string
	Class  string
	// Group is the IOMMU group number, -1 when the device has none.
	Group int
	// Peers are the group's members, this device included, as lower-case
	// PCI addresses.
	Peers []string
}

func parseNodeDevice(s string) (nodeDevice, error) {
	var d struct {
		Driver string `xml:"driver>name"`
		Cap    struct {
			Type  string `xml:"type,attr"`
			Class string `xml:"class"`
			Group *struct {
				Number int `xml:"number,attr"`
				Addrs  []struct {
					Domain   string `xml:"domain,attr"`
					Bus      string `xml:"bus,attr"`
					Slot     string `xml:"slot,attr"`
					Function string `xml:"function,attr"`
				} `xml:"address"`
			} `xml:"iommuGroup"`
		} `xml:"capability"`
	}
	if err := xml.Unmarshal([]byte(s), &d); err != nil {
		return nodeDevice{}, fmt.Errorf("parsing node device: %w", err)
	}
	if d.Cap.Type != "pci" {
		return nodeDevice{}, fmt.Errorf("node device is not a PCI device (capability %q)", d.Cap.Type)
	}
	out := nodeDevice{Driver: d.Driver, Class: strings.ToLower(strings.TrimSpace(d.Cap.Class)), Group: -1}
	if g := d.Cap.Group; g != nil {
		out.Group = g.Number
		for _, a := range g.Addrs {
			out.Peers = append(out.Peers, pciAddress(a.Domain, a.Bus, a.Slot, a.Function))
		}
	}
	return out, nil
}

// pciAddress joins libvirt's 0x-prefixed address attributes into the
// lower-case 0000:03:00.0 form.
func pciAddress(domain, bus, slot, function string) string {
	hex := func(v string) string { return strings.TrimPrefix(strings.ToLower(v), "0x") }
	return fmt.Sprintf("%04s:%02s:%02s.%s", hex(domain), hex(bus), hex(slot), hex(function))
}

// parseHostdevAddresses returns the host PCI addresses a domain passes
// through. The guest-side address child and hostdevs of other kinds are not
// host addresses and are ignored.
func parseHostdevAddresses(s string) ([]string, error) {
	var d struct {
		Hostdevs []struct {
			Mode   string `xml:"mode,attr"`
			Type   string `xml:"type,attr"`
			Source struct {
				Address pciAddressDoc `xml:"address"`
			} `xml:"source"`
		} `xml:"devices>hostdev"`
	}
	if err := xml.Unmarshal([]byte(s), &d); err != nil {
		return nil, fmt.Errorf("parsing domain XML: %w", err)
	}
	var out []string
	for _, h := range d.Hostdevs {
		a := h.Source.Address
		if h.Mode != "subsystem" || h.Type != "pci" || a.Bus == "" {
			continue
		}
		out = append(out, pciAddress(a.Domain, a.Bus, a.Slot, a.Function))
	}
	return out, nil
}

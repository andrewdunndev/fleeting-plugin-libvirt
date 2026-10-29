package libvirtplugin

import (
	"encoding/xml"
	"fmt"
)

type domainSpec struct {
	Name          string
	Title         string
	Prefix        string
	Partition     string
	Network       string
	DiskPath      string
	VCPU          int
	MemoryMiB     int
	CredentialKey string // credential name; empty seeds nothing
	AuthorizedKey string
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
	Type   string     `xml:"type"`
	SMBIOS *smbiosDoc `xml:"smbios,omitempty"`
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
	Model typeAttr `xml:"model"`
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

const guestAgentChannel = "org.qemu.guest_agent.0"

func domainXML(s domainSpec) (string, error) {
	d := domainDoc{
		Type:       "kvm",
		Name:       s.Name,
		Title:      s.Title,
		Metadata:   metadataDoc{Instance: instanceMeta{Prefix: s.Prefix}},
		Memory:     memoryDoc{Unit: "MiB", Value: s.MemoryMiB},
		VCPU:       s.VCPU,
		OS:         osDoc{Type: "hvm"},
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
	d.Devices.Channel.Type = "unix"
	d.Devices.Channel.Target.Type, d.Devices.Channel.Target.Name = "virtio", guestAgentChannel
	d.Devices.Serial.Type = "pty"
	d.Devices.Console.Type = "pty"
	d.Devices.Console.Target.Type = "serial"
	d.Devices.RNG.Model = "virtio"
	d.Devices.RNG.Backend.Model, d.Devices.RNG.Backend.Path = "random", "/dev/urandom"

	b, err := xml.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("building domain XML: %w", err)
	}
	return string(b), nil
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
	Backing struct {
		Path   string   `xml:"path"`
		Format typeAttr `xml:"format"`
	} `xml:"backingStore"`
}

// volumeXML describes a qcow2 overlay on base. The base format defaults to
// qcow2 when the pool does not report one.
func volumeXML(name string, base Base, capacity uint64) (string, error) {
	v := volumeDoc{Name: name}
	v.Capacity.Unit, v.Capacity.Value = "bytes", capacity
	v.Target.Format.Type = "qcow2"
	v.Backing.Path = base.Path
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

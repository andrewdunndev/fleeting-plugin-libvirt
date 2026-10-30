package libvirtplugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

const (
	defaultURI       = "qemu:///system"
	defaultNetwork   = "default"
	credentialPrefix = "ssh.authorized_keys."
	defaultVCPU      = 2
	defaultMemoryMiB = 4096
	unboundedSize    = math.MaxInt32
	firmwareBIOS     = "bios"
	firmwareEFI      = "efi"
	// minEFILibvirt is the first libvirt with the nvram format attribute, in
	// the major*1000000 + minor*1000 + release form libvirt reports.
	minEFILibvirt = 9002000
)

var prefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*-$`)

// Config is the plugin_config table. Every key is a json tag so that
// fleeting's json.Unmarshal into the InstanceGroup reaches it.
type Config struct {
	URI               string `json:"uri"`
	PoolName          string `json:"pool_name"`
	BaseVolumeName    string `json:"base_volume_name"`
	VCPUCount         int    `json:"vcpu_count"`
	MemoryMiB         int    `json:"memory_mib"`
	DiskSizeGiB       int    `json:"disk_size_gib"`
	NamePrefix        string `json:"name_prefix"`
	Title             string `json:"title"`
	NetworkName       string `json:"network_name"`
	ResourcePartition string `json:"resource_partition"`
	LockFile          string `json:"lock_file"`
	AdmitCommand      string `json:"admit_command"`
	MaxSize           int    `json:"max_size"`
	Firmware          string `json:"firmware"`
	SecureBoot        bool   `json:"secure_boot"`
	NVRAMTemplate     string `json:"nvram_template"`
	TPM               bool   `json:"tpm"`
}

// UnmarshalJSON decodes strictly but defers the error to Init, so a bad key
// is reported by the call that owns configuration.
func (g *InstanceGroup) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		g.cfgErr = fmt.Errorf("plugin_config: %w; remove the key or check its spelling", err)
		return nil
	}
	g.cfgErr = nil
	g.Config = c
	return nil
}

// normalize applies defaults and validates, returning the first problem with
// the key to change.
func (c *Config) normalize() error {
	if c.URI == "" {
		c.URI = defaultURI
	}
	if c.NetworkName == "" {
		c.NetworkName = defaultNetwork
	}
	if c.Firmware == "" {
		c.Firmware = firmwareBIOS
	}
	if c.VCPUCount == 0 {
		c.VCPUCount = defaultVCPU
	}
	if c.MemoryMiB == 0 {
		c.MemoryMiB = defaultMemoryMiB
	}

	switch {
	case c.PoolName == "":
		return errors.New("plugin_config.pool_name is required: name the libvirt storage pool holding the base volume")
	case c.BaseVolumeName == "":
		return errors.New("plugin_config.base_volume_name is required: name the volume in pool_name that instances clone")
	case c.NamePrefix == "":
		return errors.New("plugin_config.name_prefix is required: it scopes every domain and volume this plugin may touch")
	case !prefixPattern.MatchString(c.NamePrefix):
		return fmt.Errorf("plugin_config.name_prefix %q must start with a letter or digit, use only letters, digits, '.', '_' and '-', and end in '-'", c.NamePrefix)
	case c.MaxSize < 0:
		return errors.New("plugin_config.max_size must not be negative; leave it unset for no limit")
	case c.VCPUCount < 0 || c.MemoryMiB < 0 || c.DiskSizeGiB < 0:
		return errors.New("plugin_config vcpu_count, memory_mib and disk_size_gib must not be negative")
	case c.Firmware != firmwareBIOS && c.Firmware != firmwareEFI:
		return fmt.Errorf("plugin_config.firmware %q must be \"bios\" or \"efi\"", c.Firmware)
	case c.SecureBoot && c.Firmware != firmwareEFI:
		return errors.New("plugin_config.secure_boot requires firmware = \"efi\"")
	case c.Firmware == firmwareEFI && c.NVRAMTemplate == "":
		return errors.New("plugin_config.nvram_template is required with firmware = \"efi\": name the volume in pool_name that each instance's NVRAM is cloned from")
	case c.NVRAMTemplate != "" && c.Firmware != firmwareEFI:
		return errors.New("plugin_config.nvram_template requires firmware = \"efi\"")
	case c.AdmitCommand != "" && c.LockFile == "":
		return errors.New("plugin_config.admit_command requires lock_file: the lock spans the command and the VM start")
	case strings.ContainsAny(c.Title, "\r\n"):
		return errors.New("plugin_config.title must be a single line")
	case c.ResourcePartition != "" && !strings.HasPrefix(c.ResourcePartition, "/"):
		return fmt.Errorf("plugin_config.resource_partition %q must be an absolute cgroup path such as /machine/ci", c.ResourcePartition)
	}
	return nil
}

// prefixTag is the prefix's share of an instance name, which is how volumes,
// having no metadata, are matched to a prefix even when one nests in another.
func prefixTag(prefix string) string {
	sum := sha256.Sum256([]byte(prefix))
	return hex.EncodeToString(sum[:3])
}

// ownsName reports whether a domain or volume name was minted for this prefix.
func (c *Config) ownsName(name string) bool {
	return strings.HasPrefix(name, c.NamePrefix+prefixTag(c.NamePrefix)+"-")
}

const nvramSuffix = ".nvram"

// nvramName is the pool volume holding a domain's NVRAM.
func nvramName(domain string) string { return domain + nvramSuffix }

// domainOfVolume is the domain a pool volume belongs to: the volume's own name
// for a disk, the name minus its suffix for NVRAM.
func domainOfVolume(vol string) string {
	d, _ := strings.CutSuffix(vol, nvramSuffix)
	return d
}

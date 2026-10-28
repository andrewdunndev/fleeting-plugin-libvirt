package libvirtplugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	defaultURI            = "qemu:///system"
	defaultNetwork        = "default"
	defaultCredentialName = "ssh.authorized_keys.root"
	defaultVCPU           = 2
	defaultMemoryMiB      = 4096
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
	Owner             string `json:"owner"`
	SSHCredentialName string `json:"ssh_credential_name"`
	MaxSize           int    `json:"max_size"`
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
	if c.SSHCredentialName == "" {
		c.SSHCredentialName = defaultCredentialName
	}
	if c.VCPUCount == 0 {
		c.VCPUCount = defaultVCPU
	}
	if c.MemoryMiB == 0 {
		c.MemoryMiB = defaultMemoryMiB
	}
	if c.Owner == "" {
		c.Owner = c.NamePrefix
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
	case c.MaxSize <= 0:
		return errors.New("plugin_config.max_size must be greater than 0: fleeting caps max_instances by it")
	case c.VCPUCount < 0 || c.MemoryMiB < 0 || c.DiskSizeGiB < 0:
		return errors.New("plugin_config vcpu_count, memory_mib and disk_size_gib must not be negative")
	case c.AdmitCommand != "" && c.LockFile == "":
		return errors.New("plugin_config.admit_command requires lock_file: the lock spans the command and the VM start")
	case strings.ContainsAny(c.Title, "\r\n"):
		return errors.New("plugin_config.title must be a single line")
	case c.ResourcePartition != "" && !strings.HasPrefix(c.ResourcePartition, "/"):
		return fmt.Errorf("plugin_config.resource_partition %q must be an absolute cgroup path such as /machine/ci", c.ResourcePartition)
	case strings.ContainsAny(c.SSHCredentialName, "= \t\r\n"):
		return fmt.Errorf("plugin_config.ssh_credential_name %q must not contain '=' or whitespace", c.SSHCredentialName)
	}
	return nil
}

// ownerTag is the owner's share of an instance name, which is how volumes,
// having no metadata, are matched to an owner.
func ownerTag(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:3])
}

// ownsName reports whether a domain or volume name was minted for this owner.
func (c *Config) ownsName(name string) bool {
	return strings.HasPrefix(name, c.NamePrefix+ownerTag(c.Owner)+"-")
}

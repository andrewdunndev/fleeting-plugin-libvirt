package libvirtplugin

// InstanceGroup implements fleeting's provider.InstanceGroup over libvirt.
type InstanceGroup struct {
	Config

	cfgErr error
}

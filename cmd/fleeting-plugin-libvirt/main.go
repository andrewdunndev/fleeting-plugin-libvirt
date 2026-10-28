package main

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	"gitlab.com/dunn.dev/fleeting-plugin-libvirt/internal/libvirtplugin"
)

func main() {
	plugin.Main(&libvirtplugin.InstanceGroup{}, plugin.VersionInfo{Name: "fleeting-plugin-libvirt"})
}

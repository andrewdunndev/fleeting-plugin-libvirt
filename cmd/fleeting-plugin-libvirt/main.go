package main

import (
	"os/signal"
	"syscall"

	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	"gitlab.com/dunn.dev/fleeting-plugin-libvirt/internal/libvirtplugin"
)

// SIGQUIT is ignored so a terminal Ctrl-\ or a control-group SIGQUIT cannot
// kill the plugin while instances run jobs; forgehand's drain relies on the
// signal reaching only the daemon.
func init() { signal.Ignore(syscall.SIGQUIT) }

func main() {
	plugin.Main(&libvirtplugin.InstanceGroup{}, plugin.VersionInfo{Name: "fleeting-plugin-libvirt"})
}

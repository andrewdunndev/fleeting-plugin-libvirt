package main

import (
	"os/signal"
	"syscall"

	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	"gitlab.com/dunn.dev/fleeting-plugin-libvirt/internal/libvirtplugin"
)

// ignoreQuit keeps a terminal Ctrl-\ or a control-group SIGQUIT, which
// forgehand's drain relies on reaching only the daemon, from killing the
// plugin while instances are still running jobs.
func ignoreQuit() { signal.Ignore(syscall.SIGQUIT) }

func main() {
	ignoreQuit()
	plugin.Main(&libvirtplugin.InstanceGroup{}, plugin.VersionInfo{Name: "fleeting-plugin-libvirt"})
}

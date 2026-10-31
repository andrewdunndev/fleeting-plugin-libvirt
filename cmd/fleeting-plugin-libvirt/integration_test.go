package main

import (
	"os"
	"testing"
	"time"

	"gitlab.com/gitlab-org/fleeting/fleeting/integration"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// TestProvisioning runs fleeting's provider integration harness against a real
// libvirt host. It skips unless FLEETING_LIBVIRT_BASE_VOLUME is set.
func TestProvisioning(t *testing.T) {
	baseVolume := os.Getenv("FLEETING_LIBVIRT_BASE_VOLUME")
	if baseVolume == "" {
		t.Skip("FLEETING_LIBVIRT_BASE_VOLUME is not set")
	}

	pluginConfig := map[string]any{
		"base_volume_name": baseVolume,
		"pool_name":        envOr("FLEETING_LIBVIRT_POOL", "images"),
		"name_prefix":      envOr("FLEETING_LIBVIRT_NAME_PREFIX", "itest-"),
		"network_name":     envOr("FLEETING_LIBVIRT_NETWORK", "default"),
	}
	if uri := os.Getenv("FLEETING_LIBVIRT_URI"); uri != "" {
		pluginConfig["uri"] = uri
	}

	pluginPath := integration.BuildPluginBinary(t, ".", "fleeting-plugin-libvirt")

	integration.TestProvisioning(t, pluginPath, integration.Config{
		PluginConfig: pluginConfig,
		ConnectorConfig: provider.ConnectorConfig{
			Username: envOr("FLEETING_LIBVIRT_SSH_USERNAME", "root"),
			Timeout:  10 * time.Minute,
		},
		MaxInstances: 3,
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

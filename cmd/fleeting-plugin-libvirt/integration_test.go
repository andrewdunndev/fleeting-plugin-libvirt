package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/dunn.dev/fleeting-plugin-libvirt/internal/libvirtplugin"
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

	t.Cleanup(func() {
		if t.Failed() {
			removeLeftovers(t, pluginConfig)
		}
	})

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

// removeLeftovers drives the plugin itself to delete what a failed run left
// under the name prefix: Update lists the instances and sweeps orphan volumes,
// Decrease removes each instance with its volumes.
func removeLeftovers(t *testing.T, pluginConfig map[string]any) {
	t.Helper()
	ctx := context.Background()

	raw, err := json.Marshal(pluginConfig)
	if err != nil {
		t.Errorf("cleanup: encoding plugin config: %v", err)
		return
	}
	g := &libvirtplugin.InstanceGroup{}
	if err := json.Unmarshal(raw, g); err != nil {
		t.Errorf("cleanup: decoding plugin config: %v", err)
		return
	}
	if _, err := g.Init(ctx, hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Errorf("cleanup: init: %v", err)
		return
	}
	defer func() {
		if err := g.Shutdown(ctx); err != nil {
			t.Errorf("cleanup: shutdown: %v", err)
		}
	}()

	var names []string
	if err := g.Update(ctx, func(instance string, _ provider.State) {
		names = append(names, instance)
	}); err != nil {
		t.Errorf("cleanup: listing leftovers: %v", err)
		return
	}
	removed, err := g.Decrease(ctx, names)
	if err != nil {
		t.Errorf("cleanup: removing %v: %v", names, err)
	}
	t.Logf("cleanup: removed leftover instances %v", removed)
}

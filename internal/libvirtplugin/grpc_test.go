package libvirtplugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-hclog"
	fleeting "gitlab.com/gitlab-org/fleeting/fleeting"
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

const helperEnv = "FLEETING_LIBVIRT_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		plugin.Main(&InstanceGroup{connect: func(string) (Client, error) { return newFake(), nil }},
			plugin.VersionInfo{Name: "helper"})
		return
	}
	os.Exit(m.Run())
}

func TestPartialIncreaseAndDecreaseSurviveGRPC(t *testing.T) {
	t.Setenv(helperEnv, "1")
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	cfg := baseConfig()
	cfg.LockFile = filepath.Join(dir, "lock")
	cfg.AdmitCommand = `n=$(cat ` + counter + ` 2>/dev/null || echo 0); [ "$n" -lt 2 ] && echo $((n+1)) > ` + counter
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := fleeting.RunPlugin(os.Args[0], raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Kill)
	ig := r.InstanceGroup()
	ctx := context.Background()
	if _, err := ig.Init(ctx, hclog.NewNullLogger(), provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{Username: "root"},
	}); err != nil {
		t.Fatal(err)
	}

	n, err := ig.Increase(ctx, 3)
	if n != 2 || err != nil {
		t.Fatalf("increase over gRPC: n=%d err=%v, want 2 and nil", n, err)
	}
	var names []string
	if err := ig.Update(ctx, func(name string, _ provider.State) { names = append(names, name) }); err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("update reported %v", names)
	}
	done, err := ig.Decrease(ctx, append(names, "other-deadbeef"))
	if len(done) != 2 || err != nil {
		t.Fatalf("decrease over gRPC: done=%v err=%v, want 2 and nil", done, err)
	}
}

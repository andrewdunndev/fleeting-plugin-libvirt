package libvirtplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
)

func baseConfig() Config {
	return Config{
		PoolName: "pool", BaseVolumeName: "golden", NamePrefix: "job-", MaxSize: 4,
		Title: "ci", NetworkName: "runner", ResourcePartition: "/machine/ci",
	}
}

func newGroup(t *testing.T, c Config, static bool) (*InstanceGroup, *fakeClient) {
	t.Helper()
	f := newFake()
	g := &InstanceGroup{Config: c, connect: func(string) (Client, error) { return f, nil }}
	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{Username: "root", UseStaticCredentials: static},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return g, f
}

func fastTimers(t *testing.T) {
	t.Helper()
	cw, pi := connectWait, pollInterval
	connectWait, pollInterval = 300*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { connectWait, pollInterval = cw, pi })
}

func states(t *testing.T, g *InstanceGroup) map[string]provider.State {
	t.Helper()
	out := map[string]provider.State{}
	if err := g.Update(context.Background(), func(i string, s provider.State) { out[i] = s }); err != nil {
		t.Fatal(err)
	}
	return out
}

// ledgerGroup builds a group whose admit_command admits while fewer than
// slots files sit in a directory the fake fills as domains start.
func ledgerGroup(t *testing.T, slots int) (*InstanceGroup, *fakeClient, string) {
	t.Helper()
	dir := t.TempDir()
	ledger := filepath.Join(dir, "ledger")
	if err := os.Mkdir(ledger, 0o755); err != nil {
		t.Fatal(err)
	}
	c := baseConfig()
	c.LockFile = filepath.Join(dir, "lock")
	c.AdmitCommand = fmt.Sprintf(`[ "$(ls %s | wc -l)" -lt %d ]`, ledger, slots)
	g, f := newGroup(t, c, false)
	f.onDomainCreated = func(name string) {
		_ = os.WriteFile(filepath.Join(ledger, name), nil, 0o600)
	}
	return g, f, ledger
}

func TestConcurrentIncreasesAdmitOneAgainstOneSlotLedger(t *testing.T) {
	g, f, _ := ledgerGroup(t, 1)
	f.createDelay = 150 * time.Millisecond
	var wg sync.WaitGroup
	got := make([]int, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = g.Increase(context.Background(), 1)
		}()
	}
	wg.Wait()
	if got[0]+got[1] != 1 {
		t.Fatalf("admitted %v, want exactly one", got)
	}
	if len(f.domains) != 1 {
		t.Fatalf("%d domains started", len(f.domains))
	}
}

func TestAdmissionRefusalIsPartialIncreaseAndLeaksNothing(t *testing.T) {
	g, f, _ := ledgerGroup(t, 2)
	n, err := g.Increase(context.Background(), 3)
	if n != 2 || err == nil || !strings.Contains(err.Error(), "admission refused") {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.domains) != 2 || len(f.volumes) != 2 {
		t.Fatalf("domains=%d volumes=%d", len(f.domains), len(f.volumes))
	}
}

func TestLockFileAloneAllowedAndAdmitNeedsLock(t *testing.T) {
	c := baseConfig()
	c.LockFile = filepath.Join(t.TempDir(), "lock")
	g, f := newGroup(t, c, false)
	if n, err := g.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.domains) != 1 {
		t.Fatal("no domain")
	}
	c = baseConfig()
	c.AdmitCommand = "true"
	if err := c.normalize(); err == nil || !strings.Contains(err.Error(), "lock_file") {
		t.Fatalf("admit_command without lock_file accepted: %v", err)
	}
}

func TestAdmitCommandSeesInstanceEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env")
	c := baseConfig()
	c.LockFile = filepath.Join(dir, "lock")
	c.MemoryMiB = 2048
	c.AdmitCommand = fmt.Sprintf(`echo "$FLEETING_LIBVIRT_NAME $FLEETING_LIBVIRT_MEMORY_MIB" > %s`, out)
	g, f := newGroup(t, c, false)
	if n, _ := g.Increase(context.Background(), 1); n != 1 {
		t.Fatal("not created")
	}
	b, _ := os.ReadFile(out)
	var name string
	for n := range f.domains {
		name = n
	}
	if strings.TrimSpace(string(b)) != name+" 2048" {
		t.Fatalf("env = %q, want %q", b, name+" 2048")
	}
}

func TestUpdateIgnoresDomainOfAnotherOwnerWithOurPrefix(t *testing.T) {
	c := baseConfig()
	c.Owner = "me"
	g, f := newGroup(t, c, false)
	if n, _ := g.Increase(context.Background(), 1); n != 1 {
		t.Fatal("not created")
	}
	foreign, _ := domainXML(domainSpec{Name: "job-zzzz-1", Prefix: "job-", Owner: "someone-else", Network: "n", DiskPath: "/p", VCPU: 1, MemoryMiB: 1})
	if err := f.CreateDomain(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	got := states(t, g)
	if len(got) != 1 {
		t.Fatalf("Update saw %v", got)
	}
	if _, ok := got["job-zzzz-1"]; ok {
		t.Fatal("foreign domain visible")
	}
}

func TestOrphanVolumeReportedDeletingAndRemovedByDecrease(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	orphan := "job-" + ownerTag("job-") + "-deadbeef"
	f.volumes[orphan] = "<volume/>"
	f.volumes["job-other-volume"] = "<volume/>"
	f.volumes["unrelated"] = "<volume/>"
	got := states(t, g)
	if len(got) != 1 || got[orphan] != provider.StateDeleting {
		t.Fatalf("Update = %v", got)
	}
	done, err := g.Decrease(context.Background(), []string{orphan})
	if err != nil || len(done) != 1 {
		t.Fatalf("Decrease: %v %v", done, err)
	}
	if _, ok := f.volumes[orphan]; ok {
		t.Fatal("orphan survived")
	}
	if len(f.volumes) != 2 {
		t.Fatalf("foreign volumes touched: %v", f.volumes)
	}
}

func TestVolumeBeingCreatedIsNotAnOrphan(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	name, _ := g.newName()
	g.track(name)
	f.volumes[name] = "<volume/>"
	if got := states(t, g); len(got) != 0 {
		t.Fatalf("in-flight volume reported: %v", got)
	}
	g.untrack(name)
	if got := states(t, g); got[name] != provider.StateDeleting {
		t.Fatalf("settled orphan not reported: %v", got)
	}
}

func TestFailedStartLeavesNoVolume(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	f.createErr = errors.New("no VF free")
	n, err := g.Increase(context.Background(), 1)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "no VF free") {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.volumes) != 0 || len(f.domains) != 0 {
		t.Fatalf("leaked volumes=%v domains=%d", f.volumes, len(f.domains))
	}
}

func TestFailedVolumeCreateStartsNothing(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	f.volErr = errors.New("pool full")
	if n, err := g.Increase(context.Background(), 1); n != 0 || err == nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.domains) != 0 {
		t.Fatal("domain started without a disk")
	}
}

func TestIncreaseWritesIdentityAndSizing(t *testing.T) {
	c := baseConfig()
	c.Owner = "session-1"
	c.DiskSizeGiB = 40
	c.VCPUCount, c.MemoryMiB = 4, 8192
	g, f := newGroup(t, c, false)
	if n, _ := g.Increase(context.Background(), 1); n != 1 {
		t.Fatal("not created")
	}
	for name, d := range f.domains {
		for _, want := range []string{
			"<title>ci</title>", "<partition>/machine/ci</partition>", `<source network="runner">`,
			"<owner>session-1</owner>", "<vcpu>4</vcpu>", `unit="MiB">8192<`, "org.qemu.guest_agent.0",
			"<name>" + name + "</name>", `<source file="/pool/` + name + `.qcow2">`,
		} {
			if !strings.Contains(d.xml, want) {
				t.Errorf("domain XML lacks %s:\n%s", want, d.xml)
			}
		}
		if v := f.volumes[name]; !strings.Contains(v, `unit="bytes">42949672960<`) || !strings.Contains(v, "/pool/base.qcow2") {
			t.Errorf("volume XML: %s", v)
		}
	}
}

func TestVolumeKeepsBaseCapacityWhenDiskSizeSmaller(t *testing.T) {
	c := baseConfig()
	c.DiskSizeGiB = 1
	g, f := newGroup(t, c, false)
	_, _ = g.Increase(context.Background(), 1)
	for _, v := range f.volumes {
		if !strings.Contains(v, `unit="bytes">10737418240<`) {
			t.Fatalf("capacity shrank below base: %s", v)
		}
	}
}

func authorizedFromPEM(t *testing.T, pemKey []byte) string {
	t.Helper()
	s, err := ssh.ParsePrivateKey(pemKey)
	if err != nil {
		t.Fatalf("ConnectInfo key does not parse: %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
}

func TestGeneratedKeySeedsPublicHalfAndConnectInfoReturnsPrivate(t *testing.T) {
	fastTimers(t)
	g, f := newGroup(t, baseConfig(), false)
	if n, _ := g.Increase(context.Background(), 2); n != 2 {
		t.Fatal("not created")
	}
	var pubs []string
	for name, d := range f.domains {
		info, err := g.ConnectInfo(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		pub := authorizedFromPEM(t, info.Key)
		want := "io.systemd.credential:ssh.authorized_keys.root=" + pub
		if !strings.Contains(d.xml, want) {
			t.Fatalf("domain does not carry the public half of the returned key:\n%s", d.xml)
		}
		pubs = append(pubs, pub)
	}
	if pubs[0] != pubs[1] {
		t.Fatal("key generated per instance, want one per Init")
	}
}

func TestCustomCredentialName(t *testing.T) {
	c := baseConfig()
	c.SSHCredentialName = "ssh.authorized_keys.runner"
	g, f := newGroup(t, c, false)
	_, _ = g.Increase(context.Background(), 1)
	for _, d := range f.domains {
		if !strings.Contains(d.xml, "io.systemd.credential:ssh.authorized_keys.runner=ssh-ed25519 ") {
			t.Fatalf("credential name ignored: %s", d.xml)
		}
	}
}

func TestStaticCredentialsSeedNothingAndPassKeyThrough(t *testing.T) {
	fastTimers(t)
	f := newFake()
	g := &InstanceGroup{Config: baseConfig(), connect: func(string) (Client, error) { return f, nil }}
	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{ConnectorConfig: provider.ConnectorConfig{
		Username: "runner", Key: []byte("configured"), UseStaticCredentials: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = g.Increase(context.Background(), 1)
	for name, d := range f.domains {
		if strings.Contains(d.xml, "credential") || strings.Contains(d.xml, "sysinfo") {
			t.Fatalf("static mode seeded a credential: %s", d.xml)
		}
		info, err := g.ConnectInfo(context.Background(), name)
		if err != nil || string(info.Key) != "configured" || !info.UseStaticCredentials || info.Username != "runner" {
			t.Fatalf("info=%+v err=%v", info, err)
		}
	}
}

func TestDecreaseIsIdempotent(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var name string
	for n := range f.domains {
		name = n
	}
	for i := 0; i < 2; i++ {
		done, err := g.Decrease(context.Background(), []string{name})
		if err != nil || len(done) != 1 || done[0] != name {
			t.Fatalf("pass %d: %v %v", i, done, err)
		}
	}
	if len(f.domains) != 0 || len(f.volumes) != 0 {
		t.Fatal("instance not removed")
	}
}

func TestDecreaseRemovesVolumeWhenDomainAlreadyGone(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	for n := range f.domains {
		delete(f.domains, n)
		if done, err := g.Decrease(context.Background(), []string{n}); err != nil || len(done) != 1 {
			t.Fatalf("%v %v", done, err)
		}
	}
	if len(f.volumes) != 0 {
		t.Fatal("volume survived")
	}
}

func TestDecreaseRefusesNamesItDidNotMint(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	f.volumes["job-stranger"] = "<volume/>"
	done, err := g.Decrease(context.Background(), []string{"job-stranger", "unrelated"})
	if err == nil || len(done) != 0 {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(f.deletedVolumes) != 0 || len(f.destroyed) != 0 {
		t.Fatal("deleted something it did not own")
	}
}

func TestInitAndShutdownDeleteNothing(t *testing.T) {
	f := newFake()
	mine := "job-" + ownerTag("job-") + "-aaaaaaaa"
	f.volumes[mine] = "<volume/>"
	x, _ := domainXML(domainSpec{Name: mine, Prefix: "job-", Owner: "job-", Network: "n", DiskPath: "/p", VCPU: 1, MemoryMiB: 1})
	_ = f.CreateDomain(context.Background(), x)
	g := &InstanceGroup{Config: baseConfig(), connect: func(string) (Client, error) { return f, nil }}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatal(err)
	}
	if got := states(t, g); got[mine] != provider.StateRunning {
		t.Fatalf("existing instance not reported: %v", got)
	}
	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.destroyed) != 0 || len(f.deletedVolumes) != 0 || len(f.domains) != 1 || len(f.volumes) != 1 {
		t.Fatalf("destroyed=%v deleted=%v", f.destroyed, f.deletedVolumes)
	}
	if !f.closed {
		t.Fatal("connection left open")
	}
}

func TestInitFailsNamingMissingBaseVolume(t *testing.T) {
	f := newFake()
	f.baseErr = fmt.Errorf("volume: %w", ErrNotFound)
	g := &InstanceGroup{Config: baseConfig(), connect: func(string) (Client, error) { return f, nil }}
	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil || !strings.Contains(err.Error(), "golden") || !f.closed {
		t.Fatalf("err=%v closed=%v", err, f.closed)
	}
}

func TestInitReportsMaxSize(t *testing.T) {
	f := newFake()
	g := &InstanceGroup{Config: baseConfig(), connect: func(string) (Client, error) { return f, nil }}
	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil || info.MaxSize != 4 {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestStoppedDomainIsReportedDeleting(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	for _, d := range f.domains {
		d.info.Running = false
	}
	for _, s := range states(t, g) {
		if s != provider.StateDeleting {
			t.Fatalf("state %v", s)
		}
	}
}

func TestConnectInfoWaitsForGuestAgent(t *testing.T) {
	fastTimers(t)
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var name string
	for n := range f.domains {
		name = n
	}
	f.agentErr = errors.New("agent not connected")
	go func() {
		time.Sleep(40 * time.Millisecond)
		f.mu.Lock()
		f.agentErr = nil
		f.addrs = []string{"127.0.0.1", "fe80::1", "fd00::5", "10.9.8.7"}
		f.mu.Unlock()
	}()
	info, err := g.ConnectInfo(context.Background(), name)
	if err != nil || info.InternalAddr != "10.9.8.7" || info.ExternalAddr != "10.9.8.7" || info.ID != name {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestConnectInfoIsBoundedAndNamesTheFix(t *testing.T) {
	fastTimers(t)
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var name string
	for n := range f.domains {
		name = n
	}
	f.agentErr = errors.New("agent not connected")
	start := time.Now()
	_, err := g.ConnectInfo(context.Background(), name)
	if err == nil || !strings.Contains(err.Error(), "qemu-guest-agent") || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestConnectInfoHonoursContext(t *testing.T) {
	cw := connectWait
	connectWait = time.Minute
	t.Cleanup(func() { connectWait = cw })
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var name string
	for n := range f.domains {
		name = n
	}
	f.agentErr = errors.New("agent not connected")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := g.ConnectInfo(ctx, name); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestConnectInfoFailsFastForMissingDomain(t *testing.T) {
	fastTimers(t)
	cw := connectWait
	connectWait = time.Minute
	t.Cleanup(func() { connectWait = cw })
	g, _ := newGroup(t, baseConfig(), false)
	start := time.Now()
	_, err := g.ConnectInfo(context.Background(), "job-gone")
	if !errors.Is(err, ErrNotFound) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestHeartbeatNeedsRunningDomainAndOpenPort(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var name string
	for n := range f.domains {
		name = n
	}
	var dialed string
	orig := dialContext
	t.Cleanup(func() { dialContext = orig })
	dialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	}
	if err := g.Heartbeat(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if dialed != "10.1.2.3:22" {
		t.Fatalf("dialed %q", dialed)
	}

	dialContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	if err := g.Heartbeat(context.Background(), name); !errors.Is(err, provider.ErrInstanceUnhealthy) {
		t.Fatalf("closed port healthy: %v", err)
	}

	dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dialed a stopped domain")
		return nil, nil
	}
	f.domains[name].info.Running = false
	if err := g.Heartbeat(context.Background(), name); !errors.Is(err, provider.ErrInstanceUnhealthy) {
		t.Fatalf("stopped domain healthy: %v", err)
	}
	if err := g.Heartbeat(context.Background(), "job-gone"); !errors.Is(err, provider.ErrInstanceUnhealthy) {
		t.Fatalf("missing domain healthy: %v", err)
	}
}

func TestSuspendResumeUnsupported(t *testing.T) {
	g, _ := newGroup(t, baseConfig(), false)
	if _, err := g.Suspend(context.Background(), nil); !errors.Is(err, provider.ErrSuspendResumeNotSupported) {
		t.Fatal(err)
	}
	if _, err := g.Resume(context.Background(), nil); !errors.Is(err, provider.ErrSuspendResumeNotSupported) {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"pool", func(c *Config) { c.PoolName = "" }, "pool_name"},
		{"base", func(c *Config) { c.BaseVolumeName = "" }, "base_volume_name"},
		{"prefix missing", func(c *Config) { c.NamePrefix = "" }, "name_prefix"},
		{"prefix no dash", func(c *Config) { c.NamePrefix = "job" }, "end in '-'"},
		{"prefix slash", func(c *Config) { c.NamePrefix = "a/b-" }, "name_prefix"},
		{"max_size", func(c *Config) { c.MaxSize = 0 }, "max_size"},
		{"negative", func(c *Config) { c.MemoryMiB = -1 }, "negative"},
		{"admit without lock", func(c *Config) { c.AdmitCommand = "true" }, "lock_file"},
		{"multiline title", func(c *Config) { c.Title = "a\nb" }, "title"},
		{"relative partition", func(c *Config) { c.ResourcePartition = "machine/ci" }, "resource_partition"},
		{"credential with =", func(c *Config) { c.SSHCredentialName = "a=b" }, "ssh_credential_name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			tc.mutate(&c)
			err := c.normalize()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{PoolName: "p", BaseVolumeName: "b", NamePrefix: "job-", MaxSize: 1}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.URI != "qemu:///system" || c.Owner != "job-" || c.NetworkName != "default" ||
		c.SSHCredentialName != "ssh.authorized_keys.root" || c.VCPUCount != 2 || c.MemoryMiB != 4096 {
		t.Fatalf("%+v", c)
	}
}

func TestUnknownKeyFailsInitNamingIt(t *testing.T) {
	raw := `{"pool_name":"p","base_volume_name":"b","name_prefix":"job-","max_size":1,"vpcu_count":2}`
	g := &InstanceGroup{connect: func(string) (Client, error) { return newFake(), nil }}
	if err := json.Unmarshal([]byte(raw), g); err != nil {
		t.Fatalf("decode error must wait for Init: %v", err)
	}
	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil || !strings.Contains(err.Error(), "vpcu_count") {
		t.Fatalf("err=%v", err)
	}
	good := `{"pool_name":"p","base_volume_name":"b","name_prefix":"job-","max_size":1,"vcpu_count":2}`
	g = &InstanceGroup{connect: func(string) (Client, error) { return newFake(), nil }}
	if err := json.Unmarshal([]byte(good), g); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatal(err)
	}
}

func TestPickAddress(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{[]string{"fd00::5", "10.0.0.2"}, "10.0.0.2"},
		{[]string{"127.0.0.1", "::1", "169.254.1.1", "fe80::1"}, ""},
		{[]string{"fd00::5"}, "fd00::5"},
		{[]string{"junk", "0.0.0.0", "192.168.1.4"}, "192.168.1.4"},
	}
	for _, tc := range tests {
		if got := pickAddress(tc.in); got != tc.want {
			t.Errorf("pickAddress(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLockWaitHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := acquireLock(ctx, path); err == nil {
		t.Fatal("second holder got the lock")
	}
	unlock()
	u2, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	u2()
}

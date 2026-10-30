package libvirtplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	cw, pi, dc := connectWait, pollInterval, dialContext
	connectWait, pollInterval = 300*time.Millisecond, 5*time.Millisecond
	dialContext = func(context.Context, string, string) (net.Conn, error) {
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	}
	t.Cleanup(func() { connectWait, pollInterval, dialContext = cw, pi, dc })
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
	if n != 2 || err != nil {
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

func TestUpdateDeletesOrphanVolumeAndNeverReportsIt(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	orphan := "job-" + prefixTag("job-") + "-deadbeef"
	f.volumes[orphan] = "<volume/>"
	f.volumes["job-other-volume"] = "<volume/>"
	f.volumes["unrelated"] = "<volume/>"
	if got := states(t, g); len(got) != 0 {
		t.Fatalf("Update = %v", got)
	}
	if _, ok := f.volumes[orphan]; ok {
		t.Fatal("orphan survived")
	}
	if len(f.volumes) != 2 || len(f.deletedVolumes) != 1 {
		t.Fatalf("foreign volumes touched: %v %v", f.volumes, f.deletedVolumes)
	}
}

func TestUpdateKeepsVolumeOfLiveDomainAndInflightCreate(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	name, _ := g.newName()
	g.track(name)
	f.volumes[name] = "<volume/>"
	if got := states(t, g); len(got) != 1 {
		t.Fatalf("Update = %v", got)
	}
	if _, ok := f.volumes[name]; !ok || len(f.deletedVolumes) != 0 || len(f.volumes) != 2 {
		t.Fatalf("volumes %v deleted %v", f.volumes, f.deletedVolumes)
	}
	g.untrack(name)
	states(t, g)
	if _, ok := f.volumes[name]; ok {
		t.Fatal("settled orphan survived")
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
	c.DiskSizeGiB = 40
	c.VCPUCount, c.MemoryMiB = 4, 8192
	g, f := newGroup(t, c, false)
	if n, _ := g.Increase(context.Background(), 1); n != 1 {
		t.Fatal("not created")
	}
	for name, d := range f.domains {
		for _, want := range []string{
			"<title>ci</title>", "<partition>/machine/ci</partition>", `<source network="runner">`,
			"<prefix>job-</prefix>", "<vcpu>4</vcpu>", `unit="MiB">8192<`, "org.qemu.guest_agent.0",
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

func TestCredentialNameDerivesFromUsername(t *testing.T) {
	for _, tc := range []struct{ username, want string }{
		{"runner", "ssh.authorized_keys.runner"},
		{"", "ssh.authorized_keys.root"},
	} {
		f := newFake()
		g := &InstanceGroup{Config: baseConfig(), connect: func(string) (Client, error) { return f, nil }}
		_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{
			ConnectorConfig: provider.ConnectorConfig{Username: tc.username},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = g.Increase(context.Background(), 1)
		for _, d := range f.domains {
			if !strings.Contains(d.xml, "io.systemd.credential:"+tc.want+"=ssh-ed25519 ") {
				t.Fatalf("username %q: want credential %s in %s", tc.username, tc.want, d.xml)
			}
		}
	}
}

func TestInitRejectsUsernameThatCannotNameACredential(t *testing.T) {
	g := &InstanceGroup{Config: baseConfig(), connect: func(string) (Client, error) { return newFake(), nil }}
	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{Username: "a=b"},
	})
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("err=%v", err)
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

func TestDomainOwnershipIsMetadataAndVolumesAreNameTag(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	add := func(name, prefix string) {
		x, _ := domainXML(domainSpec{Name: name, Prefix: prefix, Network: "n", DiskPath: "/p", VCPU: 1, MemoryMiB: 1})
		if err := f.CreateDomain(context.Background(), x); err != nil {
			t.Fatal(err)
		}
	}
	custom := "job-custom"
	add(custom, g.NamePrefix)
	f.volumes[custom] = "<volume/>"
	minted := g.NamePrefix + prefixTag(g.NamePrefix) + "-deadbeef"
	add(minted, "job-a-")

	got := states(t, g)
	if len(got) != 1 || got[custom] != provider.StateRunning {
		t.Fatalf("Update reported %v", got)
	}
	done, err := g.Decrease(context.Background(), []string{minted})
	if err == nil || len(done) != 0 || f.domains[minted] == nil {
		t.Fatalf("removed a foreign-metadata domain: %v %v", done, err)
	}
	done, err = g.Decrease(context.Background(), []string{custom})
	if err != nil || len(done) != 1 || f.domains[custom] != nil {
		t.Fatalf("Decrease = %v, %v", done, err)
	}
	if _, ok := f.volumes[custom]; !ok {
		t.Fatal("deleted a volume under a name it did not mint")
	}
}

func TestInitAndShutdownDeleteNothing(t *testing.T) {
	f := newFake()
	mine := "job-" + prefixTag("job-") + "-aaaaaaaa"
	f.volumes[mine] = "<volume/>"
	x, _ := domainXML(domainSpec{Name: mine, Prefix: "job-", Network: "n", DiskPath: "/p", VCPU: 1, MemoryMiB: 1})
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

func TestUnsetMaxSizeIsUnbounded(t *testing.T) {
	c := baseConfig()
	c.MaxSize = 0
	g := &InstanceGroup{Config: c, connect: func(string) (Client, error) { return newFake(), nil }}
	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil || info.MaxSize != math.MaxInt32 {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestStoppedDomainIsRunningAndUnhealthy(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	for _, d := range f.domains {
		d.running = false
	}
	got := states(t, g)
	if len(got) != 1 {
		t.Fatalf("states %v", got)
	}
	for name, s := range got {
		if s != provider.StateRunning {
			t.Fatalf("state %v", s)
		}
		if err := g.Heartbeat(context.Background(), name); !errors.Is(err, provider.ErrInstanceUnhealthy) {
			t.Fatalf("stopped heartbeat = %v", err)
		}
	}
}

func TestNestedPrefixesNeverMatchEachOthersInstances(t *testing.T) {
	outer := baseConfig()
	inner := baseConfig()
	inner.NamePrefix = "job-a-"
	f := newFake()
	mk := func(c Config) *InstanceGroup {
		g := &InstanceGroup{Config: c, connect: func(string) (Client, error) { return f, nil }}
		if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
			t.Fatal(err)
		}
		return g
	}
	outerG, innerG := mk(outer), mk(inner)
	if n, _ := innerG.Increase(context.Background(), 1); n != 1 {
		t.Fatal("not created")
	}
	var theirs string
	for n := range f.domains {
		theirs = n
	}
	orphan := inner.NamePrefix + prefixTag(inner.NamePrefix) + "-deadbeef"
	f.volumes[orphan] = "<volume/>"
	if got := states(t, outerG); len(got) != 0 {
		t.Fatalf("outer Update saw %v", got)
	}
	if _, ok := f.volumes[orphan]; !ok || len(f.deletedVolumes) != 0 {
		t.Fatalf("outer reaped the inner prefix's volume: %v", f.deletedVolumes)
	}
	done, err := outerG.Decrease(context.Background(), []string{theirs, orphan})
	if err == nil || len(done) != 0 || f.domains[theirs] == nil || f.volumes[theirs] == "" {
		t.Fatalf("outer removed the inner prefix's instance: %v %v", done, err)
	}
}

func TestCreateFinishingDuringUpdateIsNotReaped(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	name, _ := g.newName()
	g.track(name)
	f.volumes[name] = "<volume/>"
	f.afterListVolumes = func() {
		xml, _ := domainXML(domainSpec{Name: name, Prefix: g.NamePrefix, Network: "n", DiskPath: "/p", VCPU: 1, MemoryMiB: 1})
		_ = f.CreateDomain(context.Background(), xml)
		g.untrack(name)
	}
	if got := states(t, g); got[name] != provider.StateRunning || len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if _, ok := f.volumes[name]; !ok || len(f.deletedVolumes) != 0 {
		t.Fatalf("finished create reaped: %v", f.deletedVolumes)
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

func TestConnectInfoWaitsForSSHPort(t *testing.T) {
	fastTimers(t)
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var name string
	for n := range f.domains {
		name = n
	}
	var mu sync.Mutex
	var tries int
	var dialed string
	dialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		tries++
		dialed = addr
		if tries < 4 {
			return nil, errors.New("refused")
		}
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	}
	info, err := g.ConnectInfo(context.Background(), name)
	mu.Lock()
	defer mu.Unlock()
	if err != nil || info.InternalAddr != "10.1.2.3" || tries != 4 || dialed != "10.1.2.3:22" {
		t.Fatalf("info=%+v err=%v tries=%d dialed=%q", info, err, tries, dialed)
	}
}

func TestConnectInfoFailsWhenSSHPortNeverOpens(t *testing.T) {
	fastTimers(t)
	g, f := newGroup(t, baseConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var name string
	for n := range f.domains {
		name = n
	}
	dialContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	start := time.Now()
	_, err := g.ConnectInfo(context.Background(), name)
	if err == nil || !strings.Contains(err.Error(), "ssh port") || time.Since(start) > 3*time.Second {
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

	g.settings.ProtocolPort = 2222
	if err := g.Heartbeat(context.Background(), name); err != nil || dialed != "10.1.2.3:2222" {
		t.Fatalf("err=%v dialed %q", err, dialed)
	}
	g.settings.ProtocolPort = 0

	dialContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	if err := g.Heartbeat(context.Background(), name); !errors.Is(err, provider.ErrInstanceUnhealthy) {
		t.Fatalf("closed port healthy: %v", err)
	}

	dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dialed a stopped domain")
		return nil, nil
	}
	f.domains[name].running = false
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
		{"max_size negative", func(c *Config) { c.MaxSize = -1 }, "max_size"},
		{"negative", func(c *Config) { c.MemoryMiB = -1 }, "negative"},
		{"admit without lock", func(c *Config) { c.AdmitCommand = "true" }, "lock_file"},
		{"multiline title", func(c *Config) { c.Title = "a\nb" }, "title"},
		{"relative partition", func(c *Config) { c.ResourcePartition = "machine/ci" }, "resource_partition"},
		{"unknown firmware", func(c *Config) { c.Firmware = "uefi" }, "firmware"},
		{"secure boot on bios", func(c *Config) { c.SecureBoot = true }, "secure_boot requires"},
		{"secure boot on explicit bios", func(c *Config) { c.Firmware, c.SecureBoot = "bios", true }, "secure_boot requires"},
		{"efi without template", func(c *Config) { c.Firmware = "efi" }, "nvram_template is required"},
		{"template on bios", func(c *Config) { c.NVRAMTemplate = "tmpl" }, "nvram_template requires"},
		{"secure boot without template", func(c *Config) { c.Firmware, c.SecureBoot = "efi", true }, "nvram_template is required"},
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
	if c.URI != "qemu:///system" || c.NetworkName != "default" ||
		c.VCPUCount != 2 || c.MemoryMiB != 4096 || c.Firmware != "bios" || c.SecureBoot {
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
	raw = `{"pool_name":"p","base_volume_name":"b","name_prefix":"job-","max_size":1,"ssh_credential_name":"k"}`
	g = &InstanceGroup{connect: func(string) (Client, error) { return newFake(), nil }}
	if err := json.Unmarshal([]byte(raw), g); err != nil {
		t.Fatal(err)
	}
	_, err = g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil || !strings.Contains(err.Error(), "ssh_credential_name") {
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
		{[]string{"224.0.0.1", "ff02::1", "255.255.255.255"}, ""},
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

type lockSwitchClient struct {
	*fakeClient
	g    *InstanceGroup
	held string
	once sync.Once
}

func (c *lockSwitchClient) CreateDomain(ctx context.Context, xml string) error {
	err := c.fakeClient.CreateDomain(ctx, xml)
	c.once.Do(func() { c.g.LockFile = c.held })
	return err
}

func TestLockTimeoutAfterProgressReturnsCount(t *testing.T) {
	old := lockTimeout
	lockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { lockTimeout = old })
	dir := t.TempDir()
	held := filepath.Join(dir, "held")
	unlock, err := acquireLock(context.Background(), held)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	c := baseConfig()
	c.LockFile = filepath.Join(dir, "free")
	f := newFake()
	cl := &lockSwitchClient{fakeClient: f, held: held}
	g := &InstanceGroup{Config: c, connect: func(string) (Client, error) { return cl, nil }}
	cl.g = g
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n, err := g.Increase(context.Background(), 3)
	if n != 1 || err != nil || time.Since(start) > 2*lockTimeout+lockTimeout/2 {
		t.Fatalf("Increase = %d, %v after %s", n, err, time.Since(start))
	}
	if len(f.domains) != 1 {
		t.Fatalf("domains = %d, want 1", len(f.domains))
	}
}

func TestLockTimeoutRefusesIncrease(t *testing.T) {
	old := lockTimeout
	lockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { lockTimeout = old })
	c := baseConfig()
	c.LockFile = filepath.Join(t.TempDir(), "lock")
	g, f := newGroup(t, c, false)
	unlock, err := acquireLock(context.Background(), c.LockFile)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n, err := g.Increase(context.Background(), 3)
	if n != 0 || !errors.Is(err, errLockTimedOut) || time.Since(start) > 2*lockTimeout+lockTimeout/2 {
		t.Fatalf("Increase = %d, %v after %s", n, err, time.Since(start))
	}
	if len(f.volumes) != 0 || len(f.domains) != 0 {
		t.Fatal("timed-out wait created resources")
	}
	unlock()
	if n, err := g.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("after release Increase = %d, %v", n, err)
	}
}

func TestHungAdmitCommandIsKilledAndReleasesLock(t *testing.T) {
	old := admitTimeout
	admitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { admitTimeout = old })
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-survived")
	c := baseConfig()
	c.LockFile = filepath.Join(dir, "lock")
	c.AdmitCommand = fmt.Sprintf("(sleep 1; touch %s) & sleep 30", marker)
	g, f := newGroup(t, c, false)
	start := time.Now()
	n, err := g.Increase(context.Background(), 1)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("Increase = %d, %v", n, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if len(f.volumes) != 0 || len(f.domains) != 0 {
		t.Fatal("hung admission created resources")
	}
	unlock, err := acquireLock(context.Background(), c.LockFile)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("admit_command child outlived its kill")
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("no /dev/fd")
	}
	return len(ents)
}

func TestLockReleaseAndCancelledWaitCloseTheirDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	before := openFDs(t)
	for range 5 {
		unlock, err := acquireLock(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	}
	if got := openFDs(t); got != before {
		t.Fatalf("release leaked descriptors: %d -> %d", before, got)
	}
	unlock, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	held := openFDs(t)
	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := acquireLock(ctx, path)
		cancel()
		if err == nil {
			t.Fatal("got a held lock")
		}
	}
	if got := openFDs(t); got != held {
		t.Fatalf("cancelled wait leaked descriptors: %d -> %d", held, got)
	}
	unlock()
}

func TestIncreaseWithCancelledContextCreatesNothing(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := g.Increase(ctx, 3)
	if n != 0 || err == nil {
		t.Fatalf("Increase = %d, %v", n, err)
	}
	if len(f.volumes) != 0 || len(f.domains) != 0 {
		t.Fatal("cancelled Increase created resources")
	}
}

func TestCancelledCreateStillRemovesItsVolume(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	ctx, cancel := context.WithCancel(context.Background())
	f.createErr = errors.New("start interrupted")
	f.onCreateDomain = cancel
	n, err := g.Increase(ctx, 1)
	if n != 0 || err == nil {
		t.Fatalf("Increase = %d, %v", n, err)
	}
	if len(f.volumes) != 0 {
		t.Fatalf("volume left behind: %v", f.volumes)
	}
}

func TestFirstAdmissionRefusalStopsIncrease(t *testing.T) {
	c := baseConfig()
	c.LockFile = filepath.Join(t.TempDir(), "lock")
	marker := filepath.Join(t.TempDir(), "calls")
	c.AdmitCommand = "echo x >> " + marker + "; false"
	g, _ := newGroup(t, c, false)
	n, err := g.Increase(context.Background(), 3)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "admission refused") {
		t.Fatalf("n=%d err=%v", n, err)
	}
	b, _ := os.ReadFile(marker)
	if strings.Count(string(b), "x") != 1 {
		t.Fatalf("admit ran %q, want once", b)
	}
}

func TestPartialDecreaseReturnsNilError(t *testing.T) {
	g, _ := newGroup(t, baseConfig(), false)
	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	_ = g.Update(context.Background(), func(n string, _ provider.State) { names = append(names, n) })
	done, err := g.Decrease(context.Background(), append(names, "foreign"))
	if len(done) != 1 || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	done, err = g.Decrease(context.Background(), []string{"foreign"})
	if len(done) != 0 || err == nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
}

func TestLockFileCanBeReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens read-only files for writing")
	}
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestLockOpensExistingFileWithoutCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var flags []int
	orig := openFile
	openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		flags = append(flags, flag)
		return orig(name, flag, perm)
	}
	t.Cleanup(func() { openFile = orig })
	unlock, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if len(flags) != 1 || flags[0]&os.O_CREATE != 0 {
		t.Fatalf("open flags %v, want one open without O_CREATE", flags)
	}
}

func TestLockCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerKeyIsRejectedAsUnknown(t *testing.T) {
	raw := `{"pool_name":"p","base_volume_name":"b","name_prefix":"job-","max_size":1,"owner":"x"}`
	g := &InstanceGroup{connect: func(string) (Client, error) { return newFake(), nil }}
	if err := json.Unmarshal([]byte(raw), g); err != nil {
		t.Fatal(err)
	}
	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("err=%v", err)
	}
}

func TestInitGroupIDIsPoolAndPrefix(t *testing.T) {
	f := newFake()
	g := &InstanceGroup{Config: baseConfig(), connect: func(string) (Client, error) { return f, nil }}
	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil || info.ID != "libvirt/pool/job-" {
		t.Fatalf("ID = %q, %v", info.ID, err)
	}
}

func initEFI(t *testing.T, version uint64, verr error) (*fakeClient, error) {
	t.Helper()
	f := newFake()
	f.libVersion, f.libVersionErr = version, verr
	c := baseConfig()
	c.Firmware, c.NVRAMTemplate = "efi", "tmpl"
	g := &InstanceGroup{Config: c, connect: func(string) (Client, error) { return f, nil }}
	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	return f, err
}

func TestInitEFIRequiresLibvirt92(t *testing.T) {
	for _, v := range []uint64{9001000, 8000000, 9001999} {
		f, err := initEFI(t, v, nil)
		if err == nil || !strings.Contains(err.Error(), "9.2.0") || !f.closed {
			t.Fatalf("version %d: err=%v closed=%v", v, err, f.closed)
		}
	}
	for _, v := range []uint64{9002000, 10010000} {
		if _, err := initEFI(t, v, nil); err != nil {
			t.Fatalf("version %d: %v", v, err)
		}
	}
	if f, err := initEFI(t, 0, errors.New("boom")); err == nil || !f.closed {
		t.Fatalf("version read failure not reported: err=%v closed=%v", err, f.closed)
	}
}

func TestInitBIOSNeverAsksLibvirtVersion(t *testing.T) {
	f := newFake()
	f.libVersion, f.libVersionErr = 1000000, errors.New("must not be read")
	g := &InstanceGroup{Config: baseConfig(), connect: func(string) (Client, error) { return f, nil }}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatal(err)
	}
}

func TestInitRejectsFirmwareRules(t *testing.T) {
	for name, c := range map[string]Config{
		"secure_boot on bios": {Firmware: "bios", SecureBoot: true},
		"unknown firmware":    {Firmware: "coreboot"},
	} {
		cfg := baseConfig()
		cfg.Firmware, cfg.SecureBoot = c.Firmware, c.SecureBoot
		g := &InstanceGroup{Config: cfg, connect: func(string) (Client, error) { return newFake(), nil }}
		_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
		if err == nil || !strings.Contains(err.Error(), "plugin_config.") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestCreateEmitsFirmwareXML(t *testing.T) {
	c := baseConfig()
	c.Firmware, c.SecureBoot, c.NVRAMTemplate = "efi", true, "tmpl"
	g, f := newGroup(t, c, false)
	if n, err := g.Increase(context.Background(), 1); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, d := range f.domains {
		for _, part := range []string{`<os firmware="efi">`, `name="secure-boot"`, `<smm state="on">`} {
			if !strings.Contains(d.xml, part) {
				t.Errorf("%s missing: %s", part, d.xml)
			}
		}
	}
}

func efiConfig() Config {
	c := baseConfig()
	c.Firmware, c.NVRAMTemplate = "efi", "tmpl"
	return c
}

func TestInitNVRAMTemplateRules(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeClient)
		want   string
	}{
		{"template volume absent", func(f *fakeClient) { f.templateErr = ErrNotFound }, "nvram template"},
		{"template format raw", func(f *fakeClient) { f.template.Format = "raw" }, `"tmpl" has format "raw"; plugin_config.nvram_template must be a qcow2 volume`},
		{"template format other", func(f *fakeClient) { f.template.Format = "vmdk" }, `has format "vmdk"; plugin_config.nvram_template must be a qcow2 volume`},
		{"template format unreported", func(f *fakeClient) { f.template.Format = "" }, `has format ""; plugin_config.nvram_template must be a qcow2 volume`},
		{"template format qcow2", func(f *fakeClient) { f.template.Format = "qcow2" }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			tc.mutate(f)
			g := &InstanceGroup{Config: efiConfig(), connect: func(string) (Client, error) { return f, nil }}
			_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
			if tc.want == "" {
				if err != nil || f.closed {
					t.Fatalf("err=%v closed=%v", err, f.closed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !f.closed {
				t.Fatalf("err=%v closed=%v", err, f.closed)
			}
		})
	}
}

func TestInitRejectsEFIWithoutTemplateAndTemplateWithoutEFI(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"efi alone":        func(c *Config) { c.NVRAMTemplate = "" },
		"secure boot":      func(c *Config) { c.NVRAMTemplate, c.SecureBoot = "", true },
		"template on bios": func(c *Config) { c.Firmware = "bios" },
	} {
		c := efiConfig()
		mutate(&c)
		g := &InstanceGroup{Config: c, connect: func(string) (Client, error) { return newFake(), nil }}
		_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
		if err == nil || !strings.Contains(err.Error(), "nvram_template") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestBIOSNeverTouchesNVRAM(t *testing.T) {
	g, f := newGroup(t, baseConfig(), false)
	if n, err := g.Increase(context.Background(), 1); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.clones) != 0 || len(f.volumes) != 1 {
		t.Fatalf("clones=%v volumes=%v", f.clones, f.volumes)
	}
	for _, d := range f.domains {
		if strings.Contains(d.xml, "nvram") {
			t.Fatalf("bios domain carries nvram: %s", d.xml)
		}
		_, _ = g.Decrease(context.Background(), []string{d.info.Name})
	}
	if len(f.deletedVolumes) != 1 {
		t.Fatalf("deleted %v", f.deletedVolumes)
	}
}

func TestIncreaseClonesTemplateIntoNVRAMBeforeDomain(t *testing.T) {
	f := newFake()
	g := &InstanceGroup{Config: efiConfig(), connect: func(string) (Client, error) { return f, nil }}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatal(err)
	}
	f.onCreateDomain = func() {
		if len(f.clones) != 1 {
			t.Errorf("domain defined before the NVRAM clone: %v", f.clones)
		}
	}
	if n, err := g.Increase(context.Background(), 1); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for name, d := range f.domains {
		nv := name + ".nvram"
		if _, ok := f.volumes[nv]; !ok || f.clones[0] != "tmpl>"+nv {
			t.Fatalf("volumes=%v clones=%v", f.volumes, f.clones)
		}
		if !g.ownsName(nv) || !strings.Contains(d.xml, `<nvram format="qcow2">/pool/`+nv+`</nvram>`) {
			t.Fatalf("ownership or xml: %s", d.xml)
		}
		if !strings.Contains(f.volumes[nv], `<format type="qcow2">`) {
			t.Fatalf("clone XML: %s", f.volumes[nv])
		}
	}
}

func TestDecreaseDeletesNVRAMWithDisk(t *testing.T) {
	g, f := newGroup(t, efiConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var name string
	for n := range f.domains {
		name = n
	}
	done, err := g.Decrease(context.Background(), []string{name})
	if err != nil || len(done) != 1 || len(f.volumes) != 0 {
		t.Fatalf("done=%v err=%v volumes=%v", done, err, f.volumes)
	}
	f.volumes[name+".nvram"] = "<volume/>"
	if done, err := g.Decrease(context.Background(), []string{name}); err != nil || len(done) != 1 || len(f.volumes) != 0 {
		t.Fatalf("gone domain: done=%v err=%v volumes=%v", done, err, f.volumes)
	}
}

func TestFailedCreateDeletesNVRAM(t *testing.T) {
	g, f := newGroup(t, efiConfig(), false)
	f.createErr = errors.New("no firmware")
	if n, err := g.Increase(context.Background(), 1); n != 0 || err == nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.volumes) != 0 || len(f.clones) != 1 {
		t.Fatalf("leaked volumes=%v clones=%v", f.volumes, f.clones)
	}
}

func TestFailedNVRAMCloneDeletesDiskAndStartsNothing(t *testing.T) {
	g, f := newGroup(t, efiConfig(), false)
	f.cloneErr = errors.New("pool full")
	if n, err := g.Increase(context.Background(), 1); n != 0 || err == nil || !strings.Contains(err.Error(), "pool full") {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.volumes) != 0 || len(f.domains) != 0 {
		t.Fatalf("leaked volumes=%v domains=%d", f.volumes, len(f.domains))
	}
}

func TestUpdateKeepsLiveNVRAMAndSweepsOrphanNVRAM(t *testing.T) {
	g, f := newGroup(t, efiConfig(), false)
	_, _ = g.Increase(context.Background(), 1)
	var live string
	for n := range f.domains {
		live = n
	}
	gone := "job-" + prefixTag("job-") + "-deadbeef"
	f.volumes[gone+".nvram"] = "<volume/>"
	inflight := "job-" + prefixTag("job-") + "-cafe0000"
	g.track(inflight)
	f.volumes[inflight+".nvram"] = "<volume/>"
	f.volumes["job-other.nvram"] = "<volume/>"
	if got := states(t, g); len(got) != 1 {
		t.Fatalf("Update = %v", got)
	}
	if _, ok := f.volumes[live+".nvram"]; !ok {
		t.Fatal("live instance's NVRAM deleted")
	}
	if _, ok := f.volumes[gone+".nvram"]; ok {
		t.Fatal("orphan NVRAM survived")
	}
	if _, ok := f.volumes[inflight+".nvram"]; !ok {
		t.Fatal("in-flight create's NVRAM deleted")
	}
	if _, ok := f.volumes["job-other.nvram"]; !ok {
		t.Fatal("foreign volume deleted")
	}
	g.untrack(inflight)
	states(t, g)
	if _, ok := f.volumes[inflight+".nvram"]; ok {
		t.Fatal("settled orphan NVRAM survived")
	}
}

func TestTPMKeyFailsInitAsUnknown(t *testing.T) {
	raw := `{"pool_name":"p","base_volume_name":"b","name_prefix":"job-","max_size":1,"tpm":true}`
	g := &InstanceGroup{connect: func(string) (Client, error) { return newFake(), nil }}
	if err := json.Unmarshal([]byte(raw), g); err != nil {
		t.Fatal(err)
	}
	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil || !strings.Contains(err.Error(), `"tpm"`) {
		t.Fatalf("err=%v", err)
	}
}

func TestQcow2TemplateCarriesFormatAndCapacityIntoCloneAndDomain(t *testing.T) {
	f := newFake()
	f.template.Format, f.template.CapacityBytes = "qcow2", 540672
	g := &InstanceGroup{Config: efiConfig(), connect: func(string) (Client, error) { return f, nil }}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatal(err)
	}
	if n, err := g.Increase(context.Background(), 1); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for name, d := range f.domains {
		nv := name + ".nvram"
		if !strings.Contains(d.xml, `<nvram format="qcow2">/pool/`+nv+`</nvram>`) {
			t.Fatalf("domain xml: %s", d.xml)
		}
		vol := f.volumes[nv]
		if !strings.Contains(vol, `<format type="qcow2">`) || !strings.Contains(vol, `>540672</capacity>`) {
			t.Fatalf("clone XML: %s", vol)
		}
	}
}

func TestCreateFailsAndDeletesDiskWhenTemplateUnreadable(t *testing.T) {
	g, f := newGroup(t, efiConfig(), false)
	f.templateErr = errors.New("template gone")
	n, err := g.Increase(context.Background(), 1)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "template gone") {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.volumes) != 0 || len(f.clones) != 0 || len(f.domains) != 0 {
		t.Fatalf("leaked volumes=%v clones=%v domains=%d", f.volumes, f.clones, len(f.domains))
	}
}

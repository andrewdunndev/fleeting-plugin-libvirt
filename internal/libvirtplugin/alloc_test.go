package libvirtplugin

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// hostdevXML is a domain as libvirt returns it: 0x-prefixed host addresses
// under source, a guest-side address beside it, and hostdevs of other kinds.
const hostdevXML = `<domain type='kvm'><name>other</name><devices>
<hostdev mode='subsystem' type='pci' managed='yes'><source><address domain='0x0000' bus='0x0A' slot='0x00' function='0x1'/></source><address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/></hostdev>
<hostdev mode='subsystem' type='usb' managed='yes'><source><vendor id='0x1234'/><product id='0x5678'/><address bus='1' device='4'/></source></hostdev>
<hostdev mode='subsystem' type='scsi'><source><adapter name='scsi_host0'/><address bus='0' target='0' unit='0'/></source></hostdev>
</devices></domain>`

func TestParseHostdevAddresses(t *testing.T) {
	got, err := parseHostdevAddresses(hostdevXML)
	if err != nil || !slices.Equal(got, []string{"0000:0a:00.1"}) {
		t.Fatalf("got %v err=%v", got, err)
	}
	if got, err := parseHostdevAddresses("<domain><name>x</name></domain>"); err != nil || len(got) != 0 {
		t.Fatalf("got %v err=%v", got, err)
	}
	if _, err := parseHostdevAddresses("<domain>"); err == nil {
		t.Fatal("malformed XML accepted")
	}
}

func TestDomainXMLHostdev(t *testing.T) {
	s := domainSpec{Name: "n", Network: "default", DiskPath: "/d", VCPU: 1, MemoryMiB: 1, PCIAddress: devA}
	out, err := domainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `<hostdev mode="subsystem" type="pci" managed="no"><source><address domain="0x0000" bus="0x03" slot="0x00" function="0x0"></address></source></hostdev>`
	if !strings.Contains(out, want) {
		t.Fatalf("hostdev missing:\n%s", out)
	}
	if got, _ := parseHostdevAddresses(out); !slices.Equal(got, []string{devA}) {
		t.Fatalf("round trip %v", got)
	}
	s.PCIAddress = ""
	if out, _ = domainXML(s); strings.Contains(out, "hostdev") {
		t.Fatal("hostdev without a pool address")
	}
}

// outside starts a domain this plugin knows nothing about, holding addrs.
func (f *fakeClient) outside(name string, addrs ...string) {
	var b strings.Builder
	b.WriteString("<domain type='kvm'><name>" + name + "</name><devices>")
	for _, a := range addrs {
		b.WriteString("<hostdev mode='subsystem' type='pci' managed='yes'><source><address domain='0x" + a[0:4] + "' bus='0x" + a[5:7] + "' slot='0x" + a[8:10] + "' function='0x" + a[11:] + "'/></source></hostdev>")
	}
	b.WriteString("</devices></domain>")
	f.mu.Lock()
	f.domains[name] = &fakeDomain{info: DomainInfo{Name: name}, running: true, xml: b.String()}
	f.mu.Unlock()
}

func (f *fakeClient) drop(name string) {
	f.mu.Lock()
	delete(f.domains, name)
	f.mu.Unlock()
}

func (f *fakeClient) volumeNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.volumes {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// devicesOf lists the pool addresses held by the domains this group made.
func (f *fakeClient) devicesOf(t *testing.T) []string {
	t.Helper()
	var out []string
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, d := range f.domains {
		if !strings.HasPrefix(n, "job-") {
			continue
		}
		a, err := parseHostdevAddresses(d.xml)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a...)
	}
	slices.Sort(out)
	return out
}

func startPool(t *testing.T, f *fakeClient, devs ...string) *InstanceGroup {
	t.Helper()
	g := &InstanceGroup{Config: pciConfig(devs...), connect: func(string) (Client, error) { return f, nil }}
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return g
}

func TestAllocatorRestartRediscovery(t *testing.T) {
	f := pciHost()
	first := startPool(t, f, devA, devB)
	if n, err := first.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.devicesOf(t); !slices.Equal(got, []string{devA}) {
		t.Fatalf("first took %v", got)
	}

	second := startPool(t, f, devA, devB)
	if n, err := second.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.devicesOf(t); !slices.Equal(got, []string{devA, devB}) {
		t.Fatalf("restarted plugin double-assigned or lost a device: %v", got)
	}
	if _, err := second.Increase(context.Background(), 1); !errors.Is(err, errNoDevice) {
		t.Fatalf("full pool: %v", err)
	}

	for name, d := range f.domains {
		if a, _ := parseHostdevAddresses(d.xml); slices.Equal(a, []string{devA}) {
			f.drop(name)
		}
	}
	if n, err := second.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.devicesOf(t); !slices.Equal(got, []string{devA, devB}) {
		t.Fatalf("freed device not reused: %v", got)
	}
}

func TestAllocatorSkipsDevicesHeldElsewhere(t *testing.T) {
	f := pciHost()
	f.nodeDevices["pci_0000_03_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP)
	f.nodeDevices["pci_0000_03_00_1"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP)
	f.outside("other", peerP)
	g := startPool(t, f, devA, devB)
	if n, err := g.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.devicesOf(t); !slices.Equal(got, []string{devB}) {
		t.Fatalf("a group member in use elsewhere did not block devA: %v", got)
	}
}

func TestAllocatorBusyFallback(t *testing.T) {
	f := pciHost()
	g := startPool(t, f, devA, devB)
	f.createHook = func(xml string) error {
		if strings.Contains(xml, `bus="0x03"`) {
			f.outside("late", devA)
			return errors.New("device or resource busy")
		}
		return nil
	}
	if n, err := g.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.devicesOf(t); !slices.Equal(got, []string{devB}) {
		t.Fatalf("devices %v", got)
	}
	if got := f.volumeNames(); len(got) != 1 {
		t.Fatalf("the retry made new volumes: %v", got)
	}
	if len(f.deletedVolumes) != 0 {
		t.Fatalf("volumes deleted during fallback: %v", f.deletedVolumes)
	}
	f.drop("late")
	f.createHook = nil
	if n, err := g.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("released reservation not reusable: n=%d err=%v", n, err)
	}
}

func TestAllocatorTriedSetSpansIncrease(t *testing.T) {
	f := pciHost()
	g := startPool(t, f, devA, devB)
	attempts := 0
	f.createHook = func(xml string) error {
		if strings.Contains(xml, `bus="0x03"`) {
			attempts++
			f.outside("late", devA)
			return errors.New("busy")
		}
		f.drop("late")
		return nil
	}
	n, err := g.Increase(context.Background(), 2)
	if n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.devicesOf(t); !slices.Equal(got, []string{devB}) {
		t.Fatalf("a device proven busy in this call was picked again: %v", got)
	}
	if attempts != 1 {
		t.Fatalf("busy device attempted %d times in one call", attempts)
	}
}

func TestAllocatorRealErrorPassesThrough(t *testing.T) {
	f := pciHost()
	g := startPool(t, f, devA, devB)
	boom := errors.New("libvirt exploded")
	f.createHook = func(string) error { return boom }
	n, err := g.Increase(context.Background(), 1)
	if n != 0 || !errors.Is(err, boom) || errors.Is(err, errNoDevice) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.volumeNames(); len(got) != 0 {
		t.Fatalf("volumes left: %v", got)
	}
	f.createHook = nil
	if n, err := g.Increase(context.Background(), 2); n != 2 || err != nil {
		t.Fatalf("reservation leaked: n=%d err=%v", n, err)
	}
}

func TestAllocatorExhaustionCleansVolumes(t *testing.T) {
	f := pciHost()
	g := startPool(t, f, devA)
	f.createHook = func(string) error {
		f.outside("late", devA)
		return errors.New("busy")
	}
	n, err := g.Increase(context.Background(), 1)
	if n != 0 || !errors.Is(err, errNoDevice) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.volumeNames(); len(got) != 0 {
		t.Fatalf("pool not empty: %v", got)
	}

	f2 := pciHost()
	f2.outside("held", devA)
	g2 := startPool(t, f2, devA)
	if n, err := g2.Increase(context.Background(), 1); n != 0 || !errors.Is(err, errNoDevice) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f2.volumeNames(); len(got) != 0 {
		t.Fatalf("volumes made without a device: %v", got)
	}
	if len(f2.createdVolumes) != 0 || len(f2.deletedVolumes) != 0 {
		t.Fatalf("volume churn before the claim: created %v deleted %v", f2.createdVolumes, f2.deletedVolumes)
	}
}

func TestAllocatorClaimFailsClosedOnActiveReadError(t *testing.T) {
	f := pciHost()
	g := startPool(t, f, devA)
	f.activeErr = errors.New("connection reset")
	n, err := g.Increase(context.Background(), 1)
	if n != 0 || err == nil || errors.Is(err, errNoDevice) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("read error lost: %v", err)
	}
	if len(f.createdVolumes) != 0 || len(f.domains) != 0 {
		t.Fatalf("instance started without a device: volumes %v domains %d", f.createdVolumes, len(f.domains))
	}
	if _, err := g.claimDevice(context.Background(), map[string]bool{}); err == nil {
		t.Fatal("claim succeeded on a failed read")
	}
	if len(g.reserved) != 0 {
		t.Fatalf("reservation leaked: %v", g.reserved)
	}
}

func TestAllocatorPartialIncrease(t *testing.T) {
	f := pciHost()
	g := startPool(t, f, devA, devB)
	if n, err := g.Increase(context.Background(), 3); n != 2 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.devicesOf(t); !slices.Equal(got, []string{devA, devB}) {
		t.Fatalf("devices %v", got)
	}
	reads := f.activeReads
	if n, err := g.Increase(context.Background(), 5); n != 0 || !errors.Is(err, errNoDevice) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if f.activeReads != reads+1 {
		t.Fatalf("errNoDevice did not stop the loop: %d reads", f.activeReads-reads)
	}
}

func TestAllocatorConcurrentClaimsAreDistinct(t *testing.T) {
	f := pciHost()
	g := startPool(t, f, devA, devB)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var got []string
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := g.claimDevice(context.Background(), map[string]bool{})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			got = append(got, a)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.Sort(got)
	if !slices.Equal(got, []string{devA, devB}) {
		t.Fatalf("claims %v", got)
	}
}

// A used set read before the lock goes stale while another claim takes an
// address, starts its domain and releases the reservation; a claim built on
// that read would hand the same address out again.
func TestAllocatorUsedSetReadUnderReservationLock(t *testing.T) {
	f := pciHost()
	g := startPool(t, f, devA, devB)
	var fired atomic.Bool
	var other string
	done := make(chan struct{})
	f.afterActiveRead = func() {
		if fired.CompareAndSwap(false, true) {
			go func() {
				defer close(done)
				other, _ = g.claimDevice(context.Background(), map[string]bool{})
				f.outside("started", other)
				g.releaseDevice(other)
			}()
			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	first, err := g.claimDevice(context.Background(), map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if first == other {
		t.Fatalf("both claims took %s", first)
	}
}

func TestAllocatorBusyFallbackOnGroupMember(t *testing.T) {
	f := pciHost()
	f.nodeDevices["pci_0000_03_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP)
	f.nodeDevices["pci_0000_03_00_1"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP)
	g := startPool(t, f, devA, devB)
	f.createHook = func(xml string) error {
		if strings.Contains(xml, `bus="0x03"`) {
			f.outside("late", peerP)
			return errors.New("device or resource busy")
		}
		return nil
	}
	if n, err := g.Increase(context.Background(), 1); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.devicesOf(t); !slices.Equal(got, []string{devB}) {
		t.Fatalf("devices %v", got)
	}
}

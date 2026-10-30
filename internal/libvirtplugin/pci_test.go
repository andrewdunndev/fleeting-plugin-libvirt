package libvirtplugin

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// nodeXML is the shape of libvirt's node-device description for a PCI
// function: the bound driver, the class, and the IOMMU group members as
// 0x-prefixed address attributes.
func nodeXML(driver, class string, group int, peers ...string) string {
	var b strings.Builder
	b.WriteString("<device><name>pci_x</name><path>/sys/devices/pci0000:00/x</path><parent>pci_0000_00_01_1</parent>")
	if driver != "" {
		b.WriteString("<driver><name>" + driver + "</name></driver>")
	}
	b.WriteString("<capability type='pci'><class>" + class + "</class><domain>0</domain><bus>3</bus><slot>0</slot><function>0</function>")
	if group >= 0 {
		b.WriteString("<iommuGroup number='" + strconv.Itoa(group) + "'>")
		for _, p := range peers {
			b.WriteString("<address domain='0x" + p[0:4] + "' bus='0x" + p[5:7] + "' slot='0x" + p[8:10] + "' function='0x" + p[11:] + "'/>")
		}
		b.WriteString("</iommuGroup>")
	}
	b.WriteString("</capability></device>")
	return b.String()
}

const (
	devA   = "0000:03:00.0"
	devB   = "0000:04:00.0"
	peerP  = "0000:03:00.1"
	bridge = "0000:02:00.0"
)

func pciConfig(devs ...string) Config {
	c := baseConfig()
	c.PCIDevices = devs
	return c
}

// pciHost is a host with devA in group 1 and devB in group 2, both vfio-bound
// with no peers beyond themselves.
func pciHost() *fakeClient {
	f := newFake()
	f.nodeDevices = map[string]string{
		"pci_0000_03_00_0": nodeXML("vfio-pci", "0x030000", 1, devA),
		"pci_0000_04_00_0": nodeXML("vfio-pci", "0x030000", 2, devB),
	}
	return f
}

func TestParseNodeDevice(t *testing.T) {
	d, err := parseNodeDevice(nodeXML("vfio-pci", "0x0C0330", 7, "0000:03:00.0", "0000:03:00.1"))
	if err != nil || d.Driver != "vfio-pci" || d.Class != "0x0c0330" || d.Group != 7 ||
		!slices.Equal(d.Peers, []string{"0000:03:00.0", "0000:03:00.1"}) {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	d, err = parseNodeDevice(nodeXML("", "0x030000", -1))
	if err != nil || d.Driver != "" || d.Group != -1 || len(d.Peers) != 0 {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if _, err := parseNodeDevice("<device><capability type='net'/></device>"); err == nil {
		t.Fatal("non-PCI node device accepted")
	}
	if _, err := parseNodeDevice("<device>"); err == nil {
		t.Fatal("malformed XML accepted")
	}
}

func TestPCINodeName(t *testing.T) {
	if got := pciNodeName("0000:03:00.1"); got != "pci_0000_03_00_1" {
		t.Fatal(got)
	}
}

func TestNormalizePCI(t *testing.T) {
	c := pciConfig("0000:0A:00.0", devB)
	if err := c.normalize(); err != nil || c.PCIDevices[0] != "0000:0a:00.0" {
		t.Fatalf("err=%v devices=%v", err, c.PCIDevices)
	}
	bad := map[string]Config{
		"short":     pciConfig("03:00.0"),
		"function":  pciConfig("0000:03:00.8"),
		"junk":      pciConfig("0000:03:00.0 "),
		"empty":     pciConfig(""),
		"duplicate": pciConfig(devA, devB, devA),
		"case dup":  pciConfig("0000:0a:00.0", "0000:0A:00.0"),
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			if err := c.normalize(); err == nil || !strings.Contains(err.Error(), "pci_devices") {
				t.Fatalf("err=%v", err)
			}
		})
	}
	for _, uri := range []string{"qemu:///session", "qemu+unix:///session?socket=/run/x"} {
		c := pciConfig(devA)
		c.URI = uri
		if err := c.normalize(); err == nil || !strings.Contains(err.Error(), "session") {
			t.Fatalf("%s: err=%v", uri, err)
		}
	}
	c = baseConfig()
	c.URI = "qemu:///session"
	if err := c.normalize(); err != nil {
		t.Fatalf("session URI refused without pci_devices: %v", err)
	}
	c = pciConfig(devA)
	c.URI = "qemu+ssh://host/system"
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestInitAcceptsPCIPool(t *testing.T) {
	f := pciHost()
	g := &InstanceGroup{Config: pciConfig("0000:03:00.0", devB), connect: func(string) (Client, error) { return f, nil }}
	info, err := g.Init(t.Context(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil || info.MaxSize != 2 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if !slices.Equal(f.nodeReads, []string{"pci_0000_03_00_0", "pci_0000_04_00_0"}) {
		t.Fatalf("reads=%v", f.nodeReads)
	}
}

func TestInitLowerCasesPCIPool(t *testing.T) {
	f := pciHost()
	f.nodeDevices["pci_0000_0a_00_0"] = nodeXML("vfio-pci", "0x030000", 3, "0000:0a:00.0")
	if err := initWith(t, pciConfig("0000:0A:00.0"), f); err != nil {
		t.Fatal(err)
	}
}

func TestPCIMaxSize(t *testing.T) {
	cases := []struct{ pool, max, want int }{{2, 0, 2}, {2, 1, 1}, {2, 2, 2}, {2, 9, 2}}
	for _, tc := range cases {
		c := pciConfig(devA, devB)
		c.MaxSize = tc.max
		g := &InstanceGroup{Config: c}
		if got := g.maxSize(); got != tc.want {
			t.Errorf("max_size %d: got %d, want %d", tc.max, got, tc.want)
		}
	}
	g := &InstanceGroup{Config: baseConfig()}
	if got := g.maxSize(); got != 4 {
		t.Errorf("no pool: got %d", got)
	}
	g.MaxSize = 0
	if got := g.maxSize(); got != unboundedSize {
		t.Errorf("no pool, no max_size: got %d", got)
	}
}

func TestInitRefusesPCIPool(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeClient)
		want   string
	}{
		"missing device": {func(f *fakeClient) { delete(f.nodeDevices, "pci_0000_04_00_0") }, devB},
		"host driver":    {func(f *fakeClient) { f.nodeDevices["pci_0000_04_00_0"] = nodeXML("nouveau", "0x030000", 2, devB) }, devB},
		"no driver":      {func(f *fakeClient) { f.nodeDevices["pci_0000_04_00_0"] = nodeXML("", "0x030000", 2, devB) }, devB},
		"no group":       {func(f *fakeClient) { f.nodeDevices["pci_0000_04_00_0"] = nodeXML("vfio-pci", "0x030000", -1) }, "IOMMU"},
		"shared group number": {func(f *fakeClient) {
			f.nodeDevices["pci_0000_04_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devB)
		}, "share IOMMU group"},
		"pool peer listed": {func(f *fakeClient) {
			f.nodeDevices["pci_0000_03_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devA, devB)
		}, "share IOMMU group"},
		"peer on host driver": {func(f *fakeClient) {
			f.nodeDevices["pci_0000_03_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP)
			f.nodeDevices["pci_0000_03_00_1"] = nodeXML("snd_hda_intel", "0x040300", 1, devA, peerP)
		}, peerP},
		"peer without driver": {func(f *fakeClient) {
			f.nodeDevices["pci_0000_03_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP)
			f.nodeDevices["pci_0000_03_00_1"] = nodeXML("", "0x040300", 1, devA, peerP)
		}, peerP},
		"peer unreadable": {func(f *fakeClient) {
			f.nodeDevices["pci_0000_03_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP)
		}, peerP},
		"peer class only looks like a bridge": {func(f *fakeClient) {
			f.nodeDevices["pci_0000_03_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP)
			f.nodeDevices["pci_0000_03_00_1"] = nodeXML("xhci_hcd", "0x0c0330", 1, devA, peerP)
		}, peerP},
		"no pci hostdev caps": {func(f *fakeClient) {
			f.caps = &DomainCaps{TPM: true}
		}, "vfio"},
		"no vfio backend": {func(f *fakeClient) {
			f.caps = &DomainCaps{PCIHostdev: true, PCIBackends: []string{"default", "kvm"}}
		}, "vfio"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := pciHost()
			tc.mutate(f)
			err := initWith(t, pciConfig(devA, devB), f)
			if err == nil || !f.closed || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "pci_devices") {
				t.Fatalf("err=%v closed=%v", err, f.closed)
			}
		})
	}
}

func TestInitAcceptsOutOfPoolPeers(t *testing.T) {
	f := pciHost()
	f.nodeDevices["pci_0000_03_00_0"] = nodeXML("vfio-pci", "0x030000", 1, devA, peerP, bridge)
	f.nodeDevices["pci_0000_03_00_1"] = nodeXML("vfio-pci", "0x040300", 1, devA, peerP, bridge)
	f.nodeDevices["pci_0000_02_00_0"] = nodeXML("pcieport", "0x060400", 1, devA, peerP, bridge)
	if err := initWith(t, pciConfig(devA, devB), f); err != nil {
		t.Fatal(err)
	}
}

func TestInitReadsCapsForPCIPool(t *testing.T) {
	f := pciHost()
	if err := initWith(t, pciConfig(devA), f); err != nil || len(f.capsCalls) != 1 {
		t.Fatalf("err=%v calls=%v", err, f.capsCalls)
	}
	f = pciHost()
	if err := initWith(t, baseConfig(), f); err != nil || len(f.nodeReads) != 0 {
		t.Fatalf("err=%v reads=%v", err, f.nodeReads)
	}
}

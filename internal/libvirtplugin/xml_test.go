package libvirtplugin

import (
	"slices"
	"strings"
	"testing"
)

const goldenDomain = `<domain type="kvm"><name>job-a-1</name><title>t</title><metadata><instance xmlns="urn:fleeting-plugin-libvirt:v1"><prefix>job-</prefix></instance></metadata><memory unit="MiB">4096</memory><vcpu>2</vcpu><resource><partition>/machine/ci</partition></resource><os><type>hvm</type><smbios mode="sysinfo"></smbios></os><sysinfo type="smbios"><oemStrings><entry>io.systemd.credential:ssh.authorized_keys.root=ssh-ed25519 AAAA</entry></oemStrings></sysinfo><features><acpi></acpi><apic></apic></features><cpu mode="host-passthrough" check="none"></cpu><clock offset="utc"><timer name="rtc" tickpolicy="catchup"></timer><timer name="pit" tickpolicy="delay"></timer><timer name="hpet" present="no"></timer></clock><on_poweroff>destroy</on_poweroff><on_reboot>restart</on_reboot><on_crash>destroy</on_crash><devices><disk type="file" device="disk"><driver name="qemu" type="qcow2"></driver><source file="/p/d.qcow2"></source><target dev="vda" bus="virtio"></target></disk><interface type="network"><source network="runner"></source><model type="virtio"></model></interface><channel type="unix"><target type="virtio" name="org.qemu.guest_agent.0"></target></channel><serial type="pty"><target port="0"></target></serial><console type="pty"><target type="serial" port="0"></target></console><rng model="virtio"><backend model="random">/dev/urandom</backend></rng></devices></domain>`

const goldenVolume = `<volume><name>job-a-1</name><capacity unit="bytes">1073741824</capacity><target><format type="qcow2"></format></target><backingStore><path>/p/b</path><format type="raw"></format></backingStore></volume>`

func fullSpec() domainSpec {
	return domainSpec{
		Name: "job-a-1", Title: "t", Prefix: "job-", Partition: "/machine/ci",
		Network: "runner", DiskPath: "/p/d.qcow2", VCPU: 2, MemoryMiB: 4096,
		CredentialKey: "ssh.authorized_keys.root", AuthorizedKey: "ssh-ed25519 AAAA",
	}
}

func TestDomainXMLGolden(t *testing.T) {
	got, err := domainXML(fullSpec())
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenDomain {
		t.Fatalf("domain XML drifted:\n got %s\nwant %s", got, goldenDomain)
	}
}

func TestDomainXMLOptionalPartsAbsent(t *testing.T) {
	s := fullSpec()
	s.Partition, s.CredentialKey, s.AuthorizedKey, s.Title = "", "", "", ""
	got, _ := domainXML(s)
	for _, part := range []string{"<resource>", "<sysinfo", "<smbios", "<title>"} {
		if strings.Contains(got, part) {
			t.Errorf("%s present without its input: %s", part, got)
		}
	}
}

func TestDomainXMLEscapes(t *testing.T) {
	s := fullSpec()
	s.Title = `a<b>&"c"`
	got, _ := domainXML(s)
	if !strings.Contains(got, "<title>a&lt;b&gt;&amp;&#34;c&#34;</title>") {
		t.Fatalf("title not escaped: %s", got)
	}
	if info, err := parseDomainXML(got); err != nil || info.Name != "job-a-1" {
		t.Fatalf("escaped XML does not parse: %v %v", info, err)
	}
}

func TestVolumeXMLGolden(t *testing.T) {
	got, err := volumeXML("job-a-1", Base{Path: "/p/b", Format: "raw"}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenVolume {
		t.Fatalf("volume XML drifted:\n got %s\nwant %s", got, goldenVolume)
	}
	got, _ = volumeXML("x", Base{Path: "/p/b"}, 1)
	if !strings.Contains(got, `<backingStore><path>/p/b</path><format type="qcow2">`) {
		t.Fatalf("empty base format not defaulted: %s", got)
	}
}

func TestParseDomainXMLReadsOwnership(t *testing.T) {
	info, err := parseDomainXML(goldenDomain)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "job-a-1" || info.Prefix != "job-" {
		t.Fatalf("got %+v", info)
	}
	other := `<domain><name>x</name><metadata><instance xmlns="urn:other"><prefix>job-</prefix></instance></metadata></domain>`
	if info, _ := parseDomainXML(other); info.Prefix != "" {
		t.Fatalf("foreign namespace read as ours: %+v", info)
	}
}

func TestParseVolumeFormat(t *testing.T) {
	f, err := parseVolumeFormat(`<volume><name>b</name><target><path>/p</path><format type='raw'/></target></volume>`)
	if err != nil || f != "raw" {
		t.Fatalf("got %q %v", f, err)
	}
}

func TestParseDomainMAC(t *testing.T) {
	x := `<domain><devices><interface type='network'><mac address='52:54:00:12:34:56'/><source network='default'/></interface></devices></domain>`
	if got, err := parseDomainMAC(x); err != nil || got != "52:54:00:12:34:56" {
		t.Fatalf("got %q %v", got, err)
	}
}

const (
	goldenOSBIOS = `<os><type>hvm</type><smbios mode="sysinfo"></smbios></os>`
	goldenOSEFI  = `<os firmware="efi"><type machine="q35">hvm</type><firmware><feature enabled="no" name="secure-boot"></feature></firmware><loader format="qcow2"></loader><nvram format="qcow2">/p/d.nvram</nvram><smbios mode="sysinfo"></smbios></os>`
	goldenOSSB   = `<os firmware="efi"><type machine="q35">hvm</type><firmware><feature enabled="yes" name="secure-boot"></feature><feature enabled="no" name="enrolled-keys"></feature></firmware><loader format="qcow2"></loader><nvram format="qcow2">/p/d.nvram</nvram><smbios mode="sysinfo"></smbios></os>`
	goldenFeat   = `<features><acpi></acpi><apic></apic></features>`
	goldenFeatSB = `<features><acpi></acpi><apic></apic><smm state="on"></smm></features>`
)

func TestDomainXMLFirmwareModes(t *testing.T) {
	tests := []struct {
		name         string
		firmware     string
		secure       bool
		wantOS       string
		wantFeatures string
		want         string
	}{
		{"unset is bios", "", false, goldenOSBIOS, goldenFeat, goldenDomain},
		{"bios", "bios", false, goldenOSBIOS, goldenFeat, goldenDomain},
		{"efi", "efi", false, goldenOSEFI, goldenFeat, ""},
		{"secure boot", "efi", true, goldenOSSB, goldenFeatSB, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fullSpec()
			s.Firmware, s.SecureBoot = tc.firmware, tc.secure
			if tc.firmware == "efi" {
				s.NVRAMPath, s.NVRAMFormat = "/p/d.nvram", "qcow2"
			}
			got, err := domainXML(s)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = strings.Replace(goldenDomain, goldenOSBIOS, tc.wantOS, 1)
				want = strings.Replace(want, goldenFeat, tc.wantFeatures, 1)
			}
			if got != want {
				t.Fatalf("domain XML:\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestDomainXMLNVRAMFormatFollowsTemplate(t *testing.T) {
	s := fullSpec()
	s.Firmware, s.NVRAMPath, s.NVRAMFormat = "efi", "/p/d.nvram", "raw"
	got, _ := domainXML(s)
	if !strings.Contains(got, `<nvram format="raw">/p/d.nvram</nvram>`) {
		t.Fatalf("nvram element: %s", got)
	}
	s.Firmware = "bios"
	if got, _ := domainXML(s); strings.Contains(got, "nvram") {
		t.Fatalf("bios domain carries nvram: %s", got)
	}
}

func TestNVRAMVolumeXMLGolden(t *testing.T) {
	got, err := nvramXML("job-a-1.nvram", "raw", 540672)
	if err != nil {
		t.Fatal(err)
	}
	const want = `<volume><name>job-a-1.nvram</name><capacity unit="bytes">540672</capacity><target><format type="raw"></format></target></volume>`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestDomainXMLTPM(t *testing.T) {
	s := fullSpec()
	s.TPM = true
	got, _ := domainXML(s)
	want := `<tpm model="tpm-tis"><backend type="emulator" version="2.0"></backend></tpm>`
	if !strings.Contains(got, want) {
		t.Fatalf("tpm element missing: %s", got)
	}
	if strings.Contains(got, "persistent_state") {
		t.Fatalf("tpm state must not persist: %s", got)
	}
	s.TPM = false
	if got, _ := domainXML(s); got != goldenDomain || strings.Contains(got, "<tpm") {
		t.Fatalf("tpm element present without the key: %s", got)
	}
}

func TestParseDomainCaps(t *testing.T) {
	c, err := parseDomainCaps(fullCapsXML)
	if err != nil {
		t.Fatal(err)
	}
	if !c.TPM || !slices.Equal(c.TPMModels, []string{"tpm-tis", "tpm-crb"}) ||
		!slices.Equal(c.TPMBackends, []string{"passthrough", "emulator"}) ||
		!slices.Equal(c.TPMVersions, []string{"2.0"}) ||
		!c.PCIHostdev || !slices.Equal(c.PCIBackends, []string{"default", "vfio"}) {
		t.Fatalf("caps = %+v", c)
	}
	c, err = parseDomainCaps(`<domainCapabilities><devices><tpm supported='no'/><hostdev supported='yes'><enum name='subsysType'><value>usb</value></enum></hostdev></devices></domainCapabilities>`)
	if err != nil || c.TPM || c.PCIHostdev || len(c.TPMModels) != 0 {
		t.Fatalf("unsupported caps = %+v %v", c, err)
	}
	if _, err := parseDomainCaps("<domainCapabilities>"); err == nil {
		t.Fatal("malformed XML accepted")
	}
}

func TestDomainXMLNWFilter(t *testing.T) {
	s := fullSpec()
	s.NWFilter = "clean-traffic"
	s.NWFilterParams = map[string]string{"DHCPSERVER": "192.0.2.1", "CTRL_IP_LEARNING": "dhcp"}
	got, err := domainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	want := `<model type="virtio"></model><filterref filter="clean-traffic"><parameter name="CTRL_IP_LEARNING" value="dhcp"></parameter><parameter name="DHCPSERVER" value="192.0.2.1"></parameter></filterref></interface>`
	if !strings.Contains(got, want) {
		t.Fatalf("filterref missing or misplaced: %s", got)
	}
	s.NWFilter, s.NWFilterParams = "", nil
	if got, _ := domainXML(s); got != goldenDomain || strings.Contains(got, "filterref") {
		t.Fatalf("filterref present without the key: %s", got)
	}
}

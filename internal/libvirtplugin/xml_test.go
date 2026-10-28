package libvirtplugin

import (
	"strings"
	"testing"
)

const goldenDomain = `<domain type="kvm"><name>job-a-1</name><title>t</title><metadata><instance xmlns="urn:fleeting-plugin-libvirt:v1"><prefix>job-</prefix><owner>o</owner></instance></metadata><memory unit="MiB">4096</memory><vcpu>2</vcpu><resource><partition>/machine/ci</partition></resource><os><type>hvm</type><smbios mode="sysinfo"></smbios></os><sysinfo type="smbios"><oemStrings><entry>io.systemd.credential:ssh.authorized_keys.root=ssh-ed25519 AAAA</entry></oemStrings></sysinfo><features><acpi></acpi><apic></apic></features><cpu mode="host-passthrough" check="none"></cpu><clock offset="utc"><timer name="rtc" tickpolicy="catchup"></timer><timer name="pit" tickpolicy="delay"></timer><timer name="hpet" present="no"></timer></clock><on_poweroff>destroy</on_poweroff><on_reboot>restart</on_reboot><on_crash>destroy</on_crash><devices><disk type="file" device="disk"><driver name="qemu" type="qcow2"></driver><source file="/p/d.qcow2"></source><target dev="vda" bus="virtio"></target></disk><interface type="network"><source network="runner"></source><model type="virtio"></model></interface><channel type="unix"><target type="virtio" name="org.qemu.guest_agent.0"></target></channel><serial type="pty"><target port="0"></target></serial><console type="pty"><target type="serial" port="0"></target></console><rng model="virtio"><backend model="random">/dev/urandom</backend></rng></devices></domain>`

const goldenVolume = `<volume><name>job-a-1</name><capacity unit="bytes">1073741824</capacity><target><format type="qcow2"></format></target><backingStore><path>/p/b</path><format type="raw"></format></backingStore></volume>`

func fullSpec() domainSpec {
	return domainSpec{
		Name: "job-a-1", Title: "t", Prefix: "job-", Owner: "o", Partition: "/machine/ci",
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
	if info.Name != "job-a-1" || info.Prefix != "job-" || info.Owner != "o" {
		t.Fatalf("got %+v", info)
	}
	other := `<domain><name>x</name><metadata><instance xmlns="urn:other"><owner>o</owner></instance></metadata></domain>`
	if info, _ := parseDomainXML(other); info.Owner != "" {
		t.Fatalf("foreign namespace read as ours: %+v", info)
	}
}

func TestParseVolumeFormat(t *testing.T) {
	f, err := parseVolumeFormat(`<volume><name>b</name><target><path>/p</path><format type='raw'/></target></volume>`)
	if err != nil || f != "raw" {
		t.Fatalf("got %q %v", f, err)
	}
}

package libvirtplugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type fakeDomain struct {
	info    DomainInfo
	running bool
	xml     string
}

type fakeClient struct {
	mu      sync.Mutex
	base    Base
	baseErr error
	// template answers BaseVolume for the name "tmpl".
	template    Base
	templateErr error
	cloneErr    error
	// libVersion of 0 reads as a current libvirt.
	libVersion    uint64
	libVersionErr error
	volumes       map[string]string
	domains       map[string]*fakeDomain
	createErr     error
	volErr        error
	agentErr      error
	addrs         []string
	leaseAddrs    []string
	leaseErr      error
	// createDelay widens the window between admission and the domain
	// appearing, so a lock that is released early is observable.
	createDelay time.Duration
	// onDomainCreated lets a test mirror the domain into a ledger.
	onDomainCreated func(name string)
	// afterListVolumes runs once ListVolumes has taken its snapshot.
	afterListVolumes func()
	// onCreateDomain runs when a domain start begins.
	onCreateDomain func()
	// caps answers DomainCaps; nil reads as a host with everything.
	caps      *DomainCaps
	capsErr   error
	capsCalls []string
	// nodeDevices answers NodeDeviceXML by node-device name.
	nodeDevices map[string]string
	nodeReads   []string
	activeReads int
	activeErr   error
	// createHook runs inside a domain start with the domain XML and decides
	// whether it fails.
	createHook func(xml string) error
	// afterActiveRead runs after an active-domain read has snapshotted and
	// released the fake's lock, so the snapshot is already stale to its caller.
	afterActiveRead func()
	closed          bool
	destroyed       []string
	deletedVolumes  []string
	clones          []string
}

func newFake() *fakeClient {
	return &fakeClient{
		base:     Base{Path: "/pool/base.qcow2", Format: "qcow2", CapacityBytes: 10 << 30},
		template: Base{Path: "/pool/tmpl.fd", Format: "qcow2", CapacityBytes: 540672},
		volumes:  map[string]string{},
		domains:  map[string]*fakeDomain{},
		addrs:    []string{"10.1.2.3"},
	}
}

func (f *fakeClient) Close() error { f.closed = true; return nil }

func (f *fakeClient) LibVersion(context.Context) (uint64, error) {
	if f.libVersionErr != nil {
		return 0, f.libVersionErr
	}
	if f.libVersion == 0 {
		return 10010000, nil
	}
	return f.libVersion, nil
}

func (f *fakeClient) DomainCaps(_ context.Context, machine string) (DomainCaps, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capsCalls = append(f.capsCalls, machine)
	if f.capsErr != nil {
		return DomainCaps{}, f.capsErr
	}
	if f.caps != nil {
		return *f.caps, nil
	}
	return parseDomainCaps(fullCapsXML)
}

func (f *fakeClient) BaseVolume(_ context.Context, _, name string) (Base, error) {
	if name == "tmpl" {
		return f.template, f.templateErr
	}
	return f.base, f.baseErr
}

func (f *fakeClient) CreateVolume(_ context.Context, pool, name, xml string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volErr != nil {
		return "", f.volErr
	}
	f.volumes[name] = xml
	return "/pool/" + name + ".qcow2", nil
}

func (f *fakeClient) CloneVolume(_ context.Context, _, src, name, xml string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cloneErr != nil {
		return "", f.cloneErr
	}
	f.volumes[name] = xml
	f.clones = append(f.clones, src+">"+name)
	return "/pool/" + name, nil
}

func (f *fakeClient) DeleteVolume(ctx context.Context, _, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[name]; !ok {
		return fmt.Errorf("volume %s: %w", name, ErrNotFound)
	}
	delete(f.volumes, name)
	f.deletedVolumes = append(f.deletedVolumes, name)
	return nil
}

func (f *fakeClient) ListVolumes(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.volumes {
		out = append(out, n)
	}
	sort.Strings(out)
	if cb := f.afterListVolumes; cb != nil {
		f.mu.Unlock()
		cb()
		f.mu.Lock()
	}
	return out, nil
}

func (f *fakeClient) CreateDomain(_ context.Context, xml string) error {
	if f.onCreateDomain != nil {
		f.onCreateDomain()
	}
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	if f.createHook != nil {
		if err := f.createHook(xml); err != nil {
			return err
		}
	}
	f.mu.Lock()
	if f.createErr != nil {
		f.mu.Unlock()
		return f.createErr
	}
	d := parseDomainForTest(xml)
	f.domains[d.Name] = &fakeDomain{info: d, xml: xml, running: true}
	cb := f.onDomainCreated
	f.mu.Unlock()
	if cb != nil {
		cb(d.Name)
	}
	return nil
}

func (f *fakeClient) DestroyDomain(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.domains[name]; !ok {
		return fmt.Errorf("domain %s: %w", name, ErrNotFound)
	}
	delete(f.domains, name)
	f.destroyed = append(f.destroyed, name)
	return nil
}

func (f *fakeClient) ListDomains(_ context.Context, namePrefix string) ([]DomainInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []DomainInfo
	for _, d := range f.domains {
		if strings.HasPrefix(d.info.Name, namePrefix) {
			out = append(out, d.info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeClient) DomainRunning(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[name]
	if !ok {
		return false, fmt.Errorf("domain %s: %w", name, ErrNotFound)
	}
	return d.running, nil
}

func (f *fakeClient) GuestAddresses(_ context.Context, name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.domains[name]; !ok {
		return nil, fmt.Errorf("domain %s: %w", name, ErrNotFound)
	}
	if f.agentErr != nil {
		return nil, f.agentErr
	}
	return f.addrs, nil
}

func (f *fakeClient) LeaseAddresses(_ context.Context, name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.domains[name]; !ok {
		return nil, fmt.Errorf("domain %s: %w", name, ErrNotFound)
	}
	if f.leaseErr != nil {
		return nil, f.leaseErr
	}
	return f.leaseAddrs, nil
}

// parseDomainForTest reads back the identity the plugin wrote, through the
// same parser the real client uses.
func parseDomainForTest(xml string) DomainInfo {
	d, err := parseDomainXML(xml)
	if err != nil {
		panic(errors.New("fake: " + err.Error()))
	}
	return d
}

const fullCapsXML = `<domainCapabilities><devices>
<hostdev supported='yes'><enum name='mode'><value>subsystem</value></enum><enum name='subsysType'><value>usb</value><value>pci</value></enum><enum name='pciBackend'><value>default</value><value>vfio</value></enum></hostdev>
<tpm supported='yes'><enum name='model'><value>tpm-tis</value><value>tpm-crb</value></enum><enum name='backendModel'><value>passthrough</value><value>emulator</value></enum><enum name='backendVersion'><value>2.0</value></enum></tpm>
</devices></domainCapabilities>`

func (f *fakeClient) NodeDeviceXML(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodeReads = append(f.nodeReads, name)
	s, ok := f.nodeDevices[name]
	if !ok {
		return "", fmt.Errorf("node device %q: %w", name, ErrNotFound)
	}
	return s, nil
}

func (f *fakeClient) ActiveDomainsXML(context.Context) ([]string, error) {
	if f.afterActiveRead != nil {
		defer f.afterActiveRead()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeReads++
	var out []string
	for _, d := range f.domains {
		if d.running {
			out = append(out, d.xml)
		}
	}
	sort.Strings(out)
	return out, f.activeErr
}

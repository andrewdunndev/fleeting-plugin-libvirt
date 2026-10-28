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
	info DomainInfo
	xml  string
}

type fakeClient struct {
	mu        sync.Mutex
	base      Base
	baseErr   error
	volumes   map[string]string
	domains   map[string]*fakeDomain
	createErr error
	volErr    error
	agentErr  error
	addrs     []string
	// createDelay widens the window between admission and the domain
	// appearing, so a lock that is released early is observable.
	createDelay time.Duration
	// onDomainCreated lets a test mirror the domain into a ledger.
	onDomainCreated func(name string)
	// afterListVolumes runs once ListVolumes has taken its snapshot.
	afterListVolumes func()
	closed           bool
	destroyed        []string
	deletedVolumes   []string
}

func newFake() *fakeClient {
	return &fakeClient{
		base:    Base{Path: "/pool/base.qcow2", Format: "qcow2", CapacityBytes: 10 << 30},
		volumes: map[string]string{},
		domains: map[string]*fakeDomain{},
		addrs:   []string{"10.1.2.3"},
	}
}

func (f *fakeClient) Close() error { f.closed = true; return nil }

func (f *fakeClient) BaseVolume(_ context.Context, _, _ string) (Base, error) {
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

func (f *fakeClient) DeleteVolume(_ context.Context, _, name string) error {
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
	if f.createDelay > 0 {
		time.Sleep(f.createDelay)
	}
	f.mu.Lock()
	if f.createErr != nil {
		f.mu.Unlock()
		return f.createErr
	}
	d := parseDomainForTest(xml)
	f.domains[d.Name] = &fakeDomain{info: d, xml: xml}
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
	return d.info.Running, nil
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

// parseDomainForTest reads back the identity the plugin wrote, through the
// same parser the real client uses.
func parseDomainForTest(xml string) DomainInfo {
	d, err := parseDomainXML(xml)
	if err != nil {
		panic(errors.New("fake: " + err.Error()))
	}
	d.Running = true
	return d
}

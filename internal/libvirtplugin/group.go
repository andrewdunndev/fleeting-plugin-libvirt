package libvirtplugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

var (
	connectWait  = 2 * time.Minute
	pollInterval = time.Second
	dialTimeout  = 5 * time.Second
	dialContext  = (&net.Dialer{}).DialContext
	admitTimeout = 2 * time.Minute
)

// InstanceGroup implements fleeting's provider.InstanceGroup over libvirt.
type InstanceGroup struct {
	Config

	cfgErr   error
	log      hclog.Logger
	settings provider.Settings
	client   Client
	connect  func(uri string) (Client, error)

	privatePEM []byte
	authorized string

	mu       sync.Mutex
	inflight map[string]struct{}
}

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

func (g *InstanceGroup) Init(ctx context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	if g.cfgErr != nil {
		return provider.ProviderInfo{}, g.cfgErr
	}
	if err := g.normalize(); err != nil {
		return provider.ProviderInfo{}, err
	}
	g.log = log
	g.settings = settings
	g.inflight = map[string]struct{}{}

	if !settings.UseStaticCredentials {
		priv, pub, err := generateKey()
		if err != nil {
			return provider.ProviderInfo{}, err
		}
		g.privatePEM, g.authorized = priv, pub
		if acct, ok := strings.CutPrefix(g.SSHCredentialName, credentialPrefix); ok && acct != settings.Username {
			log.Warn("generated key is seeded for a different account than connector_config.username; set ssh_credential_name to match or SSH login will fail",
				"credential", g.SSHCredentialName, "username", settings.Username)
		}
	}

	connect := g.connect
	if connect == nil {
		connect = dialLibvirt
	}
	client, err := connect(g.URI)
	if err != nil {
		return provider.ProviderInfo{}, err
	}
	if _, err := client.BaseVolume(ctx, g.PoolName, g.BaseVolumeName); err != nil {
		_ = client.Close()
		return provider.ProviderInfo{}, fmt.Errorf("base volume %q in pool %q: %w; check plugin_config.pool_name and base_volume_name, and refresh the pool if the volume is new", g.BaseVolumeName, g.PoolName, err)
	}
	g.client = client

	return provider.ProviderInfo{
		ID:      "libvirt/" + g.PoolName + "/" + g.Owner,
		MaxSize: g.MaxSize,
	}, nil
}

func (g *InstanceGroup) track(name string) {
	g.mu.Lock()
	g.inflight[name] = struct{}{}
	g.mu.Unlock()
}

func (g *InstanceGroup) untrack(name string) {
	g.mu.Lock()
	delete(g.inflight, name)
	g.mu.Unlock()
}

func (g *InstanceGroup) isInflight(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.inflight[name]
	return ok
}

func (g *InstanceGroup) newName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return g.NamePrefix + ownerTag(g.Owner) + "-" + hex.EncodeToString(b[:]), nil
}

// Update reports our domains only; a stopped domain is reported Running so
// its failing Heartbeat leads fleeting to Decrease, the only path that removes
// an instance. Our volumes with no domain and no create in flight are deleted
// here, best effort.
func (g *InstanceGroup) Update(ctx context.Context, fn func(instance string, state provider.State)) error {
	vols, err := g.client.ListVolumes(ctx, g.PoolName)
	if err != nil {
		return fmt.Errorf("listing volumes: %w", err)
	}
	// A volume untracked here has finished its create, so its domain is
	// already visible to the listing that follows.
	var settled []string
	for _, v := range vols {
		if g.ownsName(v) && !g.isInflight(v) {
			settled = append(settled, v)
		}
	}
	doms, err := g.client.ListDomains(ctx, g.NamePrefix)
	if err != nil {
		return fmt.Errorf("listing domains: %w", err)
	}
	seen := map[string]bool{}
	for _, d := range doms {
		if d.Prefix != g.NamePrefix || d.Owner != g.Owner {
			continue
		}
		seen[d.Name] = true
		fn(d.Name, provider.StateRunning)
	}
	for _, v := range settled {
		if seen[v] {
			continue
		}
		if err := g.client.DeleteVolume(ctx, g.PoolName, v); err != nil && !errors.Is(err, ErrNotFound) {
			g.log.Warn("deleting orphan volume", "volume", v, "err", err)
			continue
		}
		g.log.Info("deleted orphan volume", "volume", v)
	}
	return nil
}

func (g *InstanceGroup) Increase(ctx context.Context, n int) (int, error) {
	var errs []error
	created := 0
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := g.createOne(ctx); err != nil {
			errs = append(errs, err)
			continue
		}
		created++
	}
	return created, errors.Join(errs...)
}

func (g *InstanceGroup) createOne(ctx context.Context) error {
	name, err := g.newName()
	if err != nil {
		return err
	}
	g.track(name)
	defer g.untrack(name)

	if g.LockFile != "" {
		unlock, err := acquireLock(ctx, g.LockFile)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if g.AdmitCommand != "" {
		if err := g.admit(ctx, name); err != nil {
			return err
		}
	}

	base, err := g.client.BaseVolume(ctx, g.PoolName, g.BaseVolumeName)
	if err != nil {
		return fmt.Errorf("base volume: %w", err)
	}
	capacity := max(base.CapacityBytes, uint64(g.DiskSizeGiB)<<30)
	volXML, err := volumeXML(name, base, capacity)
	if err != nil {
		return err
	}
	path, err := g.client.CreateVolume(ctx, g.PoolName, name, volXML)
	if err != nil {
		return fmt.Errorf("creating volume %s: %w", name, err)
	}

	spec := domainSpec{
		Name: name, Title: g.Title, Prefix: g.NamePrefix, Owner: g.Owner,
		Partition: g.ResourcePartition, Network: g.NetworkName, DiskPath: path,
		VCPU: g.VCPUCount, MemoryMiB: g.MemoryMiB,
	}
	if g.authorized != "" {
		spec.CredentialKey, spec.AuthorizedKey = g.SSHCredentialName, g.authorized
	}
	domXML, err := domainXML(spec)
	if err == nil {
		err = g.client.CreateDomain(ctx, domXML)
	}
	if err != nil {
		cleanup := context.WithoutCancel(ctx)
		if derr := g.client.DeleteVolume(cleanup, g.PoolName, name); derr != nil && !errors.Is(derr, ErrNotFound) {
			g.log.Warn("removing volume after failed create", "volume", name, "error", derr)
		}
		return fmt.Errorf("starting domain %s: %w", name, err)
	}
	g.log.Info("instance created", "name", name)
	return nil
}

// admit runs admit_command; a non-zero exit refuses this instance. It runs
// under the shared lock, so it is bounded and its whole process group is
// killed at the deadline.
func (g *InstanceGroup) admit(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, admitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", g.AdmitCommand)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Env = append(os.Environ(),
		"FLEETING_LIBVIRT_NAME="+name,
		"FLEETING_LIBVIRT_MEMORY_MIB="+strconv.Itoa(g.MemoryMiB),
		"FLEETING_LIBVIRT_VCPU_COUNT="+strconv.Itoa(g.VCPUCount),
	)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	if ctx.Err() != nil {
		return fmt.Errorf("admission for %s: admit_command did not finish in %s and was killed: %w", name, admitTimeout, ctx.Err())
	}
	return fmt.Errorf("admission refused for %s: admit_command: %w (%s)", name, err, msg)
}

func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	var done []string
	var errs []error
	for _, name := range instances {
		if !g.ownsName(name) {
			errs = append(errs, fmt.Errorf("refusing to delete %q: not created by owner %q", name, g.Owner))
			continue
		}
		if err := g.client.DestroyDomain(ctx, name); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, fmt.Errorf("destroying domain %s: %w", name, err))
			continue
		}
		if err := g.client.DeleteVolume(ctx, g.PoolName, name); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, fmt.Errorf("deleting volume %s: %w", name, err))
			continue
		}
		done = append(done, name)
	}
	return done, errors.Join(errs...)
}

func (g *InstanceGroup) ConnectInfo(ctx context.Context, instance string) (provider.ConnectInfo, error) {
	info := provider.ConnectInfo{ConnectorConfig: g.settings.ConnectorConfig, ID: instance}
	if !g.settings.UseStaticCredentials {
		info.Key = g.privatePEM
	}
	addr, err := g.waitAddress(ctx, instance)
	if err != nil {
		return info, err
	}
	info.InternalAddr, info.ExternalAddr = addr, addr
	return info, nil
}

func (g *InstanceGroup) waitAddress(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, connectWait)
	defer cancel()
	var last error
	for {
		addrs, err := g.client.GuestAddresses(ctx, name)
		if errors.Is(err, ErrNotFound) {
			return "", err
		}
		if err == nil {
			if a := pickAddress(addrs); a != "" {
				return a, nil
			}
			err = errors.New("agent reported no usable address")
		}
		last = err
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("no address for %s from the guest agent: %w (last: %v); check that qemu-guest-agent runs in the base image and the guest has a lease", name, ctx.Err(), last)
		case <-time.After(pollInterval):
		}
	}
}

// pickAddress prefers a routable IPv4 address over IPv6 and ignores loopback
// and link-local ones.
func pickAddress(addrs []string) string {
	var v6 string
	for _, s := range addrs {
		a, err := netip.ParseAddr(s)
		if err != nil || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
			continue
		}
		if a.Is4() {
			return a.String()
		}
		if v6 == "" {
			v6 = a.String()
		}
	}
	return v6
}

func (g *InstanceGroup) Heartbeat(ctx context.Context, instance string) error {
	running, err := g.client.DomainRunning(ctx, instance)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrInstanceUnhealthy, err)
	}
	if !running {
		return fmt.Errorf("%w: domain %s is not running", provider.ErrInstanceUnhealthy, instance)
	}
	addrs, err := g.client.GuestAddresses(ctx, instance)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrInstanceUnhealthy, err)
	}
	addr := pickAddress(addrs)
	if addr == "" {
		return fmt.Errorf("%w: no address for %s", provider.ErrInstanceUnhealthy, instance)
	}
	port := g.settings.ProtocolPort
	if port == 0 {
		port = 22
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := dialContext(dctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrInstanceUnhealthy, err)
	}
	return conn.Close()
}

func (g *InstanceGroup) Suspend(context.Context, []string) ([]string, error) {
	return nil, provider.ErrSuspendResumeNotSupported
}

func (g *InstanceGroup) Resume(context.Context, []string) ([]string, error) {
	return nil, provider.ErrSuspendResumeNotSupported
}

// Shutdown only closes the libvirt connection: deleting is Decrease's job.
func (g *InstanceGroup) Shutdown(context.Context) error {
	if g.client == nil {
		return nil
	}
	return g.client.Close()
}

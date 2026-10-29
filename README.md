![hero](hero.svg)

# fleeting-plugin-libvirt

[![pipeline](https://gitlab.com/dunn.dev/fleeting-plugin-libvirt/badges/main/pipeline.svg)](https://gitlab.com/dunn.dev/fleeting-plugin-libvirt/-/pipelines)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)
![built with GitLab](https://img.shields.io/badge/built_with-GitLab-FC6D26?logo=gitlab)

A [fleeting](https://gitlab.com/gitlab-org/fleeting/fleeting) plugin that runs each instance as a libvirt VM cloned from one base volume.

Each instance is a qcow2 overlay on `base_volume_name` plus a transient domain
that libvirt removes when it stops. The plugin talks to libvirt over its socket
in pure Go, so the binary is static and needs no libvirt headers. It deletes
only what it created for its `owner`, and deletes nothing at Init or Shutdown,
so a restart or a second process with its own `name_prefix` (or `owner`) on
the same libvirt never destroys a running instance. No hardware is needed to build or test it.

## Install

Pull a release from this project's container registry with
[fleeting-artifact](https://gitlab.com/gitlab-org/fleeting/fleeting-artifact),
or build `./cmd/fleeting-plugin-libvirt` and put the binary on `PATH`.

```
fleeting-artifact install registry.gitlab.com/dunn.dev/fleeting-plugin-libvirt:0.1
```

Set `FLEETING_PLUGIN_PATH` to the same directory when you install and when you run, or set `plugin` to a binary path.

## Use with GitLab Runner

```toml
[runners.autoscaler]
  plugin = "registry.gitlab.com/dunn.dev/fleeting-plugin-libvirt:0.1"

  [runners.autoscaler.plugin_config]
    pool_name = "images"
    base_volume_name = "golden.qcow2"
    name_prefix = "ci-"

  [runners.autoscaler.connector_config]
    username = "root"
```

## Use with forgehand

```toml
[[scalesets]]
name = "example"
url = "https://github.com/owner/repo"
labels = ["self-hosted", "example"]

[scalesets.github_app]
client_id = "Iv1.0000000000000000"
installation_id = 12345678
private_key_path = "/run/credentials/forgehand.service/app.pem"

[scalesets.runner]
dir = "/opt/actions-runner"

[scalesets.autoscaler]
plugin = "registry.gitlab.com/dunn.dev/fleeting-plugin-libvirt:0.1"
max_instances = 4

[scalesets.autoscaler.plugin_config]
pool_name = "images"
base_volume_name = "golden.qcow2"
name_prefix = "forgehand-"

[scalesets.autoscaler.connector_config]
protocol = "ssh"
username = "runner"
```

`name_prefix` must differ from any gitlab-runner's on the same host, and forgehand does not set `owner`. The unit's user needs read and write access to the libvirt socket.

## Configuration

An unknown key fails Init and names the key.

| Key | Default | Meaning |
|---|---|---|
| `uri` | `qemu:///system` | libvirt URI |
| `pool_name` | required | storage pool of the base volume and the overlays |
| `base_volume_name` | required | volume that instances clone |
| `name_prefix` | required | must end in `-`; every domain and volume is named `<prefix><owner tag>-<id>` |
| `max_size` | unbounded | upper bound on instances; the scaler's own cap applies either way |
| `owner` | `name_prefix` | scopes ownership with the prefix; set it per daemon sharing a prefix, or each deletes the other's VMs |
| `vcpu_count` | 2 | vCPUs per instance |
| `memory_mib` | 4096 | memory per instance |
| `disk_size_gib` | base size | overlay size, never below the base volume |
| `network_name` | `default` | libvirt network for the one interface |
| `title` | none | domain `<title>`, one line |
| `resource_partition` | none | cgroup partition such as `/machine/ci`; it must exist |
| `lock_file` | none | held across admission, volume creation and start of each instance; waiting longer than five minutes for it refuses that Increase. Use a directory that is not sticky and world-writable (for example under `/run`), shared by every account that runs the plugin |
| `admit_command` | none | run with `/bin/sh -c` under the lock; non-zero refuses that instance, and it is killed with its children after two minutes. Requires `lock_file` |

`admit_command` sees `FLEETING_LIBVIRT_NAME`, `FLEETING_LIBVIRT_MEMORY_MIB` and
`FLEETING_LIBVIRT_VCPU_COUNT`. A refusal stops Increase; the instances already
started are returned without an error, and fleeting asks again later.

## How it behaves

- **Ownership.** Domains are listed by `name_prefix` and
  owned by metadata: among those, a domain is ours when its metadata names our
  prefix and `owner`; Update, Decrease and orphan reaping all use that
  rule. Volumes carry no metadata, so they match by the owner tag in their
  name: Decrease deletes a volume only under a minted name, and with no domain
  it removes only minted names. Update reports only our domains, all as
  running; a stopped or paused domain is removed only when the caller runs
  Heartbeat (forgehand with a heartbeat function, or the runner with
  `FF_USE_FLEETING_ACQUIRE_HEARTBEATS`), which fails and leads to Decrease.
  Update also deletes our volumes that have no domain and no create in
  flight, and never reports them. Two processes
  stay out of each other's way only with distinct owners.
- **Keys.** With `use_static_credentials` false, one ed25519 key is generated
  at Init; its public half is seeded into each guest as an SMBIOS credential and
  the private half is returned in ConnectInfo. The key is per plugin process
  and is seeded as `ssh.authorized_keys.<connector_config.username>` (`root`
  when empty). With it true nothing is seeded
  and `connector_config` passes through, so the base image must trust that key.
- **Guest.** The base image needs the QEMU guest agent, which supplies the
  address, and systemd to import the credential. systemd's stock
  `provision.conf` imports only `ssh.authorized_keys.root`, so a non-root SSH
  account needs a oneshot unit that imports its own credential and writes the
  file before sshd starts (shown for `username = "runner"`):

  ```ini
  [Unit]
  DefaultDependencies=no
  After=local-fs.target
  Before=ssh.socket sshd.service

  [Service]
  Type=oneshot
  RemainAfterExit=yes
  ImportCredential=ssh.authorized_keys.runner
  ExecStart=/bin/sh -c 'install -d -m 700 -o runner -g runner /home/runner/.ssh && install -m 600 -o runner -g runner "$CREDENTIALS_DIRECTORY/ssh.authorized_keys.runner" /home/runner/.ssh/authorized_keys'

  [Install]
  WantedBy=multi-user.target
  ```

- **Address and health.** ConnectInfo waits up to two minutes for the agent's
  address on the interface carrying the domain's MAC (IPv4 first, then global
  IPv6; never loopback or link-local), so bridges and container networks in
  the guest are ignored. It then waits within the same two minutes for the SSH
  port (`protocol_port`) to accept connections. Heartbeat needs a running domain
  and an open SSH port.
- **Connection.** The libvirt connection is redialed on the next call after
  libvirtd drops it.
- **Removal.** Decrease destroys the domain and deletes the volume, and treats
  missing ones as done.

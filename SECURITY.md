# Security

fleeting-plugin-libvirt creates and destroys virtual machines and their disks
through the libvirt API, and seeds an SSH public key into each guest. This
document says how to report a problem and what is in scope.

## Reporting a vulnerability

Email **andrew@dunn.dev** with the subject line
`[SECURITY] fleeting-plugin-libvirt`. Include:

- The version affected (commit SHA or tag).
- A description of the issue and its impact.
- Steps to reproduce, where applicable.
- Whether you intend to disclose publicly, and any timeline you prefer.

I will acknowledge receipt within seven days, then aim to ship a fix or agree a
disclosure date within thirty days. Please do not open a public issue first.

## In scope

- Destroying or deleting a domain or volume that the plugin did not create under
  its `name_prefix`.
- Leaking the generated private key through logs, errors or the domain XML.
- Starting a VM without taking `lock_file` or running `admit_command` when they
  are configured.
- Command execution beyond `admit_command` as the operator wrote it.

## Out of scope

- Vulnerabilities in libvirt, QEMU, fleeting or GitLab Runner. Report those to
  their maintainers.
- Issues that need an attacker who already controls the host, the libvirt
  socket or the plugin configuration.
- Theoretical issues without a concrete attack path.

## Guest network

Guest SSH host keys are not verified, because fleeting's connector ignores
them. The guest network must stop guests spoofing each other's IP or MAC: use
VF spoofchk plus port isolation.

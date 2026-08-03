# Rendered by code-vm from ~/.config/code-vm/config.yaml. Do not edit by hand:
# `code-vm` regenerates this file on every start.
minimumLimaVersion: 2.2.0

# Only the image is inherited. The upstream default mounts section is
# deliberately NOT inherited: it mounts the host $HOME read-only, which would
# expose ~/.ssh and ~/.aws.
base:
- template:_images/ubuntu-lts

cpus: {{.Config.CPUs}}
memory: "{{.Config.Memory}}"
disk: "{{.Config.Disk}}"

# containerd is not used; Docker is installed by provisioning instead.
containerd:
  system: false
  user: false

# Lima's own guest user. Privileged, used only by code-vm for session setup.
# The agent runs as {{.AgentUser}} (UID {{.AgentUID}}), created by provisioning
# with no sudo. UID 60000 avoids colliding with the host UID.
user:
  name: limaadmin
  uid: 60000
  comment: "code-vm admin"
  passwordlessSudo: true

ssh:
  # Do not copy host SSH public keys into the guest.
  loadDotSSHPubKeys: false

mounts:
{{- range .Config.Mounts}}
- location: "{{.}}"
  mountPoint: "{{.}}"
  writable: true
{{- end}}
# virtiofs preserves host UIDs, which is what makes the agent's UID match the
# host user's and keeps workspace files host-owned. An SSH-based reverse mount
# would map ownership to the SSH user and break that.
mountType: virtiofs

# No automatic port forwarding: nothing in the guest needs to be reachable
# from the host, and the Docker socket is deliberately not exposed.
# guestIP 0.0.0.0 (with guestIPMustBeZero left false) matches every bind
# address and proto:any covers UDP — a bare guestPortRange ignore was observed
# to still forward wildcard-bound TCP ports like Squid's 3128.
portForwards:
- guestIP: "0.0.0.0"
  guestPortRange: [1, 65535]
  proto: any
  ignore: true

hostResolver:
  hosts: {}

provision:
{{- range .DataFiles}}
- mode: data
  path: "{{.Path}}"
  owner: "root:root"
  permissions: "{{.Permissions}}"
  overwrite: true
  content: |
{{indent 4 (escapeData .Content)}}
{{- end}}
- mode: system
  script: |
    #!/bin/bash
    set -euo pipefail
    exec /usr/local/lib/sandbox/provision-system.sh

probes:
# sandbox-boot.sh writes /run/sandbox-boot-complete as its final act, and only
# reaches it if the firewall verified (init-firewall.sh exits non-zero otherwise)
# and the agent CLI update has finished. Waiting on that file — rather than on
# the firewall's own verify file — is what makes `limactl start` return only when
# the VM is genuinely usable: no unfiltered egress, and no CLI still downloading.
# The failure marker turns a broken boot into an immediate failure rather than a
# silent five-minute timeout.
- mode: readiness
  description: sandbox boot sequence to finish
  script: |
    #!/bin/bash
    set -eu
    timeout 300s bash -c 'until [ -f /run/sandbox-boot-complete ]; do
      if [ -f /run/sandbox-boot-failed ]; then
        echo "the sandbox boot sequence failed; the firewall is not up" >&2
        exit 1
      fi
      sleep 2
    done'
  hint: |
    The sandbox boot sequence did not finish, so the egress firewall is not
    verified and the VM is deliberately not reported ready. Inspect it with:
      limactl shell {{.Config.Instance}} sudo journalctl -u sandbox-boot.service

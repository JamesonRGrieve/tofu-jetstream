<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# terraform-provider-jetstream

A native OpenTofu/Terraform provider for **TP-Link JetStream smart switches**
(e.g. the **TL-SG2008**) — driven over **TELNET**. It manages the switch's
IOS-style CLI configuration generically as **running-config blocks**.

## Why telnet

The TL-SG2008 (SW 3.0.1, IPSSH-6.6.0) offers no usable management transport
except its telnet CLI:

- **SSH** presents only a legacy `ssh-dss` host key, which OpenSSH 10 and modern
  Go `x/crypto/ssh` have removed — SSH cannot connect without re-enabling a
  deprecated algorithm.
- **HTTP/REST** — no documented REST API on this firmware.
- **Telnet (port 23)** — the full IOS-style CLI (`show running-config`,
  `configure`, `interface …`, `vlan …`, `copy running-config startup-config`).
  `show running-config` is an exact, structured read of any config block, which
  is exactly what a manage-declared-only subset model needs to import to 0-diff.

Go has no telnet in the standard library, so the provider implements the minimum
itself (no extra Go dependencies): NVT IAC option negotiation (it refuses every
option), `User:`/`Password:` login + `enable`, "Press any key to continue"
paging, and command/prompt framing. A single telnet session is serialized with a
mutex (the switch permits few concurrent sessions) and the running-config is
cached, invalidated on every write.

## Resources

### `jetstream_object` (resource)

Manages a declared set of config lines within a running-config **block**
(context). CRUD + `ImportState`.

```hcl
# A VLAN definition.
resource "jetstream_object" "vlan_telus" {
  context = "vlan 2"
  lines   = jsonencode(["name \"TELUS_WAN\""])
}

# A tagged trunk port.
resource "jetstream_object" "port6" {
  context = "interface gigabitEthernet 1/0/6"
  lines   = jsonencode(["switchport general allowed vlan 2-3 tagged"])
}

# A switched-virtual-interface (SVI).
resource "jetstream_object" "svi1" {
  context = "interface vlan 1"
  lines = jsonencode([
    "ip address 192.168.1.253 255.255.255.0",
    "ipv6 enable",
  ])
}
```

On **create/update**: the declared lines are entered under `context` in
`configure` mode, then `copy running-config startup-config` saves. On **destroy**:
each declared line this resource *added* (absent at create/import) is negated
with `no …`, then saved — pre-existing lines are left untouched.

**Manage-declared-only / 0-diff imports.** `lines` declares *only* the lines you
manage. A plan modifier suppresses the diff when every declared line already
appears in the block on the device, so:

- importing an existing config (`tofu import` / `import {}`) lands at **0-diff**
  with no apply against the switch, and
- every other line in the block (and the rest of the config) is left alone.

| Attribute | | Meaning |
|-----------|---|---------|
| `context` | optional | The running-config block to manage (`vlan 2`, `interface vlan 1`, `interface gigabitEthernet 1/0/6`); omit for global config |
| `lines` | required | JSON array of the managed config lines within the block |
| `previous` | computed | snapshot of the block's lines at create/import, used to restore on destroy |
| `id` | computed | the normalized `context` (or `(global)`) |

**Import id** is the context verbatim (or `(global)`):

```sh
tofu import jetstream_object.port6 'interface gigabitEthernet 1/0/6'
```

### `jetstream_reconcile` (resource)

Re-applies a list of config-mode `commands` (then saves) unconditionally per
`triggers` change — to heal config-vs-live drift Terraform cannot detect.

```hcl
resource "jetstream_reconcile" "save" {
  commands = ["interface gigabitEthernet 1/0/6", "switchport general allowed vlan 2-3 tagged"]
  triggers = { always = timestamp() }
}
```

### `jetstream_object` (data source)

```hcl
data "jetstream_object" "p6"  { context = "interface gigabitEthernet 1/0/6" }  # .lines + .present
data "jetstream_object" "all" {}                                               # .all = full show running-config
```

## Provider configuration

```hcl
terraform {
  required_providers {
    jetstream = { source = "registry.terraform.io/jamesonrgrieve/jetstream" }
  }
}

provider "jetstream" {
  host        = "192.168.2.182"   # host or host:port, no scheme (default telnet port 23)
  username    = "admin"           # optional, default admin
  password    = var.switch_password
  telnet_port = 23                # optional, default 23
}
```

Inject `password` from the secret store (e.g. an ephemeral OpenBao read) — never
hard-code it.

## Local build / dev install

```sh
make build          # -> terraform-provider-jetstream
make install        # installs to $DEV_BIN_DIR for a dev_overrides .tfrc
make check          # tidy + fmt + vet + test + build (pre-commit / CI gate)
```

For runners without registry access, install into a filesystem mirror:
`<plugins>/registry.terraform.io/jamesonrgrieve/jetstream/<ver>/<os>_<arch>/terraform-provider-jetstream_v<ver>`
and point a `.terraformrc` `provider_installation { filesystem_mirror {...} }` at it.

## License

AGPL-3.0-or-later.

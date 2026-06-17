# jetstream — Agent Operating Guide

Native OpenTofu/Terraform provider for **TP-Link JetStream smart switches**
(e.g. the **TL-SG2008**) driven over **TELNET**. Sibling of `../tofu-aruba-aos`
(the in-house managed-switch provider) and `../tofu-ddwrt` / `../tofu-tomato`
(in-house CLI providers — same generic-over-the-device philosophy, same
toolchain). The workspace-root `../CLAUDE.md` and `/home/jameson/source/ai-prompts/go.md`
apply; this adds specifics.

## What this is / isn't

- **Is:** a provider for standalone (non-Omada) JetStream smart switches running
  the IOS-style CLI (`show running-config`, `configure`, `interface …`, `vlan …`,
  `copy running-config startup-config`), managing **running-config blocks**
  generically over telnet.
- **Isn't:** an Omada-controller provider (that's `net/mgmt/omada`), nor an
  ArubaOS-Switch provider (that's `../tofu-aruba-aos`, REST). The switch here is
  in **standalone** mode (`no controller cloud-based`).

## Transport — telnet, and why (decision record)

Three transports were considered for the TL-SG2008 (SW 3.0.1):

- **SSH:** the switch (IPSSH-6.6.0) presents only a legacy `ssh-dss` host key.
  OpenSSH 10 and modern Go `x/crypto/ssh` have **removed** ssh-dss, so SSH cannot
  connect without re-enabling a deprecated, insecure algorithm. Rejected.
- **HTTP/REST:** no documented REST API on this firmware. Rejected.
- **Telnet (port 23):** the full IOS-style CLI is available. `show
  running-config` is an exact, structured read of **any** config block —
  precisely what the manage-declared-only subset model needs to compute a 0-diff
  on import. **Chosen.**

Go has no telnet in the standard library, so `internal/jetstream/client.go`
implements the minimum itself (no new Go deps): NVT **IAC** option negotiation
(we refuse every option — DO→WONT, WILL→DONT), `User:`/`Password:` login +
`enable` (no enable password on this switch), `Press any key to continue` paging
(answered with a space), and command/prompt framing. The protocol plumbing is
factored into **pure functions** (`processTelnet`, `parseRunningConfig`,
`endsWithPrompt`, `cleanOutput`, `applyCommands`/`removeCommands`,
`NormalizeLine`) so it is unit-tested without a live switch; the networked
session layer is a thin shell over them.

The single telnet session is **mutex-serialized** (the switch permits very few
concurrent telnet sessions), established lazily, healed on error, and the
running-config is **cached and invalidated on every write**.

## Resource model — generic config blocks (decision record)

The house style (matching `arubaos_object` / `ddwrt_nvram`) is **generic over
the device, not typed-per-feature**. The generic resource here is
**`jetstream_object`**, keyed by a running-config **context** (block):

- `context` — the block to manage: `vlan 2`, `interface vlan 1`,
  `interface gigabitEthernet 1/0/6`; empty for the global config context.
- `lines` — a JSON array of the config lines this resource manages within that
  block (manage-declared-only).
- The subset plan modifier `lineSubsetMatches` suppresses the diff when every
  declared line already appears in the live block, so an existing config imports
  to **0-diff** and unmanaged lines are never clobbered.
- `previous` snapshots the block at create/import; **destroy negates only the
  lines this resource added** (lines that pre-existed are left — adoption-safe).
- Lines compare **normalization-insensitively** (whitespace collapsed, case and
  quoting preserved), so the device's rendering and the declared line match.

This one resource covers the entire CLI grammar (VLANs, SVIs, port VLAN
membership/PVID, global commands) by construction. Typed sugar
(`jetstream_vlan`/`_port`/`_svi`) is a future ergonomics option layered on top,
never the only path.

`jetstream_reconcile` re-applies a declared list of config-mode `commands` (then
saves) unconditionally per `triggers` change — to heal config-vs-live drift
Terraform cannot detect (out-of-band edits, a startup-config that diverged from
running). Mirrors the `*_reconcile` resource on the sibling router providers.

## CLI grammar rendered

`vlan N` / `name "X"`; `interface vlan N` / `ip address A.B.C.D MASK` |
`ip address-alloc dhcp` / `description "X"` / `[no] ipv6 enable`;
`interface gigabitEthernet 1/0/X` / `switchport general allowed vlan <range>
tagged|untagged` / `switchport pvid N`. Save: `copy running-config
startup-config`. Read: `show running-config` (paged).

## Toolchain

General Go/provider standards: see `/home/jameson/source/ai-prompts/go.md`.

- Go 1.26.4 (`/home/jameson/.local/go`), `terraform-plugin-framework` v1.19.0.
- **No new Go deps** — the telnet client is pure stdlib (`net`), so `go.mod`
  matches the sibling providers byte-for-byte.
- Provider address: `registry.terraform.io/jamesonrgrieve/jetstream`. Binary:
  `terraform-provider-jetstream`. Single-token TypeName `jetstream` so resources
  are `jetstream_object` / `jetstream_reconcile` (the repo carries the `tofu-`
  prefix).
- `make check` = tidy + fmt + vet + test + build; `.githooks/pre-commit` re-runs
  the gate. Never `--no-verify`.

## Hard rules

- **No secrets in the repo.** The telnet password comes from the provider config
  (OpenBao → `TF_VAR_*` via Semaphore), never hard-coded.
- **The target is a SHARED/production switch at the shop** (DIR uplinks on ports
  6/7; WAN VLANs). It is reachable only from shop-opnsense. Build + unit-test
  locally; READ-ONLY telnet characterization is fine, but **any config write goes
  through the Tofu/Semaphore pipeline** — plan-first, 0-diff, additive only.
- Drive any live changes via Semaphore, plan-first, in a change window.

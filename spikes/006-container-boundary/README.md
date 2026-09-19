# Spike 006: Container boundary — does deny-by-default actually deny?

**Question (spike #2):** Does the design doc's default namespace state (`--network none --cap-drop ALL --read-only --user nobody`, no socket, narrow workspace mount) actually produce the denials it claims — or is it theater?

**Design:** Run the exact flags against alpine on a real Linux host (a throwaway Ubuntu 24.04 VM, docker 29.1.3). Eight probes, each testing one claim from the doc's "Default namespace state" section. Every probe run fresh (`--rm`), no reuse.

## Results

| # | Claim | Flag(s) | Result |
|---|---|---|---|
| 1 | No egress | `--network none` | **DENIED** — `Network unreachable` (both wget and ping) |
| 2 | No capabilities | `--cap-drop ALL` | **DENIED** — `CapEff: 0000000000000000`. (Loopback ping still works — loopback isn't a capability; correct behavior.) |
| 3 | Immutable root fs | `--read-only` | **DENIED** — `Read-only file system` |
| 4 | Non-root, can't touch system files | `--user nobody` | **DENIED** — `uid=65534`, `/etc/passwd` write rejected |
| 5 | No runtime socket | (default, socket not mounted) | **DENIED** — `/var/run/docker.sock: No such file` |
| 6 | Narrow `:rw` workspace mount works | `-v /tmp/ws:/workspace:rw` | **Works — but gated on host-dir ownership** (see finding) |
| 7 | Host fs not reachable via `/proc` | `--cap-drop ALL --user nobody` | **ISOLATED** — `/proc/1/root` inside the container is the *container's* root, not the host's. Host `aj` user absent (`grep -c aj` = 0 inside, 1 on host). |
| 8 | (follow-up to 7) `/proc/1/root` contents | — | Container root, `cat /proc/1/comm` = container init, not host `systemd`. |

## Findings

### 1. The five denial claims all hold. The container boundary is real, not theater.

`--network none` + `--cap-drop ALL` + `--read-only` + `--user nobody` + no socket mount produces exactly the blast-radius containment the doc claims. A dumb or hijacked agent inside this namespace can touch only its own root (read-only), loopback, and whatever mount is explicitly granted.

### 2. LOAD-BEARING: the `:rw` mount works under `--read-only` + `--user nobody`, but grant success is gated on **host-side directory ownership**, not the Docker flags.

The initial probe of the `:rw` mount *failed* with `Permission denied` — and the cause was **not** any of the deny-by-default flags. It was the host workspace dir being owned by a non-`nobody` user at mode `775`, which `nobody` (uid 65534) cannot write. After `chmod 777`, the exact same `--read-only --user nobody -v :rw` combo wrote fine.

**Why this matters for Gaap:** `--read-only` applies to the container root fs only — it does not affect a `:rw` bind-mount. So when the governor grants a path, the grant *silently fails or succeeds based on host directory ownership*, independent of the docker flags. If the host dir isn't writable by the container's uid, the agent gets `Permission denied`, misreads it as "I need more access," and re-requests — when the real problem is host-side perms misconfiguration. The grant path must own the host-side chown/chmod of the workspace dir (to the agent's uid) as part of granting, or every path grant is a coin-flip.

### 3. `/proc/1/root` is the container's root, not the host's — the isolation is genuine (corrected an initial mislabel).

First probe of `/proc/1/root/etc/passwd` looked like host reachability. It is not: PID 1 inside the container is the container's init, so `/proc/1/root` is the container root. Confirmed by `cat /proc/1/comm` = `cat` (container) vs `systemd` (host), and `grep -c aj` = 0 inside vs 1 on host. No host filesystem reachability via `/proc`.

## Verdict: CONFIRMED, with one actionable gap

The default namespace state in the design doc is correct as written. All five denial claims hold on a real Linux host with docker. The one thing the doc does **not** say — and must — is that **path grants require the governor to fix host-side directory ownership as part of the grant**, because a `:rw` bind-mount under `--user nobody` writes or fails purely on host dir perms, not on any docker flag.

## Recommendation for the plan

1. The default namespace flags are settled — ship them as written.
2. Add to the grant taxonomy: a **Path** grant = bind-mount *plus* `chown <agent-uid> / chmod` on the host dir, performed by the governor (which runs as root/the sovereign's account) before the mount is live. Without this, path grants are non-deterministic.
3. Consider a **per-agent uid** (not shared `nobody`) so two agents' workspace mounts don't collide and per-agent grants are isolatable. `nobody` is fine for a single agent; breaks the moment there are two.

## Environment

- Host: a throwaway Ubuntu 24.04 LTS VM (kernel 6.8.0-139, docker 29.1.3), 8 vCPU / 16 GB, bridged network.
- VM built via cloud image + cloud-init seed (FAT `cidata` as **virtio disk**, not cdrom — see build notes).
- Probe image: `alpine:latest`.

## Build notes (the VM itself, for reproducibility)

- Cloud image: `ubuntu-24.04-server-cloudimg-amd64.img` (checksum `612b2c0c...`), overlay `gaap-dev.qcow2` 120G sparse.
- **Cloud-init seed MUST be a virtio disk, not a SATA cdrom** — cdrom-attached `cidata` is invisible to cloud-init's initramfs scan and yields `DataSourceNone` (first attempt failed exactly this way; fixed by switching `device='disk' bus='virtio'`).
- Seed built with `mkfs.vfat -n cidata` + `mcopy` (no `cloud-localds`/`genisoimage` on the Unraid host used).
- A non-root `aj` user with passwordless sudo + docker group, authorized via an SSH public key (not password).
- NVRAM file copied from `/usr/share/qemu/ovmf-x64/OVMF_VARS-pure-efi.fd` (the Unraid host has no `/etc/libvirt/qemu/nvram/` auto-provision).

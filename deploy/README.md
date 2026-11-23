# Deploying gaapd

The broker is a single process with a unix socket and a state directory. One job, no
dependencies: no database, no network, no tool execution.

## Where the state lives, and why not `/var/lib/gaap`

`/var/lib/gaap` is the dedicated dockerd data root on a host that runs containerized agents
(`platform.DockerEnforcer`; `/var/lib/gaap/ws/<agent-id>` is an agent's workspace). A broker
whose policy file sat there would let the governed workloads rewrite the rules that judge them,
so:

| thing | path | account |
|---|---|---|
| workload container runtime | `/var/lib/gaap`, `/run/gaap.sock` | `gaap` |
| **decision broker** | `/var/lib/gaapd`, `/run/gaapd/govern.sock` | `gaapd` |

`gaapd` refuses to start if `-state` resolves inside `/var/lib/gaap`.

## What lives in the state directory

| file | what | if it goes missing |
|---|---|---|
| `policy.json` | the standing orders | the daemon refuses to start (exit 78) |
| `memory-grants.json` | `uid → agents`, the identity mapping | refuses to start: with no principals it cannot tell one agent from another |
| `ledger.jsonl` | append-only, hash-chained, fsynced per decision | created |
| `keys.json`, `refs.json` | keyed commitments and opaque subject handles | created — **losing `keys.json` makes existing rows unverifiable against their commitments** |
| `kill.json` | the fleet stop | created |
| `denials.json` | the sustained-refusal counter | created |

Back up `keys.json` with the ledger or neither is worth much.

## systemd (Linux)

```sh
sudo install -m 0755 gaapd /usr/local/bin/gaapd
sudo install -m 0644 deploy/systemd/gaapd.service /etc/systemd/system/
sudo useradd --system --no-create-home --shell /usr/sbin/nologin gaapd
sudo systemctl daemon-reload && sudo systemctl enable --now gaapd
```

`StateDirectory=gaapd` creates `/var/lib/gaapd` owned by `gaapd` at 0700; put `policy.json` and
`memory-grants.json` there, readable by that account. The socket is `/run/gaapd/govern.sock` at
0660 `gaap:gaap`, so a client must be in the `gaap` group.

Verify:

```sh
systemctl is-active gaapd
journalctl -u gaapd -n 5 --no-pager
python3 - <<'PY'
import json, socket
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); s.connect('/run/gaapd/govern.sock')
s.sendall(json.dumps({"op": "status"}).encode()); s.shutdown(socket.SHUT_WR)
print(s.recv(65536).decode())
PY
```

`Restart=on-failure` with `RestartPreventExitStatus=78`: a configuration error stops the unit
instead of looping. Stopping the unit is a real stop — agents that use it then refuse their
tool calls, which is the designed failure mode, not a bug to route around.

## macOS (dogfood host)

A LaunchAgent plist is in `deploy/launchd/`. Absolute paths, because the plist format does not
expand `~`: replace `/Users/aj` if this is not that host.

```sh
sudo install -m 0755 gaapd /usr/local/bin/gaapd
mkdir -p ~/.gaapd && cp policy.json memory-grants.json ~/.gaapd/
cp deploy/launchd/local.gaapd.plist ~/Library/LaunchAgents/
```

Load it with `launchctl bootstrap gui/$(id -u) <plist>` and stop it with
`launchctl bootout gui/$(id -u)/<label>`. `KeepAlive.SuccessfulExit=false` means a clean exit
is final, so stopping is not quietly undone. Run those two from a normal shell, not from inside
another supervised process.

## Changing policy

There is no reload signal and no restart. The daemon re-reads the file when its `(size, mtime)`
changes, which happens before the next decision, and every response carries the `revision` it
was decided under. A file that does not parse keeps the last good policy in force and keeps
reporting the old revision — a typo cannot take the fleet down, and cannot silently install a
different policy either.

To see what is loaded: `status` returns the policy path, the revision, the principal count, and
the agents that have registered a tool catalogue.

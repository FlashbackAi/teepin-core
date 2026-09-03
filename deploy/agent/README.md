# TEEPIN home compute — node install

Turn a machine you own into a TEEPIN **home compute node** (CPU capacity).

The agent always runs **inside Linux**. What differs per device is how that
Linux gets there:

| Your device | Use | Linux comes from | GPU usable |
|---|---|---|---|
| Linux PC | `install.sh` directly | native | yes |
| Windows | `bootstrap-windows.ps1` | WSL2 | no (CPU only) |
| macOS | `bootstrap-macos.sh` | Lima VM | no (CPU only) |

Home nodes sell **CPU + memory**, so CPU-only on Windows/macOS is by design.

## 0. Get an enrollment token

In the control centre → **Nodes** → **Generate enrollment token**. Choose the
class **home** and copy the `tne_…` token — it is shown once and expires. The
class is fixed on the token: the node cannot make itself a datacenter node.

## 0.5. Get the install package (no Go, no git checkout needed)

Download and extract the latest release from
[github.com/FlashbackAi/teepin-core/releases](https://github.com/FlashbackAi/teepin-core/releases)
(tag `agent-v*`) — pick the tarball for your architecture:

```bash
curl -LO https://github.com/FlashbackAi/teepin-core/releases/latest/download/teepin-agent-linux-amd64.tar.gz
tar xzf teepin-agent-linux-amd64.tar.gz
cd teepin-agent-linux-amd64  # install.sh and the teepin-agent binary are both here
```

(`arm64` in place of `amd64` on ARM hardware — Apple-Silicon Macs via
`bootstrap-macos.sh` need this one.) `install.sh` already looks for a
`teepin-agent` binary sitting next to itself, so nothing else is needed —
this is the same install.sh referenced below, just run from inside the
extracted tarball instead of a git checkout.

## Linux

```bash
sudo bash install.sh --token tne_XXXX --control-plane https://api.teepin.com
```

Installs k3s, drops the agent, enrolls, and runs it as a systemd service.
Verify: `systemctl status teepin-agent`, or watch the control centre.

## Windows (WSL2)

In an **Administrator PowerShell**:

```powershell
.\bootstrap-windows.ps1 -Token tne_XXXX -ControlPlane https://api.teepin.com
```

Ensures WSL2 + a distro + systemd, then runs `install.sh` inside it. If WSL2
was not already present, Windows may ask for a **reboot** after the first run —
reboot and re-run. WSL2 does not auto-start at boot; the script registers a
logon task to wake it, so log in after a reboot.

## macOS (Lima)

```bash
bash bootstrap-macos.sh --token tne_XXXX --control-plane https://api.teepin.com
```

Ensures Lima + a Linux VM (Homebrew required), then runs `install.sh` inside
it. The VM is set to start on login. Apple-Silicon Macs report `arm64`; only
`arm64` workloads place there.

## What "runs" means at each stage

- **Stage 1 (shipped):** the node enrolls, connects, and shows **online**; it
  survives control-plane restarts. It cannot run workloads yet.
- **Stage 2 (this):** the node runs **CPU containers** via k3s and they get
  **metered**. CPU rates default to `$0` — a home instance bills nothing until
  an operator sets a rate (control centre → Pricing → Home compute). Public
  HTTPS access to the workload is **Stage 3** (tunnel + wildcard TLS).

## Updates

An already-enrolled node **updates itself automatically** — no per-node
manual step. Once a day (with a randomized startup delay so a fleet does not
all check at once), the agent checks
[the latest GitHub release](https://github.com/FlashbackAi/teepin-core/releases)
and, if it is genuinely newer than the running binary, downloads it,
verifies it against the release's published `SHA256SUMS`, swaps it into
place, and exits — `systemd`'s `Restart=always` brings it straight back up
running the new version. A brief (~5s) reconnect is the only visible effect,
the same kind of interruption a control-plane restart already causes.

This only applies to a binary built from a tagged release (`-ldflags
"-X main.Version=..."`, which the release workflow sets) — a `go build`
from source (`Version = "dev"`) never self-updates, so a developer running
from a checkout is never silently replaced.

To update manually instead (skip the wait, or roll back to an older
release), just re-run `install.sh` on the node — it detects the existing
enrollment and swaps the binary in place without touching k3s or the
credential (see the Notes section below on `--binary`).

## Removing a node

- Operator side: control centre → Nodes → **Disable** (stops scheduling, the
  credential stops authenticating).
- Node side: `sudo systemctl disable --now teepin-agent` and, if you like,
  `/usr/local/bin/k3s-uninstall.sh` to remove k3s.

## Notes

- `--grpc host:port` overrides the gRPC channel address (defaults to the
  control-plane host on :443).
- `--binary /path/to/teepin-agent` installs a prebuilt agent instead of
  building from source (no Go needed on the node).
- The credential lives at `/etc/teepin/agent.json` (mode 0600). It is the
  node's secret; treat it like an API key.

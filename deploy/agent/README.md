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

# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0
#
# TEEPIN home-node bootstrap for Windows.
#
# The agent runs inside Linux. On Windows that Linux is WSL2. This script
# ensures WSL2 + a distro + systemd, then runs the Linux core installer
# (install.sh) INSIDE the distro. The node is CPU-only (WSL2 GPU passthrough
# is not relied on).
#
# Run in an ADMINISTRATOR PowerShell:
#   .\bootstrap-windows.ps1 -Token <tne_...> -ControlPlane https://api.teepin.com
#
# If WSL2 was not already installed, Windows may require a REBOOT after the
# first run; re-run this script afterwards to finish.

param(
    [Parameter(Mandatory = $true)][string]$Token,
    [Parameter(Mandatory = $true)][string]$ControlPlane,
    [string]$Distro = "Ubuntu",
    [string]$Grpc = ""
)

$ErrorActionPreference = "Stop"
function Info($m) { Write-Host "[bootstrap] $m" }
function Fail($m) { Write-Error "[bootstrap] $m"; exit 1 }

# --- admin check ---------------------------------------------------------
$isAdmin = ([Security.Principal.WindowsPrincipal] `
    [Security.Principal.WindowsIdentity]::GetCurrent()
).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) { Fail "run this in an Administrator PowerShell." }

# --- 1. WSL2 + distro ----------------------------------------------------
$wsl = Get-Command wsl.exe -ErrorAction SilentlyContinue
if (-not $wsl) {
    Info "installing WSL2 with $Distro (a reboot may be required)..."
    wsl --install -d $Distro
    Info "WSL2 installed. If Windows asks you to reboot, do so and re-run this script."
    exit 0
}

# Ensure the distro exists.
$installed = (wsl --list --quiet) -join "`n"
if ($installed -notmatch [regex]::Escape($Distro)) {
    Info "installing distro $Distro..."
    wsl --install -d $Distro
    Info "distro installed. If prompted to create a UNIX user, do so, then re-run this script."
    exit 0
}

# --- 2. enable systemd in the distro ------------------------------------
# WSL2 runs the agent as a systemd service; systemd must be turned on in
# /etc/wsl.conf. Without this the service will not start.
Info "enabling systemd in $Distro..."
wsl -d $Distro -u root -- bash -c @'
set -e
if ! grep -q "systemd=true" /etc/wsl.conf 2>/dev/null; then
  printf "[boot]\nsystemd=true\n" >> /etc/wsl.conf
  echo "systemd enabled (a WSL restart is needed to take effect)"
fi
'@
Info "restarting WSL to apply systemd..."
wsl --shutdown
Start-Sleep -Seconds 3

# --- 3. run the Linux core installer inside the distro ------------------
# Copy this script's directory into the distro and run install.sh there.
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$wslPath = (wsl -d $Distro -- wslpath -a "$here").Trim()

$grpcArg = ""
if ($Grpc -ne "") { $grpcArg = "--grpc $Grpc" }

Info "running the Linux installer inside $Distro..."
wsl -d $Distro -u root -- bash -c "cd '$wslPath' && bash install.sh --token '$Token' --control-plane '$ControlPlane' $grpcArg"

Info "done. The node should appear in the control centre (Nodes) as online within a minute."
Info "check inside WSL:  wsl -d $Distro -- systemctl status teepin-agent"
Info "NOTE: WSL2 does not auto-start at boot. To keep the node online across reboots,"
Info "      this script registers a logon task; ensure you log in after a reboot, or"
Info "      run 'wsl -d $Distro -- true' to wake it."

# --- 4. survive reboot: logon task to wake WSL --------------------------
# WSL2 only runs while a distro is active. Register a Scheduled Task that
# touches the distro on logon so its systemd (and the agent) come back.
$taskName = "TeepinWakeWSL"
$action = New-ScheduledTaskAction -Execute "wsl.exe" -Argument "-d $Distro -- true"
$trigger = New-ScheduledTaskTrigger -AtLogOn
try {
    Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Force | Out-Null
    Info "registered logon task '$taskName' to keep the node online across reboots."
} catch {
    Info "could not register the logon task ($_). The node still runs now; wake it manually after a reboot."
}

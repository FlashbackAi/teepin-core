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
# Run in an ADMINISTRATOR PowerShell, first install:
#   .\bootstrap-windows.ps1 -Token <tne_...> -ControlPlane https://api.teepin.com
#
# If WSL2 was not already installed, Windows may require a REBOOT after the
# first run; re-run this script afterwards to finish.
#
# Re-run with NO ARGUMENTS to update an already-enrolled node's agent binary
# (install.sh detects the existing enrollment itself and skips straight to
# update mode) -- Token/ControlPlane are only required the first time.

param(
    [string]$Token = "",
    [string]$ControlPlane = "",
    # Distro is OPTIONAL. If omitted, the script uses an existing Ubuntu distro
    # (e.g. Ubuntu-22.04) when one is present, and only installs a fresh
    # "Ubuntu" when none exists. Passing -Distro forces a specific one.
    [string]$Distro = "",
    [string]$Grpc = "",
    # How much RAM to give WSL2's Linux (in GB). WSL2 defaults to only ~50% of
    # host RAM, which halves what this node can rent out. 0 (the default)
    # AUTO-SIZES to host RAM minus a headroom reserve for Windows. Pass a
    # number to set it explicitly, or -1 to leave WSL's memory config alone.
    [int]$MemoryGB = 0,
    # GB of host RAM to keep for Windows when auto-sizing. Windows needs
    # headroom; starving it makes the whole machine sluggish.
    [int]$WindowsReserveGB = 4
)

$ErrorActionPreference = "Stop"
function Info($m) { Write-Host "[bootstrap] $m" }
function Fail($m) { Write-Error "[bootstrap] $m"; exit 1 }

# Get-WslDistros returns the installed distro names as a clean string array.
# wsl.exe emits UTF-16LE with embedded NUL bytes; read as normal text those
# NULs corrupt every match (the false "distro did not install" failures). We
# force the console output encoding to UTF-16LE for the call, then strip stray
# NULs and blanks. This is the single most important fix in this script.
function Get-WslDistros {
    $prev = [Console]::OutputEncoding
    try {
        [Console]::OutputEncoding = [System.Text.Encoding]::Unicode
        $raw = (& wsl.exe --list --quiet) 2>$null
    } finally {
        [Console]::OutputEncoding = $prev
    }
    return @($raw | ForEach-Object { ($_ -replace "`0", "").Trim() } | Where-Object { $_ -ne "" })
}

# Get-WslRunning returns currently-running distro names, decoded the same way.
function Get-WslRunning {
    $prev = [Console]::OutputEncoding
    try {
        [Console]::OutputEncoding = [System.Text.Encoding]::Unicode
        $raw = (& wsl.exe --list --running --quiet) 2>$null
    } finally {
        [Console]::OutputEncoding = $prev
    }
    return @($raw | ForEach-Object { ($_ -replace "`0", "").Trim() } | Where-Object { $_ -ne "" })
}

# --- admin check ---------------------------------------------------------
$isAdmin = ([Security.Principal.WindowsPrincipal] `
    [Security.Principal.WindowsIdentity]::GetCurrent()
).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) { Fail "run this in an Administrator PowerShell." }

# Token/ControlPlane are only required for a first install. A re-run against
# an already-enrolled distro is an update -- install.sh detects that itself
# (existing /etc/teepin/agent.json + systemd unit) and needs neither. Whether
# THIS run is fresh or an update is not known yet at this point (it depends
# on the distro this script is about to resolve/create below), so the actual
# gate is deferred to just before the install.sh invocation.

# --- 1. WSL2 + distro ----------------------------------------------------
$wsl = Get-Command wsl.exe -ErrorAction SilentlyContinue
if (-not $wsl) {
    $target = if ($Distro) { $Distro } else { "Ubuntu" }
    Info "installing WSL2 with $target (a reboot may be required)..."
    wsl --install -d $target
    if ($LASTEXITCODE -ne 0) {
        Fail "WSL2 install did not complete (exit $LASTEXITCODE) - most often a network interruption during download. Check your connection and re-run this script."
    }
    Info "WSL2 install started. If Windows asks you to reboot, do so, then re-run this script to continue."
    exit 0
}

# Resolve which distro to use. Prefer an explicit -Distro; otherwise reuse an
# existing Ubuntu (so a machine that already set one up is not made to install
# a second, empty 'Ubuntu' - the bug that kept targeting the wrong distro).
$distros = Get-WslDistros
if (-not $Distro) {
    if ($distros -contains "Ubuntu") {
        $Distro = "Ubuntu"
    } else {
        $ubuntu = $distros | Where-Object { $_ -like "Ubuntu*" } | Select-Object -First 1
        $Distro = if ($ubuntu) { $ubuntu } else { "Ubuntu" }
    }
}
Info "using distro: $Distro"

# Install the distro only if it is genuinely absent.
if ($distros -notcontains $Distro) {
    Info "installing distro $Distro..."
    wsl --install -d $Distro
    # Verify against a freshly-decoded list (see Get-WslDistros) rather than
    # trusting wsl.exe's exit code, which it does not set reliably on a failed
    # download. If it is present now, the install worked despite any noise.
    Start-Sleep -Seconds 2
    if ((Get-WslDistros) -notcontains $Distro) {
        Fail "distro '$Distro' did not install (likely a network interruption during download). Check your connection and re-run this script."
    }
    Info "distro installed. If prompted to create a UNIX user, do so, then re-run this script."
    exit 0
}

# --- 2a. enable cgroup v2 (Windows-side kernel config) ------------------
# Modern k3s refuses to run on cgroup v1 (the kubelet exits at startup), and
# WSL2 defaults to cgroup v1. cgroup v2 is set via the WSL KERNEL command line
# in %USERPROFILE%\.wslconfig -- a WINDOWS file the Linux installer cannot
# reach, which is exactly why this must live in the PowerShell bootstrap.
#
# IMPORTANT: .wslconfig is MACHINE-WIDE. kernelCommandLine applies to EVERY
# WSL2 distro, and the restart below stops ALL of them. This block is written
# to be a careful neighbour: it backs the file up, never overwrites an existing
# kernelCommandLine, only touches cgroup settings, and confirms before the
# global restart if other distros are running. (cgroup v2 is the modern default
# that Docker Desktop's WSL2 backend already expects, so enabling it is very
# unlikely to disturb other apps -- but the script does not assume that.)
$wslConfig = Join-Path $env:USERPROFILE ".wslconfig"
$cgroupFlags = "cgroup_no_v1=all systemd.unified_cgroup_hierarchy=1"

$existing = ""
if (Test-Path $wslConfig) { $existing = Get-Content $wslConfig -Raw }

$needsRestartForCgroup = $false
if ($existing -match "systemd\.unified_cgroup_hierarchy=1") {
    Info "cgroup v2 already configured in .wslconfig (leaving it as-is)"
} elseif ($existing -match "(?m)^\s*kernelCommandLine\s*=") {
    # A kernelCommandLine already exists. Do NOT edit it -- it may carry
    # settings another tool or the user set deliberately. Tell them exactly
    # what to add and stop, rather than risk breaking their config.
    Fail @"
$wslConfig already defines a kernelCommandLine, which this script will not modify.
Append these flags to that existing line yourself, then re-run:
    $cgroupFlags
(Full line would look like: kernelCommandLine = <your existing flags> $cgroupFlags)
"@
} else {
    # Safe to add. Back up first so any mistake is trivially recoverable.
    if (Test-Path $wslConfig) {
        $backup = "$wslConfig.teepin-backup"
        Copy-Item $wslConfig $backup -Force
        Info "backed up existing .wslconfig to $backup"
    }
    Info "enabling cgroup v2 in $wslConfig (this is a machine-wide WSL setting)..."
    if ($existing -match "(?m)^\s*\[wsl2\]") {
        # [wsl2] section exists, no kernelCommandLine -- insert our line under it.
        $updated = $existing -replace "(?m)^\s*\[wsl2\]", "[wsl2]`r`nkernelCommandLine = $cgroupFlags"
        Set-Content -Path $wslConfig -Value $updated -Encoding UTF8
    } else {
        # No [wsl2] section -- append a fresh one without touching the rest.
        Add-Content -Path $wslConfig -Value "`r`n[wsl2]`r`nkernelCommandLine = $cgroupFlags"
    }
    $needsRestartForCgroup = $true
}

# --- 2a-mem. give WSL2 more RAM (Windows-side, machine-wide) -------------
# WSL2 exposes only ~50% of host RAM to Linux by default, which halves what
# this node can rent out. Set an explicit memory= in [wsl2]. Like cgroup, this
# is a WINDOWS setting the Linux installer cannot reach.
#
# -MemoryGB 0 (default): auto-size to (host RAM - WindowsReserveGB).
# -MemoryGB N: set exactly N GB.  -MemoryGB -1: leave WSL memory alone.
$needsRestartForMemory = $false
if ($MemoryGB -ge 0) {
    $targetMemGB = $MemoryGB
    if ($targetMemGB -eq 0) {
        # Auto-size from total physical RAM, keeping headroom for Windows.
        $hostBytes = (Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory
        $hostGB = [math]::Floor($hostBytes / 1GB)
        $targetMemGB = $hostGB - $WindowsReserveGB
        if ($targetMemGB -lt 2) { $targetMemGB = [math]::Max(2, $hostGB - 1) }
        Info "auto-sizing WSL memory to ${targetMemGB}GB (host ${hostGB}GB minus ${WindowsReserveGB}GB for Windows)"
    }

    # Re-read the file (the cgroup step above may have created/edited it).
    $existing = ""
    if (Test-Path $wslConfig) { $existing = Get-Content $wslConfig -Raw }
    $memLine = "memory=${targetMemGB}GB"

    if ($existing -match "(?m)^\s*memory\s*=\s*${targetMemGB}GB\s*$") {
        Info "WSL memory already set to ${targetMemGB}GB"
    } elseif ($existing -match "(?m)^\s*memory\s*=") {
        # A memory= already exists with a different value -- replace only that
        # line, leaving everything else untouched.
        if (-not (Test-Path "$wslConfig.teepin-backup")) {
            Copy-Item $wslConfig "$wslConfig.teepin-backup" -Force
        }
        $updated = $existing -replace "(?m)^\s*memory\s*=.*$", $memLine
        Set-Content -Path $wslConfig -Value $updated -Encoding UTF8
        Info "updated WSL memory to ${targetMemGB}GB"
        $needsRestartForMemory = $true
    } elseif ($existing -match "(?m)^\s*\[wsl2\]") {
        $updated = $existing -replace "(?m)^\s*\[wsl2\]", "[wsl2]`r`n$memLine"
        Set-Content -Path $wslConfig -Value $updated -Encoding UTF8
        Info "set WSL memory to ${targetMemGB}GB"
        $needsRestartForMemory = $true
    } else {
        Add-Content -Path $wslConfig -Value "`r`n[wsl2]`r`n$memLine"
        Info "set WSL memory to ${targetMemGB}GB"
        $needsRestartForMemory = $true
    }
}

# --- 2b. enable systemd in the distro -----------------------------------
# WSL2 runs the agent as a systemd service; systemd must be turned on in
# /etc/wsl.conf (a per-DISTRO file, not machine-wide). Without this the service
# will not start.
Info "enabling systemd in $Distro..."
# Single-line bash (no PowerShell here-string): here-strings are fragile about
# the closing token's placement and file encoding, and a malformed one makes
# PowerShell report a misleading brace error far from the real cause.
$enableSystemd = "grep -q 'systemd=true' /etc/wsl.conf 2>/dev/null && echo present || (printf '[boot]\nsystemd=true\n' >> /etc/wsl.conf && echo added)"
$systemdResult = (wsl -d $Distro -u root -- bash -c $enableSystemd).Trim()
$needsRestartForSystemd = ($systemdResult -eq "added")

# A restart is needed only if we actually changed something. This avoids a
# gratuitous global shutdown when re-running an already-configured machine.
if ($needsRestartForCgroup -or $needsRestartForMemory -or $needsRestartForSystemd) {
    # The shutdown is GLOBAL -- it stops every WSL distro, including Docker
    # Desktop's backend if present. Warn, and if other distros are running,
    # ask before pulling the rug out from under them.
    $running = @(Get-WslRunning | Where-Object { $_ -ne $Distro })
    if ($running.Count -gt 0) {
        Info "NOTE: 'wsl --shutdown' will also stop these running distros: $($running -join ', ')"
        $answer = Read-Host "Proceed with the WSL restart? [y/N]"
        if ($answer -notmatch '^[Yy]') {
            Fail "aborted before restart. Re-run when it is safe to restart WSL, or run 'wsl --shutdown' yourself and re-run."
        }
    }
    Info "restarting WSL to apply configuration..."
    wsl --shutdown
    Start-Sleep -Seconds 5
}

# Verify cgroup v2 actually took effect before proceeding -- if it did not,
# k3s would crash-loop later with an opaque error, so fail clearly here.
$cgroupType = (wsl -d $Distro -- stat -fc %T /sys/fs/cgroup 2>$null).Trim()
if ($cgroupType -ne "cgroup2fs") {
    Fail "cgroup v2 did not take effect (got '$cgroupType'). Check $wslConfig, run 'wsl --shutdown', and re-run."
}
Info "cgroup v2 active."

# --- 3. run the Linux core installer inside the distro ------------------
# Run install.sh from this script's directory, inside the distro.
$here = Split-Path -Parent $MyInvocation.MyCommand.Path

# Translate the Windows path to its WSL /mnt path OURSELVES rather than calling
# `wslpath` through `wsl -- ...`: PowerShell mangles backslashes when passing a
# Windows path as an argument across that boundary (E:\Data -> E:Data), so
# wslpath receives garbage. A drive-letter path maps deterministically to
# /mnt/<lowercase-drive>/<forward-slash-path>, which we build directly.
if ($here -match '^([A-Za-z]):\\(.*)$') {
    $drive = $Matches[1].ToLower()
    $rest = $Matches[2] -replace '\\', '/'
    $wslPath = "/mnt/$drive/$rest"
} else {
    # Fallback (e.g. a UNC path): let wslpath try, quoting to preserve slashes.
    $wslPath = (& wsl.exe -d $Distro -- wslpath -a "$here" 2>$null | Out-String).Trim()
}
if (-not $wslPath) { Fail "could not resolve the WSL path for '$here'." }
Info "installer path in WSL: $wslPath"

# Whether this is a fresh install or an update of an already-enrolled node is
# install.sh's own call (it checks for /etc/teepin/agent.json + the systemd
# unit) -- but Token/ControlPlane must be validated HERE, before invoking it,
# because install.sh's fresh-install argument parsing runs inside the distro
# and a missing -Token there would surface as a confusing bash error instead
# of a clear PowerShell one.
$alreadyEnrolled = (wsl -d $Distro -u root -- bash -c "[ -f /etc/teepin/agent.json ] && [ -f /etc/systemd/system/teepin-agent.service ] && echo yes || echo no").Trim()

if ($alreadyEnrolled -eq "yes") {
    Info "existing enrollment found inside $Distro -- updating the agent binary only (Token/ControlPlane not needed)."
    $installArgs = ""
} else {
    if (-not $Token -or -not $ControlPlane) {
        Fail "-Token and -ControlPlane are required for a first install (no existing enrollment found inside $Distro)."
    }
    $grpcArg = ""
    if ($Grpc -ne "") { $grpcArg = "--grpc $Grpc" }
    $installArgs = "--token '$Token' --control-plane '$ControlPlane' $grpcArg"
}

Info "running the Linux installer inside $Distro..."
wsl -d $Distro -u root -- bash -c "cd '$wslPath' && bash install.sh $installArgs"

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

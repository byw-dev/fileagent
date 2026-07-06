#Requires -RunAsAdministrator
<#
.SYNOPSIS
    Install the FileAgent edge agent as a Windows service using NSSM.

.DESCRIPTION
    Registers agent.exe as an auto-starting Windows service wrapped by NSSM
    (the Non-Sucking Service Manager). The agent runs as a foreground process
    and exits cleanly on stop, so NSSM can supervise it directly and capture its
    stdout/stderr to log files (the agent's own log rotation is not yet wired).

    This script does NOT build agent.exe. Obtain it from CI
    (.github/workflows/build-agent.yml -> agent-windows-amd64) or cross-compile
    (see the repo Makefile `build-agent-windows`), and pass its path via -AgentExe.

.PARAMETER AgentExe
    Path to agent.exe. Default: .\agent.exe next to this script.

.PARAMETER NssmExe
    Path to nssm.exe. Default: looked up on PATH. Download from https://nssm.cc/.

.PARAMETER InstallDir
    Where the binary and config live. Default: C:\ProgramData\fileagent.

.PARAMETER ServiceName
    Windows service name. Default: FileAgent.

.EXAMPLE
    .\install-agent.ps1 -AgentExe .\agent.exe
#>
[CmdletBinding()]
param(
    [string]$AgentExe    = (Join-Path $PSScriptRoot 'agent.exe'),
    [string]$NssmExe     = 'nssm.exe',
    [string]$InstallDir  = 'C:\ProgramData\fileagent',
    [string]$ServiceName = 'FileAgent'
)

$ErrorActionPreference = 'Stop'

# ── Preconditions ───────────────────────────────────────────────────────────
if (-not (Test-Path $AgentExe)) {
    throw "agent.exe not found at '$AgentExe'. Build it via CI (build-agent.yml) or 'make build-agent-windows', then pass -AgentExe."
}
if (-not (Get-Command $NssmExe -ErrorAction SilentlyContinue)) {
    throw "nssm.exe not found (looked for '$NssmExe'). Install NSSM from https://nssm.cc/ and put it on PATH or pass -NssmExe."
}

# ── Lay out install + state directories ─────────────────────────────────────
$LogDir = Join-Path $InstallDir 'logs'
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $LogDir     | Out-Null

$TargetExe  = Join-Path $InstallDir 'agent.exe'
$ConfigPath = Join-Path $InstallDir 'agent.toml'
Copy-Item -Path $AgentExe -Destination $TargetExe -Force

if (-not (Test-Path $ConfigPath)) {
    Write-Warning "No config at '$ConfigPath'. Copy deploy/config/agent.windows.toml.example there and edit server.endpoint before starting the service."
}

# ── (Re)install the service ─────────────────────────────────────────────────
# Probe existence by exit code (nssm status exits 0 only when the service
# exists) rather than by parsing stdout, which some error messages also produce.
& $NssmExe status $ServiceName 2>$null | Out-Null
if ($LASTEXITCODE -eq 0) {
    Write-Host "Service '$ServiceName' already exists — reconfiguring."
    & $NssmExe stop $ServiceName | Out-Null
    # Point the existing service at the freshly copied binary.
    & $NssmExe set $ServiceName Application $TargetExe
} else {
    & $NssmExe install $ServiceName $TargetExe '--config' $ConfigPath
}

& $NssmExe set $ServiceName AppDirectory   $InstallDir
& $NssmExe set $ServiceName AppParameters  "--config `"$ConfigPath`""
& $NssmExe set $ServiceName Start          SERVICE_AUTO_START
& $NssmExe set $ServiceName AppStdout      (Join-Path $LogDir 'agent.out.log')
& $NssmExe set $ServiceName AppStderr      (Join-Path $LogDir 'agent.err.log')
# Rotate NSSM-captured logs (the agent does not rotate its own output yet).
& $NssmExe set $ServiceName AppRotateFiles 1
& $NssmExe set $ServiceName AppRotateBytes 10485760

Write-Host "Installed service '$ServiceName'. Start it with: nssm start $ServiceName"
Write-Host "The agent will register with the Control Plane and wait for admin approval in the Web UI."

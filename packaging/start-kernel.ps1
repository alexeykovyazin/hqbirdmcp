<#
.SYNOPSIS
    Start the fbmcp kernel unattended (8M/M2 helper): serve mode + logs.

.DESCRIPTION
    Starts dist\fbmcp.exe with -serve (no stdio transport: a detached
    kernel must not exit on stdin EOF). stdout/stderr stream into
    <state dir>\kernel-stdout.log / kernel-stderr.log. Readiness is the
    REAL check — fbmcpctl ping (the attach socket answers); fbmcpctl
    status is an offline state.json read and proves nothing. Returns once
    the kernel is reachable; the kernel keeps running independently.

    Stop: Get-Process fbmcp | Stop-Process

.EXAMPLE
    .\start-kernel.ps1          # idempotent: no-op when already reachable
#>
[CmdletBinding()]
param(
    [string]$ConfigPath = '',
    [string]$ModuleDir = ''
)

$ErrorActionPreference = 'Stop'

if (-not $ModuleDir) {
    if (-not $PSScriptRoot) { throw "cannot locate script dir (run via -File)" }
    $ModuleDir = Split-Path -Parent $PSScriptRoot
}
if (-not $ConfigPath) { $ConfigPath = Join-Path $ModuleDir 'fbmcp.dev.yaml' }
$kernel = Join-Path $ModuleDir 'dist\fbmcp.exe'
$ctl    = Join-Path $ModuleDir 'dist\fbmcpctl.exe'

& $ctl ping $ConfigPath 3s *> $null
if ($LASTEXITCODE -eq 0) {
    Write-Host "kernel already reachable"
    exit 0
}

$cfgText = Get-Content $ConfigPath -Raw
if ($cfgText -match '(?m)^\s*dir:\s*(.+)$') {
    $stateDir = $Matches[1].Trim()
} else {
    $stateDir = Join-Path $ModuleDir 'state-logs'
}
if (-not (Test-Path $stateDir)) { New-Item -ItemType Directory -Path $stateDir -Force | Out-Null }
$outLog = Join-Path $stateDir 'kernel-stdout.log'
$errLog = Join-Path $stateDir 'kernel-stderr.log'

Start-Process -FilePath $kernel `
    -ArgumentList @('-config', $ConfigPath, '-serve') `
    -WorkingDirectory $ModuleDir `
    -WindowStyle Hidden `
    -RedirectStandardOutput $outLog `
    -RedirectStandardError $errLog

$ready = $false
for ($i = 0; $i -lt 45; $i++) {
    Start-Sleep -Seconds 1
    & $ctl ping $ConfigPath 3s *> $null
    if ($LASTEXITCODE -eq 0) { $ready = $true; break }
}
if (-not $ready) { throw "kernel did not become reachable within 45s (see $errLog)" }
Write-Host "kernel reachable (serve mode; logs: $errLog)"

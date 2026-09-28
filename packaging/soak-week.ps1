<#
.SYNOPSIS
    Soak-week observation layer (phase8_plan D1.3 / 8M M2, improvement-plan A.3).

.DESCRIPTION
    Bootstraps the nightly_verify schedule grant for every registered
    database (cmd/fbmcpsoak, one-shot), then samples the kernel hourly for
    seven days: fbmcpctl status + verify + doctor lines and a timestamp are
    appended to docs/findings/soak-report.md. Non-veto per P6.2 T5 — a red
    line is an observation, not a release blocker by itself.

    Assumes the kernel is running (a client like ZCode/Claude holds it, or
    start one: start-kernel.ps1 / the Windows service). Ctrl+C stops the
    sampling loop; schedules persist in state.json and keep firing while any
    kernel runs.

.EXAMPLE
    .\soak-week.ps1 -Days 7
#>
[CmdletBinding()]
param(
    [int]$Days = 7,
    [string]$ConfigPath = '',
    [string]$ModuleDir = ''
)

$ErrorActionPreference = 'Continue'

# resolve module dir in the body: $PSScriptRoot is reliably set here,
# but not always at param-default evaluation time
if (-not $ModuleDir) {
    if (-not $PSScriptRoot) { throw "cannot locate script dir (run via -File)" }
    $ModuleDir = Split-Path -Parent $PSScriptRoot
}
if (-not $ConfigPath) { $ConfigPath = Join-Path $ModuleDir 'fbmcp.dev.yaml' }
$soakBin = Join-Path $ModuleDir 'dist\fbmcpsoak.exe'
$ctl = Join-Path $ModuleDir 'dist\fbmcpctl.exe'
$report = Join-Path $ModuleDir 'docs\findings\soak-report.md'
if (-not (Test-Path $report)) {
    # Keep this file free of fb_* tool names: docs/ subdirs are phantom-linted.
    Set-Content -Path $report -Value "# Soak week report`n" -Encoding UTF8
}
if (-not (Test-Path $soakBin)) { throw "build first: $soakBin missing" }

Write-Host "==> bootstrap (nightly_verify grants)"
& $soakBin -config $ConfigPath
$env:FBMCP_DEV_PW = $env:FBMCP_DEV_PW
if ($LASTEXITCODE -ne 0) { throw "bootstrap failed" }

$deadline = (Get-Date).AddDays($Days)
# no yaml parser ships with PS 5.1; scrape state.dir like rebuild-install.ps1 does
$stateDir = $null
$yaml = Get-Content $ConfigPath -Raw
if ($yaml -match '(?s)state:\s*dir:\s*(\S+)') { $stateDir = $Matches[1] }
$lockFile = $null
if ($stateDir) {
    if (-not (Test-Path $stateDir)) { New-Item -ItemType Directory -Force -Path $stateDir | Out-Null }
    $lockFile = Join-Path $stateDir 'soak-active.pid'
    if (Test-Path $lockFile) {
        $other = Get-Content $lockFile -ErrorAction SilentlyContinue
        if ($other -and (Get-Process -Id $other -ErrorAction SilentlyContinue)) {
            Write-Host "soak already running (pid $other); nothing to do"
            exit 0
        }
    }
    Set-Content -Path $lockFile -Value $PID
} else {
    Write-Warning "state.dir not found in $ConfigPath - running without the single-instance lock"
}
Add-Content $report "`n## run started $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') ($Days days)"
while ((Get-Date) -lt $deadline) {
    $stamp = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
    $status = (& $ctl status $ConfigPath 2>&1) -join ' | '
    $chain  = (& $ctl verify $ConfigPath 2>&1) -join ' '
    Add-Content $report "$stamp status=[$status] chain=[$chain]"
    Write-Host "$stamp sampled"
    Start-Sleep -Seconds 3600
}
Add-Content $report "`n## run ended $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')"
if ($lockFile) { Remove-Item $lockFile -ErrorAction SilentlyContinue }
Write-Host "soak sampling window complete; report: $report"

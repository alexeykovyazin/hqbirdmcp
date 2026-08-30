<#
.SYNOPSIS
    Nightly chaos-harness runner (phase8_plan D1.1).

.DESCRIPTION
    Runs the kill harness N times against the live HQBird dev instances and
    appends a dated result line to docs/findings/chaos-log.md. Register it
    with register-chaos-task.ps1 (Task Scheduler, nightly) — the run itself
    is manual/ops work (phase 8M / M3).

.PARAMETER Count
    Repetitions of the full harness battery (default 50, per plan A.1).

.EXAMPLE
    .\chaos-nightly.ps1 -Count 10
#>
[CmdletBinding()]
param(
    [int]$Count = 50,
    [string]$ModuleDir = ''
)

$ErrorActionPreference = 'Stop'

# resolve module dir in the body: $PSScriptRoot is reliably set here,
# but not always at param-default evaluation time
if (-not $ModuleDir) {
    if (-not $PSScriptRoot) { throw "cannot locate script dir (run via -File)" }
    $ModuleDir = Split-Path -Parent $PSScriptRoot
}
$log = Join-Path $ModuleDir 'docs\findings\chaos-log.md'
if (-not (Test-Path $log)) {
    # Keep this file free of fb_* tool names: docs/ subdirs are phantom-linted.
    Set-Content -Path $log -Value "# Chaos harness nightly log`n" -Encoding UTF8
}

$env:FBMCP_KILLHARNESS = '1'
$env:FBMCP_REQUIRE_FIREBIRD = '1'

Push-Location $ModuleDir
try {
    $stamp = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
    & go test ./internal/killharness/ -count $Count -timeout 24h 2>&1 | Tee-Object -Variable out | Out-Null
    $code = $LASTEXITCODE
}
finally { Pop-Location }

$verdict = if ($code -eq 0) { 'GREEN' } else { 'RED' }
Add-Content -Path $log -Value "$stamp count=$Count verdict=$verdict exit=$code"
Write-Host "chaos run: $verdict (logged to $log)"
if ($code -ne 0) {
    $out | Select-Object -Last 40 | Write-Host
    exit $code
}

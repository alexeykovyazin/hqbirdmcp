<#
.SYNOPSIS
    One-command soak-week kickoff (8M/M2): kernel (if needed) + bootstrap
    + 7-day sampling loop, all unattended.

.DESCRIPTION
    1. Starts packaging\start-kernel.ps1 as a DETACHED guardian (it holds
       the kernel's stdin pipe open for the kernel's lifetime).
    2. Waits for the attach socket.
    3. Runs soak-week.ps1 synchronously: bootstraps the nightly_verify
       grants and samples hourly into docs/findings/soak-report.md.

    Register unattended (survives logoff):
      .\register-soak-task.ps1        # task fbmcp-soak-week, then Start it

    Stop everything:
      Unregister-ScheduledTask -TaskName fbmcp-soak-week -Confirm:$false
      Get-Process fbmcp | Stop-Process          # stops the kernel itself
#>
[CmdletBinding()]
param(
    [int]$Days = 7,
    [string]$ConfigPath = '',
    [string]$ModuleDir = ''
)

$ErrorActionPreference = 'Stop'

if (-not $ModuleDir) {
    if (-not $PSScriptRoot) { throw "cannot locate script dir (run via -File)" }
    $ModuleDir = Split-Path -Parent $PSScriptRoot
}
if (-not $ConfigPath) { $ConfigPath = Join-Path $ModuleDir 'fbmcp.dev.yaml' }
$ctl = Join-Path $ModuleDir 'dist\fbmcpctl.exe'

# 1. detached guardian for the kernel (no-op when a kernel already answers)
$guardianArgs = @(
    '-NoProfile', '-ExecutionPolicy', 'Bypass',
    '-File', "$ModuleDir\packaging\start-kernel.ps1",
    '-ConfigPath', $ConfigPath,
    '-ModuleDir', $ModuleDir
)
Start-Process powershell -ArgumentList $guardianArgs -WindowStyle Hidden | Out-Null

# 2. wait for the attach socket
$ready = $false
for ($i = 0; $i -lt 90; $i++) {
    & $ctl status $ConfigPath *> $null
    if ($LASTEXITCODE -eq 0) { $ready = $true; break }
    Start-Sleep -Seconds 1
}
if (-not $ready) { throw "kernel attach socket never came up" }
Write-Host "kernel ready"

# 3. soak bootstrap + hourly sampling (blocks for $Days days)
& (Join-Path $PSScriptRoot 'soak-week.ps1') -Days $Days -ConfigPath $ConfigPath -ModuleDir $ModuleDir

<#
.SYNOPSIS
    Registers the soak-week kickoff as an unattended scheduled task (8M/M2).

.DESCRIPTION
    Task "fbmcp-soak-week" runs packaging\start-soak.ps1 (-Days 7) as the
    current user with S4U (no stored password; survives logoff, no console).
    StartWhenAvailable so a sleeping/rebooting host runs it on wake.
    Idempotent (-Force). Remove with:
      Unregister-ScheduledTask -TaskName fbmcp-soak-week -Confirm:$false

    Kick the run off immediately with:
      Start-ScheduledTask -TaskName fbmcp-soak-week
#>
[CmdletBinding()]
param(
    [string]$ModuleDir = '',
    [int]$Days = 7
)

$ErrorActionPreference = 'Stop'

if (-not $ModuleDir) {
    if (-not $PSScriptRoot) { throw "cannot locate script dir (run via -File)" }
    $ModuleDir = Split-Path -Parent $PSScriptRoot
}
$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$ModuleDir\packaging\start-soak.ps1`" -Days $Days -ModuleDir `"$ModuleDir`""
# daily self-heal: restarts kernel/loop after reboots; the lock file in
# the state dir makes a second start a no-op while the loop is alive
$trigger = New-ScheduledTaskTrigger -Daily -At 02:00
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -DontStopIfGoingOnBatteries -AllowStartIfOnBatteries
# NOTE: interactive-token principal (S4U registration requires elevation and
# failed with access denied on the dev host). The task therefore dies on
# logoff; re-register elevated with -LogonType S4U for full unattended use.
Register-ScheduledTask -TaskName 'fbmcp-soak-week' -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Write-Host "registered fbmcp-soak-week (daily 02:00 self-heal; runs start-soak.ps1 -Days $Days; interactive token)"

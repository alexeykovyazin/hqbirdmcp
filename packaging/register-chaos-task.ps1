<#
.SYNOPSIS
    Registers the nightly chaos run as a Windows scheduled task (phase8_plan D1.1).

.DESCRIPTION
    Task "fbmcp-chaos-nightly" runs packaging\chaos-nightly.ps1 every night at
    02:30 as the current user. Remove with:
      Unregister-ScheduledTask -TaskName fbmcp-chaos-nightly -Confirm:$false

.EXAMPLE
    .\register-chaos-task.ps1
#>
[CmdletBinding()]
param(
    [string]$ModuleDir = (Split-Path -Parent $PSScriptRoot),
    [string]$Time = '02:30'
)

$ErrorActionPreference = 'Stop'
$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
    -Argument "-NoProfile -ExecutionPolicy Bypass -File `"$ModuleDir\packaging\chaos-nightly.ps1`""
$trigger = New-ScheduledTaskTrigger -Daily -At $Time
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName 'fbmcp-chaos-nightly' -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Write-Host "registered fbmcp-chaos-nightly (daily at $Time, runs $($ModuleDir)\packaging\chaos-nightly.ps1)"

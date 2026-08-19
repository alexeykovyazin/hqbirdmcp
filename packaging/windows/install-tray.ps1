# Register fbmcp-tray to launch at user logon (Startup-folder shortcut —
# no elevation needed, unlike a scheduled task). Run as the operator who
# will click Approve/Deny, not as the service account.
# The tray process needs read/write access to State.Dir from fbmcp.yaml
# (approvals/, denials/, state.json) — see packaging/README.md.
param(
  [string]$Bin = "C:\fbmcp\fbmcp-tray.exe",
  [string]$Config = "C:\fbmcp\fbmcp.yaml"
)
$startup = [Environment]::GetFolderPath("Startup")
$shortcut = Join-Path $startup "fbmcp-tray.lnk"
$ws = New-Object -ComObject WScript.Shell
$lnk = $ws.CreateShortcut($shortcut)
$lnk.TargetPath = $Bin
$lnk.Arguments = "`"$Config`""
$lnk.WorkingDirectory = Split-Path $Bin
$lnk.Description = "fbmcp Approve/Deny tray"
$lnk.Save()
Write-Host "created $shortcut -- starts at next logon, or run now with:"
Write-Host "  $Bin $Config"
Write-Host "remove with: Remove-Item $shortcut"

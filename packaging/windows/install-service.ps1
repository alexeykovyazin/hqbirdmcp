# Install fbmcp as a Windows service (restart on failure).
# Run elevated. Adjust paths.
param(
  [string]$Bin = "C:\fbmcp\fbmcp.exe",
  [string]$Config = "C:\fbmcp\fbmcp.yaml",
  [string]$Name = "fbmcp"
)
# -serve: no stdio transport; the service Stop ends the kernel cleanly (phase 12 B0.4).
# Remote mode (HTTPS, firewall, ACLs): use install-remote.ps1 instead.
sc.exe create $Name binPath= "`"$Bin`" -config `"$Config`" -serve" start= auto
sc.exe failure $Name reset= 86400 actions= restart/5000/restart/5000/restart/5000
sc.exe description $Name "Firebird DBA MCP server"
# A folder made under C:\ lets every user change files, and this service runs
# them as LocalSystem. install-remote.ps1 locks the folder; here, check it.
$dir = Split-Path -Parent $Bin
Write-Host "check: only SYSTEM and Administrators may write to $dir, for example:"
Write-Host "  icacls.exe $dir /inheritance:r /grant:r *S-1-5-18:(OI)(CI)F /grant:r *S-1-5-32-544:(OI)(CI)F /grant:r *S-1-5-32-545:(OI)(CI)RX"
Write-Host "  (keep the state dir writable for the tray operator if it is inside $dir)"
Write-Host "created $Name — start with: sc.exe start $Name"
Write-Host "remove with: sc.exe stop $Name; sc.exe delete $Name"

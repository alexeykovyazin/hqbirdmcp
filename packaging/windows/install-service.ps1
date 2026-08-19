# Install fbmcp as a Windows service (restart on failure).
# Run elevated. Adjust paths.
param(
  [string]$Bin = "C:\fbmcp\fbmcp.exe",
  [string]$Config = "C:\fbmcp\fbmcp.yaml",
  [string]$Name = "fbmcp"
)
sc.exe create $Name binPath= "$Bin -config $Config" start= auto
sc.exe failure $Name reset= 86400 actions= restart/5000/restart/5000/restart/5000
sc.exe description $Name "Firebird DBA MCP server"
Write-Host "created $Name — start with: sc.exe start $Name"
Write-Host "remove with: sc.exe stop $Name; sc.exe delete $Name"

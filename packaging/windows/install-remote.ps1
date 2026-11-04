# install-remote.ps1 - install fbmcp in remote mode as a Windows service
# (phase 12 B3). Run in an elevated PowerShell. Dry run first:
#   .\install-remote.ps1 -AllowFrom 10.0.0.0/24 -FirebirdService FirebirdServerDefaultInstance -WhatIf
# then the same command without -WhatIf.
#
# Steps: service (-serve, auto start, restart on failure, chosen account),
# folders + ACLs (state, secrets, TLS key), firewall rule limited to
# -AllowFrom, start/stop rights on the Firebird service, optional read on
# fbagent configs, start, fbmcpctl doctor --remote.
# A password for -Account .\user goes only through -Credential (F10).
[CmdletBinding(SupportsShouldProcess = $true)]
param(
  [string]$Account = "NT SERVICE\fbmcp",
  [System.Management.Automation.PSCredential]$Credential,
  [int]$Port = 8443,
  [string[]]$AllowFrom,
  [string]$Bin = "C:\fbmcp\fbmcp.exe",
  [string]$Ctl = "C:\fbmcp\fbmcpctl.exe",
  [string]$Config = "C:\fbmcp\fbmcp.yaml",
  [string]$StateDir = "C:\ProgramData\fbmcp\state",
  [string]$SecretsDir = "C:\ProgramData\fbmcp\secrets",
  [string]$TlsKey = "C:\fbmcp\tls\tls.key",
  [string]$FirebirdService,
  [string[]]$GrantAgentConfig,
  [string]$OperatorGroup = "BUILTIN\Administrators",
  [string]$Name = "fbmcp",
  [switch]$NoStart
)
$ErrorActionPreference = "Stop"
$dry = [bool]$WhatIfPreference
$WhatIfPreference = $false  # the steps below decide; cmdlets must not print their own WhatIf lines

function Step([string]$text) { Write-Host "== $text" -ForegroundColor Cyan }
# Run prints the command and runs it unless -WhatIf.
function Run([string]$show, [scriptblock]$action) {
  Write-Host "+ $show"
  if (-not $dry) { & $action }
}
function Native([string]$exe, [string[]]$argv) {
  Run ("$exe " + ($argv -join " ")) {
    $out = & $exe @argv 2>&1
    if ($LASTEXITCODE -ne 0) { throw "$exe failed ($LASTEXITCODE): $out" }
  }
}

# --- checks ---
if (-not $AllowFrom -or $AllowFrom.Count -eq 0) {
  throw "-AllowFrom is required: the port must be open only to the admin network (e.g. -AllowFrom 10.0.0.0/24)"
}
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin -and -not $dry) { throw "run in an elevated PowerShell" }
foreach ($f in @($Bin, $Ctl, $Config)) {
  if (-not (Test-Path $f)) { throw "$f not found" }
}
$virtual = $Account -like "NT SERVICE\*"
if (-not $virtual -and -not $Credential) {
  throw "-Account $Account needs -Credential (Get-Credential); the password never goes on a command line"
}
if ($dry) { Write-Host "dry run (-WhatIf): nothing is changed" -ForegroundColor Yellow }

# SeServiceLogonRight for a real account (a virtual account needs none).
$lsaCode = @"
using System;
using System.Runtime.InteropServices;
public static class FbmcpLsa {
  [StructLayout(LayoutKind.Sequential)] struct LSA_UNICODE_STRING { public ushort Length; public ushort MaximumLength; public IntPtr Buffer; }
  [StructLayout(LayoutKind.Sequential)] struct LSA_OBJECT_ATTRIBUTES { public int Length; public IntPtr RootDirectory; public IntPtr ObjectName; public int Attributes; public IntPtr SecurityDescriptor; public IntPtr SecurityQualityOfService; }
  [DllImport("advapi32.dll")] static extern uint LsaOpenPolicy(IntPtr SystemName, ref LSA_OBJECT_ATTRIBUTES ObjectAttributes, int DesiredAccess, out IntPtr PolicyHandle);
  [DllImport("advapi32.dll")] static extern uint LsaAddAccountRights(IntPtr PolicyHandle, byte[] AccountSid, LSA_UNICODE_STRING[] UserRights, int CountOfRights);
  [DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr PolicyHandle);
  [DllImport("advapi32.dll")] static extern int LsaNtStatusToWinError(uint status);
  public static void AddRight(byte[] sid, string right) {
    LSA_OBJECT_ATTRIBUTES oa = new LSA_OBJECT_ATTRIBUTES();
    IntPtr h;
    uint st = LsaOpenPolicy(IntPtr.Zero, ref oa, 0x00000800 | 0x00000010, out h);
    if (st != 0) throw new System.ComponentModel.Win32Exception(LsaNtStatusToWinError(st));
    try {
      LSA_UNICODE_STRING[] r = new LSA_UNICODE_STRING[1];
      r[0].Buffer = Marshal.StringToHGlobalUni(right);
      r[0].Length = (ushort)(right.Length * 2);
      r[0].MaximumLength = (ushort)((right.Length + 1) * 2);
      st = LsaAddAccountRights(h, sid, r, 1);
      Marshal.FreeHGlobal(r[0].Buffer);
      if (st != 0) throw new System.ComponentModel.Win32Exception(LsaNtStatusToWinError(st));
    } finally { LsaClose(h); }
  }
}
"@

function AccountSid([string]$acct) {
  if ($acct -like "NT SERVICE\*") {
    $svc = $acct.Substring(11)
    $line = (& sc.exe showsid $svc) | Where-Object { $_ -match "SERVICE SID:" }
    return ($line -split ":\s*", 2)[1].Trim()
  }
  return (New-Object Security.Principal.NTAccount($acct)).Translate([Security.Principal.SecurityIdentifier]).Value
}

# --- 1. service ---
Step "1. service $Name ($Account)"
$binPath = "`"$Bin`" -config `"$Config`" -serve"
$exists = $null -ne (Get-Service -Name $Name -ErrorAction SilentlyContinue)
if ($exists) {
  Write-Host "service $Name exists: updating its command and account"
  Run "sc.exe stop $Name (if running)" { & sc.exe stop $Name | Out-Null }
  if ($virtual) {
    Native "sc.exe" @("config", $Name, "binPath=", $binPath, "start=", "auto", "obj=", $Account)
  } else {
    Native "sc.exe" @("config", $Name, "binPath=", $binPath, "start=", "auto")
    Run "Win32_Service.Change(StartName=$Account, password from -Credential)" {
      $svc = Get-CimInstance Win32_Service -Filter "Name='$Name'"
      $r = Invoke-CimMethod -InputObject $svc -MethodName Change -Arguments @{ StartName = $Account; StartPassword = $Credential.GetNetworkCredential().Password }
      if ($r.ReturnValue -ne 0) { throw "Win32_Service.Change failed: $($r.ReturnValue)" }
    }
  }
} else {
  if ($virtual) {
    Native "sc.exe" @("create", $Name, "binPath=", $binPath, "start=", "auto", "obj=", $Account)
  } else {
    Run "New-Service -Name $Name -BinaryPathName '$binPath' -StartupType Automatic -Credential <from -Credential>" {
      New-Service -Name $Name -BinaryPathName $binPath -StartupType Automatic -Credential $Credential | Out-Null
    }
  }
}
Native "sc.exe" @("sidtype", $Name, "unrestricted")
Native "sc.exe" @("failure", $Name, "reset=", "86400", "actions=", "restart/5000/restart/5000/restart/60000")
Native "sc.exe" @("description", $Name, "fbmcp - Firebird DBA MCP server (remote mode)")
if (-not $virtual) {
  Run "grant SeServiceLogonRight to $Account" {
    Add-Type -TypeDefinition $lsaCode
    $sid = New-Object Security.Principal.SecurityIdentifier((AccountSid $Account))
    $bytes = New-Object byte[] ($sid.BinaryLength)
    $sid.GetBinaryForm($bytes, 0)
    [FbmcpLsa]::AddRight($bytes, "SeServiceLogonRight")
  }
}

# --- 2. folders and ACLs ---
Step "2. folders and ACLs"
$acct = $Account
$binDir = Split-Path -Parent $Bin
$cfgDir = Split-Path -Parent $Config
foreach ($d in @($StateDir, $SecretsDir)) {
  if (-not (Test-Path $d)) { Run "New-Item -ItemType Directory $d" { New-Item -ItemType Directory -Force -Path $d | Out-Null } }
}
# State: the service writes; the operators approve (tray, fbmcpctl approve).
$stateAcl = @($StateDir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "/grant:r", "*S-1-5-32-544:(OI)(CI)F", "/grant:r", "${acct}:(OI)(CI)M")
if ($OperatorGroup -and $OperatorGroup -ne "BUILTIN\Administrators") { $stateAcl += @("/grant:r", "${OperatorGroup}:(OI)(CI)M") }
Native "icacls.exe" $stateAcl
# Secrets: the service reads; only Administrators write.
Native "icacls.exe" @($SecretsDir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "/grant:r", "*S-1-5-32-544:(OI)(CI)F", "/grant:r", "${acct}:(OI)(CI)R")
# Binaries and config: only SYSTEM and Administrators write; the service and
# Users read. A folder made under C:\ inherits Modify for Authenticated
# Users, so any local user could replace fbmcp.exe or edit fbmcp.yaml
# (phase 12 test W). Only a folder of its own is changed.
$roAcl = @("/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "/grant:r", "*S-1-5-32-544:(OI)(CI)F",
  "/grant:r", "*S-1-5-32-545:(OI)(CI)RX", "/grant:r", "${acct}:(OI)(CI)RX")
foreach ($d in @($binDir, $cfgDir) | Select-Object -Unique) {
  $other = Get-ChildItem -LiteralPath $d -Force -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -notmatch '^(fbmcp.*|tls|.*\.ya?ml|.*\.log)$' }
  if ($other) {
    Write-Host "warning: $d also holds $(@($other.Name) -join ', '); its ACL is not changed. Put fbmcp in a folder of its own, or allow writes only to SYSTEM and Administrators yourself." -ForegroundColor Yellow
    Native "icacls.exe" @($d, "/grant", "${acct}:(OI)(CI)RX")
  } else {
    Native "icacls.exe" (@($d) + $roAcl)
  }
}
if (Test-Path $TlsKey) {
  Native "icacls.exe" @($TlsKey, "/inheritance:r", "/grant:r", "*S-1-5-18:F", "/grant:r", "*S-1-5-32-544:F", "/grant:r", "${acct}:R")
} else {
  Write-Host "warning: $TlsKey not found; make it with: $Ctl tls selfsign --out $(Split-Path -Parent $TlsKey) --dns <names> --ip <addr> --grant `"$Account`"" -ForegroundColor Yellow
}

# --- 3. firewall ---
Step "3. firewall (TCP $Port from $($AllowFrom -join ','))"
Run "Remove-NetFirewallRule -DisplayName fbmcp-remote (if present)" {
  Get-NetFirewallRule -DisplayName "fbmcp-remote" -ErrorAction SilentlyContinue | Remove-NetFirewallRule
}
Run "New-NetFirewallRule -DisplayName fbmcp-remote -Direction Inbound -Action Allow -Protocol TCP -LocalPort $Port -RemoteAddress $($AllowFrom -join ',') -Program $Bin" {
  New-NetFirewallRule -DisplayName "fbmcp-remote" -Direction Inbound -Action Allow -Protocol TCP -LocalPort $Port -RemoteAddress $AllowFrom -Program $Bin -Profile Any | Out-Null
}

# --- 4. Firebird service rights (fb_service_start/stop/restart) ---
if ($FirebirdService) {
  Step "4. start/stop rights on $FirebirdService"
  Run "sc.exe sdset $FirebirdService <current SDDL + (A;;CCLCRPWPLORC;;;<sid of $Account>)>" {
    $sid = AccountSid $Account
    $sd = ((& sc.exe sdshow $FirebirdService) | Where-Object { $_ -match "^D:" }) -join ""
    if (-not $sd) { throw "sc sdshow $FirebirdService returned no SDDL" }
    if ($sd -notmatch [regex]::Escape(";;;$sid)")) {
      $ace = "(A;;CCLCRPWPLORC;;;$sid)"
      if ($sd -match "S:") { $sd = $sd -replace "S:", "${ace}S:" } else { $sd = $sd + $ace }
      $out = & sc.exe sdset $FirebirdService $sd
      if ($LASTEXITCODE -ne 0) { throw "sc sdset failed: $out" }
    } else { Write-Host "  already granted" }
  }
} else {
  Step "4. skipped: no -FirebirdService (fb_service_* will be refused by Windows)"
}

# --- 5. fbagent configs (phase 11 config tools) ---
if ($GrantAgentConfig) {
  Step "5. read on fbagent configs"
  foreach ($cfg in $GrantAgentConfig) {
    Native "icacls.exe" @($cfg, "/grant", "${acct}:R")
    Native "icacls.exe" @((Split-Path -Parent $cfg), "/grant", "${acct}:(AD,WD)")
  }
}

# --- 6. start and check ---
if ($NoStart) {
  Step "6. not started (-NoStart)"
} else {
  Step "6. start and doctor"
  Native "sc.exe" @("start", $Name)
  Run "& $Ctl doctor --remote $Config" {
    Start-Sleep -Seconds 3
    & $Ctl doctor --remote $Config
    if ($LASTEXITCODE -ne 0) { Write-Host "doctor found problems; read $StateDir\fbmcp.log" -ForegroundColor Yellow }
  }
}
Write-Host "remove: sc.exe stop $Name; sc.exe delete $Name; Remove-NetFirewallRule -DisplayName fbmcp-remote"

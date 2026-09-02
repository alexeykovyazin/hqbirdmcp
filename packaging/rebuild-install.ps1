<#
.SYNOPSIS
    One-command fbmcp rebuild + in-place update + client configuration (local deployment).

.DESCRIPTION
    Implements improvement_plan.md task H.5 / phase8_plan tooling. Replaces the
    manual procedure in docs/claude-desktop.md: build the binaries into dist\
    with the release flags, stop/backup/replace in place, ensure the state
    directory exists, and idempotently merge the fbmcp server entry into MCP
    client configs (ZCode workspace config by default; Claude Desktop /
    Claude Code optionally).

    Secrets: the dev password is written ONLY into client config env blocks
    (matching docs/claude-desktop.md for the spike setup). A persistent
    user-level FBMCP_DEV_PW is opt-in via -SetDevPw. Never point the spike
    password at production databases.

.PARAMETER ConfigPath
    fbmcp YAML config (default: fbmcp\fbmcp.dev.yaml next to this module).

.PARAMETER Clients
    Which client configs to merge the server entry into: zcode (default),
    claude (Claude Desktop, %APPDATA%\Claude\claude_desktop_config.json),
    claude-code (repo-root .mcp.json).

.PARAMETER SkipBuild
    Reuse the binaries currently in dist\.staging (or skip straight to
    update/configure when staging is empty and dist\ is current).

.PARAMETER Force
    Stop running fbmcp/fbmcp-tray processes from dist\ without prompting.

.PARAMETER SetDevPw
    Opt-in: also set FBMCP_DEV_PW as a persistent user environment variable.

.PARAMETER DevPw
    Password written into client env blocks (default: spike 'masterkey').

.EXAMPLE
    .\rebuild-install.ps1 -WhatIf
    .\rebuild-install.ps1 -Force
    .\rebuild-install.ps1 -Clients zcode,claude -Force
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$ConfigPath = '',
    [ValidateSet('zcode', 'claude', 'claude-code')]
    [string[]]$Clients = @('zcode'),
    [switch]$SkipBuild,
    [switch]$Force,
    [switch]$SetDevPw,
    [string]$DevPw = 'masterkey'
)

$ErrorActionPreference = 'Stop'
$ModuleDir = Split-Path -Parent $PSScriptRoot   # fbmcp\
$RepoDir   = Split-Path -Parent $ModuleDir      # repo root (AIDBA\)
if (-not $ConfigPath) { $ConfigPath = Join-Path $ModuleDir 'fbmcp.dev.yaml' }
$ConfigPath = [System.IO.Path]::GetFullPath($ConfigPath)
$DistDir = Join-Path $ModuleDir 'dist'
$ExeNames = @('fbmcp', 'fbmcpctl', 'fbmcp-tray', 'fbmcpsoak')
$ExePaths = @{
    'fbmcp'      = './cmd/fbmcp'
    'fbmcpctl'   = './cmd/fbmcpctl'
    'fbmcp-tray' = './cmd/fbmcp-tray'
    'fbmcpsoak'  = './cmd/fbmcpsoak'
}
# fbmcp-tray is the only GUI-subsystem binary: -H=windowsgui keeps Windows
# from allocating a console window at logon. The others must stay console —
# fbmcp is an stdio MCP server, fbmcpctl/fbmcpsoak are CLIs.
$ExeLdExtra = @{
    'fbmcp-tray' = ' -H=windowsgui'
}

function Step([string]$msg) { Write-Host "==> $msg" -ForegroundColor Cyan }

# --- Preflight ---------------------------------------------------------------
Step 'Preflight'
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'go toolchain not found in PATH' }
if (-not (Test-Path $ConfigPath)) { throw "config not found: $ConfigPath" }
if ((Split-Path -Leaf $ConfigPath) -ne 'fbmcp.dev.yaml' -and $DevPw -eq 'masterkey') {
    Write-Warning "Non-dev config with default spike password - do not use 'masterkey' outside the spike databases."
}
$stamp = ''
try { $stamp = @(git -C $RepoDir describe --always --dirty)[0] } catch { $stamp = '' }
if ([string]::IsNullOrWhiteSpace($stamp)) { $stamp = Get-Date -Format 'yyyyMMdd-HHmmss' }
Write-Host "    version stamp: $stamp"

# --- Build -------------------------------------------------------------------
$staging = Join-Path $DistDir '.staging'
if (-not $SkipBuild) {
    Step "Building into $staging (all binaries must succeed before dist\ is touched)"
    if ($PSCmdlet.ShouldProcess($staging, 'build staged binaries')) {
        if (Test-Path $staging) { Remove-Item $staging -Recurse -Force }
        New-Item -ItemType Directory -Force -Path $staging | Out-Null
        Push-Location $ModuleDir
        try {
            $env:CGO_ENABLED = '0'
            foreach ($name in $ExeNames) {
                & go build -trimpath -ldflags "-s -w -X main.version=$stamp$($ExeLdExtra[$name])" -o (Join-Path $staging "$name.exe") $ExePaths[$name]
                if ($LASTEXITCODE -ne 0) { throw "build failed: $name" }
                Write-Host "    built $name.exe"
            }
        } finally { Pop-Location }
    } else {
        Write-Host '    build skipped (-WhatIf)'
    }
} else {
    Step 'Skipping build (-SkipBuild)'
}

# --- Update dist\ in place ----------------------------------------------------
Step 'Updating dist\'
$running = @(Get-Process -Name fbmcp, fbmcp-tray -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -and $_.Path.StartsWith($DistDir, [System.StringComparison]::OrdinalIgnoreCase) })
if ($running.Count -gt 0) {
    $ids = ($running | ForEach-Object { "$($_.Id) ($($_.Path))" }) -join ', '
    if ($Force) {
        if ($PSCmdlet.ShouldProcess("pid $($running.Id -join ', ')", 'stop running fbmcp processes')) {
            $running | Stop-Process -Force
            Start-Sleep -Milliseconds 500
        }
    } elseif ([Environment]::UserInteractive) {
        $answer = Read-Host "    Running from dist\: $ids. Stop them? (y/N)"
        if ($answer -notmatch '^[Yy]') { throw 'aborted: dist\ binaries are in use; rerun with -Force to stop them' }
        $running | Stop-Process -Force
        Start-Sleep -Milliseconds 500
    } else {
        throw "running fbmcp processes from dist\ ($ids); rerun with -Force"
    }
}

if (Test-Path $staging) {
    foreach ($name in $ExeNames) {
        $new = Join-Path $staging "$name.exe"
        if (-not (Test-Path $new)) { continue }
        $live = Join-Path $DistDir "$name.exe"
        $bak  = "$live~"
        if ($PSCmdlet.ShouldProcess($DistDir, "replace $name.exe (backup to $name.exe~)")) {
            if (Test-Path $live) {
                if (Test-Path $bak) { Remove-Item $bak -Force }
                Copy-Item $live $bak
            }
            Copy-Item $new $live -Force
        }
    }
    if ($PSCmdlet.ShouldProcess($staging, 'remove staging dir')) { Remove-Item $staging -Recurse -Force }
} else {
    Write-Host '    no staged binaries; dist\ left unchanged'
}

$rootExe = Join-Path $ModuleDir 'fbmcp.exe'
if (Test-Path $rootExe) {
    Write-Warning "stale binary outside dist\: $rootExe - dist\ is the install location referenced by client configs; remove the stale copy"
}

# --- State directory -----------------------------------------------------------
Step 'State directory'
$stateDir = $null
$m = Select-String -Path $ConfigPath -Pattern '(?m)^\s*dir:\s*(\S+)' | Select-Object -First 1
if ($m) { $stateDir = $m.Matches[0].Groups[1].Value }
if (-not $stateDir) {
    Write-Warning "state.dir not found in $ConfigPath - create the state directory manually"
} elseif (-not (Test-Path $stateDir)) {
    if ($PSCmdlet.ShouldProcess($stateDir, 'create state dir')) { New-Item -ItemType Directory -Force -Path $stateDir | Out-Null }
    Write-Host "    created $stateDir"
} else {
    Write-Host "    exists: $stateDir"
}

# --- Client configuration (idempotent merge) -----------------------------------
function Get-OrCreateProp($obj, [string]$name) {
    if ($null -eq $obj.PSObject.Properties[$name]) {
        $obj | Add-Member NoteProperty $name ([pscustomobject]@{})
    }
    return $obj.$name
}
function Set-Prop($obj, [string]$name, $value) {
    if ($null -ne $obj.PSObject.Properties[$name]) { $obj.$name = $value }
    else { $obj | Add-Member NoteProperty $name $value }
}
function Save-Json($obj, [string]$path) {
    $dir = Split-Path -Parent $path
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    # UTF-8 without BOM: node-based MCP clients fail to parse a BOM-prefixed config.
    [System.IO.File]::WriteAllText($path, ($obj | ConvertTo-Json -Depth 10))
    Write-Host "    merged: $path"
}
function Merge-ServerEntry([string]$path, [string[]]$propChain, $entry) {
    $cfg = $null
    if (Test-Path $path) { $cfg = Get-Content -Raw $path | ConvertFrom-Json }
    if (-not $cfg) { $cfg = [pscustomobject]@{} }
    $node = $cfg
    foreach ($p in $propChain) { $node = Get-OrCreateProp $node $p }
    Set-Prop $node 'fbmcp' $entry
    return $cfg
}
function New-ServerEntry([string]$pw) {
    return [pscustomobject]@{
        command = (Join-Path $DistDir 'fbmcp.exe')
        args    = @('-config', $ConfigPath)
        env     = [pscustomobject]@{ FBMCP_DEV_PW = $pw }
    }
}

Step "Configuring clients: $($Clients -join ', ')"
$entry = New-ServerEntry $DevPw
foreach ($client in $Clients) {
    switch ($client) {
        'zcode' {
            $path = Join-Path $RepoDir '.zcode\config.json'
            if ($PSCmdlet.ShouldProcess($path, 'merge mcp.servers.fbmcp')) {
                Save-Json (Merge-ServerEntry $path @('mcp', 'servers') $entry) $path
            }
        }
        'claude' {
            $path = Join-Path $env:APPDATA 'Claude\claude_desktop_config.json'
            if ($PSCmdlet.ShouldProcess($path, 'merge mcpServers.fbmcp')) {
                Save-Json (Merge-ServerEntry $path @('mcpServers') $entry) $path
            }
        }
        'claude-code' {
            $path = Join-Path $RepoDir '.mcp.json'
            if ($PSCmdlet.ShouldProcess($path, 'merge mcpServers.fbmcp')) {
                Save-Json (Merge-ServerEntry $path @('mcpServers') $entry) $path
            }
        }
    }
}

if ($SetDevPw) {
    if ($PSCmdlet.ShouldProcess('user environment', 'set FBMCP_DEV_PW')) {
        [Environment]::SetEnvironmentVariable('FBMCP_DEV_PW', $DevPw, 'User')
        Write-Host '    FBMCP_DEV_PW set at user level (new processes only)'
    }
} else {
    Write-Host '    FBMCP_DEV_PW written only into client env blocks (persistent user var: -SetDevPw)'
}

# --- Verify --------------------------------------------------------------------
Step 'Verify (fbmcpctl doctor)'
$env:FBMCP_DEV_PW = $DevPw
& (Join-Path $DistDir 'fbmcpctl.exe') doctor $ConfigPath
if ($LASTEXITCODE -ne 0) { throw "fbmcpctl doctor failed (exit $LASTEXITCODE)" }

Write-Host ''
Write-Host 'Done. Next steps:' -ForegroundColor Green
Write-Host "  - Restart MCP clients; they load servers only at startup."
if ($stateDir) { Write-Host "  - Tier >= 2 confirmations stay out-of-band: dist\fbmcpctl.exe approve $stateDir <request_id> (or the tray popup)." }
Write-Host "  - Status check: dist\fbmcpctl.exe status $ConfigPath"

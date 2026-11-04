# fbmcp release (ADR-038): build, Authenticode, tag. Nothing is published
# here: goafts' release manager takes the files from dist\fbmcp-<ver>\.
#
# Usage:
#   .\scripts\make-release.ps1 -Stage build [-Version 2027.2.20] [-SkipTests] [-DryRun]
#   .\scripts\make-release.ps1 -Stage finish -Signed dist\fbmcp-<ver>-signed.zip [-AllowTestCert] [-DryRun] [-Force]
#
# goafts ops/hqbird2027/Finish-HqbirdRelease.ps1 (step F3b, -FbmcpVersion) runs
# both stages with its signing round in between; -Yes is accepted for it.
#
# -Stage build
#   1. Pre-flight: no dirty files but VERSION, CHANGELOG.md and this script.
#   2. Version: -Version (written to VERSION) or VERSION. YYYY.N.N, above
#      the last v* tag, no tag of that name yet, and a "## [<ver>]" heading
#      in CHANGELOG.md.
#   3. go vet + go test ./... (-SkipTests skips the tests; no -race: no gcc).
#   4. Builds dist\fbmcp-<ver>\ (CGO off, -trimpath,
#      -ldflags "-s -w -X main.version=<ver>"): fbmcp-windows-amd64.exe,
#      fbmcp-linux-amd64, fbmcp-linux-arm64, sbom-*.txt.
#   5. Checks "fbmcp-windows-amd64.exe version" = {"version":"<ver>"}.
#   6. Writes dist\fbmcp-<ver>-tosign\ and its zip (the exe, manifest.txt,
#      sign.cmd) for the signing machine. manifest.txt has the format of
#      fbagent's (fbagent-tosign 1), so goafts' signing pipeline takes it
#      too. Nothing is committed.
#
# -Stage finish -Signed ZIP
#   Checks the signed zip against this build: the same manifest, HEAD and
#   tracked changes; Authenticode Valid, a thumbprint in
#   scripts\authenticode.pin, a timestamp; nothing changed but the signature
#   (PE bytes, go version -m). Then puts the signed exe in dist\fbmcp-<ver>\,
#   writes SHA256SUMS, commits VERSION and CHANGELOG.md ("release <ver>"),
#   tags v<ver> and pushes both.
#   -AllowTestCert also accepts scripts\authenticode-test.pin and no
#   timestamp. Such a run is a TEST run: the exe go to
#   dist\fbmcp-<ver>-TESTSIGN, nothing is committed, tagged or pushed.
#
# -DryRun prints the changing steps instead of running them; the checks run.

param(
    [ValidateSet("build", "finish")]
    [string]$Stage = "build",
    [Alias("NewVersion")]
    [string]$Version = "",
    [string]$Signed = "",
    [switch]$AllowTestCert,
    [switch]$SkipTests,
    [switch]$DryRun,
    [switch]$Force,
    [switch]$Yes
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
$DistDir = Join-Path $Root "dist"
$PinFile = Join-Path $PSScriptRoot "authenticode.pin"
$TestPinFile = Join-Path $PSScriptRoot "authenticode-test.pin"
$VersionFile = Join-Path $Root "VERSION"
$ChangelogFile = Join-Path $Root "CHANGELOG.md"
$WindowsExe = "fbmcp-windows-amd64.exe"
$Targets = [ordered]@{
    "fbmcp-windows-amd64.exe" = @("windows", "amd64")
    "fbmcp-linux-amd64"       = @("linux", "amd64")
    "fbmcp-linux-arm64"       = @("linux", "arm64")
}
$TestRun = [bool]$AllowTestCert

function Say([string]$msg) { Write-Host $msg }
function Step([string]$label) { Write-Host "`n==> $label" -ForegroundColor Cyan }
function Warn([string]$msg) { Write-Host "WARN: $msg" -ForegroundColor Yellow }
function Stop-Release([string]$msg) {
    Write-Host "ERROR: $msg" -ForegroundColor Red
    Pop-Location
    exit 1
}

# Runs a changing step, or prints it with -DryRun.
function Invoke-Step([string]$label, [scriptblock]$block) {
    Step $label
    if ($DryRun) { Say "  (dry run: skipped)"; return }
    $global:LASTEXITCODE = 0
    & $block
    if ($LASTEXITCODE -ne 0) { Stop-Release "$label failed (exit $LASTEXITCODE)." }
}

# Runs a native command; stderr does not stop the script (PowerShell 5.1).
function Invoke-Native([string]$exe, [string[]]$argv) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    $out = & $exe @argv 2>&1
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev
    return [pscustomobject]@{ Code = $code; Out = (@($out | ForEach-Object { "$_" }) -join "`n") }
}

if ($Stage -eq "finish" -and $Signed -eq "") { Write-Host "ERROR: -Stage finish needs -Signed <fbmcp-<ver>-signed.zip>." -ForegroundColor Red; exit 2 }
if ($Stage -eq "build" -and ($Signed -ne "" -or $AllowTestCert)) { Write-Host "ERROR: -Signed and -AllowTestCert are for -Stage finish." -ForegroundColor Red; exit 2 }

Push-Location $Root

# ---------------------------------------------------------------------------
# Helpers (copied from fbagent scripts/make-release.ps1; the same token signs
# both). The Test-* functions return $null when all is well, else the problem.
# ---------------------------------------------------------------------------
function Get-Sha256([string]$path) {
    return (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash.ToLowerInvariant()
}

# sha256 of the tracked changes against HEAD, as goafts Finish-HqbirdRelease.ps1
# computes it (stdout only: git's CRLF notes on stderr are not part of it).
function Get-TreeDiffHash {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    $diff = (git -c core.safecrlf=false diff HEAD --binary 2>$null) -join "`n"
    $ErrorActionPreference = $prev
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($diff)
    return (($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString("x2") }) -join "")
}

function Get-GoBuildInfo([string]$path) {
    $r = Invoke-Native "go" @("version", "-m", $path)
    if ($r.Code -ne 0 -or -not $r.Out) { return $null }
    $lines = @($r.Out -split "`n")
    $i = $lines[0].LastIndexOf(": ")
    if ($i -ge 0) { $lines[0] = $lines[0].Substring($i + 2) }
    return ($lines -join "`n")
}

function Read-PinFile([string]$path) {
    if (-not (Test-Path -LiteralPath $path)) { return @() }
    $pins = @()
    foreach ($line in Get-Content -LiteralPath $path) {
        $t = ($line -replace '#.*$', '') -replace '\s', ''
        if ($t -eq "") { continue }
        if ($t -notmatch '^[0-9A-Fa-f]{40}$') { throw "$path`: '$t' is not a SHA1 thumbprint (40 hex)" }
        $pins += $t.ToUpperInvariant()
    }
    return $pins
}

function Get-PeSecurityInfo([byte[]]$b) {
    if ($b.Length -lt 0x40 -or $b[0] -ne 0x4D -or $b[1] -ne 0x5A) { throw "not a PE file (no MZ header)" }
    $pe = [BitConverter]::ToInt32($b, 0x3C)
    if ($pe -lt 0 -or $pe + 24 -gt $b.Length -or [BitConverter]::ToUInt32($b, $pe) -ne 0x00004550) { throw "not a PE file (no PE signature)" }
    $opt = $pe + 24
    $magic = [BitConverter]::ToUInt16($b, $opt)
    if ($magic -eq 0x20b) { $dd = $opt + 112 } elseif ($magic -eq 0x10b) { $dd = $opt + 96 } else { throw ("unknown PE optional header magic 0x{0:x}" -f $magic) }
    if ([BitConverter]::ToUInt32($b, $dd - 4) -lt 5) { throw "PE file has no security directory entry" }
    $sec = $dd + 4 * 8
    return [pscustomobject]@{
        ChecksumAt = $opt + 64
        SecDirAt   = $sec
        CertOffset = [BitConverter]::ToUInt32($b, $sec)
        CertSize   = [BitConverter]::ToUInt32($b, $sec + 4)
    }
}

# Signing may change only the PE checksum, the security directory entry, up
# to 7 zero bytes of padding and the certificate table at the end.
function Test-PeUnsignedPartSame([string]$Original, [string]$SignedFile) {
    try {
        $a = [System.IO.File]::ReadAllBytes($Original)
        $s = [System.IO.File]::ReadAllBytes($SignedFile)
        $ia = Get-PeSecurityInfo $a
        $is = Get-PeSecurityInfo $s
    } catch {
        return "cannot read the PE headers: $($_.Exception.Message)"
    }
    if ($ia.CertSize -ne 0) { return "the original exe already has a certificate table" }
    if ($is.CertSize -eq 0 -or $is.CertOffset -eq 0) { return "the signed exe has no certificate table" }
    if ($ia.ChecksumAt -ne $is.ChecksumAt -or $ia.SecDirAt -ne $is.SecDirAt) { return "the PE headers moved" }
    if ([uint64]$is.CertOffset + $is.CertSize -ne [uint64]$s.Length) { return "the signed exe has data after its certificate table" }
    if ($is.CertOffset -lt $a.Length) { return "the certificate table starts inside the original image" }
    $pad = $is.CertOffset - $a.Length
    if ($pad -gt 7) { return "$pad bytes between the original image and the certificate table" }
    for ($i = $a.Length; $i -lt $is.CertOffset; $i++) {
        if ($s[$i] -ne 0) { return "padding before the certificate table is not zero" }
    }
    foreach ($buf in @($a, $s)) {
        for ($i = 0; $i -lt 4; $i++) { $buf[$ia.ChecksumAt + $i] = 0 }
        for ($i = 0; $i -lt 8; $i++) { $buf[$ia.SecDirAt + $i] = 0 }
    }
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $ha = [BitConverter]::ToString($sha.ComputeHash($a, 0, $a.Length))
    $hs = [BitConverter]::ToString($sha.ComputeHash($s, 0, $a.Length))
    if ($ha -ne $hs) { return "the signed exe differs from the original outside the signature" }
    return $null
}

function Test-AuthenticodeSigned([string]$path, [string[]]$pins, [bool]$test) {
    $sig = Get-AuthenticodeSignature -LiteralPath $path
    $ok = $sig.Status -eq "Valid"
    if (-not $ok -and $test -and ("$($sig.Status)" -in @("UnknownError", "NotTrusted")) -and $sig.SignerCertificate) {
        Warn "$(Split-Path -Leaf $path): Authenticode status $($sig.Status); accepted for a TEST run."
        $ok = $true
    }
    if (-not $ok) { return "Authenticode status $($sig.Status): $($sig.StatusMessage)" }
    $thumb = "$($sig.SignerCertificate.Thumbprint)".ToUpperInvariant()
    if ($thumb -notin $pins) { return "signed by $thumb ($($sig.SignerCertificate.Subject)), which is not a pinned thumbprint" }
    if (-not $sig.TimeStamperCertificate) {
        if (-not $test) { return "the signature has no timestamp (signtool sign /tr ... /td sha256)" }
        Warn "$(Split-Path -Leaf $path): no timestamp; accepted for a TEST run."
    }
    return $null
}

function Read-ToSignManifest([string]$path) {
    $m = [ordered]@{ Lines = @(Get-Content -LiteralPath $path); Files = [ordered]@{} }
    if ($m.Lines.Count -eq 0 -or $m.Lines[0] -ne "format fbagent-tosign 1") { throw "$path is not a to-sign manifest (format fbagent-tosign 1)" }
    foreach ($line in $m.Lines | Select-Object -Skip 1) {
        $f = $line -split ' '
        switch ($f[0]) {
            "file" {
                if ($f.Count -ne 4 -or $f[1] -notmatch '^[A-Za-z0-9._-]+$' -or $f[2] -notmatch '^[0-9a-f]{64}$') { throw "$path`: bad line '$line'" }
                $m.Files[$f[1]] = [pscustomobject]@{ Sha256 = $f[2]; Size = [int64]$f[3] }
            }
            default { if ($f.Count -ge 2) { $m[$f[0]] = $f[1] } }
        }
    }
    return $m
}

# sign.cmd for the signing machine: the fbagent one with fbmcp names. It reads
# THUMB and TSA from sign.local.cmd on that machine, never from the zip.
function New-SignCmd([string]$ver, [string[]]$exes) {
    $text = @'
@echo off
rem Authenticode signing of the fbmcp @VER@ Windows exe.
rem Written by fbmcp scripts\make-release.ps1 -Stage build; run on the signing machine.
rem
rem   1. Compare the sha256 of fbmcp-@VER@-tosign.zip with the one
rem      make-release.ps1 printed. Unzip it into an empty folder.
rem   2. Run sign.cmd from that folder. The token asks for its PIN.
rem   3. Take fbmcp-@VER@-signed.zip back to the release machine:
rem        .\scripts\make-release.ps1 -Stage finish -Signed <zip>
rem
rem sign.local.cmd (NOT in the zip; the same file fbagent uses) sets:
rem   set THUMB=<SHA1 thumbprint of the code-signing certificate on the token>
rem   set TSA=<RFC 3161 timestamp URL>        (none: no timestamp, lab only)
rem   set SIGNTOOL=<full path of signtool.exe> (optional, else from PATH)
rem It is taken from %SIGN_LOCAL%, else from the folder above this one,
rem else from %USERPROFILE%.
setlocal EnableExtensions
cd /d "%~dp0" || exit /b 1

set "LOCALCFG="
if defined SIGN_LOCAL if exist "%SIGN_LOCAL%" set "LOCALCFG=%SIGN_LOCAL%"
if not defined LOCALCFG if exist "%~dp0..\sign.local.cmd" set "LOCALCFG=%~dp0..\sign.local.cmd"
if not defined LOCALCFG if exist "%USERPROFILE%\sign.local.cmd" set "LOCALCFG=%USERPROFILE%\sign.local.cmd"
if not defined LOCALCFG goto :nolocal
set "THUMB="
set "TSA="
set "SIGNTOOL="
call "%LOCALCFG%"
if not defined THUMB goto :nolocal
if not defined TSA goto :nolocal
if not defined SIGNTOOL set "SIGNTOOL=signtool.exe"
echo Settings: %LOCALCFG%
echo Thumbprint: %THUMB%
echo TSA: %TSA%

set "TSARGS=/tr %TSA% /td sha256"
set "TWARG=/tw"
if /i "%TSA%"=="none" set "TSARGS="
if /i "%TSA%"=="none" set "TWARG="

if not exist manifest.txt goto :nomanifest
findstr /b /x /c:"format fbagent-tosign 1" manifest.txt >nul || goto :nomanifest
if exist signed rmdir /s /q signed
if exist signed goto :fail
mkdir signed || goto :fail
copy /y manifest.txt signed\manifest.txt >nul || goto :fail

set "FAIL="
set "COUNT=0"
for /f "usebackq tokens=1-4" %%a in ("manifest.txt") do if "%%a"=="file" if not defined FAIL call :one "%%b" "%%c"
if defined FAIL goto :fail
if "%COUNT%"=="0" goto :nomanifest

> signed\sign-report.txt echo format fbagent-signed 1
>> signed\sign-report.txt echo version @VER@
>> signed\sign-report.txt echo thumbprint %THUMB%
>> signed\sign-report.txt echo tsa %TSA%
>> signed\sign-report.txt echo signed_at %DATE% %TIME%
>> signed\sign-report.txt echo computer %COMPUTERNAME%
>> signed\sign-report.txt echo files %COUNT%

if exist "fbmcp-@VER@-signed.zip" del /q "fbmcp-@VER@-signed.zip"
"%SystemRoot%\System32\tar.exe" -a -c -f "fbmcp-@VER@-signed.zip" -C signed manifest.txt sign-report.txt @FILES@
if errorlevel 1 goto :fail
echo.
echo Done: %~dp0fbmcp-@VER@-signed.zip
certutil -hashfile "fbmcp-@VER@-signed.zip" SHA256
exit /b 0

:one
set "F=%~1"
set "WANT=%~2"
if not exist "%F%" (
  echo ERROR: %F% is missing
  set "FAIL=1"
  exit /b 1
)
set "GOT="
for /f "skip=1 delims=" %%h in ('certutil -hashfile "%F%" SHA256') do if not defined GOT set "GOT=%%h"
if not defined GOT (
  echo ERROR: certutil gave no sha256 for %F%
  set "FAIL=1"
  exit /b 1
)
set "GOT=%GOT: =%"
if /i not "%GOT%"=="%WANT%" (
  echo ERROR: sha256 of %F% is %GOT%, manifest.txt says %WANT%
  set "FAIL=1"
  exit /b 1
)
copy /y "%F%" "signed\%F%" >nul || (
  set "FAIL=1"
  exit /b 1
)
echo.
echo Signing %F% ...
"%SIGNTOOL%" sign /sha1 %THUMB% /fd sha256 %TSARGS% "signed\%F%"
if errorlevel 1 (
  echo ERROR: signtool sign failed for %F%
  set "FAIL=1"
  exit /b 1
)
"%SIGNTOOL%" verify /pa /all %TWARG% "signed\%F%"
if errorlevel 1 (
  echo ERROR: signtool verify failed for %F%
  set "FAIL=1"
  exit /b 1
)
set /a COUNT+=1
exit /b 0

:nolocal
echo ERROR: no usable sign.local.cmd: it must set THUMB and TSA. See the top of sign.cmd.
exit /b 1

:nomanifest
echo ERROR: manifest.txt is missing, is not a to-sign manifest, or lists no file.
exit /b 1

:fail
echo ERROR: signing stopped. Nothing usable was written.
if exist "fbmcp-@VER@-signed.zip" del /q "fbmcp-@VER@-signed.zip"
exit /b 1
'@
    return $text.Replace("@VER@", $ver).Replace("@FILES@", ($exes -join " "))
}

# The highest v<YYYY.N.N> tag, or $null.
function Get-LastTagVersion {
    $r = Invoke-Native "git" @("tag", "--list", "v*")
    $best = $null
    foreach ($t in ($r.Out -split "`n")) {
        $t = $t.Trim()
        if ($t -notmatch '^v(\d+\.\d+\.\d+)$') { continue }
        $v = [version]$Matches[1]
        if ($null -eq $best -or $v -gt $best) { $best = $v }
    }
    return $best
}

# ---------------------------------------------------------------------------
# 1. Pre-flight and version
# ---------------------------------------------------------------------------
Step "Pre-flight check"
$allowedDirty = @("VERSION", "CHANGELOG.md", "scripts/make-release.ps1")
$unexpected = @()
foreach ($line in ((Invoke-Native "git" @("status", "--porcelain")).Out -split "`n")) {
    if ($line.Length -lt 4) { continue }
    $path = $line.Substring(3).Trim().Trim('"')
    if ($path -notin $allowedDirty) { $unexpected += $path }
}
if ($unexpected.Count -gt 0) {
    Stop-Release "the tree has changes a release must not take in: $($unexpected -join ', '). Commit or stash them first."
}
$headSha = (Invoke-Native "git" @("rev-parse", "HEAD")).Out.Trim()
Say "HEAD $headSha, clean."

$fileVersion = ""
if (Test-Path -LiteralPath $VersionFile) { $fileVersion = (Get-Content -LiteralPath $VersionFile -TotalCount 1).Trim() }
if ($Version -ne "") {
    $targetVersion = $Version.Trim()
    if ($Stage -eq "finish" -and $targetVersion -ne $fileVersion) { Stop-Release "-Version $targetVersion, but VERSION says '$fileVersion' (-Stage finish takes the version -Stage build wrote)." }
} elseif ($fileVersion -ne "") {
    $targetVersion = $fileVersion
} else {
    Stop-Release "no VERSION file; pass -Version."
}
if ($targetVersion -notmatch '^\d{4}\.\d+\.\d+$') { Stop-Release "version '$targetVersion' is not YYYY.N.N (ADR-038)." }
if ($Stage -eq "build") {
    $last = Get-LastTagVersion
    if ($null -ne $last -and [version]$targetVersion -le $last) { Stop-Release "version $targetVersion is not above the last tag v$last." }
    if ((Invoke-Native "git" @("rev-parse", "-q", "--verify", "refs/tags/v$targetVersion")).Code -eq 0) { Stop-Release "tag v$targetVersion exists." }
}
if (-not (Test-Path -LiteralPath $ChangelogFile) -or -not (Select-String -LiteralPath $ChangelogFile -SimpleMatch "## [$targetVersion]" -Quiet)) {
    Stop-Release "CHANGELOG.md has no '## [$targetVersion] - <date>' heading."
}
Say "Version $targetVersion."
$outDir = Join-Path $DistDir "fbmcp-$targetVersion"

# ---------------------------------------------------------------------------
# -Stage build
# ---------------------------------------------------------------------------
if ($Stage -eq "build") {
    if ($targetVersion -ne $fileVersion) {
        Invoke-Step "Write VERSION = $targetVersion" {
            [System.IO.File]::WriteAllText($VersionFile, "$targetVersion`n")
        }
    }

    Step "go vet ./..."
    $r = Invoke-Native "go" @("vet", "./...")
    if ($r.Code -ne 0) { Say $r.Out; Stop-Release "go vet failed." }
    if ($SkipTests) {
        Warn "Tests skipped (-SkipTests)."
    } else {
        Step "go test ./... (a few minutes)"
        $r = Invoke-Native "go" @("test", "./...")
        if ($r.Code -ne 0) { Say $r.Out; Stop-Release "go test failed." }
        Say "Tests passed."
    }

    Invoke-Step "Build $outDir" {
        if (Test-Path -LiteralPath $outDir) { Remove-Item -LiteralPath $outDir -Recurse -Force }
        New-Item -ItemType Directory -Path $outDir -Force | Out-Null
        $oldEnv = @($env:GOOS, $env:GOARCH, $env:CGO_ENABLED)
        try {
            foreach ($name in $Targets.Keys) {
                $env:GOOS, $env:GOARCH, $env:CGO_ENABLED = $Targets[$name][0], $Targets[$name][1], "0"
                $out = Join-Path $outDir $name
                $r = Invoke-Native "go" @("build", "-trimpath", "-ldflags=-s -w -X main.version=$targetVersion", "-o", $out, "./cmd/fbmcp")
                if ($r.Code -ne 0) { Say $r.Out; Stop-Release "build of $name failed." }
                Say "  $name  $(Get-Sha256 $out)"
            }
        } finally {
            $env:GOOS, $env:GOARCH, $env:CGO_ENABLED = $oldEnv
        }
        foreach ($name in $Targets.Keys) {
            $bi = Get-GoBuildInfo (Join-Path $outDir $name)
            if (-not $bi) { Stop-Release "go version -m cannot read $name." }
            Set-Content -LiteralPath (Join-Path $outDir "sbom-$name.txt") -Value $bi -Encoding ASCII
        }
        # fbmcp's own version command (not fbagent's: that one starts the agent).
        $r = Invoke-Native (Join-Path $outDir $WindowsExe) @("version")
        if ($r.Out.Trim() -ne "{""version"":""$targetVersion""}") { Stop-Release "$WindowsExe version printed '$($r.Out)'." }
        Say "  $WindowsExe version -> $($r.Out.Trim())"
        $global:LASTEXITCODE = 0
    }

    $exportDir = Join-Path $DistDir "fbmcp-$targetVersion-tosign"
    $exportZip = "$exportDir.zip"
    Invoke-Step "Export the Windows exe for signing: $exportZip" {
        if (Test-Path -LiteralPath $exportDir) { Remove-Item -LiteralPath $exportDir -Recurse -Force }
        if (Test-Path -LiteralPath $exportZip) { Remove-Item -LiteralPath $exportZip -Force }
        New-Item -ItemType Directory -Path $exportDir -Force | Out-Null
        $src = Join-Path $outDir $WindowsExe
        Copy-Item -LiteralPath $src -Destination (Join-Path $exportDir $WindowsExe)
        $manifest = @(
            "format fbagent-tosign 1",
            "build $([guid]::NewGuid().ToString('N'))",
            "version $targetVersion",
            "commit $headSha",
            "tree $(Get-TreeDiffHash)",
            "created $((Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'))",
            "file $WindowsExe $(Get-Sha256 $src) $((Get-Item -LiteralPath $src).Length)"
        )
        Set-Content -LiteralPath (Join-Path $exportDir "manifest.txt") -Value $manifest -Encoding ASCII
        Set-Content -LiteralPath (Join-Path $exportDir "sign.cmd") -Value (New-SignCmd $targetVersion @($WindowsExe)) -Encoding ASCII
        Compress-Archive -Path (Join-Path $exportDir "*") -DestinationPath $exportZip
        $global:LASTEXITCODE = 0
    }

    Step "Next: Authenticode on the signing machine"
    Say "Version: $targetVersion (built and tested; NOT committed or tagged)"
    if (-not $DryRun) { Say "Zip:     $exportZip"; Say "sha256:  $(Get-Sha256 $exportZip)" }
    Say "  1. Carry the zip to the signing machine; compare its sha256."
    Say "  2. Unzip into an empty folder and run sign.cmd."
    Say "  3. Bring back fbmcp-$targetVersion-signed.zip, then here:"
    Say "       .\scripts\make-release.ps1 -Stage finish -Signed <path>\fbmcp-$targetVersion-signed.zip"
    Say "  Do not change the tree or HEAD until then."
    Pop-Location
    exit 0
}

# ---------------------------------------------------------------------------
# -Stage finish
# ---------------------------------------------------------------------------
Step "Check the signed exe: $Signed"
if ($TestRun) { Warn "-AllowTestCert: a TEST run. Nothing is committed, tagged or pushed." }
if (-not (Test-Path -LiteralPath $Signed)) { Stop-Release "$Signed does not exist." }
$exportDir = Join-Path $DistDir "fbmcp-$targetVersion-tosign"
$localManifestPath = Join-Path $exportDir "manifest.txt"
if (-not (Test-Path -LiteralPath $localManifestPath)) { Stop-Release "$localManifestPath is missing: run -Stage build for $targetVersion first." }
$pins = @(Read-PinFile $PinFile)
if ($TestRun) { $pins += @(Read-PinFile $TestPinFile) }
if ($pins.Count -eq 0) { Stop-Release "no pinned thumbprint in $PinFile." }

$signedDir = Join-Path $DistDir "fbmcp-$targetVersion-signed"
if (Test-Path -LiteralPath $signedDir) { Remove-Item -LiteralPath $signedDir -Recurse -Force }
try { Expand-Archive -LiteralPath $Signed -DestinationPath $signedDir } catch { Stop-Release "cannot unzip ${Signed}: $($_.Exception.Message)" }
try { $local = Read-ToSignManifest $localManifestPath } catch { Stop-Release $_.Exception.Message }
$signedManifest = Join-Path $signedDir "manifest.txt"
if (-not (Test-Path -LiteralPath $signedManifest)) { Stop-Release "the signed zip has no manifest.txt." }
if ((@(Get-Content -LiteralPath $signedManifest) -join "`n") -ne ($local.Lines -join "`n")) { Stop-Release "the manifest in the signed zip is not the one of this build." }
if ($local["version"] -ne $targetVersion) { Stop-Release "the build is version $($local['version']), VERSION says $targetVersion." }
if ($local["commit"] -ne $headSha) { Stop-Release "the build was made on commit $($local['commit']), HEAD is $headSha." }
if ($local["tree"] -ne (Get-TreeDiffHash)) {
    if (-not $Force) { Stop-Release "tracked files changed since -Stage build. -Force to go on anyway." }
    Warn "-Force: tracked files changed since -Stage build."
}
$expected = @("manifest.txt", "sign-report.txt", $WindowsExe)
$extra = @(Get-ChildItem -LiteralPath $signedDir -Recurse -File | ForEach-Object { $_.FullName.Substring($signedDir.Length + 1) } | Where-Object { $_ -notin $expected })
if ($extra.Count -gt 0) { Stop-Release "the signed zip has files that are not in the manifest: $($extra -join ', ')" }
if ($local.Files.Count -ne 1 -or -not $local.Files.Contains($WindowsExe)) { Stop-Release "the manifest must list only $WindowsExe." }

$orig = Join-Path $outDir $WindowsExe
$signedExe = Join-Path $signedDir $WindowsExe
if (-not (Test-Path -LiteralPath $orig)) { Stop-Release "$orig is missing." }
if ((Get-Sha256 $orig) -ne $local.Files[$WindowsExe].Sha256) { Stop-Release "$orig is not the exe of this build." }
if (-not (Test-Path -LiteralPath $signedExe)) { Stop-Release "the signed zip has no $WindowsExe." }
$problem = Test-AuthenticodeSigned $signedExe $pins $TestRun
if ($problem) { Stop-Release "${WindowsExe}: $problem" }
$problem = Test-PeUnsignedPartSame $orig $signedExe
if ($problem) { Stop-Release "${WindowsExe}: $problem" }
$biOrig = Get-GoBuildInfo $orig
if (-not $biOrig -or (Get-GoBuildInfo $signedExe) -ne $biOrig) { Stop-Release "${WindowsExe}: go version -m of the signed exe differs from the original." }
$sig = Get-AuthenticodeSignature -LiteralPath $signedExe
Say ("  {0}  OK  {1}  {2}" -f $WindowsExe, $sig.SignerCertificate.Thumbprint, (Get-Sha256 $signedExe))

$installDir = $outDir
if ($TestRun) { $installDir = Join-Path $DistDir "fbmcp-$targetVersion-TESTSIGN" }
Invoke-Step "Put the signed exe in $installDir and write SHA256SUMS" {
    if ($TestRun) {
        if (Test-Path -LiteralPath $installDir) { Remove-Item -LiteralPath $installDir -Recurse -Force }
        Copy-Item -LiteralPath $outDir -Destination $installDir -Recurse
    }
    Copy-Item -LiteralPath $signedExe -Destination (Join-Path $installDir $WindowsExe) -Force
    $sums = foreach ($name in $Targets.Keys) { "$(Get-Sha256 (Join-Path $installDir $name))  $name" }
    Set-Content -LiteralPath (Join-Path $installDir "SHA256SUMS") -Value $sums -Encoding ASCII
    $sums | ForEach-Object { Say "  $_" }
    $global:LASTEXITCODE = 0
}
if ($TestRun) {
    Step "TEST run complete: nothing committed, tagged or pushed"
    Say "Test-signed files: $installDir. Never publish them."
    Pop-Location
    exit 0
}

Invoke-Step "Commit VERSION and CHANGELOG.md: release $targetVersion" {
    $r = Invoke-Native "git" @("add", "--", "VERSION", "CHANGELOG.md")
    if ($r.Code -ne 0) { Say $r.Out; Stop-Release "git add failed." }
    $staged = (Invoke-Native "git" @("diff", "--cached", "--name-only")).Out.Trim()
    if ($staged -ne "") {
        $r = Invoke-Native "git" @("commit", "-m", "release $targetVersion")
        if ($r.Code -ne 0) { Say $r.Out; Stop-Release "git commit failed." }
    } else {
        Say "  nothing to commit (VERSION and CHANGELOG.md are committed)"
    }
    $global:LASTEXITCODE = 0
}
Invoke-Step "Tag v$targetVersion and push" {
    foreach ($argv in @(@("tag", "-a", "v$targetVersion", "-m", "fbmcp $targetVersion"), @("push", "origin", "HEAD"), @("push", "origin", "v$targetVersion"))) {
        $r = Invoke-Native "git" $argv
        if ($r.Code -ne 0) { Say $r.Out; Stop-Release "git $($argv -join ' ') failed." }
    }
    $global:LASTEXITCODE = 0
}

Step "Done: fbmcp $targetVersion"
Say "Files: $outDir"
Say "Next (goafts): the release manager takes fbmcp from this folder; see goafts docs/fbmcp-install-plan.ru.md."
Pop-Location
exit 0

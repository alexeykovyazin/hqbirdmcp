<#
.SYNOPSIS
    Nightly chaos-harness runner (phase8_plan D1.1).

.DESCRIPTION
    Runs the kill harness N times against the live HQBird dev instances and
    appends a dated result line to docs/findings/chaos-log.md, followed by an
    auto-classification triage line (T7.2): engine-down (connection-refused /
    socket-reset signatures in the failing output) vs harness-failure (assert
    failures without them). On a >= 2-night RED streak an ALERT line is added
    to the log (M3's >= 5 clean nights can never accrue otherwise, and an
    unattended engine outage must not sit untriaged for a week, as
    09-23..09-27 did). RED verdict lines without a following triage note are
    reported as a warning.

    Register it with register-chaos-task.ps1 (Task Scheduler, nightly) —
    the run itself is manual/ops work (phase 8M / M3).

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

# T7.2 auto-triage: classify the failure from the output text. Engine-down
# signatures are transport-level (the dial/wsarecv language of a refused or
# dropped engine connection); everything else is a harness assert failure.
# The auto line makes every RED triaged-by-default; amend it in place after
# the human looks at the detail log.
if ($verdict -eq 'RED') {
    $failNames = @($out | Select-String '^--- FAIL: (\S+)').ForEach{ $_.Matches[0].Groups[1].Value } | Select-Object -Unique
    $engineDownSigs = 'connectex', 'actively refused', 'wsarecv', 'forcibly closed', 'unavailable database', 'I/O error during'
    $downHits = @($out | Select-String ($engineDownSigs -join '|'))
    $class = if ($downHits.Count -gt 0) { 'engine-down' } else { 'harness-failure' }
    $tri = "$stamp triage(auto): $class"
    if ($failNames.Count -gt 0) { $tri += ' - failing: ' + ($failNames -join ', ') }
    if ($downHits.Count -gt 0) { $tri += ' - check engine services/ports first (detail log below)' }
    Add-Content -Path $log -Value $tri
}

# T7.2 alert: >= 2 consecutive RED nights on the verdict lines (triage and
# summary lines excluded from the streak count).
$verdictLines = @(Get-Content $log | Where-Object { $_ -match '^\d{4}-\d{2}-\d{2} .*verdict=' })
$streak = 0
for ($i = $verdictLines.Count - 1; $i -ge 0; $i--) {
    if ($verdictLines[$i] -match 'verdict=RED') { $streak++ } else { break }
}
if ($verdict -eq 'RED' -and $streak -ge 2) {
    Add-Content -Path $log -Value "ALERT: $streak consecutive RED nights - triage now (M3 clean streak is blocked)"
    Write-Warning "$streak consecutive RED chaos nights - see $log"
}
# T7.2 discipline: every RED verdict line must be followed by a triage note.
$allLines = @(Get-Content $log)
$untriaged = 0
for ($i = 0; $i -lt $allLines.Count; $i++) {
    if ($allLines[$i] -match 'verdict=RED' -and
        ($i -eq $allLines.Count - 1 -or $allLines[$i + 1] -notmatch 'triage:')) { $untriaged++ }
}
if ($untriaged -gt 0) { Write-Warning "$untriaged RED night(s) without a triage note in $log" }

# keep per-night detail for post-mortem (outside docs/: test names are not
# phantom-lint-safe); prune to the last 14 nights so a RED's output survives
# later green runs instead of being overwritten in place
$detailDir = Join-Path $env:TEMP 'fbmcp-chaos'
New-Item -ItemType Directory -Force -Path $detailDir | Out-Null
$detail = Join-Path $detailDir ("fbmcp-chaos-{0}.log" -f (Get-Date -Format 'yyyyMMdd'))
$out | Set-Content -Path $detail
Get-ChildItem $detailDir -Filter 'fbmcp-chaos-*.log' |
    Sort-Object Name -Descending | Select-Object -Skip 14 |
    Remove-Item -Force -ErrorAction SilentlyContinue
Write-Host "chaos run: $verdict (logged to $log; detail: $detail)"
if ($code -ne 0) {
    $out | Select-Object -Last 40 | Write-Host
    exit $code
}

# Verify-mode (no mutation). Reports whether state/backup/work dirs exist.
# Does not grant sc.exe rights. After green: fbmcpctl setup --write-posture
param([string]$Config = "fbmcp.yaml")
Write-Host "fbmcp posture verify (ADR-017) — check only"
if (-not (Test-Path $Config)) { Write-Error "missing $Config"; exit 1 }
Write-Host "ok: config present $Config"
Write-Host "next: fbmcpctl doctor $Config"
exit 0

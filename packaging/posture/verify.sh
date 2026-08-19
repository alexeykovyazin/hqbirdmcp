#!/bin/sh
# Verify-mode (no mutation). ADR-017.
set -eu
CONFIG="${1:-fbmcp.yaml}"
echo "fbmcp posture verify (ADR-017) — check only"
test -f "$CONFIG" || { echo "missing $CONFIG"; exit 1; }
echo "ok: config present $CONFIG"
echo "sudoers template: packaging/posture/sudoers.fbmcp"
echo "next: fbmcpctl doctor $CONFIG"

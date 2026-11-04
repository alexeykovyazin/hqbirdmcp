#!/bin/sh
# install-remote.sh - install fbmcp in remote mode on Linux (phase 12 B10).
# Run as root from the fbmcp source folder or the release folder:
#   packaging/linux/install-remote.sh --allow-from 10.0.0.0/24 --port 8443 --dry-run
# then the same command without --dry-run. Steps: folders, certificate
# (fbmcpctl tls selfsign), first config + admin key (only when the config
# does not exist), systemd unit, sudoers for the Firebird unit, firewall
# (ufw), start, fbmcpctl doctor --remote.
set -eu

USER_NAME=firebird
PORT=8443
ALLOW_FROM=""
CONFIG=/etc/fbmcp/fbmcp.yaml
STATE_DIR=/var/lib/fbmcp
SECRETS_DIR=/etc/fbmcp/secrets
TLS_DIR=/etc/fbmcp/tls
BIN_DIR=/usr/local/bin
DNS_NAMES=""
IP_ADDRS=""
FB_UNITS=""
ADMIN_ID=admin
NO_FIREWALL=0
DRY=0

usage() {
	cat <<EOF
usage: $0 --allow-from CIDR[,CIDR] [--port 8443] [--user firebird]
          [--config /etc/fbmcp/fbmcp.yaml] [--state-dir /var/lib/fbmcp]
          [--secrets-dir /etc/fbmcp/secrets] [--tls-dir /etc/fbmcp/tls]
          [--dns NAMES] [--ip ADDRS] [--firebird-unit UNIT[,UNIT]]
          [--admin-identity admin] [--bin-dir /usr/local/bin]
          [--no-firewall] [--dry-run]
EOF
	exit 2
}

while [ $# -gt 0 ]; do
	case "$1" in
	--user) USER_NAME=$2; shift 2 ;;
	--port) PORT=$2; shift 2 ;;
	--allow-from) ALLOW_FROM=$2; shift 2 ;;
	--config) CONFIG=$2; shift 2 ;;
	--state-dir) STATE_DIR=$2; shift 2 ;;
	--secrets-dir) SECRETS_DIR=$2; shift 2 ;;
	--tls-dir) TLS_DIR=$2; shift 2 ;;
	--bin-dir) BIN_DIR=$2; shift 2 ;;
	--dns) DNS_NAMES=$2; shift 2 ;;
	--ip) IP_ADDRS=$2; shift 2 ;;
	--firebird-unit) FB_UNITS=$2; shift 2 ;;
	--admin-identity) ADMIN_ID=$2; shift 2 ;;
	--no-firewall) NO_FIREWALL=1; shift ;;
	--dry-run) DRY=1; shift ;;
	-h|--help) usage ;;
	*) echo "unknown option: $1" >&2; usage ;;
	esac
done

say() { printf '%s\n' "== $*"; }
die() { printf 'install-remote: %s\n' "$*" >&2; exit 1; }
# run prints the command; it runs it unless --dry-run.
run() {
	printf '+ %s\n' "$*"
	if [ "$DRY" -eq 0 ]; then "$@"; fi
}

[ -n "$ALLOW_FROM" ] || [ "$NO_FIREWALL" -eq 1 ] || die "--allow-from is required (the port must be open only to the admin network); use --no-firewall to manage the firewall yourself"
[ "$(id -u)" -eq 0 ] || [ "$DRY" -eq 1 ] || die "run as root"
id "$USER_NAME" >/dev/null 2>&1 || die "user $USER_NAME does not exist (R7: the Firebird user)"
[ -x "$BIN_DIR/fbmcp" ] || die "$BIN_DIR/fbmcp not found: install -m 0755 fbmcp fbmcpctl $BIN_DIR/"
[ -x "$BIN_DIR/fbmcpctl" ] || die "$BIN_DIR/fbmcpctl not found"
case "$PORT" in *[!0-9]*|"") die "bad --port $PORT" ;; esac
HERE=$(cd "$(dirname "$0")" && pwd)
UNIT_SRC="$HERE/fbmcp.service"
[ -f "$UNIT_SRC" ] || die "$UNIT_SRC not found"
CFG_DIR=$(dirname "$CONFIG")

if [ -z "$DNS_NAMES" ]; then DNS_NAMES=$(hostname); fi
if [ -z "$IP_ADDRS" ]; then IP_ADDRS=$(hostname -I 2>/dev/null | awk '{print $1}'); fi
LISTEN_IP=${IP_ADDRS%%,*}
[ -n "$LISTEN_IP" ] || die "cannot find an IP address; pass --ip"
if [ -z "$FB_UNITS" ]; then
	FB_UNITS=$(systemctl list-units --type=service --all --plain --no-legend 'firebird*' 2>/dev/null | awk '{print $1}' | sed 's/\.service$//' | paste -sd, -)
fi
# The first config needs one instance (the kernel refuses an empty list):
# take bin_dir from the first Firebird unit's ExecStart and the port from
# its firebird.conf.
FB_UNIT1=${FB_UNITS%%,*}
FB_BIN=/opt/firebird/bin
if [ -n "$FB_UNIT1" ]; then
	FB_EXE=$(systemctl show -p ExecStart --value "$FB_UNIT1" 2>/dev/null | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n 1)
	[ -n "$FB_EXE" ] && FB_BIN=$(dirname "$FB_EXE")
fi
FB_PORT=$(sed -n 's/^[[:space:]]*RemoteServicePort[[:space:]]*=[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$(dirname "$FB_BIN")/firebird.conf" 2>/dev/null | tail -n 1)
[ -n "$FB_PORT" ] || FB_PORT=3050

say "settings: user=$USER_NAME port=$PORT allow-from=${ALLOW_FROM:-<none>} listen=$LISTEN_IP:$PORT firebird-units=${FB_UNITS:-<none>} first-instance=localhost:$FB_PORT,$FB_BIN"
[ "$DRY" -eq 1 ] && say "dry run: nothing is changed"

say "1. folders"
run install -d -m 0750 -o root -g "$USER_NAME" "$CFG_DIR" "$TLS_DIR"
run install -d -m 0700 -o "$USER_NAME" -g "$USER_NAME" "$SECRETS_DIR" "$STATE_DIR" /var/lib/fbmcp-backups /var/lib/fbmcp-work

say "2. certificate"
if [ -f "$TLS_DIR/tls.crt" ] && [ -f "$TLS_DIR/tls.key" ]; then
	echo "keeping the existing pair in $TLS_DIR (renew: fbmcpctl tls selfsign --renew ...)"
	[ "$DRY" -eq 1 ] || "$BIN_DIR/fbmcpctl" tls show "$TLS_DIR/tls.crt"
else
	run "$BIN_DIR/fbmcpctl" tls selfsign --out "$TLS_DIR" --dns "$DNS_NAMES" --ip "$IP_ADDRS" --grant "$USER_NAME"
fi

say "3. config"
if [ -f "$CONFIG" ]; then
	echo "keeping $CONFIG (check listen, tls, secrets.dir and identities with doctor below)"
else
	KEY_ENV=FBMCP_KEY_$(printf '%s' "$ADMIN_ID" | tr '[:lower:]' '[:upper:]' | tr -c 'A-Z0-9\n' '_')
	echo "+ write $CONFIG (template with instance fb1; add databases:/agents: after the install)"
	if [ "$DRY" -eq 0 ]; then
		umask 027
		cat >"$CONFIG" <<EOF
server_name: $(hostname)
state:
  dir: $STATE_DIR
secrets:
  dir: $SECRETS_DIR
listen: $LISTEN_IP:$PORT
tls:
  cert: $TLS_DIR/tls.crt
  key: $TLS_DIR/tls.key
identities:
  - name: $ADMIN_ID
    key_env: $KEY_ENV
    max_tier: 2
allowed_origins:
  - https://fbmcp.invalid   # required; no browser client uses it
# databases: / agents: as in docs/remote.md section 3. Check fb1 below
# (found from the Firebird unit); bin_dir is the folder with gbak/gfix.
instances:
  - id: fb1
    addr: localhost:$FB_PORT
    bin_dir: $FB_BIN
    service: ${FB_UNIT1:-firebird}
    # service_user: SYSDBA
    # service_secret_env: FBMCP_SYSDBA_PW   # fbmcpctl secret set --dir $SECRETS_DIR FBMCP_SYSDBA_PW
databases: []
EOF
		chown root:"$USER_NAME" "$CONFIG"
		chmod 0640 "$CONFIG"
	fi
	run "$BIN_DIR/fbmcpctl" apikey new "$ADMIN_ID" --env "$KEY_ENV" --max-tier 2 --secrets-dir "$SECRETS_DIR"
fi

say "4. systemd unit"
UNIT_TMP=$(mktemp)
sed -e "s|^User=.*|User=$USER_NAME|" -e "s|^Group=.*|Group=$(id -gn "$USER_NAME")|" \
	-e "s|^ExecStart=.*|ExecStart=$BIN_DIR/fbmcp -config $CONFIG -serve|" \
	-e "s|^ReadWritePaths=-/var/lib/fbmcp |ReadWritePaths=-$STATE_DIR |" "$UNIT_SRC" >"$UNIT_TMP"
if [ "$DRY" -eq 1 ]; then grep -E '^(User|Group|ExecStart|ReadWritePaths)=' "$UNIT_TMP" | sed 's/^/  /'; fi
run install -m 0644 "$UNIT_TMP" /etc/systemd/system/fbmcp.service
rm -f "$UNIT_TMP"

say "5. sudoers (fb_service_* on the Firebird unit)"
if [ -n "$FB_UNITS" ]; then
	SUDO_TMP=$(mktemp)
	{
		echo "# fbmcp: start/stop/restart the Firebird unit(s) (install-remote.sh, R9)"
		OLDIFS=$IFS; IFS=,
		for u in $FB_UNITS; do
			echo "$USER_NAME ALL=(root) NOPASSWD: /bin/systemctl start $u, /bin/systemctl stop $u, /bin/systemctl restart $u"
		done
		IFS=$OLDIFS
	} >"$SUDO_TMP"
	sed 's/^/  /' "$SUDO_TMP"
	if [ "$DRY" -eq 0 ]; then
		visudo -c -f "$SUDO_TMP" >/dev/null || { rm -f "$SUDO_TMP"; die "sudoers check failed"; }
	fi
	run install -m 0440 "$SUDO_TMP" /etc/sudoers.d/fbmcp
	rm -f "$SUDO_TMP"
else
	echo "no firebird* unit found; fb_service_* will not work (pass --firebird-unit)"
fi

say "6. firewall"
if [ "$NO_FIREWALL" -eq 1 ]; then
	echo "skipped (--no-firewall): open TCP $PORT only to the admin network yourself"
elif command -v ufw >/dev/null 2>&1; then
	# not IFS=, here: run prints "$*", which joins with the first IFS char
	for c in $(printf '%s' "$ALLOW_FROM" | tr ',' ' '); do
		run ufw allow from "$c" to any port "$PORT" proto tcp
	done
	if ufw status | grep -q 'Status: inactive'; then
		echo "warning: ufw is inactive; the rules apply only after 'ufw enable' (allow ssh first)"
	fi
else
	echo "ufw not found: open TCP $PORT only from $ALLOW_FROM with your firewall tool (nft/firewalld)"
fi

say "7. start"
run systemctl daemon-reload
if systemctl is-active --quiet fbmcp; then
	run systemctl restart fbmcp
else
	run systemctl enable --now fbmcp
fi

say "8. doctor"
if [ "$DRY" -eq 0 ]; then
	sleep 2
	"$BIN_DIR/fbmcpctl" doctor --remote "$CONFIG" || {
		echo "doctor found problems; see: journalctl -u fbmcp -n 50" >&2
		exit 1
	}
else
	echo "+ $BIN_DIR/fbmcpctl doctor --remote $CONFIG"
fi

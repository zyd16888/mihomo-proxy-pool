#!/bin/sh
set -eu
umask 077
mkdir -p "$DATA_DIR"
/usr/local/bin/mihomo-manager -bootstrap
exec /sbin/tini -- /usr/bin/supervisord -n -c /etc/mihomo-manager/supervisord.conf

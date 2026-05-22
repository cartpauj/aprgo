#!/bin/sh
# Postremove — runs after files are deleted.
#
# We DO NOT touch /var/lib/aprgo here, on either remove OR purge. Same
# policy as postgresql / mariadb-server / etc.: the operator's data
# (state.json, aprgo.conf, db.sqlite, tls keys) sticks around until
# they explicitly delete it. Print a clear pointer.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

case "${1:-}" in
    purge|0)
        # Full uninstall path. dpkg passes "purge"; rpm passes "0".
        if [ -d /var/lib/aprgo ]; then
            echo "aprgo: /var/lib/aprgo/ left in place (config + database)."
            echo "       Remove manually if you no longer need it:"
            echo "         sudo rm -rf /var/lib/aprgo"
        fi
        ;;
esac

exit 0

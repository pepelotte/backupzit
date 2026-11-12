#!/bin/sh
# Creates the connector's own user and its configuration folder. The
# service is enabled but only started once the configuration is filled in.
set -e
if ! getent passwd backupzit-connector >/dev/null; then
    useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin backupzit-connector
fi
install -d -m 0750 -o root -g backupzit-connector /etc/backupzit-connector
if [ ! -e /etc/backupzit-connector/config.json ]; then
    install -m 0600 -o backupzit-connector -g backupzit-connector \
        /usr/share/backupzit-connector/config.example.json /etc/backupzit-connector/config.json
fi
systemctl daemon-reload >/dev/null 2>&1 || true
systemctl enable backupzit-connector.service >/dev/null 2>&1 || true
# An update restarts a running connector with the new version.
if systemctl is-active --quiet backupzit-connector.service; then
    systemctl restart backupzit-connector.service || true
fi

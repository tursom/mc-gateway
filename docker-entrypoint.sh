#!/bin/sh
set -eu

if [ "$(id -u)" = "0" ]; then
    mkdir -p /data
    chown -R mc-gateway:mc-gateway /data
    exec su-exec mc-gateway "$@"
fi

exec "$@"

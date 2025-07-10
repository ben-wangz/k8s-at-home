#! /bin/sh

ARIA2_HOST=${ARIA2_HOST:-aria2}
ARIA2_PORT=${ARIA2_PORT:-6800}
envsubst '${ARIA2_HOST},${ARIA2_PORT}' < /etc/nginx/nginx.conf | sponge /etc/nginx/nginx.conf

MAX_RETRIES=10
RETRY_INTERVAL=3
RETRY_COUNT=0
while ! nc -z ${ARIA2_HOST} ${ARIA2_PORT}; do
    if [ $RETRY_COUNT -ge $MAX_RETRIES ]; then
        echo "Error: aria2 service not available after $((MAX_RETRIES * RETRY_INTERVAL)) seconds" >&2
        exit 1
    fi
    echo "WARN: Waiting for aria2 service at ${ARIA2_HOST}:${ARIA2_PORT}... (retry $((RETRY_COUNT + 1))/$MAX_RETRIES)"
    sleep $RETRY_INTERVAL
    RETRY_COUNT=$((RETRY_COUNT + 1))
done

exec nginx -g "daemon off;"

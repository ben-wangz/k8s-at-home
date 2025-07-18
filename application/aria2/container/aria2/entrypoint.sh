#! /bin/sh

set -e

export ARIA2C_DHT_LISTEN_PORT=${ARIA2C_DHT_LISTEN_PORT:-6868}
export ARIA2C_LISTEN_PORT=${ARIA2C_LISTEN_PORT:-6868}
export ARIA2C_RPC_LISTEN_PORT=${ARIA2C_RPC_LISTEN_PORT:-6800}
export ARIA2_RUNTIME_DIR=${ARIA2_RUNTIME_DIR:-/opt/aria2/runtime}

export ARIA2C_NO_CONF=${ARIA2C_NO_CONF:-true}
export ARIA2C_LOG=${ARIA2C_LOG:--}
export ARIA2C_LOG_LEVEL=${ARIA2C_LOG_LEVEL:-info}
export ARIA2C_CONSOLE_LOG_LEVEL=${ARIA2C_CONSOLE_LOG_LEVEL:-info}
export ARIA2C_DIR=${ARIA2C_DIR:-/opt/aria2/downloads}
export ARIA2C_BT_DETACH_SEED_ONLY=${ARIA2C_BT_DETACH_SEED_ONLY:-true}
export ARIA2C_BT_FORCE_ENCRYPTION=${ARIA2C_BT_FORCE_ENCRYPTION:-true}
export ARIA2C_BT_REMOVE_UNSELECTED_FILE=${ARIA2C_BT_REMOVE_UNSELECTED_FILE:-true}
export ARIA2C_DHT_FILE_PATH=${ARIA2C_DHT_FILE_PATH:-/opt/aria2/runtime}
export ARIA2C_DHT_LISTEN_PORT=${ARIA2C_DHT_LISTEN_PORT:-6868}
export ARIA2C_LISTEN_PORT=${ARIA2C_LISTEN_PORT:-6868}
export ARIA2C_ENABLE_RPC=${ARIA2C_ENABLE_RPC:-true}
export ARIA2C_RPC_LISTEN_ALL=${ARIA2C_RPC_LISTEN_ALL:-true}
export ARIA2C_RPC_LISTEN_PORT=${ARIA2C_RPC_LISTEN_PORT:-6800}
export ARIA2C_RPC_SAVE_UPLOAD_METADATA=${ARIA2C_RPC_SAVE_UPLOAD_METADATA:-false}
if [ -z "$ARIA2C_RPC_SECRET" ]; then
    export ARIA2C_RPC_SECRET=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 16)
    echo "WARN: RPC_SECRET not set, use random string: ${ARIA2C_RPC_SECRET}"
fi
export ARIA2C_DISABLE_IPV6=${ARIA2C_DISABLE_IPV6:-true}
export ARIA2C_FILE_ALLOCATION=${ARIA2C_FILE_ALLOCATION:-none}
export ARIA2C_SAVE_SESSION=${ARIA2C_SAVE_SESSION:-true}
export ARIA2C_SAVE_SESSION_INTERVAL=${ARIA2C_SAVE_SESSION_INTERVAL:-60}
export ARIA2C_CONTINUE=${ARIA2C_CONTINUE:-true}

export OPTIONS=""
export OPTIONS_PRINTABLE=""
for ENV_VAR_WITH_PREFIX in $(env | cut -d= -f1 | grep '^ARIA2C_'); do
    ARG_NAME="--$(echo ${ENV_VAR_WITH_PREFIX#ARIA2C_} | tr '[:upper:]' '[:lower:]' | tr '_' '-')"
    eval "ARG_VALUE=\$$ENV_VAR_WITH_PREFIX"
    OPTIONS="${OPTIONS} ${ARG_NAME}=${ARG_VALUE}"
    if [ "--rpc-secret" == "${ARG_NAME}" ]; then
        OPTIONS_PRINTABLE="${OPTIONS_PRINTABLE} ${ARG_NAME}=********"
    else
        OPTIONS_PRINTABLE="${OPTIONS_PRINTABLE} ${ARG_NAME}=${ARG_VALUE}"
    fi
done

echo "OPTIONS: ${OPTIONS_PRINTABLE}"
exec aria2c ${OPTIONS}

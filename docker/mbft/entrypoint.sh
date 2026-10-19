#!/bin/sh
set -e
set -f

DATADIR="/data"
GENESIS="/genesis.json"
KEYSTORE_SRC="/config/keystore"
PASSWORD_FILE="/config/password.txt"
NODEKEY_FILE="/config/nodekey"

# 1. Initialize genesis if datadir is empty
if [ ! -d "$DATADIR/geth/chaindata" ]; then
    echo "[mBFT] Initializing blockchain with genesis: $GENESIS"
    geth init --datadir "$DATADIR" "$GENESIS"
fi

# 2. Ensure keystore is copied into datadir
if [ -d "$KEYSTORE_SRC" ]; then
    mkdir -p "$DATADIR/keystore"
    cp -r "$KEYSTORE_SRC/"* "$DATADIR/keystore/" 2>/dev/null || true
fi

# 3. Assemble Geth command arguments
GETH_ARGS="--datadir $DATADIR \
    --networkid ${NETWORK_ID:-1337} \
    --port ${P2P_PORT:-30303} \
    --http \
    --http.addr 0.0.0.0 \
    --http.port ${RPC_PORT:-8545} \
    --http.api eth,net,web3,miner,mbft \
    --http.corsdomain * \
    --http.vhosts * \
    --ws \
    --ws.addr 0.0.0.0 \
    --ws.port ${WS_PORT:-8546} \
    --ws.api eth,net,web3,miner,mbft \
    --ws.origins * \
    --nodiscover"

if [ -f "$NODEKEY_FILE" ]; then
    GETH_ARGS="$GETH_ARGS --nodekey $NODEKEY_FILE"
fi

if [ -n "$ETHERBASE" ]; then
    GETH_ARGS="$GETH_ARGS --miner.pending.feeRecipient $ETHERBASE"
fi

if [ -n "$BOOTNODES" ]; then
    GETH_ARGS="$GETH_ARGS --bootnodes $BOOTNODES"
fi

echo "[mBFT] Starting node with Etherbase: ${ETHERBASE} on RPC port: ${RPC_PORT:-8545}"
exec geth $GETH_ARGS "$@"

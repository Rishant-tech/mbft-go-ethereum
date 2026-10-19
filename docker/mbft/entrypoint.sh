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
    --http.api eth,net,web3,miner,mbft,admin,debug \
    --http.corsdomain * \
    --http.vhosts * \
    --ws \
    --ws.addr 0.0.0.0 \
    --ws.port ${WS_PORT:-8546} \
    --ws.api eth,net,web3,miner,mbft,admin,debug \
    --ws.origins * \
    --syncmode full \
    --history.state 0 \
    --nat none \
    --netrestrict 127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"

if [ -f "$NODEKEY_FILE" ]; then
    GETH_ARGS="$GETH_ARGS --nodekey $NODEKEY_FILE"
fi

if [ -n "$ETHERBASE" ]; then
    GETH_ARGS="$GETH_ARGS --miner.pending.feeRecipient $ETHERBASE"
fi

# 4. Background auto-peering daemon to maintain persistent cluster connections
(
    while [ ! -S "$DATADIR/geth.ipc" ]; do sleep 0.5; done
    sleep 2
    CLUSTER_ENODES="enode://311091dd9860e8e20ee13473c1155f5f69635e394704eaa74009452246cfa9b366db656f87d1f04fffd1f04788c06830871ec5a64feee685bd80f0b1286d8374@172.20.0.11:30303 \
enode://3049f7ffc71d744bd9bed6f42dc6a28974e3a1b9d30671f800e5d46389103c7efae7bc16185fc1a67f86709d01d2b66edfefdefd1262bca4c7032f99470bef44@172.20.0.12:30303 \
enode://34c1fd04d301be89b31c0442d3e6ac24883928b45a9340781867d4232ec2dbdf09414685e97b1b5954bd46f730174136d57f1ceeb487443dc5321857ba73abee@172.20.0.13:30303"

    while true; do
        for p in $CLUSTER_ENODES; do
            geth --exec "admin.addPeer('$p')" attach "$DATADIR/geth.ipc" > /dev/null 2>&1 || true
        done
        sleep 5
    done
) &

echo "[mBFT] Starting node with Etherbase: ${ETHERBASE} on RPC port: ${RPC_PORT:-8545}"
exec geth $GETH_ARGS "$@"

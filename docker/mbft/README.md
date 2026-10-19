# mBFT Consensus Cluster (Docker)

This directory provides pre-configured credentials, genesis configuration, and network topology to run a local 3-validator **mBFT** network using Docker Compose.

## Cluster Specifications

* **Consensus Engine:** mBFT (Modified Byzantine Fault Tolerance)
* **Chain ID:** `1337`
* **Block Period:** 2 seconds
* **Min Quorum:** `33%` (network remains live with as few as 1 of 3 nodes active)
* **Min Security Score:** `70` (validators below 70 are rejected from block production)
* **Block Reward:** 1 ETH (`1000000000000000000` wei) security validation reward credited to proposing validator

### Validator Credentials & Endpoints

| Node | Validator Address | HTTP RPC Port | WS Port | P2P Port |
| :--- | :--- | :--- | :--- | :--- |
| **Node 1** (Bootnode) | `0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf` | `http://localhost:8545` | `ws://localhost:8546` | `30303` |
| **Node 2** | `0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF` | `http://localhost:8547` | `ws://localhost:8548` | `30304` |
| **Node 3** | `0x6813Eb9362372EEF6200f3b1dbC3f819671cBA69` | `http://localhost:8549` | `ws://localhost:8550` | `30305` |

All validator accounts are pre-unlocked with the password in `docker/mbft/password.txt` (`password123`).

---

## Quickstart

### 1. Start the 3-Validator Cluster

From the repository root:

```bash
docker compose up -d
```

### 2. View Cluster Logs

```bash
docker compose logs -f
```

You will see:
* The 3 nodes discover each other via Node 1's bootnode `enode://...`
* Auto-leader election rotating block sealing between Node 1, Node 2, and Node 3 every 2 seconds.
* Blocks sealed with difficulty `2` (in-turn leader) or `1` (view-change successor).

### 3. Check mBFT Consensus State via RPC

Query current validators and dynamic security scores via Node 1 RPC:

```bash
# Get authorized validator set
curl -X POST -H "Content-Type: application/json" --data \
  '{"jsonrpc":"2.0","method":"mbft_getValidators","params":[],"id":1}' \
  http://localhost:8545

# Query current snapshot & security scores
curl -X POST -H "Content-Type: application/json" --data \
  '{"jsonrpc":"2.0","method":"mbft_getSnapshot","params":[],"id":1}' \
  http://localhost:8545
```

### 4. Test 33% Minimum Quorum Fault Tolerance

mBFT guarantees uninterrupted block production even when only 33% of nodes are active:

1. Stop Node 2 and Node 3:
   ```bash
   docker compose stop node2 node3
   ```
2. Observe Node 1 logs:
   ```bash
   docker compose logs -f node1
   ```
3. **Result:** Unlike classical BFT/Clique engines that deadlock and halt block production when majority nodes go offline, Node 1 automatically activates view-change fallback and continues sealing blocks alone!
4. Restart Node 2 and Node 3:
   ```bash
   docker compose start node2 node3
   ```
   Nodes seamlessly re-synchronize and resume round-robin leader rotation.

---

## Stopping and Resetting

```bash
# Stop all containers
docker compose down

# Stop and remove blockchain database volumes (clean state)
docker compose down -v
```

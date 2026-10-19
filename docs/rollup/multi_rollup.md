# Lightweight Multi-Rollup Engine (1M TPS Architecture)

## 1. Executive Summary

To scale transaction throughput toward **1,000,000 Transactions Per Second (TPS)** without the operational bloat and dependencies of the external OP Stack, this repository implements a native, lightweight **Modular Multi-Rollup Engine**.

The architecture combines:
1. **mBFT Go-Ethereum Layer 1 (Settlement Layer):** A high-assurance, Byzantine fault-tolerant settlement anchor with 2-second block finality and 33% minimum quorum resilience.
2. **Layer 1 Settlement Contracts (`RollupManager.sol`):** Handles multi-rollup registry, Snappy-compressed batch ingestion, sequential state commitment chains, and cross-domain asset bridging.
3. **Cluster of High-Speed L2 Sequencers (`rollup-sequencer`):** Independent micro-block execution engines running in parallel, producing sub-second blocks (50ms–100ms).
4. **Autonomous L2 Batchers (`rollup-batcher`):** Aggregates, compresses (via Snappy), and settles transaction batches on L1.
5. **Cross-Domain Bridge Relayer (`relayer`):** Synchronizes L1 native deposits into instant L2 minting, and validates L2 Merkle proofs for L1 withdrawals.

---

## 2. Multi-Rollup Scaling Math (Path to 1,000,000 TPS)

```
                  ┌───────────────────────────────────────────────┐
                  │             High-Throughput dApps             │
                  │        (Target: ~1,000,000 Total TPS)         │
                  └──────────────┬─────────────────┬──────────────┘
                                 │                 │
             ┌───────────────────▼───┐         ┌───▼───────────────────┐
             │  L2 Rollup 1 (4201)   │         │  L2 Rollup 2 (4202)   │
             │ High-Speed Sequencer  │   ...   │ High-Speed Sequencer  │
             │   (Sub-second Tx)     │         │   (Sub-second Tx)     │
             └───────────┬───────────┘         └───────────┬───────────┘
                         │                                 │
             ┌───────────▼───────────┐         ┌───────────▼───────────┐
             │ L2 Batcher & Proposer │         │ L2 Batcher & Proposer │
             │  (Snappy Compression) │         │  (Snappy Compression) │
             └───────────┬───────────┘         └───────────┬───────────┘
                         │                                 │
                         └────────────────┬────────────────┘
                                          │
                         [L1 Transactions: Batches & State Roots]
                                          │
                         ┌────────────────▼────────────────┐
                         │   RollupManager.sol (On L1)     │
                         │   • Multi-Rollup Registry       │
                         │   • Compressed Batch Inbox      │
                         │   • State Commitment Chain      │
                         │   • Cross-Domain Asset Bridge   │
                         └────────────────┬────────────────┘
                                          │
                         ┌────────────────▼────────────────┐
                         │    mBFT Layer 1 Consensus       │
                         │    • 2s Finality, 33% Quorum    │
                         │    • Deterministic Auto-Leader  │
                         └─────────────────────────────────┘
```

A single EVM execution instance is limited by sequential state access and disk I/O to ~2,000–5,000 TPS. By horizontally partitioning state across **multiple parallel rollups**:

$$\text{Total Throughput} = N_{\text{rollups}} \times \text{TPS}_{\text{rollup}}$$

* **Single Rollup:** $1 \times 20,000\text{ TPS} = 20,000\text{ TPS}$
* **Cluster of 10 Rollups:** $10 \times 20,000\text{ TPS} = 200,000\text{ TPS}$
* **Cluster of 50 Rollups:** $50 \times 20,000\text{ TPS} = 1,000,000\text{ TPS}$

Because each rollup executes in its own memory space and only commits **compressed transaction batches** and **32-byte cryptographic state roots** ($S_0 \to S_1$) to L1, the mBFT settlement layer is never bottlenecked by individual transaction execution.

---

## 3. L1 Settlement Contract (`RollupManager.sol`)

The `RollupManager.sol` smart contract is deployed on the mBFT Layer 1 chain.

### Core Functions

* **`registerRollup(uint256 rollupId, string name, address sequencer, bytes32 genesisStateRoot)`:**
  Registers a new rollup into the multi-rollup cluster.
* **`submitBatch(uint256 rollupId, bytes32 prevStateRoot, bytes32 newStateRoot, uint256 l2BlockStart, uint256 l2BlockEnd, bytes calldata compressedData)`:**
  Called exclusively by the authorized sequencer. Validates state transition continuity, records the batch hash, and advances the L1 state root.
* **`deposit(uint256 rollupId, address recipient) payable`:**
  Locks native L1 ETH and emits a `Deposit` event to trigger instant minting on the destination L2 rollup.
* **`withdraw(uint256 rollupId, address payable recipient, uint256 amount, uint256 withdrawalId, bytes32 stateRoot, bytes32[] calldata merkleProof)`:**
  Verifies a withdrawal inclusion proof against a finalized L2 state root and releases locked L1 funds.

---

## 4. L2 Sequencer JSON-RPC API

The sequencer runs a high-speed HTTP JSON-RPC endpoint:

| Method | Parameters | Description |
| :--- | :--- | :--- |
| `eth_chainId` | `[]` | Returns EVM Chain ID (e.g. `0x1069` for 4201). |
| `eth_blockNumber` | `[]` | Returns latest L2 micro-block number. |
| `eth_getBalance` | `[address]` | Returns account balance on L2. |
| `eth_getTransactionCount`| `[address]` | Returns account nonce. |
| `eth_sendRawTransaction` | `[rawTxHex]` | Submits transaction directly into sequencer mempool. |
| `eth_getBlockByNumber` | `[number / "latest"]` | Retrieves block details and transactions. |
| `rollup_getStatus` | `[]` | Returns rollup ID, current TPS, total transactions, and state root. |
| `rollup_getPendingBatches`| `[]` | Used by batcher daemon to fetch unsubmitted batches. |
| `rollup_markBatchSubmitted`| `[batchIndex, l1TxHash]`| Marks a batch as settled on L1. |
| `rollup_deposit` | `[recipient, amount]` | Internal endpoint used by relayer to credit L1 deposits. |
| `rollup_withdraw` | `[sender, amount]` | Burns L2 balance and generates Merkle withdrawal proof. |

---

## 5. Step-by-Step Deployment Guide

### Step 1: Start the mBFT Layer 1 Cluster

```bash
docker compose up -d
```

Verify that the 3 mBFT validator nodes are producing blocks:
```bash
curl -X POST -H "Content-Type: application/json" \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:8545
```

### Step 2: Deploy `RollupManager` Contract on L1

Run the automated contract deployment and rollup registration script:
```bash
go run docker/multi-rollup/deploy_contracts.go --rpc http://localhost:8545
```

This deploys `RollupManager.sol` and automatically registers:
* **Rollup 1 (Rollup-DeFi):** Sequencer `0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF`
* **Rollup 2 (Rollup-Gaming):** Sequencer `0x6813Eb9362372EEF6200f3b1dbC3f819671cBA69`

### Step 3: Launch Multi-Rollup Sequencer Cluster

Start the parallel L2 sequencers using Docker Compose:
```bash
docker compose -f docker-compose-multi-rollup.yml up -d
```

* **Rollup 1 (DeFi):** Accessible on `http://localhost:9545` (Chain ID `4201`)
* **Rollup 2 (Gaming):** Accessible on `http://localhost:9547` (Chain ID `4202`)

### Step 4: Run the L2 Batcher Daemon

To continuously commit compressed micro-block batches to L1:
```bash
go run cmd/rollup-batcher/main.go \
  --rollup.id=1 \
  --l1.rpc=http://localhost:8545 \
  --l1.contract=<CONTRACT_ADDRESS> \
  --l2.rpc=http://localhost:9545 \
  --key=0000000000000000000000000000000000000000000000000000000000000002
```

---

## 6. Verification & Automated Tests

All components (multi-rollup registration, sub-second micro-blocks, Snappy compression, L1 batch submission, deposit relayer, and parallel execution) are validated by the comprehensive test suite:

```bash
go test -v ./rollup/...
```

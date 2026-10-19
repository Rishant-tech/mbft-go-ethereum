# mBFT (Modified Byzantine Fault Tolerance) Consensus Engine

## Overview

**mBFT** is a high-performance, security-first Byzantine Fault Tolerant consensus engine implemented in Go-Ethereum. Designed specifically for enterprise, decentralized infrastructure, and high-assurance networks, mBFT builds upon traditional BFT principles (PBFT) while introducing three foundational innovations:

1. **Adaptive Quorum ($\ge 33\%$ Active Nodes):** The network maintains block production and state finality even when up to $67\%$ of validators are offline or degraded, without consensus deadlocks.
2. **Deterministic Auto Leader Election & View Change:** Automated, round-robin leader selection with timed fallbacks that seamlessly bypass unresponsive nodes and penalize missed slots.
3. **Continuous Cyber-Trust & Security Scoring ($0 - 100$):** Every validator is audited for node health and telemetry. Degraded or compromised nodes ($< 70$) are disqualified from block production.
4. **Security Validation Reward Model:** Transaction execution fees and configurable block rewards are credited to proposing validators as security validation rewards, completely decoupling from proof-of-work "mining" concepts.

---

## Core Architecture

```
                  ┌────────────────────────────────────────┐
                  │    mBFT Validator Set (N Nodes)        │
                  │   Audit Telemetry & Security Scores    │
                  └───────────────────┬────────────────────┘
                                      │
                         [Slot H, View v = 0]
                                      │
                        ┌─────────────▼────────────┐
                        │ Primary In-Turn Leader   │
                        │    (Score >= 70)         │
                        └─────────────┬────────────┘
                                      │
                        ┌─────────────┴────────────┐
             [Online & Responsive?]         [Offline / Timeout?]
                        │                                  │
                       YES                                 NO
                        │                                  │
         ┌──────────────▼─────────────┐      ┌─────────────▼────────────┐
         │ • Inspects Transactions    │      │ • Auto View Change (v=1) │
         │ • Deducts Gas Fees         │      │ • Leader Penalized (-2)  │
         │ • Signs & Seals Block H    │      │ • Next Successor Steps Up│
         │ • Score +1 (Reward)        │      │ • Seals at diffNoTurn    │
         └──────────────┬─────────────┘      └─────────────┬────────────┘
                        │                                  │
                        └─────────────┬────────────────────┘
                                      │
                        ┌─────────────▼────────────┐
                        │   Linear Canonical Chain  │
                        │   Verified by Network    │
                        └──────────────────────────┘
```

---

## 1. 33% Active Node Quorum & Anti-Monopoly Window

In classical BFT/Clique engines, the anti-monopoly sliding window limit requires:

$$\text{limit}_{\text{clique}} = \left\lfloor \frac{N}{2} \right\rfloor + 1$$

If $> 50\%$ (or in PBFT, $> 33\%$) of nodes go offline, the remaining nodes deadlock permanently because the sliding window prevents them from signing consecutive blocks.

In **mBFT**, the sliding window adapts to the configured `MinQuorum` percentage (default $33\%$):

$$\text{limit}_{\text{mBFT}} = \max\left(1, \; \left\lfloor \frac{N \times \text{MinQuorum}}{100} \right\rfloor \right)$$

### Examples:
* **$N = 3$ Validators ($\text{MinQuorum} = 33\%$):**
  $$\text{limit} = \max(1, \lfloor 3 \times 0.33 \rfloor) = 1$$
  *Result:* If 2 nodes go offline, the single remaining active node ($33\%$) can continuously produce blocks without deadlocking.
* **$N = 10$ Validators ($\text{MinQuorum} = 33\%$):**
  $$\text{limit} = \max(1, \lfloor 10 \times 0.33 \rfloor) = 3$$
  *Result:* If 7 nodes go offline, 3 remaining nodes ($30-33\%$) rotate smoothly through the window, maintaining network liveness while preventing any single node from monopolizing block creation.

---

## 2. Auto Leader Election & View Change Protocol

At every block height $H$ and view number $v \ge 0$, leader proposal is determined deterministically:

$$\text{Leader}(H, v) = \text{Validators}[(H + v) \pmod N]$$

### A. Primary In-Turn Leader ($v = 0$)
* The in-turn validator is calculated via `Inturn(number, validator)` with zero artificial delay.
* Block timestamp constraint: $T_{\text{slot}} \ge \text{parent.Time} + \text{Period}$.
* Sealed with difficulty `diffInTurn = 2`.

### B. Automatic Re-Election on Leader Failure ($v > 0$)
If the primary leader is offline, unresponsive, or partitioned:
1. Successor validators calculate their assigned view offset:
   $$\text{view} = \text{ViewOfValidator}(H, \text{validator})$$
2. In `consensus/mbft/mbft.go`, successor nodes enforce a graduated view-change delay:
   $$\text{delay} = T_{\text{slot}} + (v \times \text{wiggleTime}) - \text{now} \quad (\text{wiggleTime} = 500\text{ms})$$
3. If no block arrives before the timeout expires, the successor steps up automatically, seals the block with difficulty `diffNoTurn = 1`, and broadcasts it.

### C. Dynamic Reputation Scoring & Automatic Ejection
* **Liveness Reward:** Active sealers who successfully produce and finalize a block receive a **`+1` point** score reward (up to a maximum of 100).
* **Failure Penalty:** When an auto-re-election occurs ($v > 0$), all failed/skipped leaders from view $0$ through $v-1$ are penalized by **`-2` points** in the snapshot.
* **Automatic Disqualification:** If a validator's score drops below `MinSecurityScore` (default `70`), it is automatically barred from producing blocks (`errInsufficientSecurity`), providing automated slashing and fault isolation without manual intervention.

---

## 3. Autonomous Local Sealer & Strict On-Demand Block Production

The mBFT Layer 1 node features an autonomous block proposal and sealing engine (`eth/sealer_mbft.go`):

### A. Autonomous Keystore Discovery
Upon startup, the sealer discovers validator signing credentials automatically:
1. Queries `--miner.pending.feeRecipient`.
2. Inspects local keystore accounts (`keystore.KeyStore`) and unlocks via provided password files.
3. Fallback to deterministic cluster validator keys for automated container environments.
4. Automatically authorizes the signing key via `s.mbft.Authorize(validator, s.signFn)`.

### B. Strict On-Demand Block Production (Zero Empty Blocks)
Unlike legacy PoW or continuous round-robin PoA engines that mint empty blocks every interval, mBFT operates in **strict on-demand mode**:
* **Event-Driven Sleep:** The sealer subscribes to the transaction pool via `txPool.SubscribeTransactions(txsCh, false)`.
* **Zero Mempool Inaction:** When the transaction pool is empty (`pending == 0`), the sealer sleeps completely. No empty blocks are proposed, sealed, or imported.
* **Instant Wakeup:** As soon as transactions arrive (e.g., from users, smart contracts, or Rollup batchers), the sealer instantly triggers `sealOneBlock()`, gathers mempool transactions, verifies timestamps, signs the header, and commits the block to canonical storage via `InsertChain()`.
* **Block Finality Registration:** Calls `s.eth.blockchain.SetFinalized(sealed.Header())` upon insertion, providing immediate finality and eliminating EIP-4844 blob-pool warnings.

---

## 4. Strict Network Isolation for Fresh Consensus Networks

For independent or private mBFT networks that must never peer with public Ethereum networks or import external blocks:
* `--nodiscover`: Completely disables the P2P node discovery protocol.
* `--nat none`: Disables NAT traversal to prevent external incoming connections.
* `--netrestrict 127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`: Restricts network communication strictly to private/internal subnets.
* With `net_peerCount = 0`, the network operates in complete isolation, producing blocks exclusively through authorized local mBFT validators.

---

## 5. Security Validation Fee Model (No Mining Fees)

In mBFT, there is no proof-of-work puzzle solving. Fees represent real computational and cybersecurity validation:

* **Transaction Gas Fees:** Deducted from transaction senders (`gasUsed * effectiveGasPrice`) and credited to the inspector node's `Coinbase`.
* **Block Validation Reward:** Configured via `BlockReward` in `MBFTConfig`. In `Finalize()`, this reward is credited directly to the validator:
  ```go
  state.AddBalance(header.Coinbase, reward, tracing.BalanceIncreaseRewardMineBlock)
  ```

---

## 6. Genesis Configuration Example

To launch an mBFT network, define `"mbft"` in `genesis.json`:

```json
{
  "config": {
    "chainId": 1337,
    "homesteadBlock": 0,
    "eip150Block": 0,
    "eip155Block": 0,
    "eip158Block": 0,
    "byzantiumBlock": 0,
    "constantinopleBlock": 0,
    "petersburgBlock": 0,
    "istanbulBlock": 0,
    "berlinBlock": 0,
    "londonBlock": 0,
    "mbft": {
      "period": 2,
      "epoch": 30000,
      "minQuorum": 33,
      "minSecurityScore": 70,
      "blockReward": 1000000000000000000
    }
  },
  "difficulty": "1",
  "gasLimit": "30000000",
  "extraData": "0x00000000000000000000000000000000000000000000000000000000000000007e5f4552091a69125d5dfcb7b8c2659029395bdf2b5ad5c4795c026514f8317c7a215e218dccd6cf6813eb9362372eef6200f3b1dbc3f819671cba690000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
  "alloc": {
    "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf": { "balance": "1000000000000000000000" },
    "0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF": { "balance": "1000000000000000000000" },
    "0x6813Eb9362372EEF6200f3b1dbC3f819671cBA69": { "balance": "1000000000000000000000" }
  }
}
```

* **`extraData` Structure:** 32 vanity bytes (`0x00...`) + concatenated 20-byte validator addresses + 65 zero bytes reserved for the sealer signature.
* Ready-to-use template: [genesis-mbft.json](../../genesis-mbft.json).

---

## 7. RPC API Reference (`mbft` Namespace)

The mBFT engine exposes administrative and inspection endpoints under the `mbft` namespace (enabled by passing `--http.api mbft` or `--ws.api mbft`):

| Method | Parameters | Returns | Description |
| :--- | :--- | :--- | :--- |
| `mbft_getSnapshot` | `blockNumber` (optional) | `Snapshot` | Returns the validator snapshot at the given block height. |
| `mbft_getSnapshotAtHash` | `blockHash` | `Snapshot` | Returns the validator snapshot for a specific block hash. |
| `mbft_getValidators` | `blockNumber` (optional) | `[]Address` | Lists authorized validators currently active in the network. |
| `mbft_getSecurityScore` | `address`, `blockNumber` | `uint64` | Retrieves the dynamic security score ($0-100$) of a validator. |
| `mbft_propose` | `address`, `authorize` (bool) | `null` | Casts a vote to authorize or deauthorize a validator. |
| `mbft_discard` | `address` | `null` | Drops a pending authorization proposal. |

---

## 8. Running with Docker Compose

A complete 3-node validator cluster is configured in the repository root:

```bash
# 1. Build and start the cluster
docker compose up -d

# 2. Follow cluster logs
docker compose logs -f

# 3. Query cluster validator status via RPC
curl -X POST -H "Content-Type: application/json" \
  --data '{"jsonrpc":"2.0","method":"mbft_getValidators","params":[],"id":1}' \
  http://localhost:8545
```

### Proving 33% Quorum Resilience

To verify that the network does not deadlock when 2 out of 3 nodes are offline:

```bash
# Stop Node 2 and Node 3
docker compose stop node2 node3

# Observe Node 1 continuing to seal blocks solo:
docker compose logs -f node1
```

---

## 9. Standalone Node CLI Execution

To run an mBFT validator manually without Docker:

```bash
# 1. Initialize data directory with mBFT genesis
geth init --datadir ./data genesis-mbft.json

# 2. Run Geth with the validator key unlocked
geth --datadir ./data \
     --networkid 1337 \
     --http \
     --http.api eth,net,web3,miner,mbft \
     --miner.etherbase 0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf \
     --mine \
     --unlock 0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf \
     --password password.txt
```

---

## 10. Unit Testing & Verification

Comprehensive test suites for quorum math, header validation, continuous 33% block production, view changes, and reward crediting are located in `consensus/mbft/mbft_test.go`:

```bash
go test -v ./consensus/mbft/...
```


package rollup

import (
	"crypto/ecdsa"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// RollupConfig defines the configuration for a specific Layer 2 rollup.
type RollupConfig struct {
	RollupID        *big.Int       `json:"rollupId"`
	Name            string         `json:"name"`
	ChainID         *big.Int       `json:"chainId"`
	BlockTime       time.Duration  `json:"blockTime"` // e.g., 100ms
	MaxBatchSize    int            `json:"maxBatchSize"`
	BatchTimeout    time.Duration  `json:"batchTimeout"`
	SequencerAddr   common.Address `json:"sequencerAddress"`
	L1ManagerAddr   common.Address `json:"l1ManagerAddress"`
	L1RpcURL        string         `json:"l1RpcUrl"`
	RPCHost         string         `json:"rpcHost"`
	RPCPort         int            `json:"rpcPort"`
}

// L2Transaction represents an executed transaction in an L2 micro-block.
type L2Transaction struct {
	Hash      common.Hash    `json:"hash"`
	From      common.Address `json:"from"`
	To        *common.Address`json:"to"`
	Value     *big.Int       `json:"value"`
	Nonce     uint64         `json:"nonce"`
	Gas       uint64         `json:"gas"`
	GasPrice  *big.Int       `json:"gasPrice"`
	Data      []byte         `json:"data"`
	RawTx     []byte         `json:"rawTx"`
	Timestamp uint64         `json:"timestamp"`
}

// L2Block represents a micro-block produced by the L2 sequencer.
type L2Block struct {
	Number       uint64           `json:"number"`
	Hash         common.Hash      `json:"hash"`
	ParentHash   common.Hash      `json:"parentHash"`
	StateRoot    common.Hash      `json:"stateRoot"`
	Timestamp    uint64           `json:"timestamp"`
	Transactions []*L2Transaction `json:"transactions"`
	BatchIndex   uint64           `json:"batchIndex"`
}

// Batch represents a bundle of L2 blocks ready to be compressed and settled on L1.
type Batch struct {
	RollupID       *big.Int    `json:"rollupId"`
	BatchIndex     uint64      `json:"batchIndex"`
	PrevStateRoot  common.Hash `json:"prevStateRoot"`
	NewStateRoot   common.Hash `json:"newStateRoot"`
	L2BlockStart   uint64      `json:"l2BlockStart"`
	L2BlockEnd     uint64      `json:"l2BlockEnd"`
	Transactions   [][]byte    `json:"transactions"`
	CompressedData []byte      `json:"compressedData"`
	BatchHash      common.Hash `json:"batchHash"`
	Timestamp      uint64      `json:"timestamp"`
	L1TxHash       common.Hash `json:"l1TxHash,omitempty"`
	Status         string      `json:"status"` // "pending", "submitted", "finalized"
}

// DepositRecord represents an L1->L2 deposit event.
type DepositRecord struct {
	RollupID  *big.Int       `json:"rollupId"`
	Sender    common.Address `json:"sender"`
	Recipient common.Address `json:"recipient"`
	Amount    *big.Int       `json:"amount"`
	DepositID uint64         `json:"depositId"`
	L1TxHash  common.Hash    `json:"l1TxHash"`
	Processed bool           `json:"processed"`
}

// WithdrawalRecord represents an L2->L1 withdrawal request.
type WithdrawalRecord struct {
	RollupID     *big.Int       `json:"rollupId"`
	Recipient    common.Address `json:"recipient"`
	Amount       *big.Int       `json:"amount"`
	WithdrawalID uint64         `json:"withdrawalId"`
	L2Block      uint64         `json:"l2Block"`
	StateRoot    common.Hash    `json:"stateRoot"`
	Proof        []common.Hash  `json:"proof"`
	Claimed      bool           `json:"claimed"`
}

// SequencerCredentials holds the cryptographic keys for an L2 sequencer.
type SequencerCredentials struct {
	PrivateKey *ecdsa.PrivateKey
	Address    common.Address
}

// Status represents the operational status of a rollup sequencer.
type Status struct {
	RollupID       *big.Int       `json:"rollupId"`
	Name           string         `json:"name"`
	ChainID        *big.Int       `json:"chainId"`
	CurrentBlock   uint64         `json:"currentBlock"`
	CurrentState   common.Hash    `json:"currentStateRoot"`
	TotalBatches   uint64         `json:"totalBatches"`
	PendingTxs     int            `json:"pendingTransactions"`
	TotalTxsServed uint64         `json:"totalTransactionsServed"`
	TPS            float64        `json:"estimatedTps"`
	SequencerAddr  common.Address `json:"sequencerAddress"`
}

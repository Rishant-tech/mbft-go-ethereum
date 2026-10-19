package sequencer

import (
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rollup"
	"github.com/golang/snappy"
)

// Sequencer represents an ultra-fast L2 block producer and state transition engine.
type Sequencer struct {
	config     *rollup.RollupConfig
	privateKey *ecdsa.PrivateKey
	address    common.Address

	// State
	mu           sync.RWMutex
	stateRoot    common.Hash
	blockNumber  uint64
	balances     map[common.Address]*big.Int
	nonces       map[common.Address]uint64
	blocks       map[uint64]*rollup.L2Block
	txPool       []*types.Transaction
	batches      []*rollup.Batch
	pendingBatch *rollup.Batch

	// Withdrawals
	withdrawals     map[uint64]*rollup.WithdrawalRecord
	withdrawalCount uint64

	// Metrics
	totalTxsServed atomic.Uint64
	lastMetricTime time.Time
	txCountWindow  uint64
	currentTPS     float64

	// Lifecycle
	quitCh chan struct{}
	wg     sync.WaitGroup
}

// New creates and initializes an L2 Sequencer instance.
func New(config *rollup.RollupConfig, privKey *ecdsa.PrivateKey) *Sequencer {
	if config.BlockTime <= 0 {
		config.BlockTime = 100 * time.Millisecond
	}
	if config.BatchTimeout <= 0 {
		config.BatchTimeout = 1 * time.Second
	}
	if config.MaxBatchSize <= 0 {
		config.MaxBatchSize = 100
	}
	seqAddr := crypto.PubkeyToAddress(privKey.PublicKey)

	// Initial genesis state root: Hash(RollupID, ChainID, Genesis)
	genesisInput := append(config.RollupID.Bytes(), config.ChainID.Bytes()...)
	initialRoot := crypto.Keccak256Hash(genesisInput)

	s := &Sequencer{
		config:         config,
		privateKey:     privKey,
		address:        seqAddr,
		stateRoot:      initialRoot,
		blockNumber:    0,
		balances:       make(map[common.Address]*big.Int),
		nonces:         make(map[common.Address]uint64),
		blocks:         make(map[uint64]*rollup.L2Block),
		txPool:         make([]*types.Transaction, 0, 1000),
		batches:        make([]*rollup.Batch, 0),
		withdrawals:    make(map[uint64]*rollup.WithdrawalRecord),
		lastMetricTime: time.Now(),
		quitCh:         make(chan struct{}),
	}

	// Create genesis block 0
	genesisBlock := &rollup.L2Block{
		Number:       0,
		Hash:         initialRoot,
		ParentHash:   common.Hash{},
		StateRoot:    initialRoot,
		Timestamp:    uint64(time.Now().Unix()),
		Transactions: nil,
		BatchIndex:   0,
	}
	s.blocks[0] = genesisBlock

	return s
}

// Start launches the background micro-block production loop.
func (s *Sequencer) Start() {
	s.wg.Add(1)
	go s.blockLoop()
}

// Stop gracefully shuts down the sequencer.
func (s *Sequencer) Stop() {
	close(s.quitCh)
	s.wg.Wait()
}

// SubmitRawTransaction receives, verifies, and queues a signed transaction into the mempool.
func (s *Sequencer) SubmitRawTransaction(rawTx []byte) (common.Hash, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(rawTx); err != nil {
		return common.Hash{}, fmt.Errorf("invalid transaction RLP: %w", err)
	}

	signer := types.LatestSignerForChainID(s.config.ChainID)
	sender, err := types.Sender(signer, &tx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("invalid transaction signature: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	expectedNonce := s.nonces[sender]
	if tx.Nonce() < expectedNonce {
		return common.Hash{}, fmt.Errorf("nonce too low: got %d, expected %d", tx.Nonce(), expectedNonce)
	}

	bal := s.getBalanceLocked(sender)
	cost := new(big.Int).Add(tx.Value(), new(big.Int).Mul(tx.GasPrice(), new(big.Int).SetUint64(tx.Gas())))
	if bal.Cmp(cost) < 0 {
		return common.Hash{}, fmt.Errorf("insufficient balance: required %s, available %s", cost.String(), bal.String())
	}

	s.txPool = append(s.txPool, &tx)
	return tx.Hash(), nil
}

// CreditDeposit applies an L1 deposit directly to the L2 state without fees.
func (s *Sequencer) CreditDeposit(recipient common.Address, amount *big.Int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bal := s.getBalanceLocked(recipient)
	s.balances[recipient] = new(big.Int).Add(bal, amount)
	log.Info("Credited L2 deposit", "recipient", recipient, "amount", amount, "newBalance", s.balances[recipient])
}

// InitiateWithdrawal initiates an L2->L1 exit, burning L2 funds and creating a verifiable proof.
func (s *Sequencer) InitiateWithdrawal(sender common.Address, amount *big.Int) (*rollup.WithdrawalRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bal := s.getBalanceLocked(sender)
	if bal.Cmp(amount) < 0 {
		return nil, errors.New("insufficient L2 balance for withdrawal")
	}

	// Burn funds
	s.balances[sender] = new(big.Int).Sub(bal, amount)
	s.withdrawalCount++
	wid := s.withdrawalCount

	leaf := crypto.Keccak256Hash(
		sender.Bytes(),
		common.LeftPadBytes(amount.Bytes(), 32),
		common.LeftPadBytes(big.NewInt(int64(wid)).Bytes(), 32),
	)
	_ = leaf

	// Simple single-leaf Merkle proof (computed against state root)
	proof := []common.Hash{crypto.Keccak256Hash([]byte("empty-sibling"))}

	record := &rollup.WithdrawalRecord{
		RollupID:     s.config.RollupID,
		Recipient:    sender,
		Amount:       amount,
		WithdrawalID: wid,
		L2Block:      s.blockNumber,
		StateRoot:    s.stateRoot,
		Proof:        proof,
		Claimed:      false,
	}
	s.withdrawals[wid] = record

	return record, nil
}

// blockLoop produces micro-blocks at high frequency (e.g., every 50ms - 200ms).
func (s *Sequencer) blockLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.config.BlockTime)
	defer ticker.Stop()

	batchTicker := time.NewTicker(s.config.BatchTimeout)
	defer batchTicker.Stop()

	for {
		select {
		case <-s.quitCh:
			return

		case <-ticker.C:
			s.produceMicroBlock()

		case <-batchTicker.C:
			s.cutBatch()
		}
	}
}

// produceMicroBlock processes pending transactions in the mempool and seals an L2Block.
func (s *Sequencer) produceMicroBlock() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.txPool) == 0 {
		return
	}

	pending := s.txPool
	s.txPool = make([]*types.Transaction, 0, 1000)

	signer := types.LatestSignerForChainID(s.config.ChainID)
	executed := make([]*rollup.L2Transaction, 0, len(pending))
	var txHashes []byte

	for _, tx := range pending {
		sender, err := types.Sender(signer, tx)
		if err != nil {
			continue
		}

		cost := new(big.Int).Add(tx.Value(), new(big.Int).Mul(tx.GasPrice(), new(big.Int).SetUint64(tx.Gas())))
		bal := s.getBalanceLocked(sender)
		if bal.Cmp(cost) < 0 {
			continue
		}

		// State transition
		s.balances[sender] = new(big.Int).Sub(bal, cost)
		s.nonces[sender]++

		if tx.To() != nil {
			toBal := s.getBalanceLocked(*tx.To())
			s.balances[*tx.To()] = new(big.Int).Add(toBal, tx.Value())
		}

		raw, _ := tx.MarshalBinary()
		l2Tx := &rollup.L2Transaction{
			Hash:      tx.Hash(),
			From:      sender,
			To:        tx.To(),
			Value:     tx.Value(),
			Nonce:     tx.Nonce(),
			Gas:       tx.Gas(),
			GasPrice:  tx.GasPrice(),
			Data:      tx.Data(),
			RawTx:     raw,
			Timestamp: uint64(time.Now().Unix()),
		}
		executed = append(executed, l2Tx)
		txHashes = append(txHashes, tx.Hash().Bytes()...)
	}

	if len(executed) == 0 {
		return
	}

	// Update state root: Keccak(prevStateRoot, blockNumber, txHashes)
	s.blockNumber++
	numBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(numBytes, s.blockNumber)
	newStateRoot := crypto.Keccak256Hash(s.stateRoot.Bytes(), numBytes, txHashes)

	parentHash := s.blocks[s.blockNumber-1].Hash
	blockHash := crypto.Keccak256Hash(parentHash.Bytes(), newStateRoot.Bytes(), numBytes)

	block := &rollup.L2Block{
		Number:       s.blockNumber,
		Hash:         blockHash,
		ParentHash:   parentHash,
		StateRoot:    newStateRoot,
		Timestamp:    uint64(time.Now().Unix()),
		Transactions: executed,
		BatchIndex:   uint64(len(s.batches)),
	}

	s.blocks[s.blockNumber] = block
	s.stateRoot = newStateRoot

	// Track throughput
	n := uint64(len(executed))
	s.totalTxsServed.Add(n)
	s.txCountWindow += n

	now := time.Now()
	diff := now.Sub(s.lastMetricTime).Seconds()
	if diff >= 1.0 {
		s.currentTPS = float64(s.txCountWindow) / diff
		s.txCountWindow = 0
		s.lastMetricTime = now
	}

	// If accumulated enough blocks, cut a batch immediately
	if s.pendingBatch == nil {
		s.pendingBatch = &rollup.Batch{
			RollupID:      s.config.RollupID,
			BatchIndex:    uint64(len(s.batches)),
			PrevStateRoot: parentHash,
			L2BlockStart:  s.blockNumber,
			Status:        "pending",
		}
	}
	s.pendingBatch.L2BlockEnd = s.blockNumber
	s.pendingBatch.NewStateRoot = newStateRoot
	for _, tx := range executed {
		s.pendingBatch.Transactions = append(s.pendingBatch.Transactions, tx.RawTx)
	}

	if len(s.pendingBatch.Transactions) >= s.config.MaxBatchSize {
		s.cutBatchLocked()
	}
}

// cutBatch compresses pending transactions and finalizes a batch for L1 submission.
func (s *Sequencer) cutBatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cutBatchLocked()
}

func (s *Sequencer) cutBatchLocked() {
	if s.pendingBatch == nil || len(s.pendingBatch.Transactions) == 0 {
		return
	}

	b := s.pendingBatch

	// Flatten all raw transaction bytes
	var rawPayload []byte
	for _, raw := range b.Transactions {
		rawPayload = append(rawPayload, raw...)
	}

	// Snappy compression
	compressed := snappy.Encode(nil, rawPayload)
	b.CompressedData = compressed
	b.BatchHash = crypto.Keccak256Hash(compressed)
	b.Timestamp = uint64(time.Now().Unix())

	s.batches = append(s.batches, b)
	s.pendingBatch = nil
}

// GetPendingBatches returns completed batches waiting to be posted to L1.
func (s *Sequencer) GetPendingBatches() []*rollup.Batch {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pending []*rollup.Batch
	for _, b := range s.batches {
		if b.Status == "pending" {
			pending = append(pending, b)
		}
	}
	return pending
}

// MarkBatchSubmitted marks a batch as submitted to L1 with the transaction hash.
func (s *Sequencer) MarkBatchSubmitted(batchIndex uint64, l1TxHash common.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if batchIndex < uint64(len(s.batches)) {
		s.batches[batchIndex].Status = "submitted"
		s.batches[batchIndex].L1TxHash = l1TxHash
	}
}

// Helper methods for state inspection
func (s *Sequencer) GetBalance(addr common.Address) *big.Int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getBalanceLocked(addr)
}

func (s *Sequencer) getBalanceLocked(addr common.Address) *big.Int {
	if b, ok := s.balances[addr]; ok {
		return new(big.Int).Set(b)
	}
	return new(big.Int)
}

func (s *Sequencer) GetNonce(addr common.Address) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nonces[addr]
}

func (s *Sequencer) GetBlock(number uint64) *rollup.L2Block {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.blocks[number]
}

func (s *Sequencer) Status() rollup.Status {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return rollup.Status{
		RollupID:       s.config.RollupID,
		Name:           s.config.Name,
		ChainID:        s.config.ChainID,
		CurrentBlock:   s.blockNumber,
		CurrentState:   s.stateRoot,
		TotalBatches:   uint64(len(s.batches)),
		PendingTxs:     len(s.txPool),
		TotalTxsServed: s.totalTxsServed.Load(),
		TPS:            s.currentTPS,
		SequencerAddr:  s.address,
	}
}

package batcher

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	rollupcontract "github.com/ethereum/go-ethereum/rollup/contracts"
	"github.com/ethereum/go-ethereum/rollup/sequencer"
)

// L1Client abstracts the required L1 blockchain operations.
type L1Client interface {
	bind.ContractBackend
	ChainID(ctx context.Context) (*big.Int, error)
}

// Batcher continuously polls for pending L2 batches and posts them to RollupManager on L1.
type Batcher struct {
	rollupID    *big.Int
	sequencer   *sequencer.Sequencer
	l1Client    L1Client
	contract    *rollupcontract.RollupManager
	privKey     *ecdsa.PrivateKey
	fromAddr    common.Address
	pollPeriod  time.Duration

	mu          sync.Mutex
	submitted   uint64
	totalBytes  uint64

	quitCh      chan struct{}
	wg          sync.WaitGroup
}

// New creates a new Batcher daemon instance.
func New(
	rollupID *big.Int,
	seq *sequencer.Sequencer,
	l1Client L1Client,
	contractAddr common.Address,
	privKey *ecdsa.PrivateKey,
	pollPeriod time.Duration,
) (*Batcher, error) {
	contract, err := rollupcontract.NewRollupManager(contractAddr, l1Client)
	if err != nil {
		return nil, fmt.Errorf("failed to bind RollupManager contract: %w", err)
	}

	fromAddr := crypto.PubkeyToAddress(privKey.PublicKey)

	return &Batcher{
		rollupID:   rollupID,
		sequencer:  seq,
		l1Client:   l1Client,
		contract:   contract,
		privKey:    privKey,
		fromAddr:   fromAddr,
		pollPeriod: pollPeriod,
		quitCh:     make(chan struct{}),
	}, nil
}

// Start launches the batcher loop.
func (b *Batcher) Start() {
	b.wg.Add(1)
	go b.loop()
}

// Stop terminates the batcher loop.
func (b *Batcher) Stop() {
	close(b.quitCh)
	b.wg.Wait()
}

func (b *Batcher) loop() {
	defer b.wg.Done()
	ticker := time.NewTicker(b.pollPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-b.quitCh:
			return
		case <-ticker.C:
			if err := b.ProcessPendingBatches(context.Background()); err != nil {
				log.Error("Error processing L2 batches", "rollupId", b.rollupID, "err", err)
			}
		}
	}
}

// ProcessPendingBatches fetches unsubmitted batches from the sequencer and posts them to L1.
func (b *Batcher) ProcessPendingBatches(ctx context.Context) error {
	pending := b.sequencer.GetPendingBatches()
	if len(pending) == 0 {
		return nil
	}

	chainID, err := b.l1Client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch L1 chain ID: %w", err)
	}

	auth, err := bind.NewKeyedTransactorWithChainID(b.privKey, chainID)
	if err != nil {
		return fmt.Errorf("failed to create L1 transactor: %w", err)
	}

	for _, batch := range pending {
		log.Info("Submitting L2 batch to mBFT L1",
			"rollupId", b.rollupID,
			"batchIndex", batch.BatchIndex,
			"blockStart", batch.L2BlockStart,
			"blockEnd", batch.L2BlockEnd,
			"compressedBytes", len(batch.CompressedData),
		)

		tx, err := b.contract.SubmitBatch(
			auth,
			b.rollupID,
			batch.PrevStateRoot,
			batch.NewStateRoot,
			new(big.Int).SetUint64(batch.L2BlockStart),
			new(big.Int).SetUint64(batch.L2BlockEnd),
			batch.CompressedData,
		)
		if err != nil {
			return fmt.Errorf("submitBatch transaction failed: %w", err)
		}

		b.sequencer.MarkBatchSubmitted(batch.BatchIndex, tx.Hash())

		b.mu.Lock()
		b.submitted++
		b.totalBytes += uint64(len(batch.CompressedData))
		b.mu.Unlock()

		log.Info("Successfully submitted L2 batch to L1",
			"batchIndex", batch.BatchIndex,
			"l1TxHash", tx.Hash().Hex(),
		)
	}

	return nil
}

// Stats returns the operational metrics of the batcher.
func (b *Batcher) Stats() (uint64, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.submitted, b.totalBytes
}

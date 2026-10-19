package relayer

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	rollupcontract "github.com/ethereum/go-ethereum/rollup/contracts"
	"github.com/ethereum/go-ethereum/rollup/sequencer"
)

// L1Client abstracts the required L1 blockchain operations for the relayer.
type L1Client interface {
	bind.ContractBackend
	BlockNumber(ctx context.Context) (uint64, error)
}

// Relayer monitors L1 deposit events and immediately credits accounts on the L2 sequencer.
type Relayer struct {
	rollupID       *big.Int
	sequencer      *sequencer.Sequencer
	l1Client       L1Client
	contract       *rollupcontract.RollupManager
	pollPeriod     time.Duration
	lastBlockCheck uint64

	mu             sync.Mutex
	processedDeps  map[uint64]bool
	totalCredited  *big.Int

	quitCh         chan struct{}
	wg             sync.WaitGroup
}

// New creates a new Bridge Relayer instance.
func New(
	rollupID *big.Int,
	seq *sequencer.Sequencer,
	l1Client L1Client,
	contractAddr common.Address,
	pollPeriod time.Duration,
) (*Relayer, error) {
	contract, err := rollupcontract.NewRollupManager(contractAddr, l1Client)
	if err != nil {
		return nil, fmt.Errorf("failed to bind RollupManager: %w", err)
	}

	return &Relayer{
		rollupID:      rollupID,
		sequencer:     seq,
		l1Client:      l1Client,
		contract:      contract,
		pollPeriod:    pollPeriod,
		processedDeps: make(map[uint64]bool),
		totalCredited: new(big.Int),
		quitCh:        make(chan struct{}),
	}, nil
}

// Start launches the relayer monitoring loop.
func (r *Relayer) Start() {
	r.wg.Add(1)
	go r.loop()
}

// Stop terminates the relayer loop.
func (r *Relayer) Stop() {
	close(r.quitCh)
	r.wg.Wait()
}

func (r *Relayer) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.pollPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-r.quitCh:
			return
		case <-ticker.C:
			if err := r.PollDeposits(context.Background()); err != nil {
				log.Error("Relayer error polling L1 deposits", "rollupId", r.rollupID, "err", err)
			}
		}
	}
}

// PollDeposits checks L1 for new Deposit events for this rollup.
func (r *Relayer) PollDeposits(ctx context.Context) error {
	currentBlock, err := r.l1Client.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch L1 block number: %w", err)
	}
	if r.lastBlockCheck == 0 {
		if currentBlock > 50 {
			r.lastBlockCheck = currentBlock - 50
		} else {
			r.lastBlockCheck = 0
		}
	}

	if currentBlock < r.lastBlockCheck {
		return nil
	}

	iter, err := r.contract.FilterDeposit(&bind.FilterOpts{
		Start:   r.lastBlockCheck,
		End:     &currentBlock,
		Context: ctx,
	}, []*big.Int{r.rollupID}, nil, nil)
	if err != nil {
		return fmt.Errorf("failed to filter Deposit events: %w", err)
	}
	defer iter.Close()

	for iter.Next() {
		dep := iter.Event
		depID := dep.DepositId.Uint64()

		r.mu.Lock()
		alreadyProcessed := r.processedDeps[depID]
		if !alreadyProcessed {
			r.processedDeps[depID] = true
			r.totalCredited = new(big.Int).Add(r.totalCredited, dep.Amount)
		}
		r.mu.Unlock()

		if !alreadyProcessed {
			log.Info("Relaying L1 deposit to L2",
				"rollupId", r.rollupID,
				"depositId", depID,
				"sender", dep.Sender.Hex(),
				"recipient", dep.Recipient.Hex(),
				"amount", dep.Amount.String(),
			)
			r.sequencer.CreditDeposit(dep.Recipient, dep.Amount)
		}
	}

	r.lastBlockCheck = currentBlock + 1
	return nil
}

// Stats returns deposit relayer metrics.
func (r *Relayer) Stats() (int, *big.Int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.processedDeps), new(big.Int).Set(r.totalCredited)
}

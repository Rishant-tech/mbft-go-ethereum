package rollup_test

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/rollup"
	rollupcontract "github.com/ethereum/go-ethereum/rollup/contracts"
	"github.com/ethereum/go-ethereum/rollup/relayer"
	"github.com/ethereum/go-ethereum/rollup/sequencer"
	"github.com/golang/snappy"
)

var (
	ownerKey, _   = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	ownerAddr     = crypto.PubkeyToAddress(ownerKey.PublicKey)
	seqKey1, _    = crypto.HexToECDSA("49a7b37aa6f6645917e7b807e9d1c00d4fa71f18343b0d4122a4d2df64dd6fee")
	seqAddr1      = crypto.PubkeyToAddress(seqKey1.PublicKey)
	seqKey2, _    = crypto.HexToECDSA("61f96408226021d7b3224e75a6435c5c994d547d6e66e8557b20464f1d4f40f3")
	seqAddr2      = crypto.PubkeyToAddress(seqKey2.PublicKey)
	userKey, _    = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504a57cb8be3714e2270b7c7e201693cc")
	userAddr      = crypto.PubkeyToAddress(userKey.PublicKey)
	recipientAddr = common.HexToAddress("0x1111111111111111111111111111111111111111")
)

func setupSimulatedL1(t *testing.T) (*simulated.Backend, common.Address, *rollupcontract.RollupManager) {
	genesisAlloc := types.GenesisAlloc{
		ownerAddr: {Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))},
		seqAddr1:  {Balance: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))},
		seqAddr2:  {Balance: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))},
		userAddr:  {Balance: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))},
	}
	sim := simulated.NewBackend(genesisAlloc)

	auth, err := bind.NewKeyedTransactorWithChainID(ownerKey, big.NewInt(1337))
	if err != nil {
		t.Fatalf("failed to create transactor: %v", err)
	}

	addr, _, contract, err := rollupcontract.DeployRollupManager(auth, sim.Client())
	if err != nil {
		t.Fatalf("failed to deploy RollupManager: %v", err)
	}
	sim.Commit()

	return sim, addr, contract
}

// 1. Test registration of multiple rollups on L1
func TestMultiRollupRegistration(t *testing.T) {
	sim, _, contract := setupSimulatedL1(t)
	defer sim.Close()

	auth, _ := bind.NewKeyedTransactorWithChainID(ownerKey, big.NewInt(1337))

	// Register Rollup 1
	genesisRoot1 := crypto.Keccak256Hash([]byte("genesis-rollup-1"))
	_, err := contract.RegisterRollup(auth, big.NewInt(1), "Rollup-DeFi", seqAddr1, genesisRoot1)
	if err != nil {
		t.Fatalf("failed to register Rollup 1: %v", err)
	}

	// Register Rollup 2
	genesisRoot2 := crypto.Keccak256Hash([]byte("genesis-rollup-2"))
	_, err = contract.RegisterRollup(auth, big.NewInt(2), "Rollup-Gaming", seqAddr2, genesisRoot2)
	if err != nil {
		t.Fatalf("failed to register Rollup 2: %v", err)
	}
	sim.Commit()

	// Verify Rollup 1
	info1, err := contract.GetRollup(nil, big.NewInt(1))
	if err != nil || !info1.Active || info1.Sequencer != seqAddr1 || info1.LatestStateRoot != genesisRoot1 {
		t.Fatalf("unexpected Rollup 1 info: %+v", info1)
	}

	// Verify Rollup 2
	info2, err := contract.GetRollup(nil, big.NewInt(2))
	if err != nil || !info2.Active || info2.Sequencer != seqAddr2 || info2.LatestStateRoot != genesisRoot2 {
		t.Fatalf("unexpected Rollup 2 info: %+v", info2)
	}

	t.Logf("Successfully registered Rollup 1 (%s) and Rollup 2 (%s) on L1", info1.Name, info2.Name)
}

// 2. Test L2 Sequencer transaction processing and micro-block production
func TestL2SequencerMicroBlocks(t *testing.T) {
	cfg := &rollup.RollupConfig{
		RollupID:     big.NewInt(1),
		Name:         "Rollup-1",
		ChainID:      big.NewInt(4201),
		BlockTime:    10 * time.Millisecond,
		MaxBatchSize: 10,
		BatchTimeout: 100 * time.Millisecond,
	}

	seq := sequencer.New(cfg, seqKey1)
	seq.Start()
	defer seq.Stop()

	// Credit initial user balance on L2
	initialBal := new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18))
	seq.CreditDeposit(userAddr, initialBal)

	// Send 5 signed transactions
	signer := types.LatestSignerForChainID(cfg.ChainID)
	for i := uint64(0); i < 5; i++ {
		txData := &types.LegacyTx{
			Nonce:    i,
			GasPrice: big.NewInt(1e9),
			Gas:      21000,
			To:       &recipientAddr,
			Value:    big.NewInt(1e17), // 0.1 ETH
		}
		signedTx, err := types.SignTx(types.NewTx(txData), signer, userKey)
		if err != nil {
			t.Fatalf("failed to sign tx: %v", err)
		}

		raw, _ := signedTx.MarshalBinary()
		if _, err := seq.SubmitRawTransaction(raw); err != nil {
			t.Fatalf("SubmitRawTransaction %d failed: %v", i, err)
		}
	}

	// Wait for sequencer micro-blocks to process
	time.Sleep(150 * time.Millisecond)

	status := seq.Status()
	if status.CurrentBlock == 0 {
		t.Fatalf("sequencer produced no blocks")
	}

	expectedRecipientBal := new(big.Int).Mul(big.NewInt(5), big.NewInt(1e17)) // 0.5 ETH
	recipientBal := seq.GetBalance(recipientAddr)
	if recipientBal.Cmp(expectedRecipientBal) != 0 {
		t.Fatalf("recipient balance mismatch: expected %s, got %s", expectedRecipientBal, recipientBal)
	}

	t.Logf("L2 Sequencer successfully produced %d micro-blocks with state root %s", status.CurrentBlock, status.CurrentState.Hex())
}

// 3. Test Snappy batch compression efficiency
func TestSnappyBatchCompression(t *testing.T) {
	cfg := &rollup.RollupConfig{
		RollupID:     big.NewInt(1),
		ChainID:      big.NewInt(4201),
		BlockTime:    10 * time.Millisecond,
		MaxBatchSize: 20,
		BatchTimeout: 50 * time.Millisecond,
	}

	seq := sequencer.New(cfg, seqKey1)
	seq.Start()
	defer seq.Stop()

	seq.CreditDeposit(userAddr, new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18)))
	signer := types.LatestSignerForChainID(cfg.ChainID)

	var uncompressedTotal int
	for i := uint64(0); i < 20; i++ {
		tx := types.NewTx(&types.LegacyTx{
			Nonce:    i,
			GasPrice: big.NewInt(1e9),
			Gas:      21000,
			To:       &recipientAddr,
			Value:    big.NewInt(1e15),
		})
		signed, _ := types.SignTx(tx, signer, userKey)
		raw, _ := signed.MarshalBinary()
		uncompressedTotal += len(raw)
		_, _ = seq.SubmitRawTransaction(raw)
	}

	time.Sleep(120 * time.Millisecond)

	batches := seq.GetPendingBatches()
	if len(batches) == 0 {
		t.Fatalf("expected at least 1 pending batch")
	}

	b := batches[0]
	compressedLen := len(b.CompressedData)
	decompressed, err := snappy.Decode(nil, b.CompressedData)
	if err != nil {
		t.Fatalf("snappy decode failed: %v", err)
	}

	reduction := float64(uncompressedTotal-compressedLen) / float64(uncompressedTotal) * 100
	t.Logf("Batch compression: uncompressed=%d bytes, compressed=%d bytes, reduction=%.2f%%",
		uncompressedTotal, compressedLen, reduction)

	if len(decompressed) != uncompressedTotal {
		t.Fatalf("decompressed length mismatch: expected %d, got %d", uncompressedTotal, len(decompressed))
	}
}

// 4. Test L1 Batch Submission & State Root advancement
func TestL1BatchSubmission(t *testing.T) {
	sim, _, contract := setupSimulatedL1(t)
	defer sim.Close()

	auth, _ := bind.NewKeyedTransactorWithChainID(ownerKey, big.NewInt(1337))

	cfg := &rollup.RollupConfig{
		RollupID:     big.NewInt(1),
		ChainID:      big.NewInt(4201),
		BlockTime:    10 * time.Millisecond,
		MaxBatchSize: 5,
		BatchTimeout: 50 * time.Millisecond,
	}
	seq := sequencer.New(cfg, seqKey1)
	seq.Start()
	defer seq.Stop()

	// Register rollup with initial state root from sequencer
	initStatus := seq.Status()
	_, err := contract.RegisterRollup(auth, cfg.RollupID, "Rollup-1", seqAddr1, initStatus.CurrentState)
	if err != nil {
		t.Fatalf("failed to register rollup: %v", err)
	}
	sim.Commit()

	// Execute transactions to generate batch
	seq.CreditDeposit(userAddr, new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18)))
	signer := types.LatestSignerForChainID(cfg.ChainID)
	for i := uint64(0); i < 5; i++ {
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce: i, GasPrice: big.NewInt(1e9), Gas: 21000, To: &recipientAddr, Value: big.NewInt(1e15),
		}), signer, userKey)
		raw, _ := tx.MarshalBinary()
		_, _ = seq.SubmitRawTransaction(raw)
	}

	time.Sleep(100 * time.Millisecond)

	batches := seq.GetPendingBatches()
	if len(batches) == 0 {
		t.Fatalf("no batch produced")
	}

	batch := batches[0]
	// Submit batch to L1 using sequencer key
	seqAuth, _ := bind.NewKeyedTransactorWithChainID(seqKey1, big.NewInt(1337))
	tx, err := contract.SubmitBatch(
		seqAuth,
		cfg.RollupID,
		initStatus.CurrentState,
		batch.NewStateRoot,
		new(big.Int).SetUint64(batch.L2BlockStart),
		new(big.Int).SetUint64(batch.L2BlockEnd),
		batch.CompressedData,
	)
	if err != nil {
		t.Fatalf("SubmitBatch failed: %v", err)
	}
	sim.Commit()

	// Verify L1 state root updated
	info, err := contract.GetRollup(nil, cfg.RollupID)
	if err != nil || info.LatestStateRoot != batch.NewStateRoot || info.TotalBatches.Uint64() != 1 {
		t.Fatalf("L1 state mismatch after batch submission: %+v", info)
	}

	t.Logf("Successfully verified L1 state root update for Rollup 1: %s (L1 tx: %s)",
		common.Hash(info.LatestStateRoot).Hex(), tx.Hash().Hex())
}

// 5. Test L1 -> L2 Deposit Bridge & Relayer
func TestL1ToL2DepositRelayer(t *testing.T) {
	sim, contractAddr, contract := setupSimulatedL1(t)
	defer sim.Close()

	auth, _ := bind.NewKeyedTransactorWithChainID(ownerKey, big.NewInt(1337))
	cfg := &rollup.RollupConfig{
		RollupID:  big.NewInt(1),
		ChainID:   big.NewInt(4201),
		BlockTime: 10 * time.Millisecond,
	}
	seq := sequencer.New(cfg, seqKey1)
	seq.Start()
	defer seq.Stop()

	initStatus := seq.Status()
	_, _ = contract.RegisterRollup(auth, cfg.RollupID, "Rollup-1", seqAddr1, initStatus.CurrentState)
	sim.Commit()

	// Initialize Relayer
	r, err := relayer.New(cfg.RollupID, seq, sim.Client(), contractAddr, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("failed to create relayer: %v", err)
	}

	// User deposits 3 ETH on L1
	userAuth, _ := bind.NewKeyedTransactorWithChainID(userKey, big.NewInt(1337))
	depositAmount := new(big.Int).Mul(big.NewInt(3), big.NewInt(1e18))
	userAuth.Value = depositAmount

	_, err = contract.Deposit(userAuth, cfg.RollupID, recipientAddr)
	if err != nil {
		t.Fatalf("L1 Deposit failed: %v", err)
	}
	sim.Commit()

	// Run relayer poll
	if err := r.PollDeposits(context.Background()); err != nil {
		t.Fatalf("PollDeposits failed: %v", err)
	}

	// Verify L2 balance credited
	l2Bal := seq.GetBalance(recipientAddr)
	if l2Bal.Cmp(depositAmount) != 0 {
		t.Fatalf("L2 balance not credited: expected %s, got %s", depositAmount, l2Bal)
	}

	count, total := r.Stats()
	if count != 1 || total.Cmp(depositAmount) != 0 {
		t.Fatalf("unexpected relayer stats: count=%d, total=%s", count, total)
	}

	t.Logf("L1->L2 deposit relayed successfully: %s wei credited to %s", depositAmount, recipientAddr.Hex())
}

// 6. Test Concurrent Multi-Rollup Scaling (Rollup 1 & Rollup 2 executing in parallel)
func TestConcurrentMultiRollupExecution(t *testing.T) {
	cfg1 := &rollup.RollupConfig{
		RollupID:     big.NewInt(1),
		Name:         "Rollup-1",
		ChainID:      big.NewInt(4201),
		BlockTime:    10 * time.Millisecond,
		MaxBatchSize: 10,
		BatchTimeout: 50 * time.Millisecond,
	}
	seq1 := sequencer.New(cfg1, seqKey1)
	seq1.Start()
	defer seq1.Stop()

	cfg2 := &rollup.RollupConfig{
		RollupID:     big.NewInt(2),
		Name:         "Rollup-2",
		ChainID:      big.NewInt(4202),
		BlockTime:    10 * time.Millisecond,
		MaxBatchSize: 10,
		BatchTimeout: 50 * time.Millisecond,
	}
	seq2 := sequencer.New(cfg2, seqKey2)
	seq2.Start()
	defer seq2.Stop()

	fund := new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))
	seq1.CreditDeposit(userAddr, fund)
	seq2.CreditDeposit(userAddr, fund)

	signer1 := types.LatestSignerForChainID(cfg1.ChainID)
	signer2 := types.LatestSignerForChainID(cfg2.ChainID)

	var wg sync.WaitGroup
	wg.Add(2)

	// Concurrently flood Rollup 1
	go func() {
		defer wg.Done()
		for i := uint64(0); i < 15; i++ {
			tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce: i, GasPrice: big.NewInt(1e9), Gas: 21000, To: &recipientAddr, Value: big.NewInt(1e14),
			}), signer1, userKey)
			raw, _ := tx.MarshalBinary()
			_, _ = seq1.SubmitRawTransaction(raw)
		}
	}()

	// Concurrently flood Rollup 2
	go func() {
		defer wg.Done()
		for i := uint64(0); i < 15; i++ {
			tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce: i, GasPrice: big.NewInt(1e9), Gas: 21000, To: &recipientAddr, Value: big.NewInt(1e14),
			}), signer2, userKey)
			raw, _ := tx.MarshalBinary()
			_, _ = seq2.SubmitRawTransaction(raw)
		}
	}()

	wg.Wait()
	time.Sleep(150 * time.Millisecond)

	s1 := seq1.Status()
	s2 := seq2.Status()

	if s1.CurrentBlock == 0 || s2.CurrentBlock == 0 {
		t.Fatalf("expected both rollups to produce blocks: r1=%d, r2=%d", s1.CurrentBlock, s2.CurrentBlock)
	}

	if s1.CurrentState == s2.CurrentState {
		t.Fatalf("rollups should have completely distinct state roots: %s == %s", s1.CurrentState.Hex(), s2.CurrentState.Hex())
	}

	t.Logf("Multi-Rollup Parallel Execution Verified:\n  Rollup 1: Blocks=%d, Txs=%d, StateRoot=%s\n  Rollup 2: Blocks=%d, Txs=%d, StateRoot=%s",
		s1.CurrentBlock, s1.TotalTxsServed, s1.CurrentState.Hex(),
		s2.CurrentBlock, s2.TotalTxsServed, s2.CurrentState.Hex(),
	)
}

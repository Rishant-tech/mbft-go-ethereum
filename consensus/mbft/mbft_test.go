// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package mbft

import (
	"bytes"
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// newTestKey creates a new random ECDSA private key and address.
func newTestKey(t *testing.T) (*ecdsa.PrivateKey, common.Address) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	return key, addr
}

// signHeader signs a header with the provided key.
func signHeader(header *types.Header, key *ecdsa.PrivateKey) {
	if len(header.Extra) < extraVanity {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-len(header.Extra))...)
	}
	header.Extra = header.Extra[:extraVanity]
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	hash := SealHash(header)
	sig, err := crypto.Sign(hash.Bytes(), key)
	if err != nil {
		panic(err)
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sig)
}

// dummyChain implements consensus.ChainHeaderReader for unit testing.
type dummyChain struct {
	headers map[common.Hash]*types.Header
	config  *params.ChainConfig
}

func newDummyChain(config *params.ChainConfig) *dummyChain {
	return &dummyChain{
		headers: make(map[common.Hash]*types.Header),
		config:  config,
	}
}

func (c *dummyChain) Config() *params.ChainConfig { return c.config }
func (c *dummyChain) CurrentHeader() *types.Header { return nil }
func (c *dummyChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	return c.headers[hash]
}
func (c *dummyChain) GetHeaderByNumber(number uint64) *types.Header {
	for _, h := range c.headers {
		if h.Number.Uint64() == number {
			return h
		}
	}
	return nil
}
func (c *dummyChain) GetHeaderByHash(hash common.Hash) *types.Header {
	return c.headers[hash]
}

// TestQuorumCalculation verifies that 33% quorum calculations correctly compute
// the anti-monopoly sliding window to prevent deadlock under reduced node participation.
func TestQuorumCalculation(t *testing.T) {
	config := &params.MBFTConfig{
		Period:    1,
		Epoch:     30000,
		MinQuorum: 33, // 33% nodes threshold
	}

	snap := newSnapshot(config, nil, 0, common.Hash{}, nil)

	// 1 validator: limit = 1
	snap.Validators[common.Address{0x01}] = 100
	if limit := snap.QuorumLimit(); limit != 1 {
		t.Fatalf("expected limit 1 for 1 validator, got %d", limit)
	}

	// 3 validators: 3 * 33 / 100 = 0 -> clamped to 1.
	// Allows 1 active validator (33%) to rotate without deadlock!
	snap.Validators[common.Address{0x02}] = 100
	snap.Validators[common.Address{0x03}] = 100
	if limit := snap.QuorumLimit(); limit != 1 {
		t.Fatalf("expected limit 1 for 3 validators with 33%% quorum, got %d", limit)
	}

	// 10 validators: 10 * 33 / 100 = 3.
	// Allows 3 active validators (33%) to rotate continuously without deadlock!
	for i := 4; i <= 10; i++ {
		snap.Validators[common.Address{byte(i)}] = 100
	}
	if limit := snap.QuorumLimit(); limit != 3 {
		t.Fatalf("expected limit 3 for 10 validators with 33%% quorum, got %d", limit)
	}
}

// TestMBFTHeaderVerification tests header sealing and verification.
func TestMBFTHeaderVerification(t *testing.T) {
	key1, addr1 := newTestKey(t)
	key2, addr2 := newTestKey(t)
	_, addr3 := newTestKey(t)

	db := rawdb.NewMemoryDatabase()
	mbftConfig := &params.MBFTConfig{
		Period:           1,
		Epoch:            30000,
		MinQuorum:        33,
		MinSecurityScore: 70,
	}
	chainConfig := &params.ChainConfig{
		ChainID: big.NewInt(1337),
		MBFT:    mbftConfig,
	}

	chain := newDummyChain(chainConfig)
	engine := New(mbftConfig, db)

	genesisExtra := make([]byte, extraVanity)
	for _, addr := range []common.Address{addr1, addr2, addr3} {
		genesisExtra = append(genesisExtra, addr[:]...)
	}
	genesisExtra = append(genesisExtra, make([]byte, extraSeal)...)

	genesis := &types.Header{
		Number:     big.NewInt(0),
		Time:       1000,
		Extra:      genesisExtra,
		Difficulty: diffInTurn,
		GasLimit:   10000000,
	}
	chain.headers[genesis.Hash()] = genesis

	snap, err := engine.snapshot(chain, 0, genesis.Hash(), nil)
	if err != nil {
		t.Fatalf("failed to create genesis snapshot: %v", err)
	}

	vals := snap.validatorsAscending()
	t.Logf("Validators count: %d", len(vals))

	var (
		inTurnKey  *ecdsa.PrivateKey
		inTurnAddr common.Address
	)
	if snap.inturn(1, addr1) {
		inTurnKey, inTurnAddr = key1, addr1
	} else if snap.inturn(1, addr2) {
		inTurnKey, inTurnAddr = key2, addr2
	} else {
		inTurnKey, inTurnAddr = key1, addr1
	}

	block1 := &types.Header{
		ParentHash: genesis.Hash(),
		Number:     big.NewInt(1),
		Time:       genesis.Time + mbftConfig.Period,
		Difficulty: diffInTurn,
		GasLimit:   10000000,
	}
	if !snap.inturn(1, inTurnAddr) {
		block1.Difficulty = diffNoTurn
	}

	signHeader(block1, inTurnKey)

	if err := engine.VerifyHeader(chain, block1); err != nil {
		t.Fatalf("failed to verify valid block 1: %v", err)
	}

	author, err := engine.Author(block1)
	if err != nil {
		t.Fatalf("failed to recover author: %v", err)
	}
	if author != inTurnAddr {
		t.Fatalf("expected author %x, got %x", inTurnAddr, author)
	}
}

// TestSecurityScoreThreshold verifies that validators below MinSecurityScore are rejected.
func TestSecurityScoreThreshold(t *testing.T) {
	key, addr := newTestKey(t)
	mbftConfig := &params.MBFTConfig{
		Period:           1,
		Epoch:            30000,
		MinQuorum:        33,
		MinSecurityScore: 70,
	}
	snap := newSnapshot(mbftConfig, nil, 0, common.Hash{}, []common.Address{addr})

	snap.Validators[addr] = 50

	header := &types.Header{
		Number:     big.NewInt(1),
		Time:       1001,
		Difficulty: diffInTurn,
	}
	signHeader(header, key)

	_, err := snap.apply([]*types.Header{header})
	if err == nil {
		t.Fatal("expected error for low security score, got nil")
	}
	t.Logf("Correctly rejected degraded node: %v", err)
}

// TestUnauthorizedSigner verifies that non-validators cannot produce blocks.
func TestUnauthorizedSigner(t *testing.T) {
	_, valAddr := newTestKey(t)
	rogueKey, _ := newTestKey(t)

	mbftConfig := &params.MBFTConfig{
		Period:           1,
		Epoch:            30000,
		MinQuorum:        33,
		MinSecurityScore: 70,
	}
	snap := newSnapshot(mbftConfig, nil, 0, common.Hash{}, []common.Address{valAddr})

	header := &types.Header{
		Number:     big.NewInt(1),
		Time:       1001,
		Difficulty: diffInTurn,
	}
	signHeader(header, rogueKey)

	_, err := snap.apply([]*types.Header{header})
	if err == nil {
		t.Fatal("expected error for unauthorized rogue signer, got nil")
	}
	t.Logf("Correctly rejected unauthorized signer: %v", err)
}

// TestContinuous33PercentQuorumBlockProduction verifies that when 67% of validators are offline
// (e.g. only 1 out of 3 validators is online = 33% participation), the network does NOT deadlock
// and can continuously produce blocks under mBFT rules.
func TestContinuous33PercentQuorumBlockProduction(t *testing.T) {
	key1, addr1 := newTestKey(t)
	_, addr2 := newTestKey(t)
	_, addr3 := newTestKey(t)

	mbftConfig := &params.MBFTConfig{
		Period:           1,
		Epoch:            30000,
		MinQuorum:        33,
		MinSecurityScore: 70,
	}

	snap := newSnapshot(mbftConfig, nil, 0, common.Hash{}, []common.Address{addr1, addr2, addr3})

	currentTime := uint64(1000)
	for blockNum := uint64(1); blockNum <= 5; blockNum++ {
		header := &types.Header{
			Number:     new(big.Int).SetUint64(blockNum),
			Time:       currentTime + blockNum*mbftConfig.Period,
			Difficulty: diffInTurn,
		}
		if !snap.inturn(blockNum, addr1) {
			header.Difficulty = diffNoTurn
		}
		signHeader(header, key1)

		var err error
		snap, err = snap.apply([]*types.Header{header})
		if err != nil {
			t.Fatalf("failed to produce block %d with 33%% active quorum: %v", blockNum, err)
		}
		t.Logf("Successfully produced block %d with 33%% quorum", blockNum)
	}
}

// TestMBFTLeaderReelectionAndPenalty verifies that when a primary leader is unavailable,
// an auto-reelected leader can seal the block and the unresponsive leader receives a penalty.
func TestMBFTLeaderReelectionAndPenalty(t *testing.T) {
	key1, addr1 := newTestKey(t)
	key2, addr2 := newTestKey(t)
	key3, addr3 := newTestKey(t)

	keyMap := map[common.Address]*ecdsa.PrivateKey{
		addr1: key1,
		addr2: key2,
		addr3: key3,
	}

	mbftConfig := &params.MBFTConfig{
		Period:           1,
		Epoch:            30000,
		MinQuorum:        33,
		MinSecurityScore: 70,
	}

	snap := newSnapshot(mbftConfig, nil, 0, common.Hash{}, []common.Address{addr1, addr2, addr3})

	primaryLeader := snap.Leader(1, 0)
	successorLeader := snap.Leader(1, 1)

	if primaryLeader == successorLeader {
		t.Fatal("expected distinct leaders for consecutive views")
	}

	successorKey := keyMap[successorLeader]

	header := &types.Header{
		Number:     big.NewInt(1),
		Time:       1002, // 1000 + period (1) + 1 second view delay
		Difficulty: diffNoTurn,
	}
	signHeader(header, successorKey)

	initialPrimaryScore := snap.Validators[primaryLeader]
	newSnap, err := snap.apply([]*types.Header{header})
	if err != nil {
		t.Fatalf("failed to apply re-elected leader block: %v", err)
	}

	newPrimaryScore := newSnap.Validators[primaryLeader]
	if newPrimaryScore >= initialPrimaryScore {
		t.Fatalf("expected primary leader score to drop from %d, got %d", initialPrimaryScore, newPrimaryScore)
	}
	t.Logf("Primary leader %x penalized: %d -> %d", primaryLeader, initialPrimaryScore, newPrimaryScore)

	newSuccessorScore := newSnap.Validators[successorLeader]
	t.Logf("Successor leader %x score: %d", successorLeader, newSuccessorScore)
}

type dummyStateDB struct {
	vm.StateDB
	balances map[common.Address]*uint256.Int
}

func (s *dummyStateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	if s.balances[addr] == nil {
		s.balances[addr] = new(uint256.Int)
	}
	s.balances[addr].Add(s.balances[addr], amount)
	return *s.balances[addr]
}

func (s *dummyStateDB) GetBalance(addr common.Address) *uint256.Int {
	if b, ok := s.balances[addr]; ok {
		return b
	}
	return new(uint256.Int)
}

// TestSecurityValidationReward verifies that mBFT Finalize credits the proposing
// validator with the security validation reward (not a PoW mining fee).
func TestSecurityValidationReward(t *testing.T) {
	_, addr1 := newTestKey(t)
	mbftConfig := &params.MBFTConfig{
		Period:           1,
		Epoch:            30000,
		MinQuorum:        33,
		MinSecurityScore: 70,
		BlockReward:      big.NewInt(1000000000000000000), // 1 ETH/NAORIS reward
	}
	engine := New(mbftConfig, rawdb.NewMemoryDatabase())

	state := &dummyStateDB{balances: make(map[common.Address]*uint256.Int)}
	header := &types.Header{
		Coinbase: addr1,
		Number:   big.NewInt(1),
	}

	engine.Finalize(nil, header, state, nil)

	balance := state.GetBalance(addr1)
	expected := uint256.NewInt(1000000000000000000)
	if balance.Cmp(expected) != 0 {
		t.Fatalf("expected balance %s, got %s", expected, balance)
	}
	t.Logf("Successfully credited validator security reward: %s wei", balance)
}

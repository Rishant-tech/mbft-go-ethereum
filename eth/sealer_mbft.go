// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package eth

import (
	"context"
	"crypto/ecdsa"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/mbft"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
)

// mbftSealer autonomously proposes, seals, and inserts blocks for mBFT consensus.
type mbftSealer struct {
	eth       *Ethereum
	mbft      *mbft.MBFT
	validator common.Address
	privKey   *ecdsa.PrivateKey
	signFn    mbft.SignerFn
	quit      chan struct{}
	wg        sync.WaitGroup
}

// newMBFTSealer constructs a new autonomous mBFT block sealer.
func newMBFTSealer(eth *Ethereum, engine *mbft.MBFT) *mbftSealer {
	return &mbftSealer{
		eth:  eth,
		mbft: engine,
		quit: make(chan struct{}),
	}
}

// start configures credentials and begins the autonomous block sealing loop.
func (s *mbftSealer) start() {
	validator := s.eth.miner.PendingFeeRecipient()

	var ks *keystore.KeyStore
	if keystores := s.eth.accountManager.Backends(keystore.KeyStoreType); len(keystores) > 0 {
		ks = keystores[0].(*keystore.KeyStore)
	}

	if validator == (common.Address{}) && ks != nil && len(ks.Accounts()) > 0 {
		validator = ks.Accounts()[0].Address
		s.eth.miner.SetPendingFeeRecipient(validator)
	}

	if validator == (common.Address{}) {
		log.Info("mBFT Sealer: running in passive observer mode (no fee recipient configured)")
		return
	}
	s.validator = validator

	// Attempt keystore unlock
	passwords := []string{"password123"}
	for _, p := range []string{"/config/password.txt", "docker/mbft/password.txt"} {
		if data, err := os.ReadFile(p); err == nil {
			passwords = append([]string{strings.TrimSpace(string(data))}, passwords...)
		}
	}

	if ks != nil {
		acc := accounts.Account{Address: validator}
		for _, pass := range passwords {
			if err := ks.Unlock(acc, pass); err == nil {
				log.Info("mBFT Sealer: unlocked validator account from keystore", "validator", validator.Hex())
				s.signFn = func(signer common.Address, hash []byte) ([]byte, error) {
					return ks.SignHash(acc, hash)
				}
				break
			}
		}
	}

	// Deterministic validator keys fallback for docker/local clusters
	if s.signFn == nil {
		knownKeys := map[common.Address]string{
			common.HexToAddress("0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"): "0000000000000000000000000000000000000000000000000000000000000001",
			common.HexToAddress("0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF"): "0000000000000000000000000000000000000000000000000000000000000002",
			common.HexToAddress("0x6813Eb9362372EEF6200f3b1dbC3f819671cBA69"): "0000000000000000000000000000000000000000000000000000000000000003",
		}
		if keyHex, ok := knownKeys[validator]; ok {
			k, _ := crypto.HexToECDSA(keyHex)
			s.privKey = k
			s.signFn = func(signer common.Address, hash []byte) ([]byte, error) {
				return crypto.Sign(hash, k)
			}
			log.Info("mBFT Sealer: loaded validator signing credentials", "validator", validator.Hex())
		}
	}

	if s.signFn == nil {
		log.Warn("mBFT Sealer: signing credentials not found, running read-only", "validator", validator.Hex())
		return
	}

	s.mbft.Authorize(validator, s.signFn)
	log.Info("=== mBFT Autonomous Sealer Active ===", "validator", validator.Hex())

	s.wg.Add(1)
	go s.loop()
}

// stop halts the block sealing loop.
func (s *mbftSealer) stop() {
	select {
	case <-s.quit:
		return
	default:
		close(s.quit)
		s.wg.Wait()
	}
}

// loop runs the periodic block proposal and sealing cycle.
func (s *mbftSealer) loop() {
	defer s.wg.Done()

	periodSec := uint64(2)
	if cfg := s.eth.blockchain.Config().MBFT; cfg != nil && cfg.Period > 0 {
		periodSec = cfg.Period
	}
	period := time.Duration(periodSec) * time.Second

	ticker := time.NewTicker(period)
	defer ticker.Stop()

	// Initial seal attempt after brief startup stabilization
	select {
	case <-time.After(500 * time.Millisecond):
		s.sealOneBlock(periodSec)
	case <-s.quit:
		return
	}

	for {
		select {
		case <-s.quit:
			return
		case <-ticker.C:
			s.sealOneBlock(periodSec)
		}
	}
}

// sealOneBlock inspects pending transactions, constructs work, signs, and inserts a block.
func (s *mbftSealer) sealOneBlock(periodSec uint64) {
	parent := s.eth.blockchain.CurrentBlock()
	if parent == nil {
		return
	}
	nextNumber := parent.Number.Uint64() + 1

	snap, err := s.mbft.Snapshot(s.eth.blockchain, parent.Number.Uint64(), parent.Hash(), nil)
	if err != nil {
		return
	}

	score, authorized := snap.Validators[s.validator]
	if !authorized || score < snap.MinScore() {
		return
	}

	// Sliding window quorum check
	limit := snap.QuorumLimit()
	for seen, recent := range snap.Recents {
		if recent == s.validator && nextNumber < seen+limit {
			return
		}
	}

	// Turn calculation: in-turn leader vs view-change successor
	inturn := snap.Inturn(nextNumber, s.validator)
	if !inturn {
		view, ok := snap.ViewOfValidator(nextNumber, s.validator)
		if !ok {
			return
		}
		delay := time.Duration(view) * 500 * time.Millisecond
		select {
		case <-time.After(delay):
		case <-s.quit:
			return
		}
		// If another validator sealed the block during view wait, abort this slot
		if s.eth.blockchain.CurrentBlock().Number.Uint64() >= nextNumber {
			return
		}
	}

	now := uint64(time.Now().Unix())
	if now <= parent.Time {
		now = parent.Time + periodSec
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	args := &miner.BuildPayloadArgs{
		Parent:       parent.Hash(),
		Timestamp:    now,
		FeeRecipient: s.validator,
		Random:       common.Hash{},
		Withdrawals:  types.Withdrawals{},
		Version:      engine.PayloadV3,
	}

	payload, err := s.eth.miner.BuildPayload(ctx, args, false)
	if err != nil {
		log.Debug("mBFT Sealer: payload build error", "err", err)
		return
	}

	block := payload.Block()
	header := block.Header()

	// Consensus engine preparation: vanity padding, checkpoint signers, difficulty
	if err := s.mbft.Prepare(s.eth.blockchain, header); err != nil {
		log.Debug("mBFT Sealer: prepare error", "err", err)
		return
	}

	results := make(chan *types.Block, 1)
	stop := make(chan struct{})

	if err := s.mbft.Seal(s.eth.blockchain, block.WithSeal(header), results, stop); err != nil {
		close(stop)
		log.Debug("mBFT Sealer: seal rejected", "err", err)
		return
	}

	select {
	case sealed := <-results:
		close(stop)
		if _, err := s.eth.blockchain.InsertChain(types.Blocks{sealed}); err != nil {
			log.Warn("mBFT Sealer: insert block error", "err", err)
			return
		}
		s.eth.blockchain.SetFinalized(sealed.Header())
		log.Info("mBFT [LOCAL VALIDATOR SEALED BLOCK]",
			"number", sealed.NumberU64(),
			"hash", sealed.Hash().Hex(),
			"txs", len(sealed.Transactions()),
			"gasUsed", sealed.GasUsed(),
			"localValidator", s.validator.Hex(),
		)
	case <-s.quit:
		close(stop)
		return
	case <-time.After(3 * time.Second):
		close(stop)
		return
	}
}

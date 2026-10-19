// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package mbft

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// API is a user facing RPC API to interact with the mBFT consensus engine.
type API struct {
	chain consensus.ChainHeaderReader
	mbft  *MBFT
}

// GetSnapshot retrieves the state snapshot at a given block.
func (api *API) GetSnapshot(number *rpc.BlockNumber) (*Snapshot, error) {
	header := api.headerByNumber(number)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.mbft.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
}

// GetSnapshotAtHash retrieves the state snapshot at a given block hash.
func (api *API) GetSnapshotAtHash(hash common.Hash) (*Snapshot, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.mbft.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
}

// GetValidators retrieves the list of authorized validators at a given block.
func (api *API) GetValidators(number *rpc.BlockNumber) ([]common.Address, error) {
	snap, err := api.GetSnapshot(number)
	if err != nil {
		return nil, err
	}
	return snap.validatorsAscending(), nil
}

// GetSecurityScore returns the security score of a validator at a given block.
func (api *API) GetSecurityScore(address common.Address, number *rpc.BlockNumber) (uint64, error) {
	snap, err := api.GetSnapshot(number)
	if err != nil {
		return 0, err
	}
	score, ok := snap.Validators[address]
	if !ok {
		return 0, fmt.Errorf("validator %x not authorized", address)
	}
	return score, nil
}

// Propose injects a new authorization proposal that the validator will vote on.
func (api *API) Propose(address common.Address, auth bool) {
	api.mbft.lock.Lock()
	defer api.mbft.lock.Unlock()

	api.mbft.proposals[address] = auth
}

// Discard drops a currently running proposal.
func (api *API) Discard(address common.Address) {
	api.mbft.lock.Lock()
	defer api.mbft.lock.Unlock()

	delete(api.mbft.proposals, address)
}

// headerByNumber returns the header corresponding to the requested block number.
func (api *API) headerByNumber(number *rpc.BlockNumber) *types.Header {
	if number == nil || *number == rpc.LatestBlockNumber {
		return api.chain.CurrentHeader()
	}
	if number.Int64() < 0 {
		return nil
	}
	return api.chain.GetHeaderByNumber(uint64(number.Int64()))
}

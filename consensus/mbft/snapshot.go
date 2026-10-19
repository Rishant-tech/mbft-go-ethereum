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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
)

var (
	errRecentlySigned          = errors.New("recently signed")
	errUnauthorizedValidator   = errors.New("unauthorized validator")
	errInsufficientSecurity    = errors.New("validator security score below required threshold")
	errInvalidVoting           = errors.New("invalid voting parameters")
	errInvalidCheckpointSigners = errors.New("invalid checkpoint validator set")
)

// Vote represents a single vote that an authorized validator made to modify the
// authorization or security score of a validator.
type Vote struct {
	Signer    common.Address `json:"signer"`    // Validator that cast this vote
	Block     uint64         `json:"block"`     // Block number the vote was cast in
	Address   common.Address `json:"address"`   // Account being voted on
	Authorize bool           `json:"authorize"` // Whether to authorize or deauthorize
}

// Tally tracks current voting score for a proposed change.
type Tally struct {
	Authorize bool `json:"authorize"` // Whether the vote is about authorizing or removing
	Votes     int  `json:"votes"`     // Number of votes accumulated so far
}

type sigLRU = lru.Cache[common.Hash, common.Address]

// Snapshot represents the state of mBFT validator authorizations and security scores at a block.
type Snapshot struct {
	config   *params.MBFTConfig // Consensus parameters
	sigcache *sigLRU            // Cache of recent block signatures

	Number     uint64                    `json:"number"`     // Block number where snapshot was created
	Hash       common.Hash               `json:"hash"`       // Block hash where snapshot was created
	Validators map[common.Address]uint64 `json:"validators"` // Validator address -> Security Score (0-100)
	Recents    map[uint64]common.Address `json:"recents"`    // Recent block signers for anti-monopoly sliding window
	Votes      []*Vote                   `json:"votes"`      // List of pending votes cast in chronological order
	Tally      map[common.Address]Tally  `json:"tally"`      // Current vote tally
}

// newSnapshot creates a new snapshot with the given startup parameters.
func newSnapshot(config *params.MBFTConfig, sigcache *sigLRU, number uint64, hash common.Hash, validators []common.Address) *Snapshot {
	snap := &Snapshot{
		config:     config,
		sigcache:   sigcache,
		Number:     number,
		Hash:       hash,
		Validators: make(map[common.Address]uint64),
		Recents:    make(map[uint64]common.Address),
		Tally:      make(map[common.Address]Tally),
	}
	defaultScore := uint64(100)
	for _, val := range validators {
		snap.Validators[val] = defaultScore
	}
	return snap
}

// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(config *params.MBFTConfig, sigcache *sigLRU, db ethdb.Database, hash common.Hash) (*Snapshot, error) {
	blob, err := db.Get(append(rawdb.MbftSnapshotPrefix, hash[:]...))
	if err != nil {
		return nil, err
	}
	snap := new(Snapshot)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	snap.config = config
	snap.sigcache = sigcache
	return snap, nil
}

// store inserts the snapshot into the database.
func (s *Snapshot) store(db ethdb.Database) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return db.Put(append(rawdb.MbftSnapshotPrefix, s.Hash[:]...), blob)
}

// copy creates a deep copy of the snapshot.
func (s *Snapshot) copy() *Snapshot {
	return &Snapshot{
		config:     s.config,
		sigcache:   s.sigcache,
		Number:     s.Number,
		Hash:       s.Hash,
		Validators: maps.Clone(s.Validators),
		Recents:    maps.Clone(s.Recents),
		Votes:      slices.Clone(s.Votes),
		Tally:      maps.Clone(s.Tally),
	}
}

// QuorumLimit returns the anti-monopoly sliding window limit.
// Under mBFT, this adapts according to MinQuorum percentage (default 33%).
// This allows the network to maintain block production liveness with as few as
// 33% nodes online without causing deadlocks in the signer rotation window.
func (s *Snapshot) QuorumLimit() uint64 {
	minQ := s.config.MinQuorum
	if minQ == 0 {
		minQ = 33 // 33% fault tolerance threshold
	}
	valCount := uint64(len(s.Validators))
	if valCount == 0 {
		return 1
	}
	limit := (valCount * minQ) / 100
	if limit < 1 {
		limit = 1
	}
	return limit
}

// MinScore returns the minimum required security score for a validator to produce blocks.
func (s *Snapshot) MinScore() uint64 {
	minScore := s.config.MinSecurityScore
	if minScore == 0 {
		minScore = 70 // Default baseline security score
	}
	return minScore
}

// validatorsAscending returns the sorted list of authorized validators.
func (s *Snapshot) validatorsAscending() []common.Address {
	vals := make([]common.Address, 0, len(s.Validators))
	for val := range s.Validators {
		vals = append(vals, val)
	}
	sort.Slice(vals, func(i, j int) bool {
		return bytes.Compare(vals[i][:], vals[j][:]) < 0
	})
	return vals
}

// Leader returns the elected leader/inspector for a given block number and view.
// In mBFT: Leader(number, view) = sortedValidators[(number + view) % len(sortedValidators)]
func (s *Snapshot) Leader(number uint64, view uint64) common.Address {
	vals := s.validatorsAscending()
	if len(vals) == 0 {
		return common.Address{}
	}
	offset := (number + view) % uint64(len(vals))
	return vals[offset]
}

// ViewOfValidator returns the lowest view >= 0 at which the validator is the elected leader.
func (s *Snapshot) ViewOfValidator(number uint64, validator common.Address) (uint64, bool) {
	vals := s.validatorsAscending()
	for v := uint64(0); v < uint64(len(vals)); v++ {
		if s.Leader(number, v) == validator {
			return v, true
		}
	}
	return 0, false
}

// inturn returns true if the validator is scheduled as the primary in-turn leader (view 0).
func (s *Snapshot) inturn(number uint64, validator common.Address) bool {
	return s.Leader(number, 0) == validator
}

// validVote returns whether it makes sense to cast a vote on the address.
func (s *Snapshot) validVote(address common.Address, authorize bool) bool {
	_, exists := s.Validators[address]
	return (exists && !authorize) || (!exists && authorize)
}

// cast adds a new vote into the snapshot tally.
func (s *Snapshot) cast(address common.Address, authorize bool) bool {
	if !s.validVote(address, authorize) {
		return false
	}
	if old, ok := s.Tally[address]; ok {
		old.Votes++
		s.Tally[address] = old
	} else {
		s.Tally[address] = Tally{Authorize: authorize, Votes: 1}
	}
	return true
}

// uncast removes a previously cast vote from the tally.
func (s *Snapshot) uncast(address common.Address, authorize bool) bool {
	tally, ok := s.Tally[address]
	if !ok {
		return false
	}
	if tally.Authorize != authorize {
		return false
	}
	if tally.Votes > 1 {
		tally.Votes--
		s.Tally[address] = tally
	} else {
		delete(s.Tally, address)
	}
	return true
}

// apply creates a new snapshot by advancing through the given headers.
func (s *Snapshot) apply(headers []*types.Header) (*Snapshot, error) {
	if len(headers) == 0 {
		return s, nil
	}
	for i := 0; i < len(headers)-1; i++ {
		if headers[i+1].Number.Uint64() != headers[i].Number.Uint64()+1 {
			return nil, fmt.Errorf("non-contiguous headers: %d and %d", headers[i].Number.Uint64(), headers[i+1].Number.Uint64())
		}
	}
	if headers[0].Number.Uint64() != s.Number+1 {
		return nil, fmt.Errorf("header block %d not consecutive to snapshot %d", headers[0].Number.Uint64(), s.Number)
	}

	snap := s.copy()
	minScore := snap.MinScore()
	limit := snap.QuorumLimit()

	for _, header := range headers {
		number := header.Number.Uint64()

		// Discard oldest recent signers beyond sliding window limit
		if limit <= number {
			delete(snap.Recents, number-limit)
		}

		// Recover validator address
		validator, err := ecrecover(header, snap.sigcache)
		if err != nil {
			return nil, err
		}

		// 1. Must be authorized validator
		score, authorized := snap.Validators[validator]
		if !authorized {
			return nil, fmt.Errorf("%w: %x", errUnauthorizedValidator, validator)
		}

		// 2. Must meet security threshold (mBFT security-first invariant)
		if score < minScore {
			return nil, fmt.Errorf("%w: validator %x score %d < threshold %d", errInsufficientSecurity, validator, score, minScore)
		}

		// 3. Must not have signed within recent limit (anti-monopoly protection)
		for seen, recent := range snap.Recents {
			if recent == validator {
				return nil, fmt.Errorf("%w: validator %x signed block %d, window limit %d", errRecentlySigned, validator, seen, limit)
			}
		}
		snap.Recents[number] = validator

		// 4. mBFT Leader Re-election & Security Scoring:
		// Check whether this block was proposed by the primary in-turn leader or an auto-reelected leader
		if view, ok := snap.ViewOfValidator(number, validator); ok {
			if view > 0 {
				// Re-election occurred! Penalize skipped/offline leaders from view 0 to view-1
				for v := uint64(0); v < view; v++ {
					skipped := snap.Leader(number, v)
					if currentScore, exists := snap.Validators[skipped]; exists && currentScore > 0 {
						if currentScore > 2 {
							snap.Validators[skipped] = currentScore - 2
						} else {
							snap.Validators[skipped] = 0
						}
					}
				}
			}
			// Reward the successful proposing leader/inspector for maintaining network liveness
			if currentScore, exists := snap.Validators[validator]; exists && currentScore < 100 {
				snap.Validators[validator] = currentScore + 1
			}
		}

		// 5. Handle voting authorization proposal
		if header.Nonce != (types.BlockNonce{}) {
			beneficiary := header.Coinbase
			var authorize bool
			if bytes.Equal(header.Nonce[:], nonceAuthVote) {
				authorize = true
			} else if bytes.Equal(header.Nonce[:], nonceDropVote) {
				authorize = false
			} else {
				return nil, errors.New("invalid voting nonce")
			}

			// De-duplicate previous vote from this validator for beneficiary
			for i, v := range snap.Votes {
				if v.Signer == validator && v.Address == beneficiary {
					snap.uncast(v.Address, v.Authorize)
					snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
					break
				}
			}

			// Record and tally new vote
			if snap.cast(beneficiary, authorize) {
				snap.Votes = append(snap.Votes, &Vote{
					Signer:    validator,
					Block:     number,
					Address:   beneficiary,
					Authorize: authorize,
				})
			}

			// Check if vote tally reached required threshold
			requiredVotes := (len(snap.Validators) / 2) + 1
			if tally := snap.Tally[beneficiary]; tally.Votes >= requiredVotes {
				if tally.Authorize {
					snap.Validators[beneficiary] = defaultScoreVal
				} else {
					delete(snap.Validators, beneficiary)
					for seen, rec := range snap.Recents {
						if rec == beneficiary {
							delete(snap.Recents, seen)
						}
					}
				}
				for i := 0; i < len(snap.Votes); i++ {
					if snap.Votes[i].Address == beneficiary {
						snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
						i--
					}
				}
				delete(snap.Tally, beneficiary)
				limit = snap.QuorumLimit()
			}
		}

		// 6. Checkpoint epoch reset
		if number%snap.config.Epoch == 0 {
			snap.Votes = nil
			snap.Tally = make(map[common.Address]Tally)
		}
	}

	snap.Number = headers[len(headers)-1].Number.Uint64()
	snap.Hash = headers[len(headers)-1].Hash()
	return snap, nil
}

const defaultScoreVal = uint64(100)

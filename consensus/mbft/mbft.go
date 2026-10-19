// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Package mbft implements the Modified Byzantine Fault Tolerance (mBFT) consensus engine.
package mbft

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/keccak"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

const (
	checkpointInterval = 1024 // Number of blocks after which to save the snapshot to the database
	inmemorySnapshots  = 128  // Number of recent snapshots to keep in memory
	inmemorySignatures = 4096 // Number of recent block signatures to keep in memory
	wiggleTime         = 500 * time.Millisecond // Extra delay for out-of-turn validators
)

// mBFT protocol constants.
var (
	epochLength = uint64(30000) // Default number of blocks after which to checkpoint and reset pending votes

	extraVanity = 32                     // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal   = crypto.SignatureLength // Fixed number of extra-data suffix bytes reserved for signer seal (65 bytes)

	nonceAuthVote = hexutil.MustDecode("0xffffffffffffffff") // Nonce to vote on authorizing a validator
	nonceDropVote = hexutil.MustDecode("0x0000000000000000") // Nonce to vote on deauthorizing a validator

	diffInTurn = big.NewInt(2) // Block difficulty for in-turn signatures
	diffNoTurn = big.NewInt(1) // Block difficulty for out-of-turn signatures
)

var (
	errUnknownBlock                 = errors.New("unknown block")
	errInvalidCheckpointBeneficiary = errors.New("beneficiary in checkpoint block non-zero")
	errInvalidVote                  = errors.New("vote nonce not 0x00..0 or 0xff..f")
	errInvalidCheckpointVote        = errors.New("vote nonce in checkpoint block non-zero")
	errMissingVanity                = errors.New("extra-data 32 byte vanity prefix missing")
	errMissingSignature             = errors.New("extra-data 65 byte signature suffix missing")
	errExtraSigners                 = errors.New("non-checkpoint block contains extra signer list")
	errMismatchingCheckpointSigners = errors.New("mismatching signer list on checkpoint block")
	errInvalidDifficulty            = errors.New("invalid difficulty")
	errWrongDifficulty              = errors.New("wrong difficulty")
	errInvalidTimestamp             = errors.New("invalid timestamp")
	errWaitTurn                     = errors.New("have to wait for turn or quorum rotation")
)

// SignerFn hashes and signs data to be sealed by a backing account key.
type SignerFn func(signer common.Address, hash []byte) ([]byte, error)

// ecrecover extracts the validator address from a signed header.
func ecrecover(header *types.Header, sigcache *sigLRU) (common.Address, error) {
	hash := header.Hash()
	if sigcache != nil {
		if address, known := sigcache.Get(hash); known {
			return address, nil
		}
	}
	if len(header.Extra) < extraSeal {
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:]

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(SealHash(header).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	if sigcache != nil {
		sigcache.Add(hash, signer)
	}
	return signer, nil
}

// MBFT is the Modified Byzantine Fault Tolerance consensus engine.
type MBFT struct {
	config *params.MBFTConfig // Consensus parameters
	db     ethdb.Database     // Database to store snapshot checkpoints

	recents    *lru.Cache[common.Hash, *Snapshot] // Snapshots for recent blocks
	signatures *sigLRU                            // Signatures of recent blocks

	proposals map[common.Address]bool // Current list of pending validator proposals

	signer common.Address // Local authorized validator address
	signFn SignerFn       // Signing callback function
	lock   sync.RWMutex   // Protects signer, signFn, and proposals
}

// New creates an mBFT consensus engine with parameters from config.
func New(config *params.MBFTConfig, db ethdb.Database) *MBFT {
	conf := *config
	if conf.Epoch == 0 {
		conf.Epoch = epochLength
	}
	if conf.MinQuorum == 0 {
		conf.MinQuorum = 33 // 33% fault tolerance
	}
	if conf.MinSecurityScore == 0 {
		conf.MinSecurityScore = 70 // Minimum 70/100 security score
	}

	recents := lru.NewCache[common.Hash, *Snapshot](inmemorySnapshots)
	signatures := lru.NewCache[common.Hash, common.Address](inmemorySignatures)

	return &MBFT{
		config:     &conf,
		db:         db,
		recents:    recents,
		signatures: signatures,
		proposals:  make(map[common.Address]bool),
	}
}

// Author implements consensus.Engine, returning the address of the validator that minted the block.
func (m *MBFT) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, m.signatures)
}

// VerifyHeader checks whether a header conforms to the mBFT consensus rules.
func (m *MBFT) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return m.verifyHeader(chain, header, nil)
}

// VerifyHeaders verifies a batch of headers concurrently.
func (m *MBFT) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := m.verifyHeader(chain, header, headers[:i])
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// verifyHeader checks whether a header conforms to the consensus rules.
func (m *MBFT) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	number := header.Number.Uint64()

	// Don't waste time checking blocks from the future
	if header.Time > uint64(time.Now().Unix()) {
		return consensus.ErrFutureBlock
	}

	checkpoint := (number % m.config.Epoch) == 0
	if checkpoint && header.Coinbase != (common.Address{}) {
		return errInvalidCheckpointBeneficiary
	}

	// Nonce must be auth vote, drop vote, or 0
	if !bytes.Equal(header.Nonce[:], nonceAuthVote) && !bytes.Equal(header.Nonce[:], nonceDropVote) && !bytes.Equal(header.Nonce[:], make([]byte, 8)) {
		return errInvalidVote
	}
	if checkpoint && !bytes.Equal(header.Nonce[:], make([]byte, 8)) {
		return errInvalidCheckpointVote
	}

	// Check extra-data size
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}

	// Ensure non-checkpoint blocks do not contain signer list
	signersBytes := len(header.Extra) - extraVanity - extraSeal
	if !checkpoint && signersBytes != 0 {
		return errExtraSigners
	}
	if checkpoint && signersBytes%common.AddressLength != 0 {
		return errInvalidCheckpointSigners
	}

	// Check difficulty matches valid range
	if header.Difficulty == nil || (header.Difficulty.Cmp(diffInTurn) != 0 && header.Difficulty.Cmp(diffNoTurn) != 0) {
		return errInvalidDifficulty
	}

	// Fetch parent header
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else if number > 0 {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	// Verify timestamp progression
	if parent.Time+m.config.Period > header.Time {
		return errInvalidTimestamp
	}

	// Gas verification
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}
	if !chain.Config().IsLondon(header.Number) {
		if header.BaseFee != nil {
			return fmt.Errorf("invalid baseFee before fork: have %d, want <nil>", header.BaseFee)
		}
		if err := misc.VerifyGaslimit(parent.GasLimit, header.GasLimit); err != nil {
			return err
		}
	} else if err := eip1559.VerifyEIP1559Header(chain.Config(), parent, header); err != nil {
		return err
	}

	// Retrieve snapshot
	snap, err := m.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	// If checkpoint block, verify listed validators match snapshot
	if checkpoint {
		vals := snap.validatorsAscending()
		expected := make([]byte, len(vals)*common.AddressLength)
		for i, v := range vals {
			copy(expected[i*common.AddressLength:], v[:])
		}
		extraSuffix := len(header.Extra) - extraSeal
		if !bytes.Equal(header.Extra[extraVanity:extraSuffix], expected) {
			return errMismatchingCheckpointSigners
		}
	}

	// Verify seal and security constraints
	return m.verifySeal(snap, header)
}

// verifySeal checks that the signature on the header belongs to an authorized validator
// with adequate security score, in-turn / out-of-turn turn rules, and within recent limits.
func (m *MBFT) verifySeal(snap *Snapshot, header *types.Header) error {
	signer, err := ecrecover(header, m.signatures)
	if err != nil {
		return err
	}

	// 1. Validator authorization check
	score, ok := snap.Validators[signer]
	if !ok {
		return fmt.Errorf("%w: %x", errUnauthorizedValidator, signer)
	}

	// 2. Security score threshold check
	if score < snap.MinScore() {
		return fmt.Errorf("%w: validator %x has score %d, requires %d", errInsufficientSecurity, signer, score, snap.MinScore())
	}

	// 3. Sliding window recent signers check
	limit := snap.QuorumLimit()
	for seen, recent := range snap.Recents {
		if recent == signer {
			if header.Number.Uint64() < seen+limit {
				return fmt.Errorf("%w: validator %x signed block %d, limit %d", errRecentlySigned, signer, seen, limit)
			}
		}
	}

	// 4. Verify difficulty matches expected turn
	inturn := snap.inturn(header.Number.Uint64(), signer)
	if inturn && header.Difficulty.Cmp(diffInTurn) != 0 {
		return errWrongDifficulty
	}
	if !inturn && header.Difficulty.Cmp(diffNoTurn) != 0 {
		return errWrongDifficulty
	}

	return nil
}

// VerifyUncles verifies that no uncles exist (mBFT produces linear chains).
func (m *MBFT) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed in mBFT")
	}
	return nil
}

// Prepare initializes the consensus fields of a block header.
func (m *MBFT) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	number := header.Number.Uint64()
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	snap, err := m.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}

	m.lock.RLock()
	signer := m.signer
	// Check proposals if not checkpoint
	if number%m.config.Epoch != 0 {
		addresses := make([]common.Address, 0, len(m.proposals))
		for addr, auth := range m.proposals {
			if snap.validVote(addr, auth) {
				addresses = append(addresses, addr)
			}
		}
		if len(addresses) > 0 {
			header.Coinbase = addresses[rand.Intn(len(addresses))]
			if m.proposals[header.Coinbase] {
				copy(header.Nonce[:], nonceAuthVote)
			} else {
				copy(header.Nonce[:], nonceDropVote)
			}
		}
	}
	m.lock.RUnlock()

	// Set difficulty
	header.Difficulty = calcDifficulty(snap, signer)

	// Ensure extra data has vanity padding
	if len(header.Extra) < extraVanity {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-len(header.Extra))...)
	}
	header.Extra = header.Extra[:extraVanity]

	// Add validators list if checkpoint block
	if number%m.config.Epoch == 0 {
		for _, val := range snap.validatorsAscending() {
			header.Extra = append(header.Extra, val[:]...)
		}
	}
	// Append empty space for seal signature
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	// Ensure timestamp progression
	header.Time = parent.Time + m.config.Period
	if header.Time < uint64(time.Now().Unix()) {
		header.Time = uint64(time.Now().Unix())
	}
	return nil
}

// Finalize runs post-transaction consensus rules.
// In mBFT, this credits the proposing validator (Coinbase) with the security validation
// reward (distinct from a PoW mining fee) if configured in MBFTConfig.
func (m *MBFT) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state vm.StateDB, body *types.Body) {
	if m.config.BlockReward != nil && m.config.BlockReward.Sign() > 0 {
		reward, _ := uint256.FromBig(m.config.BlockReward)
		state.AddBalance(header.Coinbase, reward, tracing.BalanceIncreaseRewardMineBlock)
	}
}

// Authorize configures the local signer address and signing key callback.
func (m *MBFT) Authorize(signer common.Address, signFn SignerFn) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.signer = signer
	m.signFn = signFn
}

// Seal generates a sealed block using the local validator credentials.
func (m *MBFT) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	header := block.Header()
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	m.lock.RLock()
	signer, signFn := m.signer, m.signFn
	m.lock.RUnlock()

	snap, err := m.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}

	score, authorized := snap.Validators[signer]
	if !authorized {
		return errUnauthorizedValidator
	}
	if score < snap.MinScore() {
		return errInsufficientSecurity
	}

	// Sliding window check: if signer signed within recent limit, wait
	limit := snap.QuorumLimit()
	for seen, recent := range snap.Recents {
		if recent == signer {
			if number < seen+limit {
				log.Info("Signed recently, must wait for rotation", "number", number, "signedAt", seen, "limit", limit)
				return errWaitTurn
			}
		}
	}

	// Timing: calculate delay based on turn and mBFT view
	delay := time.Unix(int64(header.Time), 0).Sub(time.Now())
	if !snap.inturn(number, signer) {
		// Out-of-turn leader re-election delay
		view, ok := snap.ViewOfValidator(number, signer)
		if !ok {
			return errUnauthorizedValidator
		}
		// Each view waited provides time for preceding leaders before auto-reelection
		delay += time.Duration(view) * wiggleTime
	}

	go func() {
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}

		// Sign block header
		sealHash := SealHash(header)
		sig, err := signFn(signer, sealHash.Bytes())
		if err != nil {
			log.Error("Failed to sign mBFT block", "err", err)
			return
		}

		copy(header.Extra[len(header.Extra)-extraSeal:], sig)
		select {
		case results <- block.WithSeal(header):
		default:
			log.Warn("Sealing result dropped", "sealhash", sealHash)
		}
	}()

	return nil
}

// CalcDifficulty returns difficulty 2 for in-turn, 1 for out-of-turn.
func (m *MBFT) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	snap, err := m.snapshot(chain, parent.Number.Uint64(), parent.Hash(), nil)
	if err != nil {
		return nil
	}
	m.lock.RLock()
	signer := m.signer
	m.lock.RUnlock()

	return calcDifficulty(snap, signer)
}

func calcDifficulty(snap *Snapshot, signer common.Address) *big.Int {
	if snap.inturn(snap.Number+1, signer) {
		return new(big.Int).Set(diffInTurn)
	}
	return new(big.Int).Set(diffNoTurn)
}

// SealHash returns the hash of a block prior to it being sealed.
func (m *MBFT) SealHash(header *types.Header) common.Hash {
	return SealHash(header)
}

// Close terminates any background threads maintained by the engine.
func (m *MBFT) Close() error {
	return nil
}

// APIs returns the RPC APIs exposed by mBFT.
func (m *MBFT) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{
		{
			Namespace: "mbft",
			Service:   &API{chain: chain, mbft: m},
			Public:    true,
		},
	}
}

// Snapshot retrieves the validator authorization snapshot for a block.
func (m *MBFT) Snapshot(chain consensus.ChainHeaderReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	return m.snapshot(chain, number, hash, parents)
}

// snapshot retrieves the validator authorization snapshot for a block.
func (m *MBFT) snapshot(chain consensus.ChainHeaderReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	var (
		headers []*types.Header
		snap    *Snapshot
	)

	for snap == nil {
		if s, ok := m.recents.Get(hash); ok {
			snap = s
			break
		}
		if number%checkpointInterval == 0 {
			if s, err := loadSnapshot(m.config, m.signatures, m.db, hash); err == nil {
				snap = s
				break
			}
		}
		if number == 0 || (number%m.config.Epoch == 0 && (len(headers) > params.FullImmutabilityThreshold || chain.GetHeaderByNumber(number-1) == nil)) {
			checkpoint := chain.GetHeaderByNumber(number)
			if checkpoint != nil {
				hash := checkpoint.Hash()
				valCount := (len(checkpoint.Extra) - extraVanity - extraSeal) / common.AddressLength
				vals := make([]common.Address, valCount)
				for i := 0; i < valCount; i++ {
					copy(vals[i][:], checkpoint.Extra[extraVanity+i*common.AddressLength:])
				}
				snap = newSnapshot(m.config, m.signatures, number, hash, vals)
				if err := snap.store(m.db); err != nil {
					return nil, err
				}
				break
			}
		}
		var header *types.Header
		if len(parents) > 0 {
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				return nil, consensus.ErrUnknownAncestor
			}
			parents = parents[:len(parents)-1]
		} else {
			header = chain.GetHeader(hash, number)
			if header == nil {
				return nil, consensus.ErrUnknownAncestor
			}
		}
		headers = append(headers, header)
		number, hash = number-1, header.ParentHash
	}

	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}

	snap, err := snap.apply(headers)
	if err != nil {
		return nil, err
	}
	m.recents.Add(snap.Hash, snap)

	if snap.Number%checkpointInterval == 0 && len(headers) > 0 {
		if err := snap.store(m.db); err != nil {
			return nil, err
		}
	}
	return snap, nil
}

// SealHash calculates the hash of a header excluding the 65-byte signature seal suffix.
func SealHash(header *types.Header) (hash common.Hash) {
	hasher := keccak.NewLegacyKeccak256()
	encodeSigHeader(hasher, header)
	hasher.(crypto.KeccakState).Read(hash[:])
	return hash
}

func encodeSigHeader(w io.Writer, header *types.Header) {
	enc := []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:len(header.Extra)-crypto.SignatureLength],
		header.MixDigest,
		header.Nonce,
	}
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	if header.WithdrawalsHash != nil {
		enc = append(enc, header.WithdrawalsHash)
	}
	if header.BlobGasUsed != nil {
		enc = append(enc, header.BlobGasUsed)
	}
	if header.ExcessBlobGas != nil {
		enc = append(enc, header.ExcessBlobGas)
	}
	if header.ParentBeaconRoot != nil {
		enc = append(enc, header.ParentBeaconRoot)
	}
	if header.RequestsHash != nil {
		enc = append(enc, header.RequestsHash)
	}
	if err := rlp.Encode(w, enc); err != nil {
		panic("can't encode header for mbft: " + err.Error())
	}
}

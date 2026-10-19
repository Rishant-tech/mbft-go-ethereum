package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rollup"
	rollupcontract "github.com/ethereum/go-ethereum/rollup/contracts"
)

func main() {
	var (
		rollupIDStr = flag.String("rollup.id", "1", "Unique numeric identifier for the L2 rollup")
		l1RpcURL    = flag.String("l1.rpc", "http://localhost:8545", "HTTP RPC URL of mBFT Layer 1")
		l1Contract  = flag.String("l1.contract", "", "Address of RollupManager contract on L1")
		l2RpcURL    = flag.String("l2.rpc", "http://localhost:9545", "HTTP RPC URL of L2 Sequencer")
		keyHex      = flag.String("key", "", "Hex-encoded sequencer/batcher private key")
		pollSec     = flag.Int("poll", 2, "Poll interval in seconds")
	)
	flag.Parse()

	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))

	if *l1Contract == "" {
		log.Crit("Missing required flag --l1.contract")
	}
	if *keyHex == "" {
		log.Crit("Missing required flag --key")
	}

	privKey, err := crypto.HexToECDSA(*keyHex)
	if err != nil {
		log.Crit("Invalid private key", "err", err)
	}

	rollupID, _ := new(big.Int).SetString(*rollupIDStr, 10)
	contractAddr := common.HexToAddress(*l1Contract)

	l1Client, err := ethclient.Dial(*l1RpcURL)
	if err != nil {
		log.Crit("Failed to connect to L1 RPC", "url", *l1RpcURL, "err", err)
	}

	contract, err := rollupcontract.NewRollupManager(contractAddr, l1Client)
	if err != nil {
		log.Crit("Failed to bind RollupManager", "err", err)
	}

	log.Info("=== L2 Rollup Batcher Daemon Running ===",
		"rollupId", rollupID,
		"l1Rpc", *l1RpcURL,
		"l2Rpc", *l2RpcURL,
		"contract", contractAddr.Hex(),
		"pollInterval", fmt.Sprintf("%ds", *pollSec),
	)

	ticker := time.NewTicker(time.Duration(*pollSec) * time.Second)
	defer ticker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-sigCh:
			log.Info("Shutting down L2 Batcher...")
			return

		case <-ticker.C:
			processBatches(rollupID, *l2RpcURL, l1Client, contract, privKey)
		}
	}
}

func processBatches(
	rollupID *big.Int,
	l2RpcURL string,
	l1Client *ethclient.Client,
	contract *rollupcontract.RollupManager,
	privKey *ecdsa.PrivateKey,
) {
	// 1. Fetch pending batches from L2 Sequencer
	batches, err := fetchPendingBatches(l2RpcURL)
	if err != nil || len(batches) == 0 {
		return
	}

	ctx := context.Background()
	chainID, err := l1Client.ChainID(ctx)
	if err != nil {
		log.Error("Failed to fetch L1 chain ID", "err", err)
		return
	}

	auth, err := bind.NewKeyedTransactorWithChainID(privKey, chainID)
	if err != nil {
		log.Error("Failed to create L1 transactor", "err", err)
		return
	}

	for _, b := range batches {
		log.Info("Submitting L2 batch to L1 RollupManager",
			"rollupId", rollupID,
			"batchIndex", b.BatchIndex,
			"start", b.L2BlockStart,
			"end", b.L2BlockEnd,
			"bytes", len(b.CompressedData),
		)

		tx, err := contract.SubmitBatch(
			auth,
			rollupID,
			b.PrevStateRoot,
			b.NewStateRoot,
			new(big.Int).SetUint64(b.L2BlockStart),
			new(big.Int).SetUint64(b.L2BlockEnd),
			b.CompressedData,
		)
		if err != nil {
			log.Error("SubmitBatch failed", "err", err)
			continue
		}

		log.Info("Submitted batch successfully to L1", "txHash", tx.Hash().Hex())
		markSubmitted(l2RpcURL, b.BatchIndex, tx.Hash())
	}
}

func fetchPendingBatches(l2RpcURL string) ([]*rollup.Batch, error) {
	reqBody := `{"jsonrpc":"2.0","method":"rollup_getPendingBatches","params":[],"id":1}`
	resp, err := http.Post(l2RpcURL, "application/json", bytes.NewBufferString(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var r struct {
		Result []*rollup.Batch `json:"result"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return r.Result, nil
}

func markSubmitted(l2RpcURL string, batchIndex uint64, l1TxHash common.Hash) {
	params, _ := json.Marshal(map[string]interface{}{
		"batchIndex": batchIndex,
		"l1TxHash":   l1TxHash,
	})
	reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","method":"rollup_markBatchSubmitted","params":%s,"id":1}`, string(params))
	_, _ = http.Post(l2RpcURL, "application/json", bytes.NewBufferString(reqBody))
}

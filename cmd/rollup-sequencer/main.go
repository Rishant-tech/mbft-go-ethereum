package main

import (
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rollup"
	"github.com/ethereum/go-ethereum/rollup/sequencer"
)

func main() {
	var (
		rollupIDStr   = flag.String("rollup.id", "1", "Unique numeric identifier for this L2 rollup")
		name          = flag.String("name", "Rollup-1", "Human readable rollup name")
		chainIDStr    = flag.String("chain.id", "4201", "EVM Chain ID for this L2 rollup")
		blockTimeMs   = flag.Int("blocktime", 100, "Micro-block interval in milliseconds")
		maxBatchSize  = flag.Int("batch.maxsize", 100, "Maximum number of transactions per batch")
		batchTimeoutS = flag.Int("batch.timeout", 2, "Batch cutoff timeout in seconds")
		rpcHost       = flag.String("rpc.host", "0.0.0.0", "HTTP JSON-RPC listening interface")
		rpcPort       = flag.Int("rpc.port", 9545, "HTTP JSON-RPC listening port")
		keyHex        = flag.String("key", "", "Hex-encoded sequencer private key (32 bytes)")
	)
	flag.Parse()

	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))

	var privKey *ecdsa.PrivateKey
	var err error
	if *keyHex != "" {
		privKey, err = crypto.HexToECDSA(*keyHex)
		if err != nil {
			log.Crit("Invalid sequencer private key", "err", err)
		}
	} else {
		// Generate deterministic key if not provided (for local testing)
		privKey, _ = crypto.GenerateKey()
	}

	rollupID, _ := new(big.Int).SetString(*rollupIDStr, 10)
	chainID, _ := new(big.Int).SetString(*chainIDStr, 10)

	cfg := &rollup.RollupConfig{
		RollupID:      rollupID,
		Name:          *name,
		ChainID:       chainID,
		BlockTime:     time.Duration(*blockTimeMs) * time.Millisecond,
		MaxBatchSize:  *maxBatchSize,
		BatchTimeout:  time.Duration(*batchTimeoutS) * time.Second,
		SequencerAddr: crypto.PubkeyToAddress(privKey.PublicKey),
		RPCHost:       *rpcHost,
		RPCPort:       *rpcPort,
	}

	seq := sequencer.New(cfg, privKey)
	seq.Start()
	defer seq.Stop()

	server := sequencer.NewHTTPServer(seq, *rpcHost, *rpcPort)
	if err := server.Start(); err != nil {
		log.Crit("Failed to start HTTP RPC server", "err", err)
	}
	defer server.Stop()

	log.Info("=== L2 Micro-Block Sequencer Running ===",
		"rollupId", cfg.RollupID,
		"name", cfg.Name,
		"chainId", cfg.ChainID,
		"blockTime", cfg.BlockTime,
		"sequencerAddr", cfg.SequencerAddr.Hex(),
		"rpcEndpoint", fmt.Sprintf("http://%s:%d", *rpcHost, *rpcPort),
	)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info("Shutting down L2 Sequencer...")
}

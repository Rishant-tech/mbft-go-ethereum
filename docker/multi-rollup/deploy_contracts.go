package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	rollupcontract "github.com/ethereum/go-ethereum/rollup/contracts"
)

func main() {
	var (
		rpcURL = flag.String("rpc", "http://localhost:8545", "L1 RPC URL")
		keyHex = flag.String("key", "0000000000000000000000000000000000000000000000000000000000000001", "Deployer private key (pre-funded)")
	)
	flag.Parse()

	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))

	privKey, err := crypto.HexToECDSA(*keyHex)
	if err != nil {
		log.Crit("Invalid deployer private key", "err", err)
	}

	client, err := ethclient.Dial(*rpcURL)
	if err != nil {
		log.Crit("Failed to connect to L1 RPC", "err", err)
	}

	ctx := context.Background()
	chainID, err := client.ChainID(ctx)
	if err != nil {
		log.Crit("Failed to fetch L1 chain ID", "err", err)
	}

	auth, err := bind.NewKeyedTransactorWithChainID(privKey, chainID)
	if err != nil {
		log.Crit("Failed to create transactor", "err", err)
	}

	log.Info("Deploying RollupManager contract to mBFT Layer 1...", "deployer", auth.From.Hex())

	addr, tx, contract, err := rollupcontract.DeployRollupManager(auth, client)
	if err != nil {
		log.Crit("Failed to deploy RollupManager", "err", err)
	}

	log.Info("RollupManager deployed successfully!", "address", addr.Hex(), "txHash", tx.Hash().Hex())

	// Wait for transaction confirmation
	receipt, err := bind.WaitMined(ctx, client, tx)
	if err != nil {
		log.Crit("Failed waiting for deployment confirmation", "err", err)
	}
	log.Info("Deployment confirmed in block", "blockNumber", receipt.BlockNumber)

	// Register Rollup 1 (DeFi)
	seqKey1Hex := "0000000000000000000000000000000000000000000000000000000000000002"
	seqKey1, _ := crypto.HexToECDSA(seqKey1Hex)
	seqAddr1 := crypto.PubkeyToAddress(seqKey1.PublicKey)
	genesis1 := crypto.Keccak256Hash([]byte("genesis-rollup-1"))

	auth.Nonce = nil
	tx1, err := contract.RegisterRollup(auth, big.NewInt(1), "Rollup-DeFi", seqAddr1, genesis1)
	if err != nil {
		log.Crit("Failed to register Rollup 1", "err", err)
	}
	log.Info("Registered Rollup 1 (Rollup-DeFi)", "id", 1, "sequencer", seqAddr1.Hex(), "tx", tx1.Hash().Hex())

	// Register Rollup 2 (Gaming)
	seqKey2Hex := "0000000000000000000000000000000000000000000000000000000000000003"
	seqKey2, _ := crypto.HexToECDSA(seqKey2Hex)
	seqAddr2 := crypto.PubkeyToAddress(seqKey2.PublicKey)
	genesis2 := crypto.Keccak256Hash([]byte("genesis-rollup-2"))

	auth.Nonce = nil
	tx2, err := contract.RegisterRollup(auth, big.NewInt(2), "Rollup-Gaming", seqAddr2, genesis2)
	if err != nil {
		log.Crit("Failed to register Rollup 2", "err", err)
	}
	log.Info("Registered Rollup 2 (Rollup-Gaming)", "id", 2, "sequencer", seqAddr2.Hex(), "tx", tx2.Hash().Hex())

	// Write contract address to shared configuration file
	_ = os.WriteFile("docker/multi-rollup/rollup_manager_address.txt", []byte(addr.Hex()), 0644)
	fmt.Printf("\nSUCCESS: RollupManager Address = %s\n", addr.Hex())
}

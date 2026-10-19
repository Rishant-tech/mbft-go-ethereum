package sequencer

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/log"
)

// JSONRPCRequest represents a standard incoming JSON-RPC 2.0 payload.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      interface{}     `json:"id"`
}

// JSONRPCResponse represents a standard JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
	ID      interface{} `json:"id"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// HTTPServer exposes the L2 Sequencer via HTTP JSON-RPC.
type HTTPServer struct {
	sequencer *Sequencer
	server    *http.Server
}

// NewHTTPServer initializes an HTTP server for the L2 Sequencer.
func NewHTTPServer(seq *Sequencer, host string, port int) *HTTPServer {
	addr := fmt.Sprintf("%s:%d", host, port)
	mux := http.NewServeMux()

	s := &HTTPServer{
		sequencer: seq,
		server: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}

	mux.HandleFunc("/", s.handleRPC)
	return s
}

func (s *HTTPServer) Start() error {
	log.Info("Starting L2 Sequencer RPC server", "addr", s.server.Addr)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("L2 Sequencer RPC server error", "err", err)
		}
	}()
	return nil
}

func (s *HTTPServer) Stop() error {
	return s.server.Close()
}

func (s *HTTPServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, nil, -32700, "Parse error")
		return
	}

	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, nil, -32700, "Parse error")
		return
	}

	result, rpcErr := s.executeMethod(req.Method, req.Params)
	if rpcErr != nil {
		s.writeError(w, req.ID, rpcErr.Code, rpcErr.Message)
		return
	}

	res := JSONRPCResponse{
		JSONRPC: "2.0",
		Result:  result,
		ID:      req.ID,
	}
	_ = json.NewEncoder(w).Encode(res)
}

func (s *HTTPServer) executeMethod(method string, params json.RawMessage) (interface{}, *RPCError) {
	switch method {
	case "eth_chainId":
		return hexutil.EncodeBig(s.sequencer.config.ChainID), nil

	case "eth_blockNumber":
		s.sequencer.mu.RLock()
		bn := s.sequencer.blockNumber
		s.sequencer.mu.RUnlock()
		return hexutil.EncodeUint64(bn), nil

	case "eth_getBalance":
		var args []string
		if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
			return nil, &RPCError{Code: -32602, Message: "Invalid params"}
		}
		addr := common.HexToAddress(args[0])
		bal := s.sequencer.GetBalance(addr)
		return hexutil.EncodeBig(bal), nil

	case "eth_getTransactionCount":
		var args []string
		if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
			return nil, &RPCError{Code: -32602, Message: "Invalid params"}
		}
		addr := common.HexToAddress(args[0])
		nonce := s.sequencer.GetNonce(addr)
		return hexutil.EncodeUint64(nonce), nil

	case "eth_sendRawTransaction":
		var args []string
		if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
			return nil, &RPCError{Code: -32602, Message: "Invalid params"}
		}
		rawHex := strings.TrimPrefix(args[0], "0x")
		rawBytes, err := hex.DecodeString(rawHex)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: "Invalid hex string"}
		}
		txHash, err := s.sequencer.SubmitRawTransaction(rawBytes)
		if err != nil {
			return nil, &RPCError{Code: -32000, Message: err.Error()}
		}
		return txHash.Hex(), nil

	case "eth_getBlockByNumber":
		var args []interface{}
		if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
			return nil, &RPCError{Code: -32602, Message: "Invalid params"}
		}
		var num uint64
		if str, ok := args[0].(string); ok {
			if str == "latest" {
				s.sequencer.mu.RLock()
				num = s.sequencer.blockNumber
				s.sequencer.mu.RUnlock()
			} else {
				parsed, _ := hexutil.DecodeUint64(str)
				num = parsed
			}
		}
		b := s.sequencer.GetBlock(num)
		if b == nil {
			return nil, nil
		}
		return b, nil

	case "rollup_getStatus":
		return s.sequencer.Status(), nil

	case "rollup_getPendingBatches":
		return s.sequencer.GetPendingBatches(), nil

	case "rollup_markBatchSubmitted":
		var p struct {
			BatchIndex uint64      `json:"batchIndex"`
			L1TxHash   common.Hash `json:"l1TxHash"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: -32602, Message: "Invalid params"}
		}
		s.sequencer.MarkBatchSubmitted(p.BatchIndex, p.L1TxHash)
		return true, nil

	case "rollup_deposit":
		var p struct {
			Recipient common.Address `json:"recipient"`
			Amount    *hexutil.Big   `json:"amount"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: -32602, Message: "Invalid params"}
		}
		s.sequencer.CreditDeposit(p.Recipient, (*big.Int)(p.Amount))
		return true, nil

	case "rollup_withdraw":
		var p struct {
			Sender common.Address `json:"sender"`
			Amount *hexutil.Big   `json:"amount"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: -32602, Message: "Invalid params"}
		}
		rec, err := s.sequencer.InitiateWithdrawal(p.Sender, (*big.Int)(p.Amount))
		if err != nil {
			return nil, &RPCError{Code: -32000, Message: err.Error()}
		}
		return rec, nil

	default:
		return nil, &RPCError{Code: -32601, Message: fmt.Sprintf("Method %s not found", method)}
	}
}

func (s *HTTPServer) writeError(w http.ResponseWriter, id interface{}, code int, msg string) {
	res := JSONRPCResponse{
		JSONRPC: "2.0",
		Error:   &RPCError{Code: code, Message: msg},
		ID:      id,
	}
	_ = json.NewEncoder(w).Encode(res)
}

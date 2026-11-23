// Package replay defines the boundary between the Ethereum RPC and the chain
// application which owns the full Cosmos state machine.
package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/bytedance/sonic"
	"github.com/cosmos/gogoproto/proto"

	abci "github.com/cometbft/cometbft/abci/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/ethereum/go-ethereum/common"
)

type Request struct {
	ChainID    string
	ParentHash []byte
	Request    *abci.RequestFinalizeBlock
}

type Transaction struct {
	CosmosIndex  int
	MessageIndex int
	Hash         common.Hash
	GasUsed      uint64
	Failed       bool
	Error        string
	Trace        json.RawMessage
}

type Result struct {
	Finalize     *abci.ResponseFinalizeBlock
	Transactions []Transaction
}

// The protobuf payload preserves the public-key oneof in validator updates.
type workerResult struct {
	Finalize     []byte
	Transactions []Transaction
	Error        string `json:",omitempty"`
}

func WriteResult(w io.Writer, r Result) error {
	b, err := r.Finalize.Marshal()
	if err != nil {
		return err
	}
	return sonic.ConfigStd.NewEncoder(w).Encode(workerResult{Finalize: b, Transactions: r.Transactions})
}

// WriteError reports a failed replay in place of a result.
func WriteError(w io.Writer, err error) error {
	return sonic.ConfigStd.NewEncoder(w).Encode(workerResult{Error: err.Error()})
}

func ReadResult(r io.Reader) (*Result, error) {
	var wire workerResult
	if err := sonic.ConfigStd.NewDecoder(r).Decode(&wire); err != nil {
		return nil, err
	}
	if wire.Error != "" {
		return nil, errors.New(wire.Error)
	}
	res := new(abci.ResponseFinalizeBlock)
	if err := res.Unmarshal(wire.Finalize); err != nil {
		return nil, err
	}
	return &Result{res, wire.Transactions}, nil
}

type Executor interface {
	CheckReplayHeight(string, int64) error
	ReplayBlock(context.Context, Request) (*Result, error)
	// BlockFileOnly reports whether trace_debankBlock returns only the block file.
	BlockFileOnly() (bool, error)
}

// Verify compares deterministic ABCI fields in execution order. Index is a
// local indexing preference and Log/Info are diagnostic strings.
func Verify(want *ctypes.ResultBlockResults, got *abci.ResponseFinalizeBlock) error {
	if want == nil || got == nil {
		return fmt.Errorf("missing finalize results")
	}
	if len(want.TxsResults) != len(got.TxResults) {
		return fmt.Errorf("tx count: got %d, want %d", len(got.TxResults), len(want.TxsResults))
	}
	for i, w := range want.TxsResults {
		g := got.TxResults[i]
		if w == nil || g == nil {
			return fmt.Errorf("tx[%d]: missing result", i)
		}
		switch {
		case w.Code != g.Code || w.Codespace != g.Codespace:
			return fmt.Errorf("tx[%d] outcome: got %s/%d, want %s/%d", i, g.Codespace, g.Code, w.Codespace, w.Code)
		case w.GasWanted != g.GasWanted || w.GasUsed != g.GasUsed:
			return fmt.Errorf("tx[%d] gas wanted/used: got %d/%d, want %d/%d", i, g.GasWanted, g.GasUsed, w.GasWanted, w.GasUsed)
		case !bytes.Equal(w.Data, g.Data):
			return fmt.Errorf("tx[%d] result data mismatch", i)
		}
		if err := compareEvents(w.Events, g.Events); err != nil {
			return fmt.Errorf("tx[%d]: %w", i, err)
		}
	}
	if err := compareEvents(want.FinalizeBlockEvents, got.Events); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	if !slices.EqualFunc(want.ValidatorUpdates, got.ValidatorUpdates, func(a, b abci.ValidatorUpdate) bool { return proto.Equal(&a, &b) }) {
		return fmt.Errorf("validator updates mismatch")
	}
	if !proto.Equal(want.ConsensusParamUpdates, got.ConsensusParamUpdates) {
		return fmt.Errorf("consensus parameter updates mismatch")
	}
	if len(want.AppHash) != 32 || !bytes.Equal(want.AppHash, got.AppHash) {
		return fmt.Errorf("AppHash: got %X, want %X", got.AppHash, want.AppHash)
	}
	return nil
}

func compareEvents(want, got []abci.Event) error {
	if len(want) != len(got) {
		return fmt.Errorf("event count: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if w.Type != g.Type || !slices.EqualFunc(w.Attributes, g.Attributes, func(a, b abci.EventAttribute) bool { return a.Key == b.Key && a.Value == b.Value }) {
			return fmt.Errorf("event[%d] (%s) mismatch", i, w.Type)
		}
	}
	return nil
}

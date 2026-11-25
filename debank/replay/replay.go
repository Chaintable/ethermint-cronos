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
	"strings"

	"github.com/bytedance/sonic"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/gogo/protobuf/proto"
	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"

	dtypes "github.com/tharsis/ethermint/debank/types"
	evmtypes "github.com/tharsis/ethermint/x/evm/types"
)

// Request replays block BeginBlock.Header.Height on the state committed by
// its parent, whose application hash is ParentHash.
type Request struct {
	ChainID    string
	ParentHash []byte
	BeginBlock *abci.RequestBeginBlock
	Txs        [][]byte
}

type Transaction struct {
	CosmosIndex  int
	MessageIndex int
	Hash         common.Hash
	GasUsed      uint64
	Failed       bool
	Error        string
	Trace        json.RawMessage
	// HookLogs are the EVM logs kept in the receipt of a message that Cosmos
	// post-processing reverted.
	HookLogs []dtypes.Event `json:",omitempty"`
}

type Result struct {
	Responses    *tmstate.ABCIResponses
	AppHash      []byte
	Transactions []Transaction
}

// Protobuf payloads preserve oneofs such as validator public keys.
type wireRequest struct {
	ChainID    string
	ParentHash []byte
	BeginBlock []byte
	Txs        [][]byte
}

type wireResult struct {
	Responses    []byte
	AppHash      []byte
	Transactions []Transaction
	Error        string `json:",omitempty"`
}

func WriteRequest(w io.Writer, r Request) error {
	if r.BeginBlock == nil {
		return fmt.Errorf("missing begin block request")
	}
	b, err := r.BeginBlock.Marshal()
	if err != nil {
		return err
	}
	return sonic.ConfigStd.NewEncoder(w).Encode(wireRequest{r.ChainID, r.ParentHash, b, r.Txs})
}

func ReadRequest(r io.Reader) (Request, error) {
	var wire wireRequest
	if err := sonic.ConfigStd.NewDecoder(r).Decode(&wire); err != nil {
		return Request{}, err
	}
	begin := new(abci.RequestBeginBlock)
	if err := begin.Unmarshal(wire.BeginBlock); err != nil {
		return Request{}, err
	}
	return Request{wire.ChainID, wire.ParentHash, begin, wire.Txs}, nil
}

func WriteResult(w io.Writer, r Result) error {
	b, err := r.Responses.Marshal()
	if err != nil {
		return err
	}
	return sonic.ConfigStd.NewEncoder(w).Encode(wireResult{Responses: b, AppHash: r.AppHash, Transactions: r.Transactions})
}

// WriteError reports a failed replay in place of a result.
func WriteError(w io.Writer, err error) error {
	return sonic.ConfigStd.NewEncoder(w).Encode(wireResult{Error: err.Error()})
}

func ReadResult(r io.Reader) (*Result, error) {
	var wire wireResult
	if err := sonic.ConfigStd.NewDecoder(r).Decode(&wire); err != nil {
		return nil, err
	}
	if wire.Error != "" {
		return nil, errors.New(wire.Error)
	}
	res := new(tmstate.ABCIResponses)
	if err := res.Unmarshal(wire.Responses); err != nil {
		return nil, err
	}
	return &Result{res, wire.AppHash, wire.Transactions}, nil
}

type Executor interface {
	CheckReplayHeight(string, int64) error
	ReplayBlock(context.Context, Request) (*Result, error)
	// BlockFileOnly reports whether trace_debankBlock returns only the block file.
	BlockFileOnly() (bool, error)
}

// ExceedsBlockGas reports a transaction that executed but exceeded the block
// gas limit afterwards. The SDK of this version charges block gas before
// writing the messages, so their state changes are dropped while the ante
// handler's fee and nonce stay.
func ExceedsBlockGas(r *abci.ResponseDeliverTx) bool {
	return r.Code == 11 && strings.Contains(r.Log, "out of gas in location: block gas meter; gasWanted:")
}

// BlockGasExceeded is the root frame error of a message whose Cosmos
// transaction exceeded the block gas limit.
const BlockGasExceeded = "exceeds block gas limit"

// Verify compares deterministic ABCI fields in execution order. Event
// attribute Index is a node indexing preference and Log/Info are diagnostic,
// except the block gas failure, which decides whether a transaction is listed.
func Verify(want, got *tmstate.ABCIResponses) error {
	return VerifyBlock(want, got, nil, nil)
}

// VerifyBlock also checks historical ante hash differences against the original transaction.
func VerifyBlock(want, got *tmstate.ABCIResponses, txs [][]byte, decode sdk.TxDecoder) error {
	if want == nil || got == nil || want.BeginBlock == nil || want.EndBlock == nil || got.BeginBlock == nil || got.EndBlock == nil {
		return fmt.Errorf("missing block results")
	}
	if err := compareEvents(want.BeginBlock.Events, got.BeginBlock.Events, nil); err != nil {
		return fmt.Errorf("begin block: %w", err)
	}
	if len(want.DeliverTxs) != len(got.DeliverTxs) {
		return fmt.Errorf("tx count: got %d, want %d", len(got.DeliverTxs), len(want.DeliverTxs))
	}
	for i, w := range want.DeliverTxs {
		g := got.DeliverTxs[i]
		if w == nil || g == nil {
			return fmt.Errorf("tx[%d]: missing result", i)
		}
		switch {
		case w.Code != g.Code || w.Codespace != g.Codespace || ExceedsBlockGas(w) != ExceedsBlockGas(g):
			return fmt.Errorf("tx[%d] outcome: got %s/%d, want %s/%d", i, g.Codespace, g.Code, w.Codespace, w.Code)
		case w.GasWanted != g.GasWanted || w.GasUsed != g.GasUsed:
			return fmt.Errorf("tx[%d] gas wanted/used: got %d/%d, want %d/%d", i, g.GasWanted, g.GasUsed, w.GasWanted, w.GasUsed)
		case !bytes.Equal(w.Data, g.Data):
			return fmt.Errorf("tx[%d] result data mismatch", i)
		}
		if err := compareEvents(w.Events, g.Events, func(event int) bool {
			if decode == nil || i >= len(txs) {
				return false
			}
			tx, err := decode(txs[i])
			return err == nil && historicalAnteHash(tx, g.Events, event, w.Events[event].Attributes[0].Value)
		}); err != nil {
			return fmt.Errorf("tx[%d]: %w", i, err)
		}
	}
	if err := compareEvents(want.EndBlock.Events, got.EndBlock.Events, nil); err != nil {
		return fmt.Errorf("end block: %w", err)
	}
	if len(want.EndBlock.ValidatorUpdates) != len(got.EndBlock.ValidatorUpdates) {
		return fmt.Errorf("validator updates mismatch")
	}
	for i := range want.EndBlock.ValidatorUpdates {
		if !proto.Equal(&want.EndBlock.ValidatorUpdates[i], &got.EndBlock.ValidatorUpdates[i]) {
			return fmt.Errorf("validator updates mismatch")
		}
	}
	if !proto.Equal(want.EndBlock.ConsensusParamUpdates, got.EndBlock.ConsensusParamUpdates) {
		return fmt.Errorf("consensus parameter updates mismatch")
	}
	return nil
}

func compareEvents(want, got []abci.Event, allowAnteHash func(int) bool) error {
	if len(want) != len(got) {
		return fmt.Errorf("event count: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if w.Type != g.Type || len(w.Attributes) != len(g.Attributes) {
			return fmt.Errorf("event[%d] (%s) mismatch", i, w.Type)
		}
		for j, a := range w.Attributes {
			b := g.Attributes[j]
			if bytes.Equal(a.Key, b.Key) && bytes.Equal(a.Value, b.Value) {
				continue
			}
			if j == 0 && w.Type == evmtypes.EventTypeEthereumTx && len(w.Attributes) == 2 &&
				string(a.Key) == evmtypes.AttributeKeyEthereumTxHash && bytes.Equal(a.Key, b.Key) &&
				string(g.Attributes[1].Key) == evmtypes.AttributeKeyTxIndex && allowAnteHash != nil && allowAnteHash(i) {
				continue
			}
			return fmt.Errorf("event[%d] (%s) mismatch", i, w.Type)
		}
	}
	return nil
}

// The ante decorator emits one contiguous ethereum_tx event per top-level
// Ethereum message, before the message handlers emit their execution events.
func historicalAnteHash(tx sdk.Tx, events []abci.Event, index int, archived []byte) bool {
	first := 0
	for first < index && events[first].Type != evmtypes.EventTypeEthereumTx {
		first++
	}
	msgs := tx.GetMsgs()
	offset := index - first
	if offset >= len(msgs) || first+len(msgs) > len(events) {
		return false
	}
	for j, msg := range msgs {
		e := events[first+j]
		if _, ok := msg.(*evmtypes.MsgEthereumTx); !ok || e.Type != evmtypes.EventTypeEthereumTx ||
			len(e.Attributes) != 2 || string(e.Attributes[0].Key) != evmtypes.AttributeKeyEthereumTxHash ||
			string(e.Attributes[1].Key) != evmtypes.AttributeKeyTxIndex {
			return false
		}
	}
	msg := msgs[offset].(*evmtypes.MsgEthereumTx)
	signed := msg.AsTransaction()
	return signed != nil && msg.Hash == string(archived) && signed.Hash().Hex() == string(events[index].Attributes[0].Value)
}

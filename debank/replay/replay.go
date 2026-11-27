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
	"github.com/ethereum/go-ethereum/common"
	"github.com/gogo/protobuf/proto"
	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
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
	if want == nil || got == nil || want.BeginBlock == nil || want.EndBlock == nil || got.BeginBlock == nil || got.EndBlock == nil {
		return fmt.Errorf("missing block results")
	}
	// Every block mints, so an archived BeginBlock without events was lost by
	// the node that saved it; the next AppHash still attests the replay.
	if len(want.BeginBlock.Events) > 0 {
		if err := compareEvents(want.BeginBlock.Events, got.BeginBlock.Events); err != nil {
			return fmt.Errorf("begin block: %w", err)
		}
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
		if err := compareEvents(w.Events, g.Events); err != nil {
			events := withoutFailedAckEvents(withoutRepeatedConversions(w.Events))
			if compareEvents(events, withoutAddedDelegators(events, g.Events)) != nil {
				return fmt.Errorf("tx[%d]: %w", i, err)
			}
		}
	}
	if err := compareEvents(want.EndBlock.Events, got.EndBlock.Events); err != nil {
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

// withoutRepeatedConversions drops the second copy of the IBC voucher
// conversion events that some mainnet builds archived twice (cronos #1102):
// the run after fungible_token_packet (receipt, acknowledgement) or timeout
// (timeout refund), up to write_acknowledgement, timeout_packet or the next
// message, when it consists of the same events repeated.
func withoutRepeatedConversions(events []abci.Event) []abci.Event {
	out := make([]abci.Event, 0, len(events))
	for i := 0; i < len(events); i++ {
		out = append(out, events[i])
		if events[i].Type != "fungible_token_packet" && events[i].Type != "timeout" {
			continue
		}
		end := i + 1
		for end < len(events) && events[end].Type != "write_acknowledgement" && events[end].Type != "timeout_packet" && !(events[end].Type == "message" && hasAttribute(events[end], "action")) {
			end++
		}
		run := events[i+1 : end]
		if half := len(run) / 2; half > 0 && len(run)%2 == 0 && compareEvents(run[:half], run[half:]) == nil {
			out = append(out, run[:half]...)
			i = end - 1
		}
	}
	return out
}

// withoutFailedAckEvents drops the events of a packet callback that returned
// an error acknowledgement, which ibc-go v5.2.0 archived and v5.2.1 discards.
func withoutFailedAckEvents(events []abci.Event) []abci.Event {
	out := make([]abci.Event, 0, len(events))
	for i := 0; i < len(events); i++ {
		out = append(out, events[i])
		if events[i].Type != "recv_packet" || i+1 >= len(events) || events[i+1].Type != "message" {
			continue
		}
		end := i + 2
		for end < len(events) && events[end].Type != "write_acknowledgement" {
			end++
		}
		if end < len(events) && end > i+2 && bytes.HasPrefix(attribute(events[end], "packet_ack"), []byte(`{"error":`)) {
			out = append(out, events[i+1])
			i = end - 1
		}
	}
	return out
}

// withoutAddedDelegators drops the delegator attribute that the v1.0.12+ SDK
// fork added to withdraw_rewards, where the archived event predates it.
func withoutAddedDelegators(want, got []abci.Event) []abci.Event {
	if len(want) != len(got) {
		return got
	}
	out := make([]abci.Event, len(got))
	for i, g := range got {
		out[i] = g
		if g.Type != "withdraw_rewards" || want[i].Type != g.Type || hasAttribute(want[i], "delegator") {
			continue
		}
		attrs := make([]abci.EventAttribute, 0, len(g.Attributes))
		for _, a := range g.Attributes {
			if string(a.Key) != "delegator" {
				attrs = append(attrs, a)
			}
		}
		out[i].Attributes = attrs
	}
	return out
}

func hasAttribute(e abci.Event, key string) bool {
	for _, a := range e.Attributes {
		if string(a.Key) == key {
			return true
		}
	}
	return false
}

func attribute(e abci.Event, key string) []byte {
	for _, a := range e.Attributes {
		if string(a.Key) == key {
			return a.Value
		}
	}
	return nil
}

func compareEvents(want, got []abci.Event) error {
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
			if !bytes.Equal(a.Key, b.Key) || !bytes.Equal(a.Value, b.Value) {
				return fmt.Errorf("event[%d] (%s) mismatch", i, w.Type)
			}
		}
	}
	return nil
}

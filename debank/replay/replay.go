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
	authante "github.com/cosmos/cosmos-sdk/x/auth/ante"
	"github.com/cosmos/cosmos-sdk/x/authz"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/gogo/protobuf/proto"
	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"

	dtypes "github.com/tharsis/ethermint/debank/types"
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
	return VerifyWithTxs(want, got, nil, nil)
}

// VerifyWithTxs checks known historical SDK event formats against their original messages.
func VerifyWithTxs(want, got *tmstate.ABCIResponses, txs [][]byte, decode sdk.TxDecoder) error {
	if want == nil || got == nil || want.BeginBlock == nil || want.EndBlock == nil || got.BeginBlock == nil || got.EndBlock == nil {
		return fmt.Errorf("missing block results")
	}
	if err := compareEvents(want.BeginBlock.Events, got.BeginBlock.Events); err != nil {
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
		events := g.Events
		legacyFeeVerified := false
		if decode != nil && i < len(txs) {
			events, legacyFeeVerified = legacyFeeEvents(w.Events, events, txs[i], decode)
		}
		if err := compareEvents(w.Events, events); err != nil {
			if w.Code != 0 || decode == nil || i >= len(txs) || !matchHistoricalMessages(w.Events, events, txs[i], decode, legacyFeeVerified) {
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

// DeductFeeDecorator emits the first tx event, before signature and message
// execution events. SDK v0.45.7 added fee_payer to the previous fee-only event.
func legacyFeeEvents(want, got []abci.Event, raw []byte, decode sdk.TxDecoder) ([]abci.Event, bool) {
	if len(want) != len(got) {
		return got, false
	}
	for i, ev := range got {
		if ev.Type != sdk.EventTypeTx {
			continue
		}
		w := want[i]
		if w.Type != sdk.EventTypeTx || len(w.Attributes) != 1 || len(ev.Attributes) != 2 ||
			string(w.Attributes[0].Key) != sdk.AttributeKeyFee || string(ev.Attributes[0].Key) != sdk.AttributeKeyFee ||
			string(ev.Attributes[1].Key) != sdk.AttributeKeyFeePayer {
			break
		}
		tx, err := decode(raw)
		if err != nil {
			break
		}
		// Transactions with extension options do not use the SDK fee decorator.
		if ext, ok := tx.(authante.HasExtensionOptionsTx); ok && len(ext.GetExtensionOptions()) != 0 {
			break
		}
		feeTx, ok := tx.(sdk.FeeTx)
		if !ok || feeTx.GetFee().String() != string(ev.Attributes[0].Value) {
			break
		}
		payer := feeTx.FeeGranter()
		if payer == nil {
			payer = feeTx.FeePayer()
		}
		if len(payer) == 0 || payer.String() != string(ev.Attributes[1].Value) {
			break
		}
		// This SDK predates both fee_payer and authz dispatch indices. Keep
		// the ante mismatch if the archived MsgExec already has such indices.
		for _, event := range want {
			if !hasAuthzIndex(event) {
				continue
			}
			for _, msg := range tx.GetMsgs() {
				if _, ok := msg.(*authz.MsgExec); ok {
					return got, false
				}
			}
			break
		}
		normalized := append([]abci.Event(nil), got...)
		normalized[i].Attributes = ev.Attributes[:1]
		return normalized, true
	}
	return got, false
}

// A successful explicit zero withdrawal emits no bank transfer. The old SDK
// rendered empty coins as "", while the newer SDK emits a zero coin.
func matchZeroReward(msg sdk.Msg, want, got []abci.Event) bool {
	withdrawal, ok := msg.(*distrtypes.MsgWithdrawDelegatorReward)
	if !ok {
		return false
	}
	events := func(amount string) []abci.Event {
		return sdk.Events{
			sdk.NewEvent(distrtypes.EventTypeWithdrawRewards,
				sdk.NewAttribute(sdk.AttributeKeyAmount, amount),
				sdk.NewAttribute(distrtypes.AttributeKeyValidator, withdrawal.ValidatorAddress)),
			sdk.NewEvent(sdk.EventTypeMessage,
				sdk.NewAttribute(sdk.AttributeKeyModule, distrtypes.ModuleName),
				sdk.NewAttribute(sdk.AttributeKeySender, withdrawal.DelegatorAddress)),
		}.ToABCIEvents()
	}
	denom, _ := sdk.GetBaseDenom()
	if denom == "" {
		denom = sdk.DefaultBondDenom
	}
	return compareEvents(events(""), want) == nil &&
		compareEvents(events(sdk.NewCoin(denom, sdk.ZeroInt()).String()), got) == nil
}

// BaseApp prefixes each top-level message's handler events with its action.
func matchHistoricalMessages(want, got []abci.Event, raw []byte, decode sdk.TxDecoder, legacyFeeVerified bool) bool {
	if len(want) != len(got) {
		return false
	}
	var starts []int
	for i, e := range got {
		if e.Type == sdk.EventTypeMessage && len(e.Attributes) == 1 && string(e.Attributes[0].Key) == sdk.AttributeKeyAction {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 || compareEvents(want[:starts[0]], got[:starts[0]]) != nil {
		return false
	}
	tx, err := decode(raw)
	if err != nil || tx == nil {
		return false
	}
	msgs := tx.GetMsgs()
	if len(msgs) != len(starts) {
		return false
	}
	starts = append(starts, len(got))
	for i, msg := range msgs {
		start, end := starts[i], starts[i+1]
		if string(got[start].Attributes[0].Value) != sdk.MsgTypeURL(msg) ||
			compareEvents(want[start:start+1], got[start:start+1]) != nil {
			return false
		}
		w, g := want[start+1:end], got[start+1:end]
		if compareEvents(w, g) == nil || matchZeroReward(msg, w, g) {
			continue
		}
		exec, ok := msg.(*authz.MsgExec)
		if !ok || !matchHistoricalAuthz(exec, w, g, legacyFeeVerified) {
			return false
		}
	}
	return true
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

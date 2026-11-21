package replay

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tm "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	eth "github.com/ethereum/go-ethereum/core/types"
	wire "github.com/evmos/ethermint/debank/types"
	evm "github.com/evmos/ethermint/x/evm/types"
)

func TestBlockGasFailureWithoutMessageResponse(t *testing.T) {
	raw, err := os.ReadFile("testdata/block-gas-tx-13184000.json")
	if err != nil {
		t.Fatal(err)
	}
	var tx eth.Transaction
	if err := json.Unmarshal(raw, &tx); err != nil {
		t.Fatal(err)
	}
	var msg evm.MsgEthereumTx
	if err := msg.FromEthereumTx(&tx); err != nil {
		t.Fatal(err)
	}
	decode := func([]byte) (sdk.Tx, error) { return &msg, nil }
	result := &abci.ResponseDeliverTx{Code: 11, GasUsed: 950977623, Log: "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 950977623: out of gas"}
	block := tm.MakeBlock(13184000, []tm.Tx{{1}}, &tm.Commit{}, nil)
	bloom := abci.EventAttribute{Key: evm.AttributeKeyEthereumBloom, Value: string(make([]byte, eth.BloomByteLength))}
	r := BlockResult{Block: block, ABCIResponses: &tmstate.ABCIResponses{
		BeginBlock: &abci.ResponseBeginBlock{}, DeliverTxs: []*abci.ResponseDeliverTx{result},
		EndBlock: &abci.ResponseEndBlock{Events: []abci.Event{{Type: evm.EventTypeBlockBloom, Attributes: []abci.EventAttribute{bloom}}}},
	}}
	build := func() *wire.DebankOutPutJs {
		t.Helper()
		got, err := r.Bundle(decode, common.Address{}, 100000000, big.NewInt(1), wire.TransactionStateDiff{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := build()
	if len(got.BlockFile.Txs) != 1 {
		t.Fatalf("expected failed transaction, got %d", len(got.BlockFile.Txs))
	}
	failed := got.BlockFile.Txs[0]
	if failed.ID != tx.Hash().Hex() || failed.Status || failed.GasUsed.Uint64() != 54137 {
		t.Fatalf("wrong failed transaction: %+v", failed)
	}
	if len(got.BlockFile.Events)+len(got.BlockFile.ErrorEvents)+len(got.BlockFile.Traces)+len(got.BlockFile.ErrorTraces) != 0 {
		t.Fatal("unexpected logs or calls without an EVM observation")
	}
	result.Log = "no block gas left to run tx: out of gas"
	if skipped := build(); len(skipped.BlockFile.Txs) != 0 {
		t.Fatal("transaction rejected before processing was included")
	}
}

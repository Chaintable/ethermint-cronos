package replay

import (
	"encoding/json"

	"os"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	eth "github.com/ethereum/go-ethereum/core/types"
	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
	tm "github.com/tendermint/tendermint/types"
	wire "github.com/tharsis/ethermint/debank/types"
	"github.com/tharsis/ethermint/x/evm/keeper"
	evm "github.com/tharsis/ethermint/x/evm/types"
)

// Reuse the archived signed transaction as input with modeled v0.6 keeper output.
func TestBlockGasUsesPreUpgradeMessageResult(t *testing.T) {
	raw, err := os.ReadFile("testdata/block-gas-tx-13184000.json")
	if err != nil {
		t.Fatal(err)
	}
	var tx eth.Transaction
	if err := json.Unmarshal(raw, &tx); err != nil {
		t.Fatal(err)
	}
	var msg evm.MsgEthereumTx
	msg.FromEthereumTx(&tx)
	decode := func([]byte) (sdk.Tx, error) { return &msg, nil }
	result := &abci.ResponseDeliverTx{Code: 11, GasUsed: 950977623, Log: "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 950977623: out of gas"}
	block := tm.MakeBlock(13184000, []tm.Tx{{1}}, &tm.Commit{}, nil)
	bloom := abci.EventAttribute{Key: []byte(evm.AttributeKeyEthereumBloom), Value: make([]byte, eth.BloomByteLength)}
	r := BlockResult{Block: block, ABCIResponses: &tmstate.ABCIResponses{
		BeginBlock: &abci.ResponseBeginBlock{}, DeliverTxs: []*abci.ResponseDeliverTx{result},
		EndBlock: &abci.ResponseEndBlock{Events: []abci.Event{{Type: evm.EventTypeBlockBloom, Attributes: []abci.EventAttribute{bloom}}}},
	}}
	trace, err := json.Marshal(wire.TraceResult{Traces: []wire.Trace{{TxID: tx.Hash().Hex()}}})
	if err != nil {
		t.Fatal(err)
	}
	receipt := &evm.MsgEthereumTxResponse{Hash: tx.Hash().Hex(), GasUsed: 23001}
	obs := keeper.ReplayTransaction{CosmosHash: block.Txs[0].Hash(), Hash: tx.Hash(), GasUsed: receipt.GasUsed, Trace: trace, MessageResult: receipt}
	build := func() (*wire.DebankOutPutJs, error) {
		return r.Bundle(decode, common.Address{}, 100000000, wire.TransactionStateDiff{}, []keeper.ReplayTransaction{obs})
	}
	got, err := build()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.BlockFile.Txs) != 1 || !got.BlockFile.Txs[0].Status || got.BlockFile.Txs[0].GasUsed.Uint64() != 23001 || len(got.BlockFile.Traces) != 1 {
		t.Fatalf("lost committed pre-upgrade result: %+v", got.BlockFile)
	}
	obs.MessageResult = nil
	if _, err := build(); err == nil {
		t.Fatal("accepted missing keeper result")
	}
	result.Log = "no block gas left to run tx: out of gas"
	got, err = build()
	if err != nil || len(got.BlockFile.Txs) != 0 {
		t.Fatalf("early rejection: %v, %v", got, err)
	}
}

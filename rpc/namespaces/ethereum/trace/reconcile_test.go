package trace

import (
	"encoding/json"
	"math/big"
	"strconv"
	"testing"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/evmos/ethermint/debank/replay"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/evmos/ethermint/debank/util"
	"github.com/evmos/ethermint/encoding"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

var (
	addrA = common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	topic = common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
)

func mkTrace(id, parent string, ta []int64, pos int64, gasUsed uint64, errStr string) dtypes.Trace {
	return dtypes.Trace{
		ID: id, ParentTraceID: parent, TraceAddress: ta, PosInParentTrace: pos,
		GasUsed: new(big.Int).SetUint64(gasUsed), TxID: "0xtx", Error: errStr,
	}
}

func mkEvent(parent string, pos int64, data byte, topics ...common.Hash) dtypes.Event {
	ev := dtypes.Event{
		ID:            util.ToHash([]string{parent, strconv.FormatInt(pos, 10)}),
		Address:       "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ParentTraceID: parent, Position: pos,
		Data: []byte{data},
	}
	if len(topics) > 0 {
		ev.Selector = topics[0].Hex()
		for _, t := range topics[1:] {
			ev.Topics = append(ev.Topics, t.Hex())
		}
	}
	return ev
}

func mkLog(data byte, topics ...common.Hash) *ethtypes.Log {
	return &ethtypes.Log{Address: addrA, Topics: topics, Data: []byte{data}}
}

func TestCheckReceipt(t *testing.T) {
	root := mkTrace("root", "", nil, 0, 33, "")
	a, b := mkEvent("root", 0, 1, topic), mkEvent("root", 1, 2)
	logA, logB := mkLog(1, topic), mkLog(2)
	logA.Index = 9
	logB.Index = 12
	for _, tc := range []struct {
		name string
		tr   dtypes.TraceResult
		rc   consensusReceipt
		pass bool
	}{
		{"agree", dtypes.TraceResult{Traces: []dtypes.Trace{root}, Events: []dtypes.Event{a, b}}, consensusReceipt{Status: true, GasUsed: 100, Logs: []*ethtypes.Log{logA, logB}}, true},
		{"receipt reverts", dtypes.TraceResult{Traces: []dtypes.Trace{root}}, consensusReceipt{}, false},
		{"replay reverts", dtypes.TraceResult{ErrorTraces: []dtypes.Trace{root}}, consensusReceipt{Status: true}, false},
		{"root missing", dtypes.TraceResult{}, consensusReceipt{Status: true}, false},
		{"duplicate root", dtypes.TraceResult{Traces: []dtypes.Trace{root, root}}, consensusReceipt{Status: true}, false},
		{"different content", dtypes.TraceResult{Traces: []dtypes.Trace{root}, Events: []dtypes.Event{a}}, consensusReceipt{Status: true, GasUsed: 100, Logs: []*ethtypes.Log{logB}}, false},
		{"different order", dtypes.TraceResult{Traces: []dtypes.Trace{root}, Events: []dtypes.Event{a, b}}, consensusReceipt{Status: true, GasUsed: 100, Logs: []*ethtypes.Log{logB, logA}}, false},
		{"missing event", dtypes.TraceResult{Traces: []dtypes.Trace{root}}, consensusReceipt{Status: true, GasUsed: 100, Logs: []*ethtypes.Log{logA}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReceipt(&tc.tr, "0xtx", &tc.rc)
			if !tc.pass {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 33, tc.tr.Traces[0].GasUsed.Uint64())
			require.EqualValues(t, 9, tc.tr.Events[0].LogIndex)
			require.EqualValues(t, 12, tc.tr.Events[1].LogIndex)
			require.Equal(t, a.ParentTraceID, tc.tr.Events[0].ParentTraceID)
		})
	}
}

func TestCommittedEVMMessages(t *testing.T) {
	cfg := encoding.MakeConfig()
	evmtypes.RegisterInterfaces(cfg.InterfaceRegistry)
	envelope := func(msgs ...sdk.Msg) []byte {
		builder := cfg.TxConfig.NewTxBuilder()
		require.NoError(t, builder.SetMsgs(msgs...))
		raw, err := cfg.TxConfig.TxEncoder()(builder.GetTx())
		require.NoError(t, err)
		return raw
	}
	result := func(responses ...*evmtypes.MsgEthereumTxResponse) *abci.ExecTxResult {
		var data sdk.TxMsgData
		for _, response := range responses {
			packed, err := codectypes.NewAnyWithValue(response)
			require.NoError(t, err)
			data.MsgResponses = append(data.MsgResponses, packed)
		}
		raw, err := data.Marshal()
		require.NoError(t, err)
		return &abci.ExecTxResult{Data: raw}
	}

	first := evmtypes.NewTxWithData(&ethtypes.LegacyTx{Nonce: 1, Gas: 80000})
	second := evmtypes.NewTxWithData(&ethtypes.LegacyTx{Nonce: 2, Gas: 90000})
	third := evmtypes.NewTxWithData(&ethtypes.LegacyTx{Nonce: 3, Gas: 100000})
	// The failed envelope has no decodable transaction or responses. It consumes no EVM index.
	txs := tmtypes.Txs{envelope(first, second), {0xff}, envelope(third)}
	rc1 := &evmtypes.MsgEthereumTxResponse{Hash: first.Hash().Hex(), GasUsed: 42000, Logs: []*evmtypes.Log{{}}}
	rc2 := &evmtypes.MsgEthereumTxResponse{Hash: second.Hash().Hex(), GasUsed: 50000, VmError: "execution reverted"}
	rc3 := &evmtypes.MsgEthereumTxResponse{Hash: third.Hash().Hex(), GasUsed: 32000, Logs: []*evmtypes.Log{{TxIndex: 2, Index: 1}}}
	failed := &abci.ExecTxResult{Code: 11, Data: []byte{0xff}}
	results := []*abci.ExecTxResult{result(rc1, rc2), failed, result(rc3)}

	msgs, err := committedEVMMessages(cfg.TxConfig.TxDecoder(), txs, results)
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	for i, expected := range []struct {
		cosmosIndex, messageIndex int
		response                  *evmtypes.MsgEthereumTxResponse
	}{{0, 0, rc1}, {0, 1, rc2}, {2, 0, rc3}} {
		require.Equal(t, expected.cosmosIndex, msgs[i].cosmosIndex)
		require.Equal(t, expected.messageIndex, msgs[i].messageIndex)
		require.Equal(t, expected.response.Hash, msgs[i].tx.Hash().Hex())
		require.Equal(t, expected.response.GasUsed, msgs[i].receipt.GasUsed)
		require.Equal(t, !expected.response.Failed(), msgs[i].receipt.Status)
	}
	require.Len(t, msgs[0].receipt.Logs, 1)
	require.EqualValues(t, 0, msgs[0].receipt.Logs[0].Index)
	require.Empty(t, msgs[1].receipt.Logs)
	require.Len(t, msgs[2].receipt.Logs, 1)
	require.EqualValues(t, 2, msgs[2].receipt.Logs[0].TxIndex)
	require.EqualValues(t, 1, msgs[2].receipt.Logs[0].Index)

	badIndex := *rc3
	badIndex.Logs = []*evmtypes.Log{{TxIndex: 2, Index: 0}}
	for _, tc := range []struct {
		name    string
		results []*abci.ExecTxResult
		want    string
	}{
		{"swapped responses", []*abci.ExecTxResult{result(rc2, rc1), failed, result(rc3)}, "receipt hash"},
		{"missing response", []*abci.ExecTxResult{result(rc1), failed, result(rc3)}, "2 messages, 1 responses"},
		{"log index reset", []*abci.ExecTxResult{result(rc1, rc2), failed, result(&badIndex)}, "invalid receipt log position"},
		{"corrupt responses", []*abci.ExecTxResult{{Data: []byte{0xff}}, failed, result(rc3)}, "decode tx[0] responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := committedEVMMessages(cfg.TxConfig.TxDecoder(), txs, tc.results)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, got)
		})
	}
}

func TestBlockGasFailure(t *testing.T) {
	msg := evmtypes.NewTxWithData(&ethtypes.LegacyTx{Gas: 54137})
	cfg := encoding.MakeConfig()
	evmtypes.RegisterInterfaces(cfg.InterfaceRegistry)
	builder := cfg.TxConfig.NewTxBuilder()
	require.NoError(t, builder.SetMsgs(msg))
	raw, err := cfg.TxConfig.TxEncoder()(builder.GetTx())
	require.NoError(t, err)
	result := &abci.ExecTxResult{Code: 11, Log: rpctypes.ExceedBlockGasLimitError, GasUsed: 950977623}
	results := &ctypes.ResultBlockResults{TxsResults: []*abci.ExecTxResult{result}}
	msgs, err := committedEVMMessages(cfg.TxConfig.TxDecoder(), tmtypes.Txs{raw}, results.TxsResults)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, msg.GetGas(), msgs[0].receipt.GasUsed)
	for _, captured := range []bool{false, true} {
		replayed := &replay.Result{}
		if captured {
			frames, err := json.Marshal(dtypes.TraceResult{Traces: []dtypes.Trace{{TxID: msg.Hash().Hex()}}, Events: []dtypes.Event{{ID: "attempt"}}})
			require.NoError(t, err)
			replayed.Transactions = []replay.Transaction{{Hash: msg.Hash(), GasUsed: 21000, Trace: frames}}
		}
		obs, err := canonicalObservations(msgs, results, replayed)
		require.NoError(t, err)
		tr, err := receiptTrace(obs[0], &msgs[0].receipt)
		require.NoError(t, err)
		require.Empty(t, tr.Traces)
		require.Empty(t, tr.Events)
		if captured {
			require.Len(t, tr.ErrorTraces, 1)
			require.Equal(t, result.Log, tr.ErrorTraces[0].Error)
			require.Len(t, tr.ErrorEvents, 1)
		} else {
			require.Empty(t, tr.ErrorTraces)
			require.Empty(t, tr.ErrorEvents)
		}
	}
}

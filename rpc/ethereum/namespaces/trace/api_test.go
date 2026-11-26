package trace

import (
	"encoding/json"
	"fmt"
	"math/big"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/ed25519"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/tharsis/ethermint/debank/replay"
	dtypes "github.com/tharsis/ethermint/debank/types"
	evmtypes "github.com/tharsis/ethermint/x/evm/types"
)

// Gas of the seven Cosmos transactions of mainnet H6346; the RPC reports
// 0xac4c58 = 11291736.
func TestBlockGasUsedStopsAtBlockGasRejection(t *testing.T) {
	results := &tmstate.ABCIResponses{}
	for _, g := range []int64{242940, 248014, 450684, 10350098} {
		results.DeliverTxs = append(results.DeliverTxs, &abci.ResponseDeliverTx{GasUsed: g})
	}
	for i := 0; i < 3; i++ {
		results.DeliverTxs = append(results.DeliverTxs, &abci.ResponseDeliverTx{Code: 11, GasUsed: 11291736, Log: "no block gas left to run tx: out of gas"})
	}
	require.Equal(t, uint64(0xac4c58), blockGasUsed(results))
}

func ethTx(nonce uint64) *evmtypes.MsgEthereumTx {
	to := common.HexToAddress("0xcc")
	return evmtypes.NewTx(big.NewInt(25), nonce, &to, big.NewInt(0), 21000, big.NewInt(1), nil, nil)
}

func msgResult(t *testing.T, responses ...*evmtypes.MsgEthereumTxResponse) []byte {
	t.Helper()
	data := &sdk.TxMsgData{}
	for _, r := range responses {
		b, err := r.Marshal()
		require.NoError(t, err)
		data.Data = append(data.Data, &sdk.MsgData{MsgType: "/ethermint.evm.v1.MsgEthereumTx", Data: b})
	}
	b, err := data.Marshal()
	require.NoError(t, err)
	return b
}

func TestReceiptsFromResults(t *testing.T) {
	a, b := ethTx(0), ethTx(1)
	log := &evmtypes.Log{Address: "0x00000000000000000000000000000000000000cc", Topics: []string{common.HexToHash("0x01").Hex()}, Data: []byte{1}, Index: 7}
	results := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{
		{Data: msgResult(t, &evmtypes.MsgEthereumTxResponse{Hash: a.AsTransaction().Hash().Hex(), GasUsed: 30000, Logs: []*evmtypes.Log{log}}), GasUsed: 30000},
		{Code: 11},
		{Data: msgResult(t, &evmtypes.MsgEthereumTxResponse{Hash: b.AsTransaction().Hash().Hex(), GasUsed: 21000, VmError: "execution reverted"}), GasUsed: 21000},
	}}
	got, err := receiptsFromResults([]ethMsg{{cosmos: 0, tx: a}, {cosmos: 2, tx: b}}, results)
	require.NoError(t, err)
	require.True(t, got[0].Status)
	require.Equal(t, uint64(30000), got[0].GasUsed)
	require.Len(t, got[0].Logs, 1)
	require.Equal(t, uint(7), got[0].Logs[0].Index)
	require.False(t, got[1].Status)

	_, err = receiptsFromResults([]ethMsg{{cosmos: 2, tx: a}}, results)
	require.ErrorContains(t, err, "response hash")
}

func traceJSON(t *testing.T, tr dtypes.TraceResult) json.RawMessage {
	b, err := json.Marshal(tr)
	require.NoError(t, err)
	return b
}

func TestCheckReceipt(t *testing.T) {
	hash := common.HexToHash("0xab")
	ev := dtypes.Event{Address: "0x00000000000000000000000000000000000000cc", Selector: common.HexToHash("0x01").Hex(), Data: []byte{1}}
	lg := &ethtypes.Log{Address: common.HexToAddress("0xcc"), Topics: []common.Hash{common.HexToHash("0x01")}, Data: []byte{1}, Index: 9}
	root := dtypes.Trace{TxID: hash.Hex(), TraceAddress: []int64{}}

	tr := dtypes.TraceResult{Traces: []dtypes.Trace{root}, Events: []dtypes.Event{ev}}
	obs := replay.Transaction{Hash: hash, Trace: traceJSON(t, tr)}
	got, err := receiptTrace(obs, &receipt{Status: true, Logs: []*ethtypes.Log{lg}})
	require.NoError(t, err)
	require.Equal(t, int64(9), got.Events[0].LogIndex)

	_, err = receiptTrace(obs, &receipt{Status: false, Logs: []*ethtypes.Log{lg}})
	require.ErrorContains(t, err, "root status")
	other := *lg
	other.Data = []byte{2}
	_, err = receiptTrace(obs, &receipt{Status: true, Logs: []*ethtypes.Log{&other}})
	require.ErrorContains(t, err, "content differs")

	// Post-processing revert: the receipt keeps the EVM log, the trace reports it as an error event.
	ev.LogIndex = 0
	tr = dtypes.TraceResult{ErrorTraces: []dtypes.Trace{root}, ErrorEvents: []dtypes.Event{ev}}
	obs = replay.Transaction{Hash: hash, Trace: traceJSON(t, tr), HookLogs: []dtypes.Event{ev}}
	got, err = receiptTrace(obs, &receipt{Status: false, Logs: []*ethtypes.Log{lg}})
	require.NoError(t, err)
	require.Equal(t, int64(0), got.ErrorEvents[0].LogIndex)
	_, err = receiptTrace(obs, &receipt{Status: false})
	require.ErrorContains(t, err, "committed log count")
}

func TestCanonicalObservations(t *testing.T) {
	a, b := ethTx(0), ethTx(1)
	msgs := []ethMsg{{cosmos: 0, tx: a}, {cosmos: 2, tx: b}}
	results := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{{}, {Code: 5}, {}}}
	receipts := []*receipt{{Status: true, ExecutionGas: 21000}, {Status: false, ExecutionGas: 21000}}
	obs := func(i int, tx *evmtypes.MsgEthereumTx, failed bool) replay.Transaction {
		return replay.Transaction{CosmosIndex: i, Hash: tx.AsTransaction().Hash(), GasUsed: 21000, Failed: failed, Trace: []byte("{}")}
	}
	replayed := &replay.Result{Transactions: []replay.Transaction{obs(0, a, false), obs(1, ethTx(5), true), obs(2, b, true)}}
	got, err := canonicalObservations(msgs, results, replayed, receipts)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// At mainnet H441639 the archived response of the row exceeding the block
	// gas limit reports 800 gas less than the execution.
	receipts[1].ExecutionGas = 20200
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.ErrorContains(t, err, "receipt mismatch")
	replayed.Responses = &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{{}, {Code: 5},
		{Code: 11, Log: "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 21000: out of gas"}}}
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.NoError(t, err)

	// At mainnet H2309561 the archive reports a revert of the execution that
	// returned; the trace follows the archive.
	root := dtypes.Trace{ID: "r", TxID: b.AsTransaction().Hash().Hex(), TraceAddress: []int64{}}
	child := dtypes.Trace{ID: "c", ParentTraceID: "r", TxID: root.TxID, TraceAddress: []int64{0}, PosInParentTrace: 1}
	ev := dtypes.Event{ParentTraceID: "r", Position: 0}
	replayed.Transactions[2].Failed = false
	replayed.Transactions[2].Trace = traceJSON(t, dtypes.TraceResult{Traces: []dtypes.Trace{root, child}, Events: []dtypes.Event{ev}, StorageContracts: []string{"0xcc"}})
	receipts[1].VmError = "execution reverted"
	got, err = canonicalObservations(msgs, results, replayed, receipts)
	require.NoError(t, err)
	require.True(t, got[1].Failed)
	tr, err := receiptTrace(got[1], receipts[1])
	require.NoError(t, err)
	require.Empty(t, tr.Traces)
	require.Empty(t, tr.Events)
	require.Empty(t, tr.StorageContracts)
	require.Len(t, tr.ErrorEvents, 1)
	require.Len(t, tr.ErrorTraces, 2)
	require.Equal(t, "execution reverted", tr.ErrorTraces[0].Error)
	require.Equal(t, "parent call failed", tr.ErrorTraces[1].Error)

	// The archive cannot report success for an execution that failed.
	receipts[1].Status, receipts[1].VmError = true, ""
	replayed.Transactions[2].Failed = true
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.ErrorContains(t, err, "receipt mismatch")
	replayed.Transactions = replayed.Transactions[:2]
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.ErrorContains(t, err, "observations for 2 EVM messages")
}

type fakeChain struct {
	Chain
	headers map[int64]*tmtypes.Header
	vals    map[int64]*tmtypes.ValidatorSet
	loads   int
}

func (c *fakeChain) Header(h int64) (*tmtypes.Header, error) {
	if hd, ok := c.headers[h]; ok {
		return hd, nil
	}
	return nil, fmt.Errorf("block %d not found", h)
}

func (c *fakeChain) Validators(h int64) (*tmtypes.ValidatorSet, error) {
	c.loads++
	return c.vals[h].Copy(), nil
}

func TestReplayRequest(t *testing.T) {
	v1 := tmtypes.NewValidator(ed25519.GenPrivKey().PubKey(), 10)
	v2 := tmtypes.NewValidator(ed25519.GenPrivKey().PubKey(), 20)
	set := tmtypes.NewValidatorSet([]*tmtypes.Validator{v1, v2})
	parent := &tmtypes.Header{Height: 9, ChainID: "cronosmainnet_25-1", ValidatorsHash: set.Hash()}
	chain := &fakeChain{headers: map[int64]*tmtypes.Header{9: parent}, vals: map[int64]*tmtypes.ValidatorSet{9: set}}
	api := &API{chain: chain}

	block := func(height int64) *tmtypes.Block {
		parent.Height = height - 1
		chain.headers[height-1] = parent
		chain.vals[height-1] = set
		b := tmtypes.MakeBlock(height, []tmtypes.Tx{{1}}, &tmtypes.Commit{
			Height: height - 1, BlockID: tmtypes.BlockID{Hash: parent.Hash()},
			Signatures: []tmtypes.CommitSig{
				{BlockIDFlag: tmtypes.BlockIDFlagCommit, ValidatorAddress: set.Validators[0].Address},
				tmtypes.NewCommitSigAbsent(),
			},
		}, nil)
		b.Header.ChainID = parent.ChainID
		b.LastBlockID = tmtypes.BlockID{Hash: parent.Hash()}
		return b
	}
	req, err := api.replayRequest(block(10))
	require.NoError(t, err)
	require.Equal(t, int64(10), req.BeginBlock.Header.Height)
	require.Len(t, req.BeginBlock.LastCommitInfo.Votes, 2)
	require.True(t, req.BeginBlock.LastCommitInfo.Votes[0].SignedLastBlock)
	require.False(t, req.BeginBlock.LastCommitInfo.Votes[1].SignedLastBlock)
	require.Equal(t, set.Validators[1].VotingPower, req.BeginBlock.LastCommitInfo.Votes[1].Validator.Power)
	require.Equal(t, [][]byte{{1}}, req.Txs)

	_, err = api.replayRequest(block(11))
	require.NoError(t, err)
	require.Equal(t, 1, chain.loads, "unchanged validator set is cached")

	b := block(12)
	b.LastBlockID = tmtypes.BlockID{Hash: make([]byte, 32)}
	_, err = api.replayRequest(b)
	require.ErrorContains(t, err, "parent header hash")

	parent.ValidatorsHash = make([]byte, 32)
	api.validators = validatorCache{}
	b = block(13)
	_, err = api.replayRequest(b)
	require.ErrorContains(t, err, "parent validator hash")
}

func TestReportedIncludesBlockGasExceeded(t *testing.T) {
	exceeded := &abci.ResponseDeliverTx{Code: 11, Log: "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 1: out of gas"}
	require.True(t, reported(&abci.ResponseDeliverTx{}))
	require.True(t, reported(exceeded))
	require.False(t, reported(&abci.ResponseDeliverTx{Code: 11, Log: "no block gas left to run tx: out of gas"}))
	require.False(t, reported(&abci.ResponseDeliverTx{Code: 5}))

	// An archive without fix-unlucky-tx has no data for the committed message.
	_, err := receiptsFromResults([]ethMsg{{cosmos: 0, tx: ethTx(0)}}, &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{exceeded}})
	require.ErrorContains(t, err, "not restored by fix-unlucky-tx")
}

// Reverting a message whose hooks failed also undoes its gas refund, so the
// sender paid the whole gas limit (mainnet H1737384: limit 116711, DeliverTx
// gas 109706, fee 116711 x price with no refund).
func TestPostProcessingFailureReportsGasLimit(t *testing.T) {
	msg := ethTx(0)
	hash := msg.AsTransaction().Hash()
	results := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{{
		Data:    msgResult(t, &evmtypes.MsgEthereumTxResponse{Hash: hash.Hex(), GasUsed: 15000, VmError: evmtypes.ErrPostTxProcessing.Error()}),
		GasUsed: 15000,
	}}}
	msgs := []ethMsg{{cosmos: 0, tx: msg}}
	receipts, err := receiptsFromResults(msgs, results)
	require.NoError(t, err)
	require.True(t, receipts[0].PostProcessingFailed)
	require.False(t, receipts[0].Status)

	root := dtypes.Trace{TxID: hash.Hex(), TraceAddress: []int64{}, Error: evmtypes.ErrPostTxProcessing.Error()}
	obs := []replay.Transaction{{Hash: hash, GasUsed: 15000, Failed: true, Trace: traceJSON(t, dtypes.TraceResult{ErrorTraces: []dtypes.Trace{root}})}}
	api := &API{chainID: big.NewInt(25)}
	bf, _, err := api.assembleBlockFile(dtypes.Block{}, msgs, obs, receipts)
	require.NoError(t, err)
	require.Equal(t, int64(21000), bf.Txs[0].GasUsed.Int64())

	receipts[0].PostProcessingFailed = false
	bf, _, err = api.assembleBlockFile(dtypes.Block{}, msgs, obs, receipts)
	require.NoError(t, err)
	require.Equal(t, int64(15000), bf.Txs[0].GasUsed.Int64())
}

// An EVM message executed inside another message, such as authz MsgExec, is
// observed but not listed by the RPC.
func TestCanonicalObservationsSkipsNestedMessages(t *testing.T) {
	a := ethTx(0)
	hash := a.AsTransaction().Hash()
	results := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{
		{Data: msgResult(t, &evmtypes.MsgEthereumTxResponse{Hash: hash.Hex(), GasUsed: 21000}), GasUsed: 21000},
		{}, // MsgExec wrapping an EVM message
	}}
	msgs := []ethMsg{{cosmos: 0, tx: a}}
	receipts, err := receiptsFromResults(msgs, results)
	require.NoError(t, err)
	root := dtypes.Trace{TxID: hash.Hex(), TraceAddress: []int64{}}
	replayed := &replay.Result{Transactions: []replay.Transaction{
		{CosmosIndex: 0, Hash: hash, GasUsed: 21000, Trace: traceJSON(t, dtypes.TraceResult{Traces: []dtypes.Trace{root}})},
		{CosmosIndex: 1, Hash: ethTx(1).AsTransaction().Hash(), GasUsed: 21000, Trace: []byte("{}")},
	}}
	selected, err := canonicalObservations(msgs, results, replayed, receipts)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, hash, selected[0].Hash)
}

// At mainnet H2408223 the archived logs of the row exceeding the block gas
// limit carry a value other than the execution's; the events follow the archive.
func TestCanonicalObservationsAdoptsArchivedLogs(t *testing.T) {
	a := ethTx(0)
	hash := a.AsTransaction().Hash()
	msgs := []ethMsg{{cosmos: 0, tx: a}}
	results := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{{}}}
	topic := common.HexToHash("0x01")
	lg := &ethtypes.Log{Address: common.HexToAddress("0xcc"), Topics: []common.Hash{topic, common.HexToHash("0x02")}, Data: []byte{0x4a}, Index: 3}
	receipts := []*receipt{{Status: true, ExecutionGas: 20000, Logs: []*ethtypes.Log{lg}}}
	root := dtypes.Trace{ID: "r", TxID: hash.Hex(), TraceAddress: []int64{}}
	ev := dtypes.Event{Address: "0x00000000000000000000000000000000000000cc", Selector: topic.Hex(), Topics: []string{common.HexToHash("0x02").Hex()}, Data: []byte{0x49}, ParentTraceID: "r"}
	tr := dtypes.TraceResult{Traces: []dtypes.Trace{root}, Events: []dtypes.Event{ev}}
	replayed := &replay.Result{
		Responses:    &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{{Code: 11, Log: "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 21000: out of gas"}}},
		Transactions: []replay.Transaction{{CosmosIndex: 0, Hash: hash, GasUsed: 21000, Trace: traceJSON(t, tr)}},
	}
	got, err := canonicalObservations(msgs, results, replayed, receipts)
	require.NoError(t, err)
	out, err := receiptTrace(got[0], receipts[0])
	require.NoError(t, err)
	require.Equal(t, []byte{0x4a}, []byte(out.Events[0].Data))
	require.Equal(t, int64(3), out.Events[0].LogIndex)

	// Only the row exceeding the block gas limit follows the archive.
	replayed.Responses.DeliverTxs[0] = &abci.ResponseDeliverTx{}
	receipts[0].ExecutionGas = 21000
	got, err = canonicalObservations(msgs, results, replayed, receipts)
	require.NoError(t, err)
	_, err = receiptTrace(got[0], receipts[0])
	require.ErrorContains(t, err, "content differs")

	// A log from another contract is still rejected.
	replayed.Responses.DeliverTxs[0] = &abci.ResponseDeliverTx{Code: 11, Log: "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 21000: out of gas"}
	other := *lg
	other.Address = common.HexToAddress("0xdd")
	receipts[0].Logs = []*ethtypes.Log{&other}
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.ErrorContains(t, err, "address differs")
}

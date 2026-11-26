package trace

import (
	"context"
	"encoding/json"
	"fmt"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"math/big"
	"testing"

	sdkmath "cosmossdk.io/math"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/crypto/ed25519"
	"github.com/cometbft/cometbft/libs/log"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tmtypes "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	"github.com/evmos/ethermint/debank/replay"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	feemarkettypes "github.com/evmos/ethermint/x/feemarket/types"
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
	return evmtypes.NewTx(big.NewInt(25), nonce, &to, big.NewInt(0), 21000, big.NewInt(1), nil, nil, nil, nil)
}

func msgResult(t *testing.T, responses ...*evmtypes.MsgEthereumTxResponse) []byte {
	t.Helper()
	data := &sdk.TxMsgData{}
	for _, r := range responses {
		b, err := r.Marshal()
		require.NoError(t, err)
		data.MsgResponses = append(data.MsgResponses, &codectypes.Any{TypeUrl: "/ethermint.evm.v1.MsgEthereumTxResponse", Value: b})
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

	// Within a batch each message reports its own gas.
	batch := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{{Data: msgResult(t,
		&evmtypes.MsgEthereumTxResponse{Hash: a.AsTransaction().Hash().Hex(), GasUsed: 30000},
		&evmtypes.MsgEthereumTxResponse{Hash: b.AsTransaction().Hash().Hex(), GasUsed: 21000},
	), GasUsed: 51000}}}
	got, err = receiptsFromResults([]ethMsg{{cosmos: 0, msg: 0, tx: a}, {cosmos: 0, msg: 1, tx: b}}, batch)
	require.NoError(t, err)
	require.Equal(t, uint64(30000), got[0].GasUsed)
	require.Equal(t, uint64(21000), got[1].GasUsed)
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

	// Post-processing revert: the receipt has no logs, the trace reports them as error events.
	ev.LogIndex = 0
	tr = dtypes.TraceResult{ErrorTraces: []dtypes.Trace{root}, ErrorEvents: []dtypes.Event{ev}}
	obs = replay.Transaction{Hash: hash, Trace: traceJSON(t, tr)}
	got, err = receiptTrace(obs, &receipt{Status: false})
	require.NoError(t, err)
	require.Equal(t, int64(0), got.ErrorEvents[0].LogIndex)
	_, err = receiptTrace(obs, &receipt{Status: false, Logs: []*ethtypes.Log{lg}})
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

	replayed.Transactions[2].Failed = false
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.ErrorContains(t, err, "receipt mismatch")
	replayed.Transactions = replayed.Transactions[:2]
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.ErrorContains(t, err, "observations for 2 EVM messages")
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

type resultsChain struct {
	fakeChain
	block   *tmtypes.Block
	results *tmstate.ABCIResponses
}

func (c *resultsChain) Block(int64) (*tmtypes.Block, error)           { return c.block, nil }
func (c *resultsChain) Results(int64) (*tmstate.ABCIResponses, error) { return c.results, nil }

type checkOnlyExecutor struct{ replay.Executor }

func (checkOnlyExecutor) CheckReplayHeight(string, int64) error { return nil }
func (checkOnlyExecutor) BlockFileOnly() (bool, error)          { return true, nil }

// A block without EVM messages is not replayed, so its archived results must
// match the next header.
func TestUnreplayedBlockChecksLastResultsHash(t *testing.T) {
	parent := tmtypes.BlockID{Hash: make([]byte, 32), PartSetHeader: tmtypes.PartSetHeader{Total: 1, Hash: make([]byte, 32)}}
	commit := &tmtypes.Commit{Height: 9, BlockID: parent, Signatures: []tmtypes.CommitSig{tmtypes.NewCommitSigAbsent()}}
	block := tmtypes.MakeBlock(10, []tmtypes.Tx{{1}}, commit, nil)
	block.ChainID, block.LastBlockID, block.ProposerAddress = "cronosmainnet_25-1", parent, make([]byte, 20)
	results := &tmstate.ABCIResponses{
		DeliverTxs: []*abci.ResponseDeliverTx{{Code: 5}},
		BeginBlock: &abci.ResponseBeginBlock{}, EndBlock: &abci.ResponseEndBlock{},
	}
	next := &tmtypes.Header{Height: 11, LastBlockID: tmtypes.BlockID{Hash: block.Hash()}, LastResultsHash: make([]byte, 32)}
	chain := &resultsChain{fakeChain: fakeChain{headers: map[int64]*tmtypes.Header{11: next}}, block: block, results: results}
	// The only transaction failed, so it is never decoded.
	api := NewAPI(log.NewNopLogger(), chain, nil, big.NewInt(25), checkOnlyExecutor{})
	height := rpctypes.BlockNumber(10)
	_, err := api.DebankBlockRaw(context.Background(), rpctypes.BlockNumberOrHash{BlockNumber: &height})
	require.ErrorContains(t, err, "next header LastResultsHash")
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

	// The message's state was dropped; the sender paid the whole gas limit.
	msg := ethTx(0)
	msgs := []ethMsg{{cosmos: 0, tx: msg}}
	receipts, err := receiptsFromResults(msgs, &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{exceeded}})
	require.NoError(t, err)
	require.True(t, receipts[0].ExceededBlockGas)
	require.False(t, receipts[0].Status)
	require.Empty(t, receipts[0].Logs)

	hash := msg.AsTransaction().Hash()
	root := dtypes.Trace{TxID: hash.Hex(), TraceAddress: []int64{}, Error: replay.BlockGasExceeded}
	obs := replay.Transaction{Hash: hash, GasUsed: 15000, Failed: true, Trace: traceJSON(t, dtypes.TraceResult{ErrorTraces: []dtypes.Trace{root}})}
	replayed := &replay.Result{Transactions: []replay.Transaction{obs}}
	results := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{exceeded}}
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.NoError(t, err)
	api := &API{chainID: big.NewInt(25)}
	bf, _, err := api.assembleBlockFile(dtypes.Block{}, msgs, []replay.Transaction{obs}, receipts)
	require.NoError(t, err)
	require.Equal(t, int64(21000), bf.Txs[0].GasUsed.Int64())
	require.False(t, bf.Txs[0].Status)

	replayed.Transactions[0].Failed = false
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.ErrorContains(t, err, "not observed as failed")
}

// The refund is outside the snapshot that a hook failure reverts, so the
// sender paid the message's gas, which DeliverTx reports.
func TestPostProcessingFailureReportsChargedGas(t *testing.T) {
	msg := ethTx(0)
	hash := msg.AsTransaction().Hash()
	results := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{{
		Data:    msgResult(t, &evmtypes.MsgEthereumTxResponse{Hash: hash.Hex(), GasUsed: 15000, VmError: evmtypes.ErrPostTxProcessing.Error()}),
		GasUsed: 15000,
	}}}
	msgs := []ethMsg{{cosmos: 0, tx: msg}}
	receipts, err := receiptsFromResults(msgs, results)
	require.NoError(t, err)
	require.False(t, receipts[0].Status)

	root := dtypes.Trace{TxID: hash.Hex(), TraceAddress: []int64{}, Error: evmtypes.ErrPostTxProcessing.Error()}
	obs := []replay.Transaction{{Hash: hash, GasUsed: 15000, Failed: true, Trace: traceJSON(t, dtypes.TraceResult{ErrorTraces: []dtypes.Trace{root}})}}
	api := &API{chainID: big.NewInt(25)}
	bf, _, err := api.assembleBlockFile(dtypes.Block{}, msgs, obs, receipts)
	require.NoError(t, err)
	require.Equal(t, int64(15000), bf.Txs[0].GasUsed.Int64())
}

type storeChain struct {
	Chain
	kv map[string][]byte
}

func (c *storeChain) StoreGet(_ int64, store string, key []byte) ([]byte, error) {
	return c.kv[store+"|"+string(key)], nil
}

// The base fee lives in the fee market module params, falling back to the
// v1 store entry when the param is zero; NoBaseFee disables it.
func TestBaseFee(t *testing.T) {
	encode := func(p feemarkettypes.Params) []byte {
		bz, err := p.Marshal()
		require.NoError(t, err)
		return bz
	}
	key := "feemarket|" + string(feemarkettypes.ParamsKey)
	p := feemarkettypes.DefaultParams()
	p.BaseFee = sdkmath.NewInt(4831016520757)
	chain := &storeChain{kv: map[string][]byte{key: encode(p)}}
	api := &API{chain: chain}
	fee, err := api.baseFee(1)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(4831016520757), fee)

	p.BaseFee = sdkmath.ZeroInt()
	chain.kv = map[string][]byte{key: encode(p), "feemarket|\x02": big.NewInt(5000).Bytes()}
	fee, err = api.baseFee(1)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(5000), fee)

	p.NoBaseFee = true
	chain.kv[key] = encode(p)
	fee, err = api.baseFee(1)
	require.NoError(t, err)
	require.Equal(t, 0, fee.Sign())

	chain.kv = map[string][]byte{}
	_, err = api.baseFee(1)
	require.Error(t, err)
}

func TestEVMDenom(t *testing.T) {
	p := evmtypes.DefaultParams()
	p.EvmDenom = "basecro"
	bz, err := p.Marshal()
	require.NoError(t, err)
	api := &API{chain: &storeChain{kv: map[string][]byte{"evm|" + string(evmtypes.KeyPrefixParams): bz}}}
	denom, err := api.evmDenom(1)
	require.NoError(t, err)
	require.Equal(t, "basecro", denom)
}

// A transaction whose ante handler failed once the block gas meter was full
// is code 11 too, but the VM never ran (H7317940 tx 64).
func TestBlockGasExceededWithoutExecution(t *testing.T) {
	exceeded := &abci.ResponseDeliverTx{Code: 11, Log: "out of gas in location: block gas meter; gasWanted: 21000, gasUsed: 87450: out of gas"}
	a, b := ethTx(0), ethTx(1)
	msgs := []ethMsg{{cosmos: 0, tx: a}, {cosmos: 1, tx: b}}
	results := &tmstate.ABCIResponses{DeliverTxs: []*abci.ResponseDeliverTx{{
		Data:    msgResult(t, &evmtypes.MsgEthereumTxResponse{Hash: a.AsTransaction().Hash().Hex(), GasUsed: 21000}),
		GasUsed: 21000,
	}, exceeded}}
	receipts, err := receiptsFromResults(msgs, results)
	require.NoError(t, err)
	hash := a.AsTransaction().Hash()
	root := dtypes.Trace{TxID: hash.Hex(), TraceAddress: []int64{}}
	replayed := &replay.Result{Transactions: []replay.Transaction{{Hash: hash, GasUsed: 21000, Trace: traceJSON(t, dtypes.TraceResult{Traces: []dtypes.Trace{root}})}}}
	observed, err := canonicalObservations(msgs, results, replayed, receipts)
	require.NoError(t, err)
	api := &API{chainID: big.NewInt(25)}
	bf, _, err := api.assembleBlockFile(dtypes.Block{}, msgs, observed, receipts)
	require.NoError(t, err)
	require.Len(t, bf.Txs, 2)
	require.False(t, bf.Txs[1].Status)
	require.Equal(t, int64(21000), bf.Txs[1].GasUsed.Int64())
	require.Len(t, bf.Traces, 1)
	require.Empty(t, bf.ErrorTraces)

	// A missing observation of an executed message is still an error.
	results.DeliverTxs[1] = &abci.ResponseDeliverTx{Data: msgResult(t, &evmtypes.MsgEthereumTxResponse{Hash: b.AsTransaction().Hash().Hex(), GasUsed: 21000}), GasUsed: 21000}
	receipts, err = receiptsFromResults(msgs, results)
	require.NoError(t, err)
	_, err = canonicalObservations(msgs, results, replayed, receipts)
	require.ErrorContains(t, err, "observations for 2 EVM messages")
}

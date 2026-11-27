package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/evmos/ethermint/rpc/backend"
	rpctypes "github.com/evmos/ethermint/rpc/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/evmos/ethermint/debank/replay"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/stretchr/testify/require"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestMergeStorageContractsPreservesAndAddsAddresses(t *testing.T) {
	first := common.HexToAddress("0xaAaAaAaaAaAaAaaAaAAAAAAAAaaaAaAaAaaAaaAa")
	second := common.HexToAddress("0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB")
	got := mergeStorageContracts(
		map[common.Address]struct{}{second: {}},
		[]string{first.Hex(), second.Hex()},
	)
	require.ElementsMatch(t, []string{
		strings.ToLower(first.Hex()), strings.ToLower(second.Hex()),
	}, got)
}

type movedBlockBackend struct {
	backend.EVMBackend
	block *ctypes.ResultBlock
}

func (m movedBlockBackend) BlockNumberFromTendermint(rpctypes.BlockNumberOrHash) (rpctypes.BlockNumber, error) {
	return rpctypes.BlockNumber(m.block.Block.Height), nil
}

func (m movedBlockBackend) TendermintBlockByNumber(rpctypes.BlockNumber) (*ctypes.ResultBlock, error) {
	return m.block, nil
}

func TestHashLookupRejectsChangedBlock(t *testing.T) {
	block := tmtypes.MakeBlock(45062801, nil, nil, nil)
	block.ValidatorsHash = bytes.Repeat([]byte{1}, 32)
	hash := block.Hash()
	other := common.HexToHash("0x22")
	api := API{backend: movedBlockBackend{block: &ctypes.ResultBlock{Block: block, BlockID: tmtypes.BlockID{Hash: hash}}}}
	result, err := api.DebankBlockRaw(context.Background(), rpctypes.BlockNumberOrHash{BlockHash: &other})
	require.Nil(t, result)
	require.ErrorContains(t, err, "block identity mismatch")
	require.ErrorContains(t, err, "45062801")
}

type headBackend struct {
	backend.EVMBackend
	head uint64
}

func (b headBackend) BlockNumber() (hexutil.Uint64, error) { return hexutil.Uint64(b.head), nil }

func (b headBackend) TendermintBlockByNumber(h rpctypes.BlockNumber) (*ctypes.ResultBlock, error) {
	return &ctypes.ResultBlock{Block: tmtypes.MakeBlock(int64(h), nil, nil, nil)}, nil
}

func TestNextBlockRequired(t *testing.T) {
	_, err := (&API{backend: headBackend{head: 100}}).nextBlock(100)
	require.ErrorContains(t, err, "next block 101 not committed yet")
	next, err := (&API{backend: headBackend{head: 101}}).nextBlock(100)
	require.NoError(t, err)
	require.Equal(t, int64(101), next.Block.Height)
}

func TestEncodeOutput(t *testing.T) {
	file := &dtypes.BlockFile{Block: dtypes.Block{ID: "0x01"}}
	slim, err := encodeOutput(&dtypes.DebankOutPut{BlockFile: file})
	require.NoError(t, err)
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(slim, &keys))
	require.Len(t, keys, 1)
	require.Contains(t, keys, "block_file")

	full := &dtypes.DebankOutPut{BlockFile: file, Header: &dtypes.Header{}, StateDiff: &dtypes.BlockStorageDiff{}, ValidationHash: 7}
	got, err := encodeOutput(full)
	require.NoError(t, err)
	data, err := rlp.EncodeToBytes(full.StateDiff)
	require.NoError(t, err)
	want, err := json.Marshal(&dtypes.DebankOutPutJs{BlockFile: file, Header: full.Header, StateDiff: data, ValidationHash: 7})
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}

type countingExecutor struct {
	calls  int
	result *replay.Result
}

func (e *countingExecutor) CheckReplayHeight(string, int64) error { return nil }
func (e *countingExecutor) BlockFileOnly() (bool, error)          { return false, nil }
func (e *countingExecutor) ReplayBlock(context.Context, replay.Request) (*replay.Result, error) {
	e.calls++
	return e.result, nil
}

func TestReplayOnlyBlocksWithEVMMessages(t *testing.T) {
	appHash := bytes.Repeat([]byte{1}, 32)
	results := &ctypes.ResultBlockResults{AppHash: appHash}
	exec := &countingExecutor{result: &replay.Result{Finalize: &abci.ResponseFinalizeBlock{AppHash: appHash}}}
	api := &API{executor: exec}

	got, err := api.replayIfNeeded(context.Background(), replay.Request{}, results, 0)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Zero(t, exec.calls)

	got, err = api.replayIfNeeded(context.Background(), replay.Request{}, results, 1)
	require.NoError(t, err)
	require.Equal(t, exec.result, got)
	require.Equal(t, 1, exec.calls)

	exec.result.Finalize.AppHash = bytes.Repeat([]byte{2}, 32)
	_, err = api.replayIfNeeded(context.Background(), replay.Request{}, results, 1)
	require.ErrorContains(t, err, "AppHash")
}

func TestEVMMsgsRejectUndecodableIncludedTx(t *testing.T) {
	decoded := 0
	decode := func([]byte) (sdk.Tx, error) {
		decoded++
		return nil, errors.New("bad tx")
	}
	txs := tmtypes.Txs{{1}, {2}}
	failed := []*abci.ExecTxResult{{Code: 5, Log: "failed"}, {Code: 5, Log: "failed"}}
	msgs, err := committedEVMMessages(decode, txs, failed)
	require.NoError(t, err)
	require.Empty(t, msgs)
	require.Zero(t, decoded)

	_, err = committedEVMMessages(decode, txs, []*abci.ExecTxResult{{Code: 5}, {Code: 0}})
	require.ErrorContains(t, err, "decode tx[1]")
}

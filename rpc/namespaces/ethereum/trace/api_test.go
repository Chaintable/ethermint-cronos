package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/evmos/ethermint/rpc/backend"
	rpctypes "github.com/evmos/ethermint/rpc/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rlp"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/stretchr/testify/require"
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
	block := tmtypes.MakeBlock(101485001, nil, nil, nil)
	block.ValidatorsHash = bytes.Repeat([]byte{1}, 32)
	hash := block.Hash()
	other := common.HexToHash("0x22")
	api := API{backend: movedBlockBackend{block: &ctypes.ResultBlock{Block: block, BlockID: tmtypes.BlockID{Hash: hash}}}}
	result, err := api.DebankBlockRaw(context.Background(), rpctypes.BlockNumberOrHash{BlockHash: &other})
	require.Nil(t, result)
	require.ErrorContains(t, err, "block identity mismatch")
	require.ErrorContains(t, err, "101485001")
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

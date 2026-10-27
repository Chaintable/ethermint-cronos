package tracer

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

func TestPipelineUsesUpstreamRPCBlockTypes(t *testing.T) {
	header := &ethtypes.Header{
		Number: big.NewInt(101485000), ParentHash: hash(1), Root: hash(2),
		Difficulty: big.NewInt(0), BaseFee: big.NewInt(1000),
		GasLimit: 60000000, GasUsed: 21000, Time: 1792656000,
	}
	canonicalHash := hash(3)
	raw := rpctypes.FormatBlock(header, canonicalHash.Bytes(), 256, nil)
	block := BuildPipelineBlock(raw)
	out := BuildPilelineBlockHeader(raw)
	require.Equal(t, canonicalHash.Hex(), block.ID)
	require.Equal(t, header.Number, block.Height)
	require.Equal(t, uint64(21000), block.GasUsed.Uint64())
	require.Equal(t, canonicalHash, out.Hash)
	require.Equal(t, header.Root, out.StateRoot)
	require.Equal(t, header.Number, out.Number.ToInt())
	require.Equal(t, uint64(21000), uint64(out.GasUsed))
}

func hash(b byte) common.Hash { return common.BytesToHash([]byte{b}) }

// TestBuildBlockStateDiff covers that all four state fields come from the
// canonical IAVL transition without any tracer-side merge.
func TestBuildBlockStateDiff(t *testing.T) {
	accA, accB := hash(0xA), hash(0xB)
	slot := hash(0x01)
	canonical := dtypes.TransactionStateDiff{
		NewAccounts:     []dtypes.NewAccount{{Address: accA, Nonce: 1, Balance: uint256.NewInt(2)}},
		DeletedAccounts: []common.Hash{accB},
		StorageDiff: []dtypes.AccountStorageDiff{{Address: accA, Values: []dtypes.IndexValuePair{
			{Index: slot, Value: uint256.NewInt(3)},
		}}},
		NewCodes: []dtypes.NewCode{{CodeHash: hash(0xC1), Code: []byte{0x01}}},
	}

	out := BuildBlockStateDiff(hash(0x22), hash(0x33), canonical)
	if out.Hash != hash(0x33) || out.ParentHash != hash(0x22) {
		t.Fatalf("unexpected roots: %#v", out)
	}
	if len(out.NewAccounts) != 1 || out.NewAccounts[0].Address != accA ||
		len(out.DeletedAccounts) != 1 || out.DeletedAccounts[0] != accB ||
		len(out.StorageDiff) != 1 || out.StorageDiff[0].Values[0].Value.Uint64() != 3 ||
		len(out.NewCodes) != 1 || out.NewCodes[0].CodeHash != hash(0xC1) {
		t.Fatalf("canonical state was not preserved: %#v", out)
	}
}

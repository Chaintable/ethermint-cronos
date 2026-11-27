package tracer

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

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

// Fee fields and the creation address follow the eth RPC and the pipeline.
func TestBuildPipelineTransaction(t *testing.T) {
	from := common.HexToAddress("0x1")
	to := common.HexToAddress("0x2")
	baseFee := big.NewInt(100)
	feeCap, tipCap := big.NewInt(500), big.NewInt(30)
	cases := []struct {
		name               string
		tx                 *ethtypes.Transaction
		price, cap, tipcap int64
	}{
		{"legacy", ethtypes.NewTx(&ethtypes.LegacyTx{To: &to, GasPrice: big.NewInt(200)}), 200, 0, 0},
		{"access list", ethtypes.NewTx(&ethtypes.AccessListTx{To: &to, GasPrice: big.NewInt(200)}), 200, 0, 0},
		{"dynamic fee", ethtypes.NewTx(&ethtypes.DynamicFeeTx{To: &to, GasFeeCap: feeCap, GasTipCap: tipCap}), 130, 500, 30},
	}
	for _, c := range cases {
		got := BuildPipelineTransaction(c.tx, 0, from, big.NewInt(21000), baseFee, true)
		require.Equal(t, c.price, got.GasPrice.Int64(), c.name)
		require.Equal(t, c.cap, got.GasFeeCap.Int64(), c.name)
		require.Equal(t, c.tipcap, got.GasTipCap.Int64(), c.name)
		require.Equal(t, strings.ToLower(to.Hex()), got.To, c.name)
	}

	create := ethtypes.NewTx(&ethtypes.LegacyTx{Nonce: 7, GasPrice: big.NewInt(200)})
	got := BuildPipelineTransaction(create, 0, from, big.NewInt(53000), baseFee, false)
	require.Equal(t, strings.ToLower(crypto.CreateAddress(from, 7).Hex()), got.To)
}

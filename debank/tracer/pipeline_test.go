package tracer

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/holiman/uint256"
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

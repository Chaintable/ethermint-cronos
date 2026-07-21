package tracer

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/holiman/uint256"
)

func hash(b byte) common.Hash { return common.BytesToHash([]byte{b}) }

// TestBuildBlockStateDiff covers the block-level merge rules: canonical storage
// is used unchanged and a DeletedAccount is cancelled by a later NewAccount.
func TestBuildBlockStateDiff(t *testing.T) {
	accA, accB := hash(0xA), hash(0xB)
	slot1, slot2 := hash(0x01), hash(0x02)

	diffs := []dtypes.TransactionStateDiff{
		{
			NewAccounts: []dtypes.NewAccount{{Address: accA, Nonce: 1, Balance: uint256.NewInt(0)}},
			StorageDiff: []dtypes.AccountStorageDiff{{Address: accA, Values: []dtypes.IndexValuePair{
				{Index: slot2, Value: uint256.NewInt(99)},
				{Index: slot1, Value: uint256.NewInt(10)},
			}}},
			NewCodes: []dtypes.NewCode{{CodeHash: hash(0xC1), Code: []byte{0x01}}},
		},
		{
			// last-write-wins on slot1; delete B
			StorageDiff:     []dtypes.AccountStorageDiff{{Address: accA, Values: []dtypes.IndexValuePair{{Index: slot1, Value: uint256.NewInt(20)}}}},
			DeletedAccounts: []common.Hash{accB},
		},
		{
			// re-create B -> cancels its deletion
			NewAccounts: []dtypes.NewAccount{{Address: accB, Nonce: 5, Balance: uint256.NewInt(0)}},
		},
	}

	canonicalStorage := []dtypes.AccountStorageDiff{{Address: accB, Values: []dtypes.IndexValuePair{
		{Index: slot2, Value: uint256.NewInt(30)},
	}}}
	out := BuildBlockStateDiff(hash(0x22), hash(0x33), diffs, canonicalStorage)

	// B was deleted then re-created -> not in DeletedAccounts
	if len(out.DeletedAccounts) != 0 {
		t.Errorf("DeletedAccounts should be empty (B re-created), got %v", out.DeletedAccounts)
	}
	// A and B both present
	if len(out.NewAccounts) != 2 {
		t.Fatalf("want 2 new accounts, got %d", len(out.NewAccounts))
	}
	if len(out.StorageDiff) != 1 || out.StorageDiff[0].Address != accB || out.StorageDiff[0].Values[0].Value.Uint64() != 30 {
		t.Fatalf("canonical storage was not preserved: %#v", out.StorageDiff)
	}
}

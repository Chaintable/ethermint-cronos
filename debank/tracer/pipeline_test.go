package tracer

import (
	"testing"

	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

func hash(b byte) common.Hash { return common.BytesToHash([]byte{b}) }

// TestBuildBlockStateDiff covers the block-level merge rules: storage
// last-write-wins, DeletedAccount cancelled by a later NewAccount, and storage
// Values sorted by Index.Hex().
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

	out := BuildBlockStateDiff(hash(0x22), hash(0x33), diffs)

	// B was deleted then re-created -> not in DeletedAccounts
	if len(out.DeletedAccounts) != 0 {
		t.Errorf("DeletedAccounts should be empty (B re-created), got %v", out.DeletedAccounts)
	}
	// A and B both present
	if len(out.NewAccounts) != 2 {
		t.Fatalf("want 2 new accounts, got %d", len(out.NewAccounts))
	}
	// storage A: slot1 last-write-wins = 20, slot2 = 99, sorted by Index.Hex()
	if len(out.StorageDiff) != 1 {
		t.Fatalf("want 1 storage account, got %d", len(out.StorageDiff))
	}
	vals := out.StorageDiff[0].Values
	if len(vals) != 2 {
		t.Fatalf("want 2 slots, got %d", len(vals))
	}
	if vals[0].Index.Hex() >= vals[1].Index.Hex() {
		t.Errorf("storage values not sorted by Index.Hex(): %s then %s", vals[0].Index.Hex(), vals[1].Index.Hex())
	}
	for _, kv := range vals {
		if kv.Index == slot1 && kv.Value.Uint64() != 20 {
			t.Errorf("slot1 last-write-wins failed: got %d want 20", kv.Value.Uint64())
		}
	}
}

package replay

import (
	"fmt"

	dbm "github.com/cometbft/cometbft-db"
	"github.com/cosmos/cosmos-sdk/codec"
	sdkIAVL "github.com/cosmos/cosmos-sdk/store/iavl"
	store "github.com/cosmos/cosmos-sdk/store/types"
	auth "github.com/cosmos/cosmos-sdk/x/auth/types"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/iavl"
	wire "github.com/evmos/ethermint/debank/types"
	evm "github.com/evmos/ethermint/x/evm/types"
)

type archiveStores map[store.StoreKey]store.KVStore

func (m archiveStores) GetKVStore(k store.StoreKey) store.KVStore { return m[k] }

// ArchivedDiff derives changed keys from adjacent archived roots for a block
// without EVM messages. Cosmos transactions and block hooks can still move CRO.
func ArchivedDiff(db dbm.DB, cdc codec.BinaryCodec, height int64, denom string) (wire.TransactionStateDiff, error) {
	writes := NewStateWrites(denom)
	before, after := archiveStores{}, archiveStores{}
	for _, name := range []string{auth.StoreKey, bank.StoreKey, evm.StoreKey} {
		key := store.NewKVStoreKey(name)
		prefix := dbm.NewPrefixDB(db, []byte("s/k:"+name+"/"))
		next, err := sdkIAVL.LoadStore(prefix, nil, key, store.CommitID{Version: height}, true, 0, true)
		if err != nil {
			return wire.TransactionStateDiff{}, err
		}
		prev, err := next.(*sdkIAVL.Store).GetImmutable(height - 1)
		if err != nil {
			return wire.TransactionStateDiff{}, err
		}
		writes.GetStoreCache(key, next)
		before[key], after[key] = prev, next
		tree := iavl.NewImmutableTree(prefix, 0, true)
		found := false
		err = tree.TraverseStateChanges(height, height+1, func(version int64, changes *iavl.ChangeSet) error {
			found = version == height
			for _, pair := range changes.Pairs {
				writes.record(key, pair.Key)
			}
			return nil
		})
		if err != nil {
			return wire.TransactionStateDiff{}, err
		}
		if !found {
			return wire.TransactionStateDiff{}, fmt.Errorf("missing %s root at %d", name, height)
		}
	}
	return writes.Diff(cdc, before, after)
}

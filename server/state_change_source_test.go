package server

import (
	"sync"
	"testing"

	"cosmossdk.io/log/v2"
	dbm "github.com/cosmos/cosmos-db"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	iavlstore "github.com/cosmos/cosmos-sdk/store/v2/iavl"
	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
	"github.com/cosmos/cosmos-sdk/store/v2/wrapper"
	"github.com/cosmos/iavl"
	"github.com/stretchr/testify/require"
)

type sourceTestApp struct {
	servertypes.Application
	cms storetypes.CommitMultiStore
}

func (a sourceTestApp) CommitMultiStore() storetypes.CommitMultiStore { return a.cms }

type unnamedTestCMS struct{ storetypes.CommitMultiStore }

type namedTestCMS struct {
	storetypes.CommitMultiStore
	keys   map[string]storetypes.StoreKey
	stores map[string]storetypes.CommitKVStore
}

func (c namedTestCMS) LatestVersion() int64 { return 2 }
func (c namedTestCMS) StoreKeysByName() map[string]storetypes.StoreKey {
	return c.keys
}
func (c namedTestCMS) GetStore(key storetypes.StoreKey) storetypes.Store {
	return c.stores[key.Name()]
}

type nonIAVLTestStore struct{ storetypes.CommitKVStore }

func sourceTestIAVLStore() *iavlstore.Store {
	tree := iavl.NewMutableTree(wrapper.NewDBWrapper(dbm.NewMemDB()), 0, true, log.NewNopLogger())
	return iavlstore.UnsafeNewStore(tree)
}

func TestResolveStateChangeSourceRejectsUnsupportedStores(t *testing.T) {
	_, err := resolveStateChangeSource(sourceTestApp{cms: unnamedTestCMS{}}, nil, new(sync.Mutex))
	require.ErrorContains(t, err, "named commit multi-store")

	keys := map[string]storetypes.StoreKey{
		"acc": storetypes.NewKVStoreKey("acc"), "bank": storetypes.NewKVStoreKey("bank"), "evm": storetypes.NewKVStoreKey("evm"),
	}
	iavlStore := sourceTestIAVLStore()
	for _, missing := range []string{"acc", "bank", "evm"} {
		candidateKeys := make(map[string]storetypes.StoreKey, len(keys)-1)
		for name, key := range keys {
			if name != missing {
				candidateKeys[name] = key
			}
		}
		_, err = resolveStateChangeSource(sourceTestApp{cms: namedTestCMS{
			keys: candidateKeys,
			stores: map[string]storetypes.CommitKVStore{
				"acc": iavlStore, "bank": iavlStore, "evm": iavlStore,
			},
		}}, nil, new(sync.Mutex))
		require.ErrorContains(t, err, "requires the "+missing+" store")
	}

	stores := map[string]storetypes.CommitKVStore{
		"acc": iavlStore, "bank": iavlStore, "evm": nonIAVLTestStore{},
	}
	evmKey := storetypes.NewKVStoreKey("evm")
	keys["evm"] = evmKey
	_, err = resolveStateChangeSource(sourceTestApp{cms: namedTestCMS{keys: keys, stores: stores}}, nil, new(sync.Mutex))
	require.ErrorContains(t, err, "standard IAVL evm store")
}

func TestNamespaceEnabled(t *testing.T) {
	require.True(t, namespaceEnabled([]string{"eth", "trace"}, "trace"))
	require.False(t, namespaceEnabled([]string{"eth"}, "trace"))
}

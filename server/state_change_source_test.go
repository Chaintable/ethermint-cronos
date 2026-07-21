package server

import (
	"testing"

	storetypes "cosmossdk.io/store/types"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
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
	keys  map[string]storetypes.StoreKey
	store storetypes.CommitKVStore
}

func (c namedTestCMS) LatestVersion() int64 { return 2 }
func (c namedTestCMS) StoreKeysByName() map[string]storetypes.StoreKey {
	return c.keys
}
func (c namedTestCMS) GetCommitKVStore(storetypes.StoreKey) storetypes.CommitKVStore {
	return c.store
}

type nonIAVLTestStore struct{ storetypes.CommitKVStore }

func TestResolveStateChangeSourceRejectsUnsupportedStores(t *testing.T) {
	_, err := resolveStateChangeSource(sourceTestApp{cms: unnamedTestCMS{}})
	require.ErrorContains(t, err, "named commit multi-store")

	_, err = resolveStateChangeSource(sourceTestApp{cms: namedTestCMS{keys: map[string]storetypes.StoreKey{}}})
	require.ErrorContains(t, err, "requires the evm store")

	evmKey := storetypes.NewKVStoreKey("evm")
	_, err = resolveStateChangeSource(sourceTestApp{cms: namedTestCMS{
		keys:  map[string]storetypes.StoreKey{"evm": evmKey},
		store: nonIAVLTestStore{},
	}})
	require.ErrorContains(t, err, "standard IAVL evm store")
}

func TestNamespaceEnabled(t *testing.T) {
	require.True(t, namespaceEnabled([]string{"eth", "trace"}, "trace"))
	require.False(t, namespaceEnabled([]string{"eth"}, "trace"))
}

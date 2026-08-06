package statediff

import (
	"fmt"
	"sync"
	"testing"

	"cosmossdk.io/log"
	iavlstore "cosmossdk.io/store/iavl"
	"cosmossdk.io/store/metrics"
	storetypes "cosmossdk.io/store/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/iavl"
	"github.com/stretchr/testify/require"
)

// newLiveIAVLStore mimics a writer's live commit store: fast node storage on,
// the same defaults resolveStateChangeSource hands to the emitter.
func newLiveIAVLStore(t *testing.T) *iavlstore.Store {
	t.Helper()
	commitStore, err := iavlstore.LoadStore(
		dbm.NewMemDB(),
		log.NewNopLogger(),
		storetypes.NewKVStoreKey("bank"),
		storetypes.CommitID{},
		1000,
		false,
		metrics.NewNoOpMetrics(),
	)
	require.NoError(t, err)
	store, ok := commitStore.(*iavlstore.Store)
	require.True(t, ok)
	return store
}

func balanceRowKey(i int) []byte { return []byte(fmt.Sprintf("balance/%04d", i)) }

// TestIAVLStateChangeSourceSerializesLiveTreeAccess guards the fix for the
// H86749494 app-hash fork. The emitter reads the writer's live IAVL store from
// the JSON-RPC goroutine while block execution mutates the same tree, and
// iavl.MutableTree is not safe for concurrent use: dropping the mutex from
// iavlStore makes this test fail under -race inside nodeDB (storage version,
// node cache) and lets block execution observe stale values.
func TestIAVLStateChangeSourceSerializesLiveTreeAccess(t *testing.T) {
	store := newLiveIAVLStore(t)
	// The mutex cometbft's local client holds across consensus ABCI calls.
	abciMtx := new(sync.Mutex)
	applyBlock := func(version int) {
		abciMtx.Lock()
		defer abciMtx.Unlock()
		for i := 0; i < 64; i++ {
			store.Set(balanceRowKey(i), []byte(fmt.Sprintf("v%d-%d", version, i)))
		}
		store.Commit()
	}
	latestVersion := func() int64 {
		abciMtx.Lock()
		defer abciMtx.Unlock()
		return store.LastCommitID().Version
	}
	for version := 0; version < 16; version++ {
		applyBlock(version)
	}

	reader := iavlStore{store: store, mtx: abciMtx}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for version := 16; ; version++ {
			select {
			case <-stop:
				return
			default:
			}
			applyBlock(version)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for round := 0; round < 200; round++ {
			version := latestVersion() - 1
			if version < 1 || !reader.VersionExists(version) {
				continue
			}
			snapshot, err := reader.Snapshot(version)
			if err != nil {
				continue
			}
			for i := 0; i < 64; i++ {
				snapshot.Get(balanceRowKey(i))
			}
			_ = reader.TraverseStateChanges(version, version, func(int64, *iavl.ChangeSet) error {
				return nil
			})
		}
	}()

	wg.Wait()
}

package archive

import (
	"testing"

	dbm "github.com/cometbft/cometbft-db"
	"github.com/cosmos/iavl"
	"github.com/stretchr/testify/require"
)

func TestStoreGetAtHeight(t *testing.T) {
	app := dbm.NewMemDB()
	tree, err := iavl.NewMutableTree(dbm.NewPrefixDB(app, []byte("s/k:params/")), 0, true)
	require.NoError(t, err)
	tree.Set([]byte("evm/EvmDenom"), []byte(`"basecro"`))
	_, _, err = tree.SaveVersion()
	require.NoError(t, err)
	tree.Set([]byte("evm/EvmDenom"), []byte(`"other"`))
	_, _, err = tree.SaveVersion()
	require.NoError(t, err)

	a := New(dbm.NewMemDB(), dbm.NewMemDB(), app)
	v, err := a.StoreGet(1, "params", []byte("evm/EvmDenom"))
	require.NoError(t, err)
	require.Equal(t, `"basecro"`, string(v))
	v, err = a.StoreGet(2, "params", []byte("evm/EvmDenom"))
	require.NoError(t, err)
	require.Equal(t, `"other"`, string(v))
	_, err = a.StoreGet(3, "params", []byte("evm/EvmDenom"))
	require.Error(t, err)
}

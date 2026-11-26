// Package archive reads blocks, ABCI results, validators and application
// state of past heights directly from a stopped node's databases.
package archive

import (
	"fmt"

	dbm "github.com/cometbft/cometbft-db"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	sm "github.com/cometbft/cometbft/state"
	"github.com/cometbft/cometbft/store"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/iavl"
)

type Archive struct {
	blocks *store.BlockStore
	state  sm.Store
	app    dbm.DB
}

// New wraps the blockstore, state and application databases, which the caller
// opens read-only.
func New(blockstore, state, application dbm.DB) *Archive {
	return &Archive{
		blocks: store.NewBlockStore(blockstore),
		state:  sm.NewStore(state, sm.StoreOptions{DiscardABCIResponses: false}),
		app:    application,
	}
}

// Head is the last block in the blockstore.
func (a *Archive) Head() int64 {
	return a.blocks.Height()
}

func (a *Archive) Block(height int64) (*tmtypes.Block, error) {
	b := a.blocks.LoadBlock(height)
	if b == nil {
		return nil, fmt.Errorf("block %d not found", height)
	}
	return b, nil
}

func (a *Archive) Header(height int64) (*tmtypes.Header, error) {
	meta := a.blocks.LoadBlockMeta(height)
	if meta == nil {
		return nil, fmt.Errorf("block %d not found", height)
	}
	return &meta.Header, nil
}

func (a *Archive) BlockByHash(hash []byte) (*tmtypes.Block, error) {
	b := a.blocks.LoadBlockByHash(hash)
	if b == nil {
		return nil, fmt.Errorf("block %X not found", hash)
	}
	return b, nil
}

func (a *Archive) Results(height int64) (*tmstate.ABCIResponses, error) {
	return a.state.LoadABCIResponses(height)
}

func (a *Archive) Validators(height int64) (*tmtypes.ValidatorSet, error) {
	return a.state.LoadValidators(height)
}

func (a *Archive) ConsensusParams(height int64) (tmproto.ConsensusParams, error) {
	p, err := a.state.LoadConsensusParams(height); return p.ToProto(), err
}

// StoreGet reads key from the named module store as committed at height.
func (a *Archive) StoreGet(height int64, name string, key []byte) ([]byte, error) {
	tree, err := iavl.NewMutableTree(dbm.NewPrefixDB(a.app, []byte("s/k:"+name+"/")), 0, true)
	if err != nil {
		return nil, err
	}
	if _, err = tree.LazyLoadVersion(height); err != nil {
		return nil, fmt.Errorf("load %s store at %d: %w", name, height, err)
	}
	return tree.Get(key)
}

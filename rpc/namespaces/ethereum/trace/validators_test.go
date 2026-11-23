package trace

import (
	"context"
	"testing"

	"github.com/cometbft/cometbft/crypto/ed25519"
	"github.com/cometbft/cometbft/rpc/client"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	cosmosclient "github.com/cosmos/cosmos-sdk/client"
	"github.com/stretchr/testify/require"
)

type validatorClient struct {
	client.Client
	vals  []*tmtypes.Validator
	calls int
}

func (c *validatorClient) Validators(_ context.Context, height *int64, _, _ *int) (*ctypes.ResultValidators, error) {
	c.calls++
	return &ctypes.ResultValidators{BlockHeight: *height, Validators: c.vals, Count: len(c.vals), Total: len(c.vals)}, nil
}

func TestParentValidatorsCache(t *testing.T) {
	newVals := func(n int) []*tmtypes.Validator {
		vals := make([]*tmtypes.Validator, n)
		for i := range vals {
			vals[i] = tmtypes.NewValidator(ed25519.GenPrivKey().PubKey(), 10)
		}
		return vals
	}
	first, second := newVals(2), newVals(2)
	rpc := &validatorClient{vals: first}
	api := &API{clientCtx: cosmosclient.Context{Client: rpc}}
	block := &tmtypes.Block{Header: tmtypes.Header{Height: 101485001}, LastCommit: &tmtypes.Commit{Signatures: make([]tmtypes.CommitSig, 2)}}
	header := &tmtypes.Header{ValidatorsHash: (&tmtypes.ValidatorSet{Validators: first}).Hash()}

	got, err := api.parentValidators(context.Background(), block, header)
	require.NoError(t, err)
	require.Equal(t, first, got)
	require.Equal(t, 1, rpc.calls)

	got, err = api.parentValidators(context.Background(), block, header)
	require.NoError(t, err)
	require.Equal(t, first, got)
	require.Equal(t, 1, rpc.calls, "an unchanged set is served from the cache")

	rpc.vals = second
	changed := &tmtypes.Header{ValidatorsHash: (&tmtypes.ValidatorSet{Validators: second}).Hash()}
	got, err = api.parentValidators(context.Background(), block, changed)
	require.NoError(t, err)
	require.Equal(t, second, got)
	require.Equal(t, 2, rpc.calls)

	_, err = api.parentValidators(context.Background(), block, header)
	require.ErrorContains(t, err, "parent validator hash mismatch")
}

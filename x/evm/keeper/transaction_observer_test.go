package keeper_test

import (
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	"github.com/stretchr/testify/require"
)

// An observed message must leave state as an unobserved one does: the debug
// trace path that charges fees and bumps the nonce stays disabled.
func TestTransactionObserverKeepsState(t *testing.T) {
	suite := StateTransitionBenchmarkTestSuite{enableLondonHF: true}
	suite.SetupTest(&testing.B{})
	k := suite.App.EvmKeeper
	signer := ethtypes.LatestSignerForChainID(k.ChainID())
	tx, err := newSignedEthTx(templateLegacyTx, k.GetNonce(suite.Ctx, suite.Address),
		sdk.AccAddress(suite.Address.Bytes()), suite.Signer, signer)
	require.NoError(t, err)
	denom := k.GetParams(suite.Ctx).EvmDenom

	apply := func() (*evmtypes.MsgEthereumTxResponse, uint64, string) {
		ctx, _ := suite.Ctx.CacheContext()
		res, err := k.ApplyTransaction(ctx, tx)
		require.NoError(t, err)
		return res, k.GetNonce(ctx, suite.Address), k.GetBalance(ctx, sdk.AccAddress(suite.Address.Bytes()), denom).String()
	}
	plain, plainNonce, plainBalance := apply()

	var observed *evmtypes.MsgEthereumTxResponse
	k.SetTransactionObserver(func(_ sdk.Context, hash common.Hash) (vm.EVMLogger, func(*evmtypes.MsgEthereumTxResponse, error)) {
		require.Equal(t, tx.AsTransaction().Hash(), hash)
		return logger.NewStructLogger(nil), func(res *evmtypes.MsgEthereumTxResponse, err error) {
			require.NoError(t, err)
			observed = res
		}
	})
	defer k.SetTransactionObserver(nil)
	traced, tracedNonce, tracedBalance := apply()

	require.Same(t, traced, observed)
	require.Equal(t, plain.GasUsed, traced.GasUsed)
	require.Equal(t, plain.VmError, traced.VmError)
	require.Equal(t, plainNonce, tracedNonce)
	require.Equal(t, plainBalance, tracedBalance)
}

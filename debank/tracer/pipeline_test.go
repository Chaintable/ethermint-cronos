package tracer

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

func TestBuildPipelineTransactionFees(t *testing.T) {
	to := common.HexToAddress("0xcc")
	from := common.HexToAddress("0xaa")
	baseFee := big.NewInt(5000)

	dynamic := ethtypes.NewTx(&ethtypes.DynamicFeeTx{ChainID: big.NewInt(25), To: &to, Gas: 21000, GasFeeCap: big.NewInt(6000), GasTipCap: big.NewInt(300)})
	tx := BuildPipelineTransaction(dynamic, 0, from, big.NewInt(21000), baseFee, true)
	require.Equal(t, int64(6000), tx.GasFeeCap.Int64())
	require.Equal(t, int64(300), tx.GasTipCap.Int64())
	require.Equal(t, int64(5300), tx.GasPrice.Int64(), "base fee + tip below the cap")

	capped := ethtypes.NewTx(&ethtypes.DynamicFeeTx{ChainID: big.NewInt(25), To: &to, Gas: 21000, GasFeeCap: big.NewInt(5100), GasTipCap: big.NewInt(300)})
	require.Equal(t, int64(5100), BuildPipelineTransaction(capped, 0, from, big.NewInt(21000), baseFee, true).GasPrice.Int64())

	legacy := ethtypes.NewTx(&ethtypes.LegacyTx{To: &to, Gas: 21000, GasPrice: big.NewInt(7000)})
	tx = BuildPipelineTransaction(legacy, 0, from, big.NewInt(21000), baseFee, true)
	require.Equal(t, int64(7000), tx.GasPrice.Int64())
	require.Zero(t, tx.GasFeeCap.Sign())
	require.Zero(t, tx.GasTipCap.Sign())
}

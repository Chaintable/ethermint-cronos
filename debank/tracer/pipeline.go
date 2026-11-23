package tracer

import (
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	dtypes "github.com/tharsis/ethermint/debank/types"
)

// BuildPipelineTransaction converts one included EVM transaction. The pinned
// geth only knows legacy and access-list transactions, so both fee caps are 0.
func BuildPipelineTransaction(
	tx *ethtypes.Transaction,
	index int64,
	from common.Address,
	gasUsed *big.Int,
	success bool,
) dtypes.Transaction {
	// A contract creation reports the created address, as the receipt does.
	to := crypto.CreateAddress(from, tx.Nonce())
	if tx.To() != nil {
		to = *tx.To()
	}
	return dtypes.Transaction{
		ID:               tx.Hash().Hex(),
		From:             strings.ToLower(from.Hex()),
		To:               strings.ToLower(to.Hex()),
		Gas:              new(big.Int).SetUint64(tx.Gas()),
		GasUsed:          gasUsed,
		GasPrice:         tx.GasPrice(),
		Status:           success,
		GasFeeCap:        common.Big0,
		GasTipCap:        common.Big0,
		Input:            tx.Data(),
		Nonce:            new(big.Int).SetUint64(tx.Nonce()),
		TransactionIndex: index,
		Value:            (*hexutil.Big)(tx.Value()),
	}
}

func BuildBlockStateDiff(
	parentRoot common.Hash,
	root common.Hash,
	canonical dtypes.TransactionStateDiff,
) dtypes.BlockStorageDiff {
	return dtypes.BlockStorageDiff{
		Hash:            root,
		ParentHash:      parentRoot,
		NewAccounts:     canonical.NewAccounts,
		DeletedAccounts: canonical.DeletedAccounts,
		StorageDiff:     canonical.StorageDiff,
		NewCodes:        canonical.NewCodes,
	}
}

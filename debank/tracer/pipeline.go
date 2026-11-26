package tracer

import (
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	dtypes "github.com/evmos/ethermint/debank/types"
)

func BuildPipelineTransaction(
	tx *ethtypes.Transaction,
	index int64,
	from common.Address,
	gasUsed *big.Int,
	baseFee *big.Int,
	success bool,
) dtypes.Transaction {
	// A contract creation reports the created address, as the receipt does.
	to := crypto.CreateAddress(from, tx.Nonce())
	if tx.To() != nil {
		to = *tx.To()
	}
	transaction := dtypes.Transaction{
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
	if tx.Type() == ethtypes.DynamicFeeTxType {
		transaction.GasFeeCap = tx.GasFeeCap()
		transaction.GasTipCap = tx.GasTipCap()
		if baseFee != nil {
			transaction.GasPrice = math.BigMin(new(big.Int).Add(tx.GasTipCap(), baseFee), tx.GasFeeCap())
		}
	}
	return transaction
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
func BuildPilelineBlockHeader(header map[string]interface{}) *dtypes.Header {
	blockHeader := dtypes.Header{
		Number:           (*hexutil.Big)(new(big.Int).SetUint64(uint64(header["number"].(hexutil.Uint64)))),
		Hash:             common.BytesToHash(header["hash"].(hexutil.Bytes)),
		ParentHash:       header["parentHash"].(common.Hash),
		Nonce:            header["nonce"].(ethtypes.BlockNonce),
		MixHash:          header["mixHash"].(common.Hash),
		Sha3Uncles:       header["sha3Uncles"].(common.Hash),
		LogsBloom:        header["logsBloom"].(ethtypes.Bloom),
		StateRoot:        common.BytesToHash(header["stateRoot"].(hexutil.Bytes)),
		Miner:            header["miner"].(common.Address),
		Difficulty:       header["difficulty"].(*hexutil.Big),
		ExtraData:        hexutil.Bytes{},
		GasLimit:         header["gasLimit"].(hexutil.Uint64),
		GasUsed:          hexutil.Uint64((*big.Int)(header["gasUsed"].(*hexutil.Big)).Uint64()),
		Timestamp:        header["timestamp"].(hexutil.Uint64),
		TransactionsRoot: header["transactionsRoot"].(common.Hash),
		ReceiptsRoot:     header["receiptsRoot"].(common.Hash),
	}
	if baseFeePerGas, ok := header["baseFeePerGas"]; ok {
		blockHeader.BaseFeePerGas = baseFeePerGas.(*hexutil.Big)
	}
	return &blockHeader
}

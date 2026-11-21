package tracer

import (
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	dtypes "github.com/evmos/ethermint/debank/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

func BuildPipelineBlock(rawBlock map[string]interface{}) dtypes.Block {
	block := dtypes.Block{
		ID:                    rawBlock["hash"].(hexutil.Bytes).String(),
		Height:                new(big.Int).SetUint64(uint64(rawBlock["number"].(hexutil.Uint64))),
		ParentID:              rawBlock["parentHash"].(common.Hash).Hex(),
		BaseFeePerGas:         big.NewInt(0),
		Miner:                 strings.ToLower(rawBlock["miner"].(common.Address).Hex()),
		GasLimit:              new(big.Int).SetUint64(uint64(rawBlock["gasLimit"].(hexutil.Uint64))),
		GasUsed:               (*big.Int)(rawBlock["gasUsed"].(*hexutil.Big)),
		Timestamp:             uint64(rawBlock["timestamp"].(hexutil.Uint64)),
		ProcessStartTimestamp: time.Now().UnixMilli(),
	}
	if baseFeePerGas, ok := rawBlock["baseFeePerGas"]; ok {
		block.BaseFeePerGas = (*big.Int)(baseFeePerGas.(*hexutil.Big))
	}
	return block
}

func BuildPipelineTransaction(
	tx *ethtypes.Transaction,
	index int64,
	from common.Address,
	gasUsed *big.Int,
	baseFee *big.Int,
	success bool,
) dtypes.Transaction {
	to := common.Address{}
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
		// if the transaction has been mined, compute the effective gas price
		if baseFee != nil {
			price := evmtypes.EffectiveGasPrice(baseFee, tx.GasFeeCap(), tx.GasTipCap())
			transaction.GasPrice = price
		}
	}
	return transaction
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

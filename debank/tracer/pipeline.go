package tracer

import (
	"math/big"
	"sort"
	"strings"
	"time"

	dtypes "github.com/evmos/ethermint/debank/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

func BuildPipelineBlock(rawBlock map[string]interface{}) dtypes.Block {
	block := dtypes.Block{
		ID:                    rawBlock["hash"].(hexutil.Bytes).String(),
		Height:                big.NewInt(int64(rawBlock["number"].(hexutil.Uint64))),
		ParentID:              rawBlock["parentHash"].(common.Hash).Hex(),
		BaseFeePerGas:         big.NewInt(0),
		Miner:                 strings.ToLower(rawBlock["miner"].(common.Address).Hex()),
		GasLimit:              big.NewInt(int64(rawBlock["gasLimit"].(hexutil.Uint64))),
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
	var to = common.Address{}
	if tx.To() != nil {
		to = *tx.To()
	}
	transaction := dtypes.Transaction{
		ID:               tx.Hash().Hex(),
		From:             strings.ToLower(from.Hex()),
		To:               strings.ToLower(to.Hex()),
		Gas:              big.NewInt(int64(tx.Gas())),
		GasUsed:          gasUsed,
		GasPrice:         tx.GasPrice(),
		Status:           success,
		GasFeeCap:        common.Big0,
		GasTipCap:        common.Big0,
		Input:            tx.Data(),
		Nonce:            big.NewInt(int64(tx.Nonce())),
		TransactionIndex: index,
		Value:            (*hexutil.Big)(tx.Value()),
	}
	switch tx.Type() {
	case ethtypes.DynamicFeeTxType:
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
		Number:           (*hexutil.Big)(big.NewInt(int64(header["number"].(hexutil.Uint64)))),
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

func BuildBlockStateDiff(parentRoot common.Hash, root common.Hash, diffs []dtypes.TransactionStateDiff) dtypes.BlockStorageDiff {
	storageDiff := dtypes.BlockStorageDiff{
		Hash:            root,
		ParentHash:      parentRoot,
		NewAccounts:     make([]dtypes.NewAccount, 0),
		NewCodes:        make([]dtypes.NewCode, 0),
		DeletedAccounts: make([]common.Hash, 0),
		StorageDiff:     make([]dtypes.AccountStorageDiff, 0),
	}
	newAccountMap := make(map[common.Hash]dtypes.NewAccount)
	deleteAccountMap := make(map[common.Hash]struct{})
	codeMap := make(map[common.Hash]dtypes.NewCode)

	mergedStorage := make(map[common.Hash]map[common.Hash]*uint256.Int)

	for _, diff := range diffs {
		for _, deletedAccount := range diff.DeletedAccounts {
			delete(newAccountMap, deletedAccount)
			delete(mergedStorage, deletedAccount)
			deleteAccountMap[deletedAccount] = struct{}{}
		}

		for _, newCode := range diff.NewCodes {
			codeMap[newCode.CodeHash] = newCode
		}
		for _, newAccount := range diff.NewAccounts {
			newAccountMap[newAccount.Address] = newAccount
			delete(deleteAccountMap, newAccount.Address)
		}
		for _, accountStorageDiff := range diff.StorageDiff {
			addr := accountStorageDiff.Address
			if mergedStorage[addr] == nil {
				mergedStorage[addr] = make(map[common.Hash]*uint256.Int)
			}
			for _, kv := range accountStorageDiff.Values {
				mergedStorage[addr][kv.Index] = kv.Value
			}
		}
	}

	for deleteAccount := range deleteAccountMap {
		storageDiff.DeletedAccounts = append(storageDiff.DeletedAccounts, deleteAccount)
	}
	for _, account := range newAccountMap {
		storageDiff.NewAccounts = append(storageDiff.NewAccounts, account)
	}
	for _, code := range codeMap {
		storageDiff.NewCodes = append(storageDiff.NewCodes, code)
	}

	for addr, slots := range mergedStorage {
		accountDiff := dtypes.AccountStorageDiff{
			Address: addr,
			Values:  make([]dtypes.IndexValuePair, 0, len(slots)),
		}
		for index, value := range slots {
			accountDiff.Values = append(accountDiff.Values, dtypes.IndexValuePair{
				Index: index,
				Value: value,
			})
		}

		sort.Slice(accountDiff.Values, func(i, j int) bool {
			return accountDiff.Values[i].Index.Hex() < accountDiff.Values[j].Index.Hex()
		})

		storageDiff.StorageDiff = append(storageDiff.StorageDiff, accountDiff)
	}

	return storageDiff
}

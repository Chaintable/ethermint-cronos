package types

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

type NewAccount struct {
	Address  common.Hash
	Balance  *uint256.Int
	Nonce    uint64
	CodeHash common.Hash
}

type NewCode struct {
	CodeHash common.Hash
	Code     []byte
}

type IndexValuePair struct {
	Index common.Hash
	Value *uint256.Int
}

type AccountStorageDiff struct {
	Address common.Hash
	Values  []IndexValuePair
}

type TransactionStateDiff struct {
	NewAccounts     []NewAccount
	DeletedAccounts []common.Hash
	StorageDiff     []AccountStorageDiff
	NewCodes        []NewCode
}

type BlockStorageDiff struct {
	Hash            common.Hash
	ParentHash      common.Hash
	NewAccounts     []NewAccount
	DeletedAccounts []common.Hash
	StorageDiff     []AccountStorageDiff
	NewCodes        []NewCode
}

type Header struct {
	Number                *hexutil.Big     `json:"number"`
	Hash                  common.Hash      `json:"hash"`
	ParentHash            common.Hash      `json:"parentHash"`
	Nonce                 types.BlockNonce `json:"nonce"`
	MixHash               common.Hash      `json:"mixHash"`
	Sha3Uncles            common.Hash      `json:"sha3Uncles"`
	LogsBloom             types.Bloom      `json:"logsBloom"`
	StateRoot             common.Hash      `json:"stateRoot"`
	Miner                 common.Address   `json:"miner"`
	Difficulty            *hexutil.Big     `json:"difficulty"`
	ExtraData             hexutil.Bytes    `json:"extraData"`
	GasLimit              hexutil.Uint64   `json:"gasLimit"`
	GasUsed               hexutil.Uint64   `json:"gasUsed"`
	Timestamp             hexutil.Uint64   `json:"timestamp"`
	TransactionsRoot      common.Hash      `json:"transactionsRoot"`
	ReceiptsRoot          common.Hash      `json:"receiptsRoot"`
	BaseFeePerGas         *hexutil.Big     `json:"baseFeePerGas,omitempty"`
	WithdrawalsRoot       *common.Hash     `json:"withdrawalsRoot,omitempty"`
	BlobGasUsed           *hexutil.Uint64  `json:"blobGasUsed,omitempty"`
	ExcessBlobGas         *hexutil.Uint64  `json:"excessBlobGas,omitempty"`
	ParentBeaconBlockRoot *common.Hash     `json:"parentBeaconBlockRoot,omitempty"`
	RequestsRoot          *common.Hash     `json:"requestsRoot,omitempty"`
}

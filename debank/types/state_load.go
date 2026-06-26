package types

import "github.com/ethereum/go-ethereum/common"

type BlockLoad struct {
	Hash         common.Hash
	AccountLoads []common.Address
	StorageLoads []AccountStorageLoad
}

type AccountStorageLoad struct {
	Address common.Address
	Keys    []common.Hash
}

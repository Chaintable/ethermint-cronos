package trace

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	dtracer "github.com/evmos/ethermint/debank/tracer"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
)

// cronosGenesisAccounts are the funded accounts of the Cronos mainnet genesis
// (genesis.json sha256 58f17545056267f57a2d95f4c9c00ac1d689a580e220c5d4de96570fbbc832e1).
// The genesis carries NO EVM allocation (app_state.evm.accounts is empty) and no
// basecro balances, so block 1's EVM state is just these accounts: the gentx
// signers carry nonce 1, the rest are empty. Derived from app_state.auth.accounts
// (eth_secp256k1 -> 20-byte address); fixed for the immutable mainnet genesis.
var cronosGenesisAccounts = []common.Address{
	common.HexToAddress("0x0780adef7832a7f7682b757a5ec5bd9fe7c38b4b"),
	common.HexToAddress("0x81e3e543647e466a5abc824f5844ab0a091b6c6c"),
	common.HexToAddress("0xca5cf03d081197be24ef707081fbd7f3f11eb02d"),
	common.HexToAddress("0x4f87a3f99bd1e58d01de1c38b7f83cb967e816c2"),
	common.HexToAddress("0xef4d07d0e1b40603e0d1b3e633334f0aba5c7a60"),
	common.HexToAddress("0xf428fe419f1d0b1aac6a49a1980ce5b556e5ed54"),
	common.HexToAddress("0x5f61bc1a230051fdc3a96afcc27e706db1124be2"),
	common.HexToAddress("0xf6d4fecb1a6fb7c2ca350169a050d483bd87b883"),
}

// onGenesisBlock builds the block-1 (genesis) DebankOutPut. Genesis state is not
// produced by replaying a block, so it is reconstructed from the genesis account
// set's authoritative block-1 state (balance/nonce/code queried at height 1).
func (api *API) onGenesisBlock(block map[string]interface{}) (*dtypes.DebankOutPut, error) {
	header := dtracer.BuildPilelineBlockHeader(block)
	number := rpctypes.BlockNumber(1)
	diff := &dtypes.BlockStorageDiff{
		Hash:            header.StateRoot,
		ParentHash:      ethtypes.EmptyRootHash,
		NewAccounts:     make([]dtypes.NewAccount, 0, len(cronosGenesisAccounts)),
		DeletedAccounts: make([]common.Hash, 0),
		StorageDiff:     make([]dtypes.AccountStorageDiff, 0),
		NewCodes:        make([]dtypes.NewCode, 0),
	}
	for _, addr := range cronosGenesisAccounts {
		balance, err := api.backend.GetBalance(addr, rpctypes.BlockNumberOrHash{BlockNumber: &number})
		if err != nil {
			return nil, err
		}
		nonce, err := api.backend.GetTransactionCount(addr, number)
		if err != nil {
			return nil, err
		}
		code, err := api.backend.GetCode(addr, rpctypes.BlockNumberOrHash{BlockNumber: &number})
		if err != nil {
			return nil, err
		}
		diff.NewAccounts = append(diff.NewAccounts, dtypes.NewAccount{
			Address:  crypto.Keccak256Hash(addr.Bytes()),
			Balance:  uint256.MustFromBig((*big.Int)(balance)),
			Nonce:    uint64(*nonce),
			CodeHash: crypto.Keccak256Hash(code),
		})
		if len(code) > 0 {
			diff.NewCodes = append(diff.NewCodes, dtypes.NewCode{
				CodeHash: crypto.Keccak256Hash(code),
				Code:     code,
			})
		}
	}

	blockFile := &dtypes.BlockFile{
		Block:            dtracer.BuildPipelineBlock(block),
		Txs:              make([]dtypes.Transaction, 0),
		Events:           make([]dtypes.Event, 0),
		Traces:           make([]dtypes.Trace, 0),
		ErrorEvents:      make([]dtypes.Event, 0),
		ErrorTraces:      make([]dtypes.Trace, 0),
		StorageContracts: make([]string, 0),
	}
	return &dtypes.DebankOutPut{
		BlockFile:      blockFile,
		Header:         header,
		StateDiff:      diff,
		ValidationHash: blockFile.Validation().ValidationHash,
	}, nil
}

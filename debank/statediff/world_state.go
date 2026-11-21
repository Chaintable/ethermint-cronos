package statediff

import (
	"bytes"
	"fmt"
	"sort"

	"cosmossdk.io/collections"
	collcodec "cosmossdk.io/collections/codec"
	sdkcodec "github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/iavl"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	etherminttypes "github.com/evmos/ethermint/types"
	"github.com/holiman/uint256"

	"github.com/evmos/ethermint/debank/types"
)

const (
	authAccountPrefix = byte(0x01)
	bankBalancePrefix = byte(0x02)
	evmCodePrefix     = byte(0x01)
)

var emptyCodeHash = crypto.Keccak256Hash(nil)

type stateReader interface {
	Get([]byte) []byte
}

type authState struct {
	nonce    uint64
	codeHash common.Hash
}

type accountMutation struct {
	address common.Address
	delete  bool
	state   authState
}

type balanceMutation struct {
	address common.Address
	delete  bool
	balance [32]byte
}

type codeMutation struct {
	hash   common.Hash
	delete bool
	code   []byte
}

type worldStateDelta struct {
	accounts []accountMutation
	balances []balanceMutation
	codes    []codeMutation
}

type worldStateDecoder struct {
	accountValues collcodec.ValueCodec[sdk.AccountI]
	balanceKeys   collcodec.KeyCodec[collections.Pair[sdk.AccAddress, string]]
	evmDenom      string
}

func newWorldStateDecoder(codec sdkcodec.Codec, evmDenom string) (*worldStateDecoder, error) {
	if codec == nil {
		return nil, fmt.Errorf("account codec is required")
	}
	if evmDenom == "" {
		return nil, fmt.Errorf("evm denom is required")
	}
	return &worldStateDecoder{
		accountValues: sdkcodec.CollInterfaceValue[sdk.AccountI](codec),
		balanceKeys:   collections.PairKeyCodec(sdk.AccAddressKey, collections.StringKey),
		evmDenom:      evmDenom,
	}, nil
}

func (decoder *worldStateDecoder) decodeAccountValue(address common.Address, value []byte) (authState, error) {
	if len(value) == 0 {
		return authState{}, fmt.Errorf("account %s has an empty value", address)
	}
	account, err := decoder.accountValues.Decode(value)
	if err != nil {
		return authState{}, fmt.Errorf("decode account %s: %w", address, err)
	}
	if account == nil {
		return authState{}, fmt.Errorf("account %s decoded to nil", address)
	}
	codeHash := emptyCodeHash
	if ethAccount, ok := account.(etherminttypes.EthAccountI); ok {
		codeHash = ethAccount.GetCodeHash()
		if codeHash == (common.Hash{}) {
			codeHash = emptyCodeHash
		}
	}
	return authState{nonce: account.GetSequence(), codeHash: codeHash}, nil
}

func (decoder *worldStateDecoder) decodeAccounts(changeSet *iavl.ChangeSet) ([]accountMutation, error) {
	if changeSet == nil {
		return nil, fmt.Errorf("account changeset is required")
	}
	result := make([]accountMutation, 0, len(changeSet.Pairs))
	seen := make(map[common.Address]struct{}, len(changeSet.Pairs))
	for index, pair := range changeSet.Pairs {
		if pair == nil {
			return nil, fmt.Errorf("nil account pair %d", index)
		}
		if len(pair.Key) == 0 || pair.Key[0] != authAccountPrefix {
			continue
		}
		if len(pair.Key) != 1+common.AddressLength {
			continue
		}
		address := common.BytesToAddress(pair.Key[1:])
		if _, found := seen[address]; found {
			return nil, fmt.Errorf("account pair %d duplicates address %s", index, address)
		}
		seen[address] = struct{}{}
		mutation := accountMutation{address: address, delete: pair.Delete}
		if pair.Delete {
			if len(pair.Value) != 0 {
				return nil, fmt.Errorf("deleted account %s has a value", address)
			}
		} else {
			state, err := decoder.decodeAccountValue(address, pair.Value)
			if err != nil {
				return nil, err
			}
			mutation.state = state
		}
		result = append(result, mutation)
	}
	return result, nil
}

func (decoder *worldStateDecoder) decodeBalanceValue(address common.Address, value []byte) ([32]byte, error) {
	var result [32]byte
	if len(value) == 0 {
		return result, fmt.Errorf("balance %s has an empty value", address)
	}
	amount, err := banktypes.BalanceValueCodec.Decode(value)
	if err != nil {
		return result, fmt.Errorf("decode balance %s: %w", address, err)
	}
	if amount.IsNegative() || amount.BigInt().BitLen() > 256 {
		return result, fmt.Errorf("balance %s is outside uint256", address)
	}
	amount.BigInt().FillBytes(result[:])
	return result, nil
}

func (decoder *worldStateDecoder) decodeBalances(changeSet *iavl.ChangeSet) ([]balanceMutation, error) {
	if changeSet == nil {
		return nil, fmt.Errorf("balance changeset is required")
	}
	result := make([]balanceMutation, 0, len(changeSet.Pairs))
	seen := make(map[common.Address]struct{}, len(changeSet.Pairs))
	for index, pair := range changeSet.Pairs {
		if pair == nil {
			return nil, fmt.Errorf("nil balance pair %d", index)
		}
		if len(pair.Key) == 0 || pair.Key[0] != bankBalancePrefix {
			continue
		}
		read, key, err := decoder.balanceKeys.Decode(pair.Key[1:])
		if err != nil {
			return nil, fmt.Errorf("decode balance pair %d key: %w", index, err)
		}
		if read != len(pair.Key)-1 {
			return nil, fmt.Errorf("balance pair %d key has %d trailing bytes", index, len(pair.Key)-1-read)
		}
		if key.K2() != decoder.evmDenom || len(key.K1()) != common.AddressLength {
			continue
		}
		address := common.BytesToAddress(key.K1())
		if _, found := seen[address]; found {
			return nil, fmt.Errorf("balance pair %d duplicates address %s", index, address)
		}
		seen[address] = struct{}{}
		mutation := balanceMutation{address: address, delete: pair.Delete}
		if pair.Delete {
			if len(pair.Value) != 0 {
				return nil, fmt.Errorf("deleted balance %s has a value", address)
			}
		} else {
			balance, err := decoder.decodeBalanceValue(address, pair.Value)
			if err != nil {
				return nil, err
			}
			mutation.balance = balance
		}
		result = append(result, mutation)
	}
	return result, nil
}

func decodeCodes(changeSet *iavl.ChangeSet) ([]codeMutation, error) {
	if changeSet == nil {
		return nil, fmt.Errorf("code changeset is required")
	}
	result := make([]codeMutation, 0, len(changeSet.Pairs))
	seen := make(map[common.Hash]struct{}, len(changeSet.Pairs))
	for index, pair := range changeSet.Pairs {
		if pair == nil {
			return nil, fmt.Errorf("nil code pair %d", index)
		}
		if len(pair.Key) == 0 || pair.Key[0] != evmCodePrefix {
			continue
		}
		if len(pair.Key) != 1+common.HashLength {
			return nil, fmt.Errorf("code pair %d has invalid key length %d", index, len(pair.Key))
		}
		codeHash := common.BytesToHash(pair.Key[1:])
		if _, found := seen[codeHash]; found {
			return nil, fmt.Errorf("code pair %d duplicates hash %s", index, codeHash)
		}
		seen[codeHash] = struct{}{}
		mutation := codeMutation{hash: codeHash, delete: pair.Delete}
		if pair.Delete {
			if len(pair.Value) != 0 {
				return nil, fmt.Errorf("deleted code %s has a value", codeHash)
			}
		} else {
			if len(pair.Value) == 0 {
				return nil, fmt.Errorf("code %s has an empty body", codeHash)
			}
			if actual := crypto.Keccak256Hash(pair.Value); actual != codeHash {
				return nil, fmt.Errorf("code key %s contains body hash %s", codeHash, actual)
			}
			mutation.code = append([]byte(nil), pair.Value...)
		}
		result = append(result, mutation)
	}
	return result, nil
}

func (decoder *worldStateDecoder) accountKey(address common.Address) []byte {
	return append([]byte{authAccountPrefix}, address.Bytes()...)
}

func (decoder *worldStateDecoder) balanceKey(address common.Address) ([]byte, error) {
	key := collections.Join(sdk.AccAddress(address.Bytes()), decoder.evmDenom)
	body := make([]byte, decoder.balanceKeys.Size(key))
	written, err := decoder.balanceKeys.Encode(body, key)
	if err != nil {
		return nil, err
	}
	return append([]byte{bankBalancePrefix}, body[:written]...), nil
}

type projectedAccount struct {
	present  bool
	balance  [32]byte
	nonce    uint64
	codeHash common.Hash
}

type worldStateProjection struct {
	accounts map[common.Address]authState
	balances map[common.Address][32]byte
}

func newWorldStateProjection() *worldStateProjection {
	return &worldStateProjection{
		accounts: make(map[common.Address]authState),
		balances: make(map[common.Address][32]byte),
	}
}

func (projection *worldStateProjection) account(address common.Address) projectedAccount {
	auth, hasAuth := projection.accounts[address]
	balance, hasBalance := projection.balances[address]
	result := projectedAccount{
		present:  hasAuth || hasBalance,
		balance:  balance,
		codeHash: emptyCodeHash,
	}
	if hasAuth {
		result.nonce = auth.nonce
		result.codeHash = auth.codeHash
	}
	return result
}

func sameAccountValues(left, right projectedAccount) bool {
	return left.balance == right.balance &&
		left.nonce == right.nonce &&
		left.codeHash == right.codeHash
}

func (projection *worldStateProjection) apply(delta worldStateDelta) types.TransactionStateDiff {
	before := make(map[common.Address]projectedAccount, len(delta.accounts)+len(delta.balances))
	for _, mutation := range delta.accounts {
		before[mutation.address] = projection.account(mutation.address)
	}
	for _, mutation := range delta.balances {
		if _, found := before[mutation.address]; !found {
			before[mutation.address] = projection.account(mutation.address)
		}
	}
	for _, mutation := range delta.accounts {
		if mutation.delete {
			delete(projection.accounts, mutation.address)
		} else {
			projection.accounts[mutation.address] = mutation.state
		}
	}
	for _, mutation := range delta.balances {
		if mutation.delete || mutation.balance == ([32]byte{}) {
			delete(projection.balances, mutation.address)
		} else {
			projection.balances[mutation.address] = mutation.balance
		}
	}

	result := types.TransactionStateDiff{
		NewAccounts:     make([]types.NewAccount, 0, len(before)),
		DeletedAccounts: make([]common.Hash, 0, len(before)),
		NewCodes:        make([]types.NewCode, 0, len(delta.codes)),
	}
	for address, previous := range before {
		after := projection.account(address)
		if sameAccountValues(previous, after) {
			continue
		}
		wireAddress := crypto.Keccak256Hash(address.Bytes())
		if !after.present {
			result.DeletedAccounts = append(result.DeletedAccounts, wireAddress)
			continue
		}
		balance := new(uint256.Int)
		balance.SetBytes(after.balance[:])
		result.NewAccounts = append(result.NewAccounts, types.NewAccount{
			Address:  wireAddress,
			Balance:  balance,
			Nonce:    after.nonce,
			CodeHash: after.codeHash,
		})
	}
	for _, mutation := range delta.codes {
		if !mutation.delete {
			result.NewCodes = append(result.NewCodes, types.NewCode{
				CodeHash: mutation.hash,
				Code:     append([]byte(nil), mutation.code...),
			})
		}
	}
	sort.Slice(result.NewAccounts, func(i, j int) bool {
		return bytes.Compare(result.NewAccounts[i].Address[:], result.NewAccounts[j].Address[:]) < 0
	})
	sort.Slice(result.DeletedAccounts, func(i, j int) bool {
		return bytes.Compare(result.DeletedAccounts[i][:], result.DeletedAccounts[j][:]) < 0
	})
	sort.Slice(result.NewCodes, func(i, j int) bool {
		return bytes.Compare(result.NewCodes[i].CodeHash[:], result.NewCodes[j].CodeHash[:]) < 0
	})
	return result
}

func (source *iavlStateChangeSource) StateDiff(version int64, evmDenom string) (types.TransactionStateDiff, error) {
	decoder, err := newWorldStateDecoder(source.codec, evmDenom)
	if err != nil {
		return types.TransactionStateDiff{}, err
	}
	accountState, err := source.accounts.Snapshot(version - 1)
	if err != nil {
		return types.TransactionStateDiff{}, fmt.Errorf("load account version %d: %w", version-1, err)
	}
	balanceState, err := source.balances.Snapshot(version - 1)
	if err != nil {
		return types.TransactionStateDiff{}, fmt.Errorf("load balance version %d: %w", version-1, err)
	}
	if _, err := source.evm.Snapshot(version - 1); err != nil {
		return types.TransactionStateDiff{}, fmt.Errorf("load EVM version %d: %w", version-1, err)
	}
	accountChanges, err := changeSet(source.accounts, version)
	if err != nil {
		return types.TransactionStateDiff{}, fmt.Errorf("account changeset: %w", err)
	}
	balanceChanges, err := changeSet(source.balances, version)
	if err != nil {
		return types.TransactionStateDiff{}, fmt.Errorf("balance changeset: %w", err)
	}
	evmChanges, err := changeSet(source.evm, version)
	if err != nil {
		return types.TransactionStateDiff{}, fmt.Errorf("evm changeset: %w", err)
	}
	accounts, err := decoder.decodeAccounts(accountChanges)
	if err != nil {
		return types.TransactionStateDiff{}, err
	}
	balances, err := decoder.decodeBalances(balanceChanges)
	if err != nil {
		return types.TransactionStateDiff{}, err
	}
	codes, err := decodeCodes(evmChanges)
	if err != nil {
		return types.TransactionStateDiff{}, err
	}
	storage, err := CanonicalStorageDiff(evmChanges)
	if err != nil {
		return types.TransactionStateDiff{}, err
	}

	addresses := make(map[common.Address]struct{}, len(accounts)+len(balances))
	for _, mutation := range accounts {
		addresses[mutation.address] = struct{}{}
	}
	for _, mutation := range balances {
		addresses[mutation.address] = struct{}{}
	}
	projection := newWorldStateProjection()
	for address := range addresses {
		if value := accountState.Get(decoder.accountKey(address)); value != nil {
			state, err := decoder.decodeAccountValue(address, value)
			if err != nil {
				return types.TransactionStateDiff{}, err
			}
			projection.accounts[address] = state
		}
		key, err := decoder.balanceKey(address)
		if err != nil {
			return types.TransactionStateDiff{}, fmt.Errorf("encode balance key %s: %w", address, err)
		}
		if value := balanceState.Get(key); value != nil {
			balance, err := decoder.decodeBalanceValue(address, value)
			if err != nil {
				return types.TransactionStateDiff{}, err
			}
			if balance != ([32]byte{}) {
				projection.balances[address] = balance
			}
		}
	}
	result := projection.apply(worldStateDelta{accounts: accounts, balances: balances, codes: codes})
	result.StorageDiff = storage
	for _, named := range []struct {
		name  string
		store iavlChangeStore
	}{
		{"account", source.accounts},
		{"balance", source.balances},
		{"EVM", source.evm},
	} {
		for _, retained := range []int64{version - 1, version} {
			if _, err := named.store.Snapshot(retained); err != nil {
				return types.TransactionStateDiff{}, fmt.Errorf("reload %s version %d: %w", named.name, retained, err)
			}
		}
	}
	return result, nil
}

package replay

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/store/cachekv"
	store "github.com/cosmos/cosmos-sdk/store/types"

	auth "github.com/cosmos/cosmos-sdk/x/auth/types"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	wire "github.com/evmos/ethermint/debank/types"
	evm "github.com/evmos/ethermint/x/evm/types"
	"github.com/holiman/uint256"
)

// StateWrites collects candidate keys only. Failed Cosmos transactions can
// touch a key in a cache that never commits; both values come from committed trees.
type StateWrites struct {
	stores              map[store.StoreKey]store.CommitKVStore
	accounts, bank, evm store.StoreKey
	denom               string
	addresses           map[common.Address]struct{}
	keys                map[string]struct{}
}

func NewStateWrites(denom string) *StateWrites {
	w := &StateWrites{denom: denom}
	w.Reset()
	return w
}
func (w *StateWrites) Reset() {
	w.stores = make(map[store.StoreKey]store.CommitKVStore)
	w.NextBlock()
}
func (w *StateWrites) Unwrap(k store.StoreKey) store.CommitKVStore { return w.stores[k] }

// Observe writes exactly where the consensus cache flushes to its IAVL tree.
func (w *StateWrites) GetStoreCache(k store.StoreKey, parent store.CommitKVStore) store.CommitKVStore {
	switch k.Name() {
	case auth.StoreKey:
		w.accounts = k
	case bank.StoreKey:
		w.bank = k
	case evm.StoreKey:
		w.evm = k
	default:
		return parent
	}
	w.stores[k] = parent
	return &observedStore{CommitKVStore: parent, key: k, writes: w}
}

type observedStore struct {
	store.CommitKVStore
	key    store.StoreKey
	writes *StateWrites
}

func (s *observedStore) CacheWrap() store.CacheWrap { return cachekv.NewStore(s) }
func (s *observedStore) Set(k, v []byte) {
	s.CommitKVStore.Set(k, v)
	s.writes.record(s.key, k)
}
func (s *observedStore) Delete(k []byte) {
	s.CommitKVStore.Delete(k)
	s.writes.record(s.key, k)
}
func (w *StateWrites) NextBlock() {
	w.addresses = make(map[common.Address]struct{})
	w.keys = make(map[string]struct{})
}
func (w *StateWrites) record(s store.StoreKey, key []byte) {
	switch s {
	case w.accounts:
		if len(key) == 1+common.AddressLength && bytes.HasPrefix(key, auth.AddressStoreKeyPrefix) {
			w.addresses[common.BytesToAddress(key[1:])] = struct{}{}
		}
	case w.bank:
		if bytes.HasPrefix(key, bank.BalancesPrefix) {
			addr, denom, err := bank.AddressAndDenomFromBalancesStore(key[len(bank.BalancesPrefix):])
			if err == nil && len(addr) == common.AddressLength && denom == w.denom {
				w.addresses[common.BytesToAddress(addr)] = struct{}{}
			}
		}
	case w.evm:
		if bytes.HasPrefix(key, evm.KeyPrefixCode) || bytes.HasPrefix(key, evm.KeyPrefixStorage) {
			w.keys[string(key)] = struct{}{}
		}
	}
}

type visibleAccount struct {
	nonce   uint64
	balance uint256.Int
	code    common.Hash
}

func (w *StateWrites) account(ms store.MultiStore, cdc codec.BinaryCodec, addr common.Address) (visibleAccount, bool, error) {
	a := visibleAccount{code: crypto.Keccak256Hash(nil)}
	if ms == nil {
		return a, false, nil
	}
	raw := ms.GetKVStore(w.accounts).Get(auth.AddressStoreKey(addr.Bytes()))
	if raw != nil {
		var acc auth.AccountI
		if err := cdc.UnmarshalInterface(raw, &acc); err != nil {
			return a, false, err
		}
		a.nonce = acc.GetSequence()
		if eth, ok := acc.(interface{ GetCodeHash() common.Hash }); ok && eth.GetCodeHash() != (common.Hash{}) {
			a.code = eth.GetCodeHash()
		}
	}
	key := append(bank.CreateAccountBalancesPrefix(addr.Bytes()), w.denom...)
	if value := ms.GetKVStore(w.bank).Get(key); value != nil {
		coin, err := bankkeeper.UnmarshalBalanceCompat(cdc, value, w.denom)
		if err != nil {
			return a, false, err
		}
		if coin.Denom != w.denom || coin.Amount.IsNil() || coin.Amount.IsNegative() || coin.Amount.BigInt().BitLen() > 256 {
			return a, false, fmt.Errorf("invalid native balance for %s", addr)
		}
		a.balance.SetBytes(coin.Amount.BigInt().Bytes())
	}
	return a, raw != nil || !a.balance.IsZero(), nil
}
func (w *StateWrites) Diff(cdc codec.BinaryCodec, before, after store.MultiStore) (wire.TransactionStateDiff, error) {
	var diff wire.TransactionStateDiff
	for address := range w.addresses {
		a, _, err := w.account(before, cdc, address)
		if err != nil {
			return diff, err
		}
		b, present, err := w.account(after, cdc, address)
		if err != nil {
			return diff, err
		}
		if a == b {
			continue
		}
		h := crypto.Keccak256Hash(address.Bytes())
		if present {
			diff.NewAccounts = append(diff.NewAccounts, wire.NewAccount{Address: h, Balance: &b.balance, Nonce: b.nonce, CodeHash: b.code})
		} else {
			diff.DeletedAccounts = append(diff.DeletedAccounts, h)
		}
	}
	slots := make(map[common.Hash][]wire.IndexValuePair)
	for k := range w.keys {
		key := []byte(k)
		var previous []byte
		if before != nil {
			previous = before.GetKVStore(w.evm).Get(key)
		}
		value := after.GetKVStore(w.evm).Get(key)
		if bytes.Equal(previous, value) {
			continue
		}
		if bytes.HasPrefix(key, evm.KeyPrefixCode) {
			if len(key) != 33 {
				return diff, fmt.Errorf("invalid EVM code key %X", key)
			}
			if len(value) > 0 {
				hash := common.BytesToHash(key[1:])
				if crypto.Keccak256Hash(value) != hash {
					return diff, fmt.Errorf("code hash mismatch for %s", hash)
				}
				diff.NewCodes = append(diff.NewCodes, wire.NewCode{CodeHash: hash, Code: value})
			}
		} else {
			if len(key) != 53 || len(value) > 32 {
				return diff, fmt.Errorf("invalid EVM storage entry %X", key)
			}
			addr := crypto.Keccak256Hash(key[1:21])
			slot := crypto.Keccak256Hash(key[21:])
			slots[addr] = append(slots[addr], wire.IndexValuePair{Index: slot, Value: new(uint256.Int).SetBytes(value)})
		}
	}
	for address, values := range slots {
		sort.Slice(values, func(i, j int) bool { return bytes.Compare(values[i].Index[:], values[j].Index[:]) < 0 })
		diff.StorageDiff = append(diff.StorageDiff, wire.AccountStorageDiff{Address: address, Values: values})
	}
	sort.Slice(diff.NewAccounts, func(i, j int) bool {
		return bytes.Compare(diff.NewAccounts[i].Address[:], diff.NewAccounts[j].Address[:]) < 0
	})
	sort.Slice(diff.DeletedAccounts, func(i, j int) bool { return bytes.Compare(diff.DeletedAccounts[i][:], diff.DeletedAccounts[j][:]) < 0 })
	sort.Slice(diff.NewCodes, func(i, j int) bool {
		return bytes.Compare(diff.NewCodes[i].CodeHash[:], diff.NewCodes[j].CodeHash[:]) < 0
	})
	sort.Slice(diff.StorageDiff, func(i, j int) bool {
		return bytes.Compare(diff.StorageDiff[i].Address[:], diff.StorageDiff[j].Address[:]) < 0
	})
	return diff, nil
}

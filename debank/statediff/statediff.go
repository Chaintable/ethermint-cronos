package statediff

import (
	"bytes"
	"fmt"
	"sort"

	iavlstore "cosmossdk.io/store/iavl"
	sdkcodec "github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/iavl"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/evmos/ethermint/debank/types"
)

const storageKeyLength = 1 + common.AddressLength + common.HashLength

// StateChangeSource exposes the committed IAVL versions needed by the DeBank
// state-diff emitter.
type StateChangeSource interface {
	LatestVersion() int64
	VersionExists(int64) bool
	StateDiff(int64, string) (types.TransactionStateDiff, error)
}

type latestVersionSource interface {
	LatestVersion() int64
}

type iavlChangeStore interface {
	VersionExists(int64) bool
	TraverseStateChanges(int64, int64, func(int64, *iavl.ChangeSet) error) error
	Snapshot(int64) (stateReader, error)
}

type iavlStateChangeSource struct {
	versions latestVersionSource
	codec    sdkcodec.Codec
	accounts iavlChangeStore
	balances iavlChangeStore
	evm      iavlChangeStore
}

type iavlStore struct {
	*iavlstore.Store
}

func (store iavlStore) Snapshot(version int64) (stateReader, error) {
	return store.GetImmutable(version)
}

// NewIAVLStateChangeSource wraps the committed account, bank, and EVM stores.
func NewIAVLStateChangeSource(
	versions latestVersionSource,
	codec sdkcodec.Codec,
	accounts, balances, evm *iavlstore.Store,
) StateChangeSource {
	return newIAVLStateChangeSource(
		versions, codec, iavlStore{accounts}, iavlStore{balances}, iavlStore{evm},
	)
}

func newIAVLStateChangeSource(
	versions latestVersionSource,
	codec sdkcodec.Codec,
	accounts, balances, evm iavlChangeStore,
) StateChangeSource {
	return &iavlStateChangeSource{
		versions: versions,
		codec:    codec,
		accounts: accounts,
		balances: balances,
		evm:      evm,
	}
}

func (s *iavlStateChangeSource) LatestVersion() int64 {
	return s.versions.LatestVersion()
}

func (s *iavlStateChangeSource) VersionExists(version int64) bool {
	return s.accounts.VersionExists(version) &&
		s.balances.VersionExists(version) &&
		s.evm.VersionExists(version)
}

func changeSet(store iavlChangeStore, version int64) (*iavl.ChangeSet, error) {
	var result *iavl.ChangeSet
	callbacks := 0
	err := store.TraverseStateChanges(version, version, func(callbackVersion int64, changeSet *iavl.ChangeSet) error {
		callbacks++
		if callbacks != 1 {
			return fmt.Errorf("changeset version %d produced more than one callback", version)
		}
		if callbackVersion != version {
			return fmt.Errorf("changeset version mismatch: requested %d, got %d", version, callbackVersion)
		}
		result = changeSet
		return nil
	})
	if err != nil {
		return nil, err
	}
	if callbacks != 1 {
		return nil, fmt.Errorf("changeset version %d produced %d callbacks", version, callbacks)
	}
	return result, nil
}

// CanonicalStateAt returns the complete EVM-visible state transition N-1 -> N.
func CanonicalStateAt(source StateChangeSource, version int64, evmDenom string) (types.TransactionStateDiff, error) {
	if source == nil {
		return types.TransactionStateDiff{}, fmt.Errorf("state change source is required")
	}
	latest := source.LatestVersion()
	if latest <= version {
		return types.TransactionStateDiff{}, fmt.Errorf("changeset version %d is not behind latest version %d", version, latest)
	}
	if !source.VersionExists(version - 1) {
		return types.TransactionStateDiff{}, fmt.Errorf("previous changeset version %d is unavailable", version-1)
	}
	if !source.VersionExists(version) {
		return types.TransactionStateDiff{}, fmt.Errorf("changeset version %d is unavailable", version)
	}
	diff, err := source.StateDiff(version, evmDenom)
	if err != nil {
		return types.TransactionStateDiff{}, err
	}
	if !source.VersionExists(version-1) || !source.VersionExists(version) {
		return types.TransactionStateDiff{}, fmt.Errorf("changeset versions %d-%d became unavailable", version-1, version)
	}
	return diff, nil
}

// CanonicalStorageDiff converts EVM storage entries in an IAVL changeset to a
// deterministic BlockStorageDiff.StorageDiff value.
func CanonicalStorageDiff(changeSet *iavl.ChangeSet) ([]types.AccountStorageDiff, error) {
	if changeSet == nil {
		return nil, fmt.Errorf("nil changeset")
	}

	accounts := make(map[common.Hash][]types.IndexValuePair)
	seen := make(map[[2]common.Hash]struct{})
	for i, pair := range changeSet.Pairs {
		if pair == nil {
			return nil, fmt.Errorf("nil changeset pair %d", i)
		}
		if len(pair.Key) == 0 || pair.Key[0] != 0x02 {
			continue
		}
		if len(pair.Key) != storageKeyLength {
			return nil, fmt.Errorf("storage pair %d has invalid key length %d", i, len(pair.Key))
		}
		if !pair.Delete && (len(pair.Value) == 0 || len(pair.Value) > common.HashLength) {
			return nil, fmt.Errorf("storage pair %d has invalid value length %d", i, len(pair.Value))
		}

		address := crypto.Keccak256Hash(pair.Key[1 : 1+common.AddressLength])
		index := crypto.Keccak256Hash(pair.Key[1+common.AddressLength:])
		identity := [2]common.Hash{address, index}
		if _, ok := seen[identity]; ok {
			return nil, fmt.Errorf("storage pair %d duplicates address %s index %s", i, address, index)
		}
		seen[identity] = struct{}{}

		value := new(uint256.Int)
		if !pair.Delete {
			value.SetBytes(pair.Value)
		}
		accounts[address] = append(accounts[address], types.IndexValuePair{Index: index, Value: value})
	}

	result := make([]types.AccountStorageDiff, 0, len(accounts))
	for address, values := range accounts {
		sort.Slice(values, func(i, j int) bool {
			return bytes.Compare(values[i].Index[:], values[j].Index[:]) < 0
		})
		result = append(result, types.AccountStorageDiff{Address: address, Values: values})
	}
	sort.Slice(result, func(i, j int) bool {
		return bytes.Compare(result[i].Address[:], result[j].Address[:]) < 0
	})
	return result, nil
}

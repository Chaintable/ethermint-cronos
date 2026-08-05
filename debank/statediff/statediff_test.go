package statediff

import (
	"bytes"
	"errors"
	"testing"

	"cosmossdk.io/collections"
	sdkmath "cosmossdk.io/math"
	sdkcodec "github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	vestingtypes "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/iavl"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/evmos/ethermint/encoding"
	etherminttypes "github.com/evmos/ethermint/types"
	"github.com/stretchr/testify/require"
)

func storageKey(address byte, slot byte) []byte {
	key := make([]byte, storageKeyLength)
	key[0] = 0x02
	key[1+common.AddressLength-1] = address
	key[len(key)-1] = slot
	return key
}

func TestCanonicalStorageDiff(t *testing.T) {
	changeSet := &iavl.ChangeSet{Pairs: []*iavl.KVPair{
		{Key: []byte{0x01, 0xaa}, Value: []byte{0xff}},
		{Key: storageKey(2, 2), Value: []byte{0x01, 0x02}},
		{Key: storageKey(1, 2), Delete: true},
		{Key: storageKey(1, 1), Value: []byte{0x00}},
	}}

	got, err := CanonicalStorageDiff(changeSet)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for i := 1; i < len(got); i++ {
		require.Less(t, bytes.Compare(got[i-1].Address[:], got[i].Address[:]), 0)
	}

	byAddress := make(map[common.Hash]map[common.Hash]string)
	for _, account := range got {
		byAddress[account.Address] = make(map[common.Hash]string)
		for i, pair := range account.Values {
			if i > 0 {
				require.Less(t, bytes.Compare(account.Values[i-1].Index[:], pair.Index[:]), 0)
			}
			byAddress[account.Address][pair.Index] = pair.Value.String()
		}
	}

	address1 := crypto.Keccak256Hash(storageKey(1, 1)[1 : 1+common.AddressLength])
	slot1 := crypto.Keccak256Hash(storageKey(1, 1)[1+common.AddressLength:])
	slot2 := crypto.Keccak256Hash(storageKey(1, 2)[1+common.AddressLength:])
	require.Equal(t, "0", byAddress[address1][slot1])
	require.Equal(t, "0", byAddress[address1][slot2])

	encoded, err := rlp.EncodeToBytes(got)
	require.NoError(t, err)
	var decoded []struct {
		Address common.Hash
		Values  []struct {
			Index common.Hash
			Value []byte
		}
	}
	require.NoError(t, rlp.DecodeBytes(encoded, &decoded))
	for _, account := range decoded {
		for _, pair := range account.Values {
			if account.Address == address1 && (pair.Index == slot1 || pair.Index == slot2) {
				require.Empty(t, pair.Value, "zero must use canonical minimal RLP")
			}
		}
	}
}

func TestCanonicalStorageDiffRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name      string
		changeSet *iavl.ChangeSet
	}{
		{name: "nil", changeSet: nil},
		{name: "nil pair", changeSet: &iavl.ChangeSet{Pairs: []*iavl.KVPair{nil}}},
		{name: "short storage key", changeSet: &iavl.ChangeSet{Pairs: []*iavl.KVPair{{Key: []byte{0x02}}}}},
		{name: "empty value", changeSet: &iavl.ChangeSet{Pairs: []*iavl.KVPair{{Key: storageKey(1, 1)}}}},
		{name: "long value", changeSet: &iavl.ChangeSet{Pairs: []*iavl.KVPair{{Key: storageKey(1, 1), Value: make([]byte, 33)}}}},
		{name: "duplicate", changeSet: &iavl.ChangeSet{Pairs: []*iavl.KVPair{
			{Key: storageKey(1, 1), Value: []byte{1}},
			{Key: storageKey(1, 1), Delete: true},
		}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CanonicalStorageDiff(tc.changeSet)
			require.Error(t, err)
		})
	}
}

type fakeSource struct {
	latest int64
	exists map[int64]bool
	diff   dtypes.TransactionStateDiff
	err    error
	calls  int
}

func (s *fakeSource) LatestVersion() int64       { return s.latest }
func (s *fakeSource) VersionExists(v int64) bool { return s.exists[v] }
func (s *fakeSource) StateDiff(int64, string) (dtypes.TransactionStateDiff, error) {
	s.calls++
	return s.diff, s.err
}

func TestCanonicalStateAt(t *testing.T) {
	source := &fakeSource{
		latest: 12,
		exists: map[int64]bool{10: true, 11: true},
	}
	_, err := CanonicalStateAt(source, 11, "basecro")
	require.NoError(t, err)
	require.Equal(t, 1, source.calls)

	for name, mutate := range map[string]func(*fakeSource){
		"latest":   func(s *fakeSource) { s.latest = 11 },
		"previous": func(s *fakeSource) { s.exists[10] = false },
		"version":  func(s *fakeSource) { s.exists[11] = false },
		"source":   func(s *fakeSource) { s.err = errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := *source
			candidate.exists = map[int64]bool{10: true, 11: true}
			candidate.calls = 0
			mutate(&candidate)
			_, err := CanonicalStateAt(&candidate, 11, "basecro")
			require.Error(t, err)
		})
	}
}

type fakeVersions struct{ latest int64 }

func (v fakeVersions) LatestVersion() int64 { return v.latest }

type fakeIAVLStore struct {
	exists      map[int64]bool
	callbacks   []int64
	changes     *iavl.ChangeSet
	snapshot    stateReader
	snapshotErr error
	snapshotAt  int
	snapshots   *int
}

func (s fakeIAVLStore) VersionExists(version int64) bool { return s.exists[version] }
func (s fakeIAVLStore) TraverseStateChanges(_, _ int64, fn func(int64, *iavl.ChangeSet) error) error {
	for _, version := range s.callbacks {
		if err := fn(version, s.changes); err != nil {
			return err
		}
	}
	return nil
}
func (s fakeIAVLStore) Snapshot(int64) (stateReader, error) {
	if s.snapshots != nil {
		(*s.snapshots)++
		if s.snapshotAt == 0 || *s.snapshots < s.snapshotAt {
			return s.snapshot, nil
		}
	}
	return s.snapshot, s.snapshotErr
}

func TestChangeSetRequiresExactlyOneMatchingCallback(t *testing.T) {
	for name, callbacks := range map[string][]int64{
		"none":      nil,
		"wrong":     {10},
		"duplicate": {11, 11},
	} {
		t.Run(name, func(t *testing.T) {
			store := fakeIAVLStore{
				exists:    map[int64]bool{10: true, 11: true},
				callbacks: callbacks,
				changes:   &iavl.ChangeSet{},
			}
			_, err := changeSet(store, 11)
			require.Error(t, err)
		})
	}

	store := fakeIAVLStore{
		exists:    map[int64]bool{10: true, 11: true},
		callbacks: []int64{11},
		changes:   &iavl.ChangeSet{},
	}
	_, err := changeSet(store, 11)
	require.NoError(t, err)
}

type fakeState map[string][]byte

func (state fakeState) Get(key []byte) []byte { return state[string(key)] }

func testCodec() sdkcodec.Codec {
	config := encoding.MakeConfig()
	authtypes.RegisterInterfaces(config.InterfaceRegistry)
	vestingtypes.RegisterInterfaces(config.InterfaceRegistry)
	return config.Codec
}

func encodeAccount(t *testing.T, codec sdkcodec.Codec, account sdk.AccountI) []byte {
	t.Helper()
	value, err := sdkcodec.CollInterfaceValue[sdk.AccountI](codec).Encode(account)
	require.NoError(t, err)
	return value
}

func encodeBalance(t *testing.T, amount int64) []byte {
	t.Helper()
	value, err := banktypes.BalanceValueCodec.Encode(sdkmath.NewInt(amount))
	require.NoError(t, err)
	return value
}

func balanceKey(t *testing.T, address common.Address, denom string) []byte {
	t.Helper()
	codec := collections.PairKeyCodec(sdk.AccAddressKey, collections.StringKey)
	key := collections.Join(sdk.AccAddress(address.Bytes()), denom)
	body := make([]byte, codec.Size(key))
	written, err := codec.Encode(body, key)
	require.NoError(t, err)
	return append([]byte{bankBalancePrefix}, body[:written]...)
}

func ethAccount(address common.Address, nonce uint64, codeHash common.Hash) sdk.AccountI {
	return &etherminttypes.EthAccount{
		BaseAccount: authtypes.NewBaseAccount(address.Bytes(), nil, 0, nonce),
		CodeHash:    codeHash.Hex(),
	}
}

func TestIAVLStateChangeSourceBuildsCompleteCanonicalState(t *testing.T) {
	codec := testCodec()
	addressA := common.BytesToAddress([]byte{0x0a})
	addressB := common.BytesToAddress([]byte{0x0b})
	addressC := common.BytesToAddress([]byte{0x0c})
	addressD := common.BytesToAddress([]byte{0x0d})
	addressE := common.BytesToAddress([]byte{0x0e})
	code := []byte{0x60, 0x00}
	codeHash := crypto.Keccak256Hash(code)
	otherCodeHash := crypto.Keccak256Hash([]byte{0x60, 0x01})

	accountState := fakeState{
		string(append([]byte{authAccountPrefix}, addressB.Bytes()...)): encodeAccount(t, codec, ethAccount(addressB, 3, otherCodeHash)),
		string(append([]byte{authAccountPrefix}, addressC.Bytes()...)): encodeAccount(t, codec, ethAccount(addressC, 1, emptyCodeHash)),
		string(append([]byte{authAccountPrefix}, addressD.Bytes()...)): encodeAccount(t, codec, ethAccount(addressD, 0, emptyCodeHash)),
		string(append([]byte{authAccountPrefix}, addressE.Bytes()...)): encodeAccount(t, codec, ethAccount(addressE, 2, otherCodeHash)),
	}
	balanceState := fakeState{
		string(balanceKey(t, addressA, "basecro")): encodeBalance(t, 99),
		string(balanceKey(t, addressC, "basecro")): encodeBalance(t, 5),
		string(balanceKey(t, addressE, "basecro")): encodeBalance(t, 77),
	}
	exists := map[int64]bool{10: true, 11: true}
	accounts := fakeIAVLStore{
		exists: exists, callbacks: []int64{11}, snapshot: accountState,
		changes: &iavl.ChangeSet{Pairs: []*iavl.KVPair{
			{Key: append([]byte{authAccountPrefix}, addressA.Bytes()...), Value: encodeAccount(t, codec, ethAccount(addressA, 7, codeHash))},
			{Key: append([]byte{authAccountPrefix}, addressC.Bytes()...), Delete: true},
			{Key: append([]byte{authAccountPrefix}, addressD.Bytes()...), Delete: true},
			{Key: append([]byte{authAccountPrefix}, addressE.Bytes()...), Delete: true},
		}},
	}
	balances := fakeIAVLStore{
		exists: exists, callbacks: []int64{11}, snapshot: balanceState,
		changes: &iavl.ChangeSet{Pairs: []*iavl.KVPair{
			{Key: balanceKey(t, addressB, "basecro"), Value: encodeBalance(t, 44)},
			{Key: balanceKey(t, addressC, "basecro"), Delete: true},
			{Key: balanceKey(t, addressA, "other"), Value: encodeBalance(t, 123)},
		}},
	}
	evm := fakeIAVLStore{
		exists: exists, callbacks: []int64{11}, snapshot: fakeState{},
		changes: &iavl.ChangeSet{Pairs: []*iavl.KVPair{
			{Key: append([]byte{evmCodePrefix}, codeHash.Bytes()...), Value: code},
			{Key: append([]byte{evmCodePrefix}, otherCodeHash.Bytes()...), Delete: true},
			{Key: storageKey(1, 1), Value: []byte{9}},
		}},
	}
	source := newIAVLStateChangeSource(
		fakeVersions{latest: 12}, codec, accounts, balances, evm,
	)

	diff, err := CanonicalStateAt(source, 11, "basecro")
	require.NoError(t, err)
	require.Len(t, diff.NewAccounts, 3)
	for index := 1; index < len(diff.NewAccounts); index++ {
		require.Less(t, bytes.Compare(diff.NewAccounts[index-1].Address[:], diff.NewAccounts[index].Address[:]), 0)
	}
	byAddress := make(map[common.Hash]dtypes.NewAccount)
	for _, account := range diff.NewAccounts {
		byAddress[account.Address] = account
	}
	accountA := byAddress[crypto.Keccak256Hash(addressA.Bytes())]
	require.Equal(t, uint64(99), accountA.Balance.Uint64())
	require.Equal(t, uint64(7), accountA.Nonce)
	require.Equal(t, codeHash, accountA.CodeHash)
	accountB := byAddress[crypto.Keccak256Hash(addressB.Bytes())]
	require.Equal(t, uint64(44), accountB.Balance.Uint64())
	require.Equal(t, uint64(3), accountB.Nonce)
	require.Equal(t, otherCodeHash, accountB.CodeHash)
	accountE := byAddress[crypto.Keccak256Hash(addressE.Bytes())]
	require.Equal(t, uint64(77), accountE.Balance.Uint64())
	require.Zero(t, accountE.Nonce)
	require.Equal(t, emptyCodeHash, accountE.CodeHash)
	require.Equal(t, []common.Hash{crypto.Keccak256Hash(addressC.Bytes())}, diff.DeletedAccounts)
	require.Len(t, diff.NewCodes, 1)
	require.Equal(t, codeHash, diff.NewCodes[0].CodeHash)
	require.Equal(t, code, diff.NewCodes[0].Code)
	require.Len(t, diff.StorageDiff, 1)
}

func TestDecodeCodesRejectsMismatchedBody(t *testing.T) {
	_, err := decodeCodes(&iavl.ChangeSet{Pairs: []*iavl.KVPair{{
		Key:   append([]byte{evmCodePrefix}, common.BytesToHash([]byte{1}).Bytes()...),
		Value: []byte{2},
	}}})
	require.ErrorContains(t, err, "contains body hash")
}

func TestIAVLStateChangeSourceRejectsUnavailablePredecessorRoot(t *testing.T) {
	exists := map[int64]bool{10: true, 11: true}
	pruned := fakeIAVLStore{
		exists: exists, callbacks: []int64{11}, changes: &iavl.ChangeSet{},
		snapshotErr: errors.New("root was pruned"),
	}
	healthy := fakeIAVLStore{
		exists: exists, callbacks: []int64{11}, changes: &iavl.ChangeSet{},
		snapshot: fakeState{},
	}
	source := newIAVLStateChangeSource(
		fakeVersions{latest: 12}, testCodec(), pruned, healthy, healthy,
	)
	_, err := CanonicalStateAt(source, 11, "basecro")
	require.ErrorContains(t, err, "root was pruned")
}

func TestIAVLStateChangeSourceRejectsRootPrunedDuringConversion(t *testing.T) {
	exists := map[int64]bool{10: true, 11: true}
	snapshots := 0
	flaky := fakeIAVLStore{
		exists: exists, callbacks: []int64{11}, changes: &iavl.ChangeSet{},
		snapshot: fakeState{}, snapshotErr: errors.New("root was pruned"),
		snapshotAt: 2, snapshots: &snapshots,
	}
	healthy := fakeIAVLStore{
		exists: exists, callbacks: []int64{11}, changes: &iavl.ChangeSet{},
		snapshot: fakeState{},
	}
	source := newIAVLStateChangeSource(
		fakeVersions{latest: 12}, testCodec(), flaky, healthy, healthy,
	)
	_, err := CanonicalStateAt(source, 11, "basecro")
	require.ErrorContains(t, err, "reload account version")
}

func TestDecodeAccountsSupportsCosmosAccountTypes(t *testing.T) {
	codec := testCodec()
	decoder, err := newWorldStateDecoder(codec, "basecro")
	require.NoError(t, err)
	moduleAddress := common.BytesToAddress([]byte{0x21})
	vestingAddress := common.BytesToAddress([]byte{0x22})
	module := authtypes.NewModuleAccount(
		authtypes.NewBaseAccount(moduleAddress.Bytes(), nil, 1, 8), "module",
	)
	vesting, err := vestingtypes.NewContinuousVestingAccount(
		authtypes.NewBaseAccount(vestingAddress.Bytes(), nil, 2, 9),
		sdk.NewCoins(sdk.NewInt64Coin("basecro", 1)), 0, 1,
	)
	require.NoError(t, err)

	mutations, err := decoder.decodeAccounts(&iavl.ChangeSet{Pairs: []*iavl.KVPair{
		{Key: append([]byte{authAccountPrefix}, moduleAddress.Bytes()...), Value: encodeAccount(t, codec, module)},
		{Key: append([]byte{authAccountPrefix}, vestingAddress.Bytes()...), Value: encodeAccount(t, codec, vesting)},
	}})
	require.NoError(t, err)
	require.Len(t, mutations, 2)
	require.Equal(t, uint64(8), mutations[0].state.nonce)
	require.Equal(t, emptyCodeHash, mutations[0].state.codeHash)
	require.Equal(t, uint64(9), mutations[1].state.nonce)
	require.Equal(t, emptyCodeHash, mutations[1].state.codeHash)
}

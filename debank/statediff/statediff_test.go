package statediff

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cosmos/iavl"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
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
	latest  int64
	exists  map[int64]bool
	set     *iavl.ChangeSet
	err     error
	changes int
}

func (s *fakeSource) LatestVersion() int64                     { return s.latest }
func (s *fakeSource) VersionExists(v int64) bool               { return s.exists[v] }
func (s *fakeSource) ChangeSet(int64) (*iavl.ChangeSet, error) { s.changes++; return s.set, s.err }

func TestCanonicalStorageAt(t *testing.T) {
	source := &fakeSource{
		latest: 12,
		exists: map[int64]bool{10: true, 11: true},
		set:    &iavl.ChangeSet{Pairs: []*iavl.KVPair{{Key: storageKey(1, 1), Value: []byte{1}}}},
	}
	_, err := CanonicalStorageAt(source, 11)
	require.NoError(t, err)
	require.Equal(t, 1, source.changes)

	for name, mutate := range map[string]func(*fakeSource){
		"latest":   func(s *fakeSource) { s.latest = 11 },
		"previous": func(s *fakeSource) { s.exists[10] = false },
		"version":  func(s *fakeSource) { s.exists[11] = false },
		"source":   func(s *fakeSource) { s.err = errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := *source
			candidate.exists = map[int64]bool{10: true, 11: true}
			candidate.changes = 0
			mutate(&candidate)
			_, err := CanonicalStorageAt(&candidate, 11)
			require.Error(t, err)
		})
	}
}

type fakeVersions struct{ latest int64 }

func (v fakeVersions) LatestVersion() int64 { return v.latest }

type fakeIAVLStore struct {
	exists    bool
	callbacks []int64
}

func (s fakeIAVLStore) VersionExists(int64) bool { return s.exists }
func (s fakeIAVLStore) TraverseStateChanges(_, _ int64, fn func(int64, *iavl.ChangeSet) error) error {
	for _, version := range s.callbacks {
		if err := fn(version, &iavl.ChangeSet{}); err != nil {
			return err
		}
	}
	return nil
}

func TestIAVLStateChangeSourceRequiresExactlyOneMatchingCallback(t *testing.T) {
	for name, callbacks := range map[string][]int64{
		"none":      nil,
		"wrong":     {10},
		"duplicate": {11, 11},
	} {
		t.Run(name, func(t *testing.T) {
			source := newIAVLStateChangeSource(fakeVersions{latest: 12}, fakeIAVLStore{exists: true, callbacks: callbacks})
			_, err := source.ChangeSet(11)
			require.Error(t, err)
		})
	}

	source := newIAVLStateChangeSource(fakeVersions{latest: 12}, fakeIAVLStore{exists: true, callbacks: []int64{11}})
	_, err := source.ChangeSet(11)
	require.NoError(t, err)
}

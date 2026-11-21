package replay

import (
	"bytes"
	"encoding/binary"
	"fmt"

	store "github.com/cosmos/cosmos-sdk/store/types"
	gogotypes "github.com/gogo/protobuf/types"
	dbm "github.com/tendermint/tm-db"
)

// ArchiveReader provides the only operation used on the historical database.
type ArchiveReader interface {
	Get([]byte) ([]byte, error)
}

// ParentState exposes only one version's metadata. Immutable IAVL nodes
// are read by hash from the archive. All writes live in the private database.
type ParentState struct {
	dbm.DB
	archive ArchiveReader
}

func (db *ParentState) Get(key []byte) ([]byte, error) {
	value, err := db.DB.Get(key)
	if err != nil || value != nil {
		return value, err
	}
	boundary := len(key) - 34
	if boundary < 5 || !bytes.HasPrefix(key, []byte("s/k:")) || !bytes.Equal(key[boundary:boundary+2], []byte("/n")) {
		return nil, nil
	}
	return db.archive.Get(key)
}
func (db *ParentState) Has(key []byte) (bool, error) {
	value, err := db.Get(key)
	return value != nil, err
}
func OpenParentState(archive ArchiveReader, height int64) (*ParentState, error) {
	key := []byte(fmt.Sprintf("s/%d", height))
	raw, err := archive.Get(key)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("archive has no CommitInfo at %d", height)
	}
	var info store.CommitInfo
	if err = info.Unmarshal(raw); err != nil {
		return nil, err
	}
	if info.Version != height {
		return nil, fmt.Errorf("archive CommitInfo has version %d, requested %d", info.Version, height)
	}
	db := &ParentState{DB: dbm.NewMemDB(), archive: archive}
	batch := db.NewBatch()
	defer batch.Close()
	latest, err := gogotypes.StdInt64Marshal(height)
	if err != nil {
		return nil, err
	}
	if err = batch.Set([]byte("s/latest"), latest); err != nil {
		return nil, err
	}
	if err = batch.Set(key, raw); err != nil {
		return nil, err
	}
	var version [8]byte
	binary.BigEndian.PutUint64(version[:], uint64(height))
	for _, s := range info.StoreInfos {
		key := append([]byte("s/k:"+s.Name+"/r"), version[:]...)
		root, err := archive.Get(key)
		if err != nil {
			return nil, err
		}
		if root != nil {
			if err = batch.Set(key, root); err != nil {
				return nil, err
			}
		}
	}
	if err = batch.Write(); err != nil {
		return nil, err
	}
	return db, nil
}

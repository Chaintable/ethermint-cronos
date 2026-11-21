package keeper

import (
	"encoding/json"
	"github.com/ethereum/go-ethereum/common"
)

type ReplayTransaction struct {
	CosmosHash []byte
	Hash       common.Hash
	GasUsed    uint64
	VmError    string
	Trace      json.RawMessage
}

// SetReplaySink enables observation on an isolated replay application.
func (k *Keeper) SetReplaySink(sink func(ReplayTransaction, error)) { k.replaySink = sink }

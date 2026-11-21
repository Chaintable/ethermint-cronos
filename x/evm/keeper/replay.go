package keeper

import (
	"encoding/json"
	"github.com/ethereum/go-ethereum/common"
	wire "github.com/tharsis/ethermint/debank/types"
	evm "github.com/tharsis/ethermint/x/evm/types"
)

type ReplayTransaction struct {
	MessageResult     *evm.MsgEthereumTxResponse
	PreRollbackEvents []wire.Event
	CosmosHash        []byte
	Hash              common.Hash
	GasUsed           uint64
	RootOutput        []byte
	VmError           string
	Trace             json.RawMessage
}

// SetReplaySink enables observation on an isolated replay application.
func (k *Keeper) SetReplaySink(sink func(ReplayTransaction, error)) { k.replaySink = sink }

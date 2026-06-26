package types

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

// id = to_hash(trace['tx_id'], trace['parent_trace_id'], trace['pos_in_parent_trace'])
type Trace struct {
	ID                string        `json:"id"`
	From              string        `json:"from_addr"`
	Gas               *big.Int      `json:"gas_limit"`
	Input             hexutil.Bytes `json:"input"`
	To                string        `json:"to_addr"`
	Value             *hexutil.Big  `json:"value"`
	GasUsed           *big.Int      `json:"gas_used"`
	Output            hexutil.Bytes `json:"output"`
	CallCreateType    string        `json:"type"` // ['create', 'suicide', 'call', 'empty']
	CallType          string        `json:"call_type"`
	TxID              string        `json:"tx_id"`
	ParentTraceID     string        `json:"parent_trace_id"`
	PosInParentTrace  int64         `json:"pos_in_parent_trace"`
	SelfStorageChange bool          `json:"self_storage_change"`
	StorageChange     bool          `json:"storage_change"`
	Subtraces         int64         `json:"subtraces"`
	TraceAddress      []int64       `json:"trace_address"`
	Error             string        `json:"error,omitempty"`
}

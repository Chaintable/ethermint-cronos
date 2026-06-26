package types

import (
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// id = to_hash(event['parent_trace_id'], event['pos_in_parent_trace'])
type Event struct {
	ID            string        `json:"id"`
	Address       string        `json:"contract_id"`
	Selector      string        `json:"selector"`
	Topics        []string      `json:"topics"`
	Data          hexutil.Bytes `json:"data"`
	ParentTraceID string        `json:"parent_trace_id"`
	Position      int64         `json:"pos_in_parent_trace"`
	LogIndex      int64         `json:"idx"`
}

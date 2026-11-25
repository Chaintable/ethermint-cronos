package types

import (
	"math/big"
)

type Block struct {
	ID                    string   `json:"id"`
	Height                *big.Int `json:"height"`
	ParentID              string   `json:"parent_id"`
	BaseFeePerGas         *big.Int `json:"base_fee_per_gas"`
	Miner                 string   `json:"miner"`
	GasLimit              *big.Int `json:"gas_limit"`
	GasUsed               *big.Int `json:"gas_used"`
	Timestamp             uint64   `json:"timestamp"`
	ProcessStartTimestamp int64    `json:"process_start_timestamp"`
}

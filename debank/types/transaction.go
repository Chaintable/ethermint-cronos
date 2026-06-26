package types

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

type Transaction struct {
	ID               string        `json:"id"`
	From             string        `json:"from_addr"`
	To               string        `json:"to_addr"` // create的时候是合约地址
	Gas              *big.Int      `json:"gas_limit"`
	GasPrice         *big.Int      `json:"gas_price"`
	GasUsed          *big.Int      `json:"gas_used"`
	Status           bool          `json:"status"`
	GasFeeCap        *big.Int      `json:"max_fee_per_gas"`
	GasTipCap        *big.Int      `json:"max_priority_fee_per_gas"`
	Input            hexutil.Bytes `json:"input"`
	Nonce            *big.Int      `json:"nonce"`
	TransactionIndex int64         `json:"idx"`
	Value            *hexutil.Big  `json:"value"`
}

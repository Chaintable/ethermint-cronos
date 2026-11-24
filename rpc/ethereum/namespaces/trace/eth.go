package trace

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

// EthAPI serves the eth methods a block-file consumer needs from an archive.
type EthAPI struct {
	chain   Chain
	chainID *big.Int
}

func NewEthAPI(chain Chain, chainID *big.Int) *EthAPI {
	return &EthAPI{chain: chain, chainID: chainID}
}

// BlockNumber is the archive head. Only blocks below it can be replayed,
// because each needs its next header.
func (e *EthAPI) BlockNumber() hexutil.Uint64 {
	return hexutil.Uint64(e.chain.Head())
}

func (e *EthAPI) ChainId() *hexutil.Big { // nolint: revive,stylecheck
	return (*hexutil.Big)(e.chainID)
}

package trace

import (
	"fmt"

	dtypes "github.com/evmos/ethermint/debank/types"
)

// onGenesisBlock builds the block-1 (genesis) DebankOutPut from the chain's
// genesis EVM state (alloc accounts + code + storage + module/validator bank
// balances), since genesis state is not produced by replaying a block.
//
// TODO(cronos-genesis): port cosmos-evm's genesisAllocToStateDiff and embed the
// real Cronos mainnet genesis `app_state.evm` (accounts/params/precompiles) plus
// the bank/gentx funded addresses. Until then block-1 tracing is unsupported;
// it does NOT affect blocks >= 2, which the ETL traces normally.
func (api *API) onGenesisBlock(_ map[string]interface{}) (*dtypes.DebankOutPut, error) {
	return nil, fmt.Errorf("trace_debankBlock: genesis block (1) tracing not yet configured for Cronos")
}

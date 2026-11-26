package trace

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"

	"github.com/tharsis/ethermint/debank/replay"
	dtypes "github.com/tharsis/ethermint/debank/types"
	evmtypes "github.com/tharsis/ethermint/x/evm/types"
)

// receipt is the result of one EVM message decoded from its archived
// DeliverTx data. A row rewritten by fix-unlucky-tx is not covered by
// LastResultsHash; matching it against the observed execution checks it.
type receipt struct {
	Status  bool
	VmError string
	// GasUsed is the Cosmos transaction's gas, which the receipt reports.
	GasUsed uint64
	// ExecutionGas is the message response's gas, which the observation reports.
	ExecutionGas uint64
	Logs         []*ethtypes.Log
	// PostProcessingFailed marks a message whose Cosmos hooks failed; reverting
	// to the snapshot before the message also undid its gas refund.
	PostProcessingFailed bool
}

func receiptsFromResults(msgs []ethMsg, results *tmstate.ABCIResponses) ([]*receipt, error) {
	out := make([]*receipt, 0, len(msgs))
	decoded := map[int]*sdk.TxMsgData{}
	for _, m := range msgs {
		if replay.ExceedsBlockGas(results.DeliverTxs[m.cosmos]) {
			return nil, fmt.Errorf("tx[%d]: result exceeding the block gas limit was not restored by fix-unlucky-tx", m.cosmos)
		}
		data := decoded[m.cosmos]
		if data == nil {
			data = new(sdk.TxMsgData)
			if err := data.Unmarshal(results.DeliverTxs[m.cosmos].Data); err != nil {
				return nil, fmt.Errorf("tx[%d] result data: %w", m.cosmos, err)
			}
			decoded[m.cosmos] = data
		}
		if m.msg >= len(data.Data) || data.Data[m.msg] == nil {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: missing message result", m.cosmos, m.msg)
		}
		var res evmtypes.MsgEthereumTxResponse
		if err := res.Unmarshal(data.Data[m.msg].Data); err != nil {
			return nil, fmt.Errorf("tx[%d]/msg[%d] response: %w", m.cosmos, m.msg, err)
		}
		if res.Hash != m.tx.AsTransaction().Hash().Hex() {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: response hash %s", m.cosmos, m.msg, res.Hash)
		}
		out = append(out, &receipt{
			Status:       res.VmError == "",
			VmError:      res.VmError,
			GasUsed:      uint64(results.DeliverTxs[m.cosmos].GasUsed),
			ExecutionGas: res.GasUsed,
			Logs:         evmtypes.LogsToEthereum(res.Logs),

			PostProcessingFailed: res.VmError == evmtypes.ErrPostTxProcessing.Error(),
		})
	}
	return out, nil
}

func receiptTrace(obs replay.Transaction, rc *receipt) (dtypes.TraceResult, error) {
	var tr dtypes.TraceResult
	if err := sonic.ConfigStd.Unmarshal(obs.Trace, &tr); err != nil {
		return tr, err
	}
	return tr, checkReceipt(&tr, obs, rc)
}

// checkReceipt leaves call-frame gas and ownership intact. Committed log
// indices are assigned only after content and execution order agree. A message
// reverted by Cosmos post-processing keeps its EVM logs in the receipt while
// the trace reports them as error events.
func checkReceipt(tr *dtypes.TraceResult, obs replay.Transaction, rc *receipt) error {
	hash := obs.Hash.Hex()
	roots := 0
	for _, bucket := range []struct {
		frames  []dtypes.Trace
		success bool
	}{{tr.Traces, true}, {tr.ErrorTraces, false}} {
		for _, f := range bucket.frames {
			if f.TxID != hash {
				return fmt.Errorf("frame %s has transaction %s", f.ID, f.TxID)
			}
			if len(f.TraceAddress) == 0 {
				roots++
				if rc.Status != bucket.success {
					return fmt.Errorf("root status differs from receipt")
				}
			}
		}
	}
	if roots != 1 {
		return fmt.Errorf("expected one root, found %d", roots)
	}
	committed := tr.Events
	if len(obs.HookLogs) > 0 {
		if rc.Status || len(tr.Events) > 0 {
			return fmt.Errorf("post-processing revert with committed events")
		}
		committed = obs.HookLogs
	}
	if len(committed) != len(rc.Logs) {
		return fmt.Errorf("committed log count: got %d, want %d", len(committed), len(rc.Logs))
	}
	for i, lg := range rc.Logs {
		if eventSig(&committed[i]) != logSig(lg) {
			return fmt.Errorf("receipt log[%d] content differs from frame event", i)
		}
	}
	if len(obs.HookLogs) == 0 {
		for i, lg := range rc.Logs {
			tr.Events[i].LogIndex = int64(lg.Index)
		}
	}
	return nil
}

func logSig(l *ethtypes.Log) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(l.Address.Hex()))
	for _, t := range l.Topics {
		b.WriteString("|")
		b.WriteString(strings.ToLower(t.Hex()))
	}
	b.WriteString("#")
	b.WriteString(hexutil.Bytes(l.Data).String())
	return b.String()
}

func eventSig(e *dtypes.Event) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(e.Address))
	if e.Selector != "" {
		b.WriteString("|")
		b.WriteString(strings.ToLower(e.Selector))
	}
	for _, t := range e.Topics {
		b.WriteString("|")
		b.WriteString(strings.ToLower(t))
	}
	b.WriteString("#")
	b.WriteString(e.Data.String())
	return b.String()
}

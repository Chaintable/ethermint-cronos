package trace

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/evmos/ethermint/debank/replay"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
)

// consensusReceipt is the committed receipt of one EVM transaction.
type consensusReceipt struct {
	Status  bool
	GasUsed uint64
	Logs    []*ethtypes.Log
}

// loadConsensus loads the block's EVM receipts; any malformed receipt fails
// the whole block.
func (api *API) loadConsensus(blockHeight rpctypes.BlockNumber) (map[string]*consensusReceipt, error) {
	rcpts, err := api.backend.GetBlockReceipts(blockHeight)
	if err != nil {
		return nil, fmt.Errorf("load consensus receipts for block %d: %w", blockHeight, err)
	}
	cons := make(map[string]*consensusReceipt, len(rcpts))
	for _, rc := range rcpts {
		var key string
		switch h := rc["transactionHash"].(type) {
		case interface{ Hex() string }: // common.Hash, what buildReceiptDirect stores today
			key = strings.ToLower(h.Hex())
		case string: // tolerate a receipt builder that stores the hex string directly
			key = strings.ToLower(h)
		default:
			return nil, fmt.Errorf("receipt transactionHash has unexpected type %T", rc["transactionHash"])
		}
		status, ok := rc["status"].(hexutil.Uint)
		if !ok {
			return nil, fmt.Errorf("receipt status has unexpected type %T", rc["status"])
		}
		gasUsed, ok := rc["gasUsed"].(hexutil.Uint64)
		if !ok {
			return nil, fmt.Errorf("receipt gasUsed has unexpected type %T", rc["gasUsed"])
		}
		// A missing log array cannot certify a transaction with zero logs.
		logs, ok := rc["logs"].([]*ethtypes.Log)
		if !ok {
			return nil, fmt.Errorf("receipt logs has unexpected type %T", rc["logs"])
		}
		if _, duplicate := cons[key]; duplicate {
			return nil, fmt.Errorf("duplicate receipt for %s", key)
		}
		for i, lg := range logs {
			if lg == nil {
				return nil, fmt.Errorf("receipt %s log[%d] missing", key, i)
			}
		}
		cons[key] = &consensusReceipt{
			Status:  status == hexutil.Uint(ethtypes.ReceiptStatusSuccessful),
			GasUsed: uint64(gasUsed),
			Logs:    logs,
		}
	}
	return cons, nil
}

func receiptTrace(obs replay.Transaction, receipt *consensusReceipt) (dtypes.TraceResult, error) {
	var tr dtypes.TraceResult
	if err := sonic.ConfigStd.Unmarshal(obs.Trace, &tr); err != nil {
		return tr, err
	}
	return tr, checkReceipt(&tr, obs.Hash.Hex(), receipt)
}

// checkReceipt leaves call-frame gas and ownership intact. Committed log
// indices are assigned only after content and execution order agree.
func checkReceipt(tr *dtypes.TraceResult, hash string, receipt *consensusReceipt) error {
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
				if receipt.Status != bucket.success {
					return fmt.Errorf("root status differs from receipt")
				}
			}
		}
	}
	if roots != 1 {
		return fmt.Errorf("expected one root, found %d", roots)
	}
	if len(tr.Events) != len(receipt.Logs) {
		return fmt.Errorf("committed log count: got %d, want %d", len(tr.Events), len(receipt.Logs))
	}
	for i, lg := range receipt.Logs {
		if eventSig(&tr.Events[i]) != logSig(lg) {
			return fmt.Errorf("receipt log[%d] content differs from frame event", i)
		}
	}
	for i, lg := range receipt.Logs {
		tr.Events[i].LogIndex = int64(lg.Index)
	}
	return nil
}

// logSig is the comparable content of a consensus log; eventSig the same for
// a replay event. A zero-topic log yields an empty selector and no topics,
// so both sides collapse to the same signature.
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

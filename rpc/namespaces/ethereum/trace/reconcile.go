package trace

import (
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"strings"

	ethtypes "github.com/ethereum/go-ethereum/core/types"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evm "github.com/evmos/ethermint/x/evm/types"
)

// consensusReceipt is the stored consensus truth for one transaction.
type consensusReceipt struct {
	Status  bool
	GasUsed uint64
	Logs    []*ethtypes.Log
}

// loadConsensus builds the per-tx consensus map for a block from the node's
// own receipt path (CometBFT block results). Failing to load it fails the
// whole trace: emitting unreconciled data would silently reintroduce the
// replay-divergence class this exists to fix.
func loadConsensus(block *ctypes.ResultBlock, results *ctypes.ResultBlockResults, decode sdk.TxDecoder) (map[string]*consensusReceipt, error) {
	cons := make(map[string]*consensusReceipt)
	for i, tx := range results.TxsResults {
		if tx == nil {
			return nil, fmt.Errorf("missing Cosmos result at %d", i)
		}
		if tx.Code != 0 {
			if rpctypes.TxExceedBlockGasLimit(tx) {
				// The SDK omits MsgResponses. The signed gas limit is charged
				// for each Ethereum message included in the failed envelope.
				envelope, err := decode(block.Block.Txs[i])
				if err != nil {
					return nil, fmt.Errorf("Cosmos tx %d: %w", i, err)
				}
				for _, m := range envelope.GetMsgs() {
					if msg, ok := m.(*evm.MsgEthereumTx); ok {
						key := msg.AsTransaction().Hash().Hex()
						if _, exists := cons[key]; exists {
							return nil, fmt.Errorf("duplicate EVM receipt %s", key)
						}
						cons[key] = &consensusReceipt{GasUsed: msg.GetGas()}
					}
				}
			}
			continue
		}
		var msgs sdk.TxMsgData
		if err := msgs.Unmarshal(tx.Data); err != nil {
			return nil, fmt.Errorf("Cosmos result %d: %w", i, err)
		}
		for j, msg := range msgs.MsgResponses {
			if msg == nil {
				return nil, fmt.Errorf("missing response at %d/%d", i, j)
			}
			if msg.TypeUrl != "/ethermint.evm.v1.MsgEthereumTxResponse" {
				continue
			}
			var receipt evm.MsgEthereumTxResponse
			if err := receipt.Unmarshal(msg.Value); err != nil {
				return nil, err
			}
			key := strings.ToLower(receipt.Hash)
			if _, ok := cons[key]; ok {
				return nil, fmt.Errorf("duplicate EVM receipt %s", key)
			}
			for n, l := range receipt.Logs {
				if l == nil {
					return nil, fmt.Errorf("missing log %d for %s", n, key)
				}
			}
			cons[key] = &consensusReceipt{Status: !receipt.Failed(), GasUsed: receipt.GasUsed, Logs: evm.LogsToEthereum(receipt.Logs)}
		}
	}
	return cons, nil
}

func (msg includedMessage) receiptTrace(receipt *consensusReceipt) (dtypes.TraceResult, error) {
	var tr dtypes.TraceResult
	if msg.blockGasError != "" && len(msg.Trace) == 0 {
		// The keeper did not observe an EVM call for this included message.
		return tr, nil
	}
	if err := json.Unmarshal(msg.Trace, &tr); err != nil {
		return tr, err
	}
	if msg.blockGasError != "" {
		for i := range tr.Traces {
			if len(tr.Traces[i].TraceAddress) == 0 {
				tr.Traces[i].Error = msg.blockGasError
			}
		}
		tr.ErrorTraces = append(tr.Traces, tr.ErrorTraces...)
		tr.ErrorEvents = append(tr.Events, tr.ErrorEvents...)
		tr.Traces, tr.Events, tr.StorageContracts = nil, nil, nil
	}
	return tr, checkReceipt(&tr, msg.Hash.Hex(), receipt)
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

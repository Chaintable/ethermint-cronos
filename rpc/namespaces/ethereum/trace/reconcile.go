package trace

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	abci "github.com/cometbft/cometbft/abci/types"
	tmtypes "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/evmos/ethermint/debank/replay"
	dtypes "github.com/evmos/ethermint/debank/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

// consensusReceipt is the committed receipt of one EVM transaction.
type consensusReceipt struct {
	Status  bool
	GasUsed uint64
	Logs    []*ethtypes.Log
}

type committedEVMMessage struct {
	cosmosIndex  int
	messageIndex int
	tx           *ethtypes.Transaction
	receipt      consensusReceipt
}

// Match signed top-level messages with their ABCI responses before deciding
// whether the block needs replay. Historical tx indexes are not required.
func committedEVMMessages(decode sdk.TxDecoder, txs tmtypes.Txs, results []*abci.ExecTxResult) ([]committedEVMMessage, error) {
	var messages []committedEVMMessage
	seen := make(map[common.Hash]struct{})
	var logIndex uint64
	for i, result := range results {
		if result == nil {
			return nil, fmt.Errorf("missing ABCI result for tx[%d]", i)
		}
		if result.Code != 0 {
			continue
		}
		envelope, err := decode(txs[i])
		if err != nil {
			return nil, fmt.Errorf("decode tx[%d]: %w", i, err)
		}
		var data sdk.TxMsgData
		if err := data.Unmarshal(result.Data); err != nil {
			return nil, fmt.Errorf("decode tx[%d] responses: %w", i, err)
		}
		msgs := envelope.GetMsgs()
		if len(data.MsgResponses) != len(msgs) {
			return nil, fmt.Errorf("tx[%d]: %d messages, %d responses", i, len(msgs), len(data.MsgResponses))
		}
		for j, msg := range msgs {
			response := data.MsgResponses[j]
			if response == nil {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: missing response", i, j)
			}
			eth, isEVM := msg.(*evmtypes.MsgEthereumTx)
			if isEVM != (response.TypeUrl == "/ethermint.evm.v1.MsgEthereumTxResponse") {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: unexpected response %s for %T", i, j, response.TypeUrl, msg)
			}
			if !isEVM {
				continue
			}
			tx := eth.AsTransaction()
			if tx == nil {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: invalid signed Ethereum transaction", i, j)
			}
			hash := tx.Hash()
			if _, ok := seen[hash]; ok {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: duplicate Ethereum transaction %s", i, j, hash)
			}
			receipt, err := decodeReceipt(response.Value, hash, uint64(len(messages)), logIndex)
			if err != nil {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: %w", i, j, err)
			}
			messages = append(messages, committedEVMMessage{i, j, tx, receipt})
			seen[hash] = struct{}{}
			logIndex += uint64(len(receipt.Logs))
		}
	}
	return messages, nil
}

func decodeReceipt(data []byte, hash common.Hash, txIndex, firstLogIndex uint64) (consensusReceipt, error) {
	var response evmtypes.MsgEthereumTxResponse
	if err := response.Unmarshal(data); err != nil {
		return consensusReceipt{}, err
	}
	if !strings.EqualFold(response.Hash, hash.Hex()) {
		return consensusReceipt{}, fmt.Errorf("receipt hash %q differs from signed hash %s", response.Hash, hash)
	}
	for i, log := range response.Logs {
		if log == nil || log.TxIndex != txIndex || log.Index != firstLogIndex+uint64(i) {
			return consensusReceipt{}, fmt.Errorf("invalid receipt log position for %s at log[%d]", hash, i)
		}
	}
	return consensusReceipt{
		Status: !response.Failed(), GasUsed: response.GasUsed, Logs: evmtypes.LogsToEthereum(response.Logs),
	}, nil
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

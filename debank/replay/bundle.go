package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	eth "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/tharsis/ethermint/debank/bankdiff"
	"github.com/tharsis/ethermint/debank/tracer"
	wire "github.com/tharsis/ethermint/debank/types"
	rpc "github.com/tharsis/ethermint/rpc/ethereum/types"
	"github.com/tharsis/ethermint/x/evm/keeper"
	evm "github.com/tharsis/ethermint/x/evm/types"
)

func (r *BlockResult) format(miner common.Address, limit int64) (map[string]interface{}, error) {
	if limit == -1 {
		limit = int64(^uint32(0))
	}
	gas := new(big.Int)
	for _, tx := range r.DeliverTxs {
		if tx.Code == 11 && tx.Log == "no block gas left to run tx: out of gas" {
			break
		}
		gas.Add(gas, big.NewInt(tx.GasUsed))
	}
	for _, evt := range r.EndBlock.Events {
		if evt.Type != evm.EventTypeBlockBloom {
			continue
		}
		for _, attr := range evt.Attributes {
			if string(attr.Key) == evm.AttributeKeyEthereumBloom && len(attr.Value) == eth.BloomByteLength {
				block := rpc.FormatBlock(r.Block.Header, r.Block.Size(), limit, gas, nil, eth.BytesToBloom(attr.Value), miner)
				// FormatBlock substitutes a zero DataHash for an empty root before hashing.
				// The mainnet identity of a block is always its original Tendermint header hash.
				block["hash"] = hexutil.Bytes(r.Block.Hash())
				return block, nil
			}
		}
	}
	return nil, fmt.Errorf("missing replayed end-block bloom")
}

func checkMessage(obs keeper.ReplayTransaction, res *evm.MsgEthereumTxResponse) (wire.TraceResult, error) {
	var t wire.TraceResult
	if !strings.EqualFold(res.Hash, obs.Hash.Hex()) || obs.GasUsed != res.GasUsed || obs.VmError != res.VmError {
		return t, fmt.Errorf("message result mismatch for %s", obs.Hash)
	}
	if err := json.Unmarshal(obs.Trace, &t); err != nil {
		return t, err
	}
	roots := t.Traces
	if res.Failed() {
		roots = t.ErrorTraces
	}
	if len(roots) == 0 || len(roots[0].TraceAddress) != 0 || !bytes.Equal(roots[0].Output, res.Ret) {
		return t, fmt.Errorf("trace root mismatch for %s", obs.Hash)
	}
	if len(res.Logs) != len(t.Events) {
		return t, fmt.Errorf("log count mismatch for %s", obs.Hash)
	}
	for i, l := range res.Logs {
		e := t.Events[i]
		topics := e.Topics
		if e.Selector != "" {
			topics = append([]string{e.Selector}, topics...)
		}
		if l == nil || !strings.EqualFold(e.Address, l.Address) || len(topics) != len(l.Topics) || !bytes.Equal(e.Data, l.Data) || e.LogIndex != int64(l.Index) {
			return t, fmt.Errorf("log %d mismatch for %s", i, obs.Hash)
		}
		for j, v := range topics {
			if !strings.EqualFold(v, l.Topics[j]) {
				return t, fmt.Errorf("topic %d mismatch in log %d", j, i)
			}
		}
	}
	return t, nil
}

func (r *BlockResult) Bundle(decoder sdk.TxDecoder, miner common.Address, limit int64, diff wire.TransactionStateDiff, observations []keeper.ReplayTransaction) (*wire.DebankOutPutJs, error) {
	block, err := r.format(miner, limit)
	if err != nil {
		return nil, err
	}
	header := tracer.BuildPilelineBlockHeader(block)
	var parent common.Hash
	if r.Parent != nil {
		parent = common.BytesToHash(r.Parent.AppHash)
	}
	stateDiff := tracer.BuildBlockStateDiff(parent, header.StateRoot, diff)
	encoded, err := rlp.EncodeToBytes(&stateDiff)
	if err != nil {
		return nil, err
	}
	file := &wire.BlockFile{Block: tracer.BuildPipelineBlock(block), Txs: []wire.Transaction{}, Events: []wire.Event{}, ErrorEvents: []wire.Event{}, Traces: []wire.Trace{}, ErrorTraces: []wire.Trace{}, StorageContracts: []string{}}
	addresses := bankdiff.CoinTouchedAddresses(r.ABCIResponses, "basecro")
	offset := 0
	for i, raw := range r.Block.Txs {
		result := r.DeliverTxs[i]
		if result.Code != 0 {
			// A later Cosmos message can roll back earlier EVM execution in this envelope.
			for offset < len(observations) && bytes.Equal(raw.Hash(), observations[offset].CosmosHash) {
				offset++
			}
			continue
		}
		decoded, err := decoder(raw)
		if err != nil {
			return nil, err
		}
		var responses sdk.TxMsgData
		if err = responses.Unmarshal(result.Data); err != nil {
			return nil, err
		}
		for j, msg := range decoded.GetMsgs() {
			ethMsg, ok := msg.(*evm.MsgEthereumTx)
			if !ok {
				continue
			}
			if offset >= len(observations) || j >= len(responses.Data) || responses.Data[j] == nil {
				return nil, fmt.Errorf("missing EVM result for Cosmos message %d/%d", i, j)
			}
			tx := ethMsg.AsTransaction()
			obs := observations[offset]
			offset++
			if obs.Hash != tx.Hash() || !bytes.Equal(obs.CosmosHash, raw.Hash()) {
				return nil, fmt.Errorf("observation identity mismatch at %d/%d", i, j)
			}
			var receipt evm.MsgEthereumTxResponse
			if err := receipt.Unmarshal(responses.Data[j].Data); err != nil {
				return nil, err
			}
			traced, err := checkMessage(obs, &receipt)
			if err != nil {
				return nil, err
			}
			sender, err := ethMsg.GetSender(big.NewInt(25))
			if err != nil {
				return nil, err
			}
			file.Txs = append(file.Txs, tracer.BuildPipelineTransaction(tx, int64(len(file.Txs)), sender, new(big.Int).SetUint64(receipt.GasUsed), nil, !receipt.Failed()))
			addresses[sender] = struct{}{}
			if to := tx.To(); to != nil {
				addresses[*to] = struct{}{}
			}
			for _, a := range traced.StorageContracts {
				addresses[common.HexToAddress(a)] = struct{}{}
			}
			file.Events = append(file.Events, traced.Events...)
			file.ErrorEvents = append(file.ErrorEvents, traced.ErrorEvents...)
			file.Traces = append(file.Traces, traced.Traces...)
			file.ErrorTraces = append(file.ErrorTraces, traced.ErrorTraces...)
		}
	}
	if offset != len(observations) {
		return nil, fmt.Errorf("extra EVM observations at %d", r.Block.Height)
	}
	for addr := range addresses {
		file.StorageContracts = append(file.StorageContracts, strings.ToLower(addr.Hex()))
	}
	sort.Strings(file.StorageContracts)
	return &wire.DebankOutPutJs{BlockFile: file, Header: header, StateDiff: encoded, ValidationHash: file.Validation().ValidationHash}, nil
}

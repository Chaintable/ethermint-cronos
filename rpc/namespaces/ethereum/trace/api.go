package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/cometbft/cometbft/libs/log"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	sm "github.com/cometbft/cometbft/state"
	tmtypes "github.com/cometbft/cometbft/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/evmos/ethermint/debank/bankdiff"
	"github.com/evmos/ethermint/debank/replay"
	dtracer "github.com/evmos/ethermint/debank/tracer"
	dtypes "github.com/evmos/ethermint/debank/types"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
	feekeeper "github.com/evmos/ethermint/x/feemarket/keeper"
	feemarkettypes "github.com/evmos/ethermint/x/feemarket/types"
)

// Chain is the archived consensus and application data of past heights.
type Chain interface {
	StateDiff(int64, string) (dtypes.TransactionStateDiff, error)
	Head() int64
	Block(height int64) (*tmtypes.Block, error)
	BlockByHash(hash []byte) (*tmtypes.Block, error)
	Header(height int64) (*tmtypes.Header, error)
	Results(height int64) (*tmstate.ABCIResponses, error)
	Validators(height int64) (*tmtypes.ValidatorSet, error)
	ConsensusParams(height int64) (tmproto.ConsensusParams, error)
	StoreGet(height int64, store string, key []byte) ([]byte, error)
}

// API exposes the DeBank trace_debankBlock emitter over JSON-RPC.
type API struct {
	logger     log.Logger
	chain      Chain
	txDecoder  sdk.TxDecoder
	chainID    *big.Int
	executor   replay.Executor
	validators validatorCache
}

func NewAPI(logger log.Logger, chain Chain, txDecoder sdk.TxDecoder, chainID *big.Int, executor replay.Executor) *API {
	return &API{
		logger:    logger.With("module", "trace"),
		chain:     chain,
		txDecoder: txDecoder,
		chainID:   chainID,
		executor:  executor,
	}
}

// DebankBlock replays the full Cosmos block on its parent state and returns the
// DeBank block file, header and validation hash, or only the block file in
// block-file-only mode. Exposed as the JSON-RPC method `trace_debankBlock`.
func (api *API) DebankBlock(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) (json.RawMessage, error) {
	output, err := api.DebankBlockRaw(ctx, blockNrOrHash)
	if err != nil {
		return nil, err
	}
	return encodeOutput(output)
}

func encodeOutput(output *dtypes.DebankOutPut) (json.RawMessage, error) {
	if output.StateDiff == nil {
		return json.Marshal(&struct {
			BlockFile *dtypes.BlockFile `json:"block_file"`
		}{output.BlockFile})
	}
	data, err := rlp.EncodeToBytes(output.StateDiff)
	if err != nil {
		return nil, err
	}
	return json.Marshal(&dtypes.DebankOutPutJs{
		BlockFile:      output.BlockFile,
		Header:         output.Header,
		StateDiff:      data,
		ValidationHash: output.ValidationHash,
	})
}

func (api *API) resolveBlock(id rpctypes.BlockNumberOrHash) (*tmtypes.Block, error) {
	if id.BlockHash != nil {
		return api.chain.BlockByHash(id.BlockHash.Bytes())
	}
	if id.BlockNumber == nil {
		return nil, fmt.Errorf("block number or hash is required")
	}
	height := id.BlockNumber.Int64()
	switch *id.BlockNumber {
	case rpctypes.EthLatestBlockNumber, rpctypes.EthPendingBlockNumber:
		height = api.chain.Head()
	case rpctypes.EthEarliestBlockNumber:
		return nil, fmt.Errorf("can't trace block 0")
	}
	block, err := api.chain.Block(height)
	if err != nil {
		return nil, err
	}
	if block.Height != height {
		return nil, fmt.Errorf("block identity mismatch")
	}
	return block, nil
}

func (api *API) DebankBlockRaw(ctx context.Context, id rpctypes.BlockNumberOrHash) (*dtypes.DebankOutPut, error) {
	return api.block(ctx, id, false, nil)
}

// ReplayHeight executes a height in the standalone verifier, including non-EVM blocks.
func ReplayHeight(ctx context.Context, api *API, height rpctypes.BlockNumber) (json.RawMessage, error) {
	out, err := api.block(ctx, rpctypes.BlockNumberOrHash{BlockNumber: &height}, true, nil)
	if err != nil {
		return nil, err
	}
	return encodeOutput(out)
}

type replayBlockFunc func(context.Context, replay.Request) (*replay.Result, error)

func (api *API) block(ctx context.Context, id rpctypes.BlockNumberOrHash, force bool, run replayBlockFunc) (_ *dtypes.DebankOutPut, err error) {
	block, err := api.resolveBlock(id)
	if err != nil {
		return nil, err
	}
	height := block.Height
	hash := block.Hash()
	defer func() {
		if err != nil {
			err = fmt.Errorf("replay block %d/%X: %w", height, []byte(hash), err)
		}
	}()
	if id.BlockHash != nil && !bytes.Equal(id.BlockHash.Bytes(), hash) {
		return nil, fmt.Errorf("block identity mismatch")
	}
	if api.executor == nil {
		return nil, fmt.Errorf("full Cosmos replay executor unavailable")
	}
	if err = api.executor.CheckReplayHeight(block.ChainID, height); err != nil {
		return nil, err
	}
	blockFileOnly, err := api.executor.BlockFileOnly()
	if err != nil {
		return nil, err
	}
	timing := newPhaseTimer()
	defer func() {
		if err == nil {
			api.logger.Debug("trace_debankBlock timing", timing.fields("height", height)...)
		}
	}()

	results, err := api.chain.Results(height)
	if err != nil {
		return nil, fmt.Errorf("load block results: %w", err)
	}
	if results == nil || len(results.DeliverTxs) != len(block.Txs) || results.BeginBlock == nil || results.EndBlock == nil {
		return nil, fmt.Errorf("incomplete ABCI results")
	}
	timing.mark("results")
	msgs, replayNeeded, err := api.ethMsgs(block, results)
	if err != nil {
		return nil, err
	}
	replayNeeded = replayNeeded || force
	timing.mark("evm_msgs")
	if err = block.ValidateBasic(); err != nil {
		return nil, err
	}
	// Only the next header attests the results and application root of this block.
	next, err := api.chain.Header(height + 1)
	if err != nil {
		return nil, fmt.Errorf("next block: %w", err)
	}
	if next == nil || next.Height != height+1 || next.ChainID != block.ChainID || !bytes.Equal(next.LastBlockID.Hash, hash) {
		return nil, fmt.Errorf("next header parent hash mismatch")
	}
	// Results of a block that is not replayed are attested by the next header only.
	if !replayNeeded && !bytes.Equal(next.LastResultsHash, sm.ABCIResponsesResultsHash(results)) {
		return nil, fmt.Errorf("archived results differ from the next header LastResultsHash")
	}

	timing.mark("next_header")
	// Only a replay needs the parent validators.
	var req replay.Request
	if replayNeeded {
		if req, err = api.replayRequest(block); err != nil {
			return nil, err
		}
	}
	timing.mark("request")
	result, err := api.replayIfNeeded(ctx, req, replayNeeded, run)
	if err != nil {
		return nil, err
	}
	timing.mark("replay")
	if result != nil {
		if err := verifyNative(height, results, result, next); err != nil {
			return nil, err
		}
	}
	timing.mark("verify")

	miner, err := api.miner(block)
	if err != nil {
		return nil, err
	}
	params, err := api.chain.ConsensusParams(height)
	if err != nil {
		return nil, fmt.Errorf("consensus params: %w", err)
	}
	gasLimit := params.Block.MaxGas
	if gasLimit == -1 {
		gasLimit = int64(^uint32(0))
	}
	baseFee, err := api.baseFee(height)
	if err != nil {
		return nil, err
	}
	pblock := dtypes.Block{
		ID:                    common.BytesToHash(hash).Hex(),
		Height:                big.NewInt(height),
		ParentID:              common.BytesToHash(block.LastBlockID.Hash).Hex(),
		BaseFeePerGas:         baseFee,
		Miner:                 strings.ToLower(miner.Hex()),
		GasLimit:              big.NewInt(gasLimit),
		GasUsed:               new(big.Int).SetUint64(blockGasUsed(results)),
		Timestamp:             uint64(block.Time.Unix()),
		ProcessStartTimestamp: time.Now().UnixMilli(),
	}
	receipts, err := receiptsFromResults(msgs, results)
	if err != nil {
		return nil, err
	}
	timing.mark("receipts")
	var observed []replay.Transaction
	if result != nil {
		if observed, err = canonicalObservations(msgs, results, result, receipts); err != nil {
			return nil, err
		}
	}
	blockFile, fromToAddress, err := api.assembleBlockFile(pblock, msgs, observed, receipts)
	if err != nil {
		return nil, err
	}
	timing.mark("assemble")

	evmDenom, err := api.evmDenom(height)
	if err != nil {
		return nil, err
	}
	// StorageContracts also include tx endpoints and native-CRO bank-event addresses.
	for addr := range bankdiff.CoinTouchedAddresses(results, evmDenom) {
		fromToAddress[addr] = struct{}{}
	}
	blockFile.StorageContracts = mergeStorageContracts(fromToAddress, blockFile.StorageContracts)
	timing.mark("storage_contracts")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if blockFileOnly {
		return &dtypes.DebankOutPut{BlockFile: blockFile}, nil
	}
	parent, err := api.parentHeader(block)
	if err != nil {
		return nil, err
	}
	var bloom ethtypes.Bloom
	found := false
	for _, event := range results.EndBlock.Events {
		if event.Type != evmtypes.EventTypeBlockBloom {
			continue
		}
		for _, attr := range event.Attributes {
			if attr.Key == evmtypes.AttributeKeyEthereumBloom && len(attr.Value) == len(bloom) {
				copy(bloom[:], attr.Value)
				found = true
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("missing EndBlock bloom")
	}
	hashes := make([]interface{}, len(msgs))
	for i, m := range msgs {
		hashes[i] = m.tx.AsTransaction().Hash()
	}
	raw := rpctypes.FormatBlock(block.Header, block.Size(), gasLimit, pblock.GasUsed, hashes, bloom, miner, baseFee)
	raw["hash"] = hexutil.Bytes(hash)
	header := dtracer.BuildPilelineBlockHeader(raw)
	var changes dtypes.TransactionStateDiff
	if result != nil {
		changes = result.StateDiff
	} else {
		changes, err = api.chain.StateDiff(height, evmDenom)
		if err != nil {
			return nil, err
		}
	}
	diff := dtracer.BuildBlockStateDiff(common.BytesToHash(parent.AppHash), header.StateRoot, changes)
	return &dtypes.DebankOutPut{Header: header, BlockFile: blockFile, StateDiff: &diff, ValidationHash: blockFile.Validation().ValidationHash}, nil
}

// phaseTimer records the elapsed milliseconds of consecutive request phases.
type phaseTimer struct {
	start, last time.Time
	phases      []interface{}
}

func newPhaseTimer() *phaseTimer {
	now := time.Now()
	return &phaseTimer{start: now, last: now}
}

func (p *phaseTimer) mark(name string) {
	now := time.Now()
	p.phases = append(p.phases, name+"_ms", now.Sub(p.last).Milliseconds())
	p.last = now
}

func (p *phaseTimer) fields(prefix ...interface{}) []interface{} {
	return append(append(prefix, p.phases...), "total_ms", time.Since(p.start).Milliseconds())
}

// replayIfNeeded executes the block unless it has no reported EVM messages;
// such a block yields no traces.
func (api *API) replayIfNeeded(ctx context.Context, req replay.Request, needed bool, run replayBlockFunc) (*replay.Result, error) {
	if !needed {
		return nil, nil
	}
	if run == nil {
		run = api.executor.ReplayBlock
	}
	result, err := run(ctx, req)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Responses == nil {
		return nil, fmt.Errorf("missing replay result")
	}
	return result, nil
}

// blockGasUsed follows the Ethereum RPC of this version: transactions after
// the first one rejected for lack of block gas are not counted.
func blockGasUsed(results *tmstate.ABCIResponses) uint64 {
	var used uint64
	for _, r := range results.DeliverTxs {
		if r.Code == 11 && strings.Contains(r.Log, "no block gas left to run tx: out of gas") {
			break
		}
		used += uint64(r.GasUsed)
	}
	return used
}

// miner is the proposer's operator address as committed at the block; like
// the Ethereum RPC of this version, an unknown proposer is the zero address.
func (api *API) miner(block *tmtypes.Block) (common.Address, error) {
	key := stakingtypes.GetValidatorByConsAddrKey(sdk.ConsAddress(block.ProposerAddress))
	op, err := api.chain.StoreGet(block.Height, stakingtypes.StoreKey, key)
	if err != nil {
		return common.Address{}, err
	}
	return common.BytesToAddress(op), nil
}

// baseFee reads the fee market's base fee as committed at the block, which
// BeginBlock set for it, the way the fee market keeper's GetBaseFee does.
func (api *API) baseFee(height int64) (*big.Int, error) {
	raw, err := api.chain.StoreGet(height, feemarkettypes.StoreKey, feemarkettypes.ParamsKey)
	if err != nil {
		return nil, err
	}
	var params feemarkettypes.Params
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing fee market params at %d", height)
	}
	if err = params.Unmarshal(raw); err != nil {
		return nil, err
	}
	if params.NoBaseFee {
		return new(big.Int), nil
	}
	if !params.BaseFee.IsNil() && params.BaseFee.Sign() != 0 {
		return params.BaseFee.BigInt(), nil
	}
	raw, err = api.chain.StoreGet(height, feemarkettypes.StoreKey, feekeeper.KeyPrefixBaseFeeV1)
	return new(big.Int).SetBytes(raw), err
}

// evmDenom reads the EVM module's denom parameter as committed at the block.
func (api *API) evmDenom(height int64) (string, error) {
	raw, err := api.chain.StoreGet(height, evmtypes.StoreKey, evmtypes.KeyPrefixParams)
	if err != nil {
		return "", err
	}
	var params evmtypes.Params
	if err = params.Unmarshal(raw); err != nil {
		return "", err
	}
	if params.EvmDenom == "" {
		return "", fmt.Errorf("missing EVM denom at %d", height)
	}
	return params.EvmDenom, nil
}

func (api *API) assembleBlockFile(block dtypes.Block, msgs []ethMsg, observed []replay.Transaction, receipts []*receipt) (*dtypes.BlockFile, map[common.Address]struct{}, error) {
	if len(observed) != len(msgs) || len(receipts) != len(msgs) {
		return nil, nil, fmt.Errorf("EVM count mismatch: %d msgs, %d observations, %d receipts", len(msgs), len(observed), len(receipts))
	}
	blockFile := &dtypes.BlockFile{
		Block:            block,
		Events:           make([]dtypes.Event, 0),
		Txs:              make([]dtypes.Transaction, 0),
		Traces:           make([]dtypes.Trace, 0),
		ErrorEvents:      make([]dtypes.Event, 0),
		ErrorTraces:      make([]dtypes.Trace, 0),
		StorageContracts: make([]string, 0),
	}
	fromToAddress := make(map[common.Address]struct{})
	for i, obs := range observed {
		realTx := msgs[i].tx.AsTransaction()
		if obs.Hash != realTx.Hash() {
			return nil, nil, fmt.Errorf("EVM tx[%d] hash mismatch", i)
		}
		traceResult, err := receiptTrace(obs, receipts[i])
		if err != nil {
			return nil, nil, fmt.Errorf("tx[%d]: %w", i, err)
		}
		from := api.sender(msgs[i].tx)
		tx := dtracer.BuildPipelineTransaction(realTx, int64(i), from, new(big.Int).SetUint64(receipts[i].GasUsed), block.BaseFeePerGas, receipts[i].Status)
		blockFile.Txs = append(blockFile.Txs, tx)
		fromToAddress[from] = struct{}{}
		if to := realTx.To(); to != nil {
			fromToAddress[*to] = struct{}{}
		}
		blockFile.Traces = append(blockFile.Traces, traceResult.Traces...)
		blockFile.Events = append(blockFile.Events, traceResult.Events...)
		blockFile.ErrorEvents = append(blockFile.ErrorEvents, traceResult.ErrorEvents...)
		blockFile.ErrorTraces = append(blockFile.ErrorTraces, traceResult.ErrorTraces...)
		blockFile.StorageContracts = append(blockFile.StorageContracts, traceResult.StorageContracts...)
	}
	return blockFile, fromToAddress, nil
}

// sender recovers the signer as the Ethereum RPC of this version does.
func (api *API) sender(msg *evmtypes.MsgEthereumTx) common.Address {
	from, err := msg.GetSenderLegacy(api.chainID)
	if err != nil {
		return common.BytesToAddress(msg.From)
	}
	return from
}

func mergeStorageContracts(addresses map[common.Address]struct{}, storageChange []string) []string {
	set := make(map[common.Address]struct{})
	for _, address := range storageChange {
		set[common.HexToAddress(address)] = struct{}{}
	}
	for addr := range addresses {
		set[addr] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for address := range set {
		out = append(out, strings.ToLower(address.Hex()))
	}
	sort.Strings(out)
	return out
}

func verifyNative(height int64, results *tmstate.ABCIResponses, result *replay.Result, next *tmtypes.Header) error {
	var checks []string
	if err := replay.Verify(results, result.Responses); err != nil {
		checks = append(checks, err.Error())
	}
	if got := sm.ABCIResponsesResultsHash(result.Responses); !bytes.Equal(next.LastResultsHash, got) {
		checks = append(checks, fmt.Sprintf("LastResultsHash: got %X, want %X", got, next.LastResultsHash))
	}
	if !bytes.Equal(next.AppHash, result.AppHash) {
		checks = append(checks, fmt.Sprintf("AppHash: got %X, want %X", result.AppHash, next.AppHash))
	}
	if height == 13184000 && result.Responses.EndBlock.GetConsensusParamUpdates().GetBlock().GetMaxGas() != 60000000 {
		checks = append(checks, fmt.Sprintf("Titan MaxGas mismatch"))
	}
	if len(checks) > 0 {
		return fmt.Errorf("%s", strings.Join(checks, "; "))
	}
	return nil
}

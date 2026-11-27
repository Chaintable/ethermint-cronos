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

	"cosmossdk.io/log"
	"github.com/bytedance/sonic"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	tmtypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/evmos/ethermint/debank/bankdiff"
	"github.com/evmos/ethermint/debank/replay"
	"github.com/evmos/ethermint/debank/statediff"
	dtracer "github.com/evmos/ethermint/debank/tracer"
	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/evmos/ethermint/rpc/backend"
	rpctypes "github.com/evmos/ethermint/rpc/types"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

// API exposes the DeBank trace_debankBlock emitter over JSON-RPC.
type API struct {
	ctx         *server.Context
	logger      log.Logger
	backend     backend.EVMBackend
	clientCtx   client.Context
	queryClient *rpctypes.QueryClient
	stateSource statediff.StateChangeSource
	executor    replay.Executor
	validators  validatorCache
}

// NewAPI creates the trace namespace API.
func NewAPI(
	ctx *server.Context,
	logger log.Logger,
	backend backend.EVMBackend,
	clientCtx client.Context,
	stateSource statediff.StateChangeSource,
	executor replay.Executor,
) *API {
	return &API{
		ctx:         ctx,
		logger:      logger.With("module", "trace"),
		backend:     backend,
		clientCtx:   clientCtx,
		queryClient: rpctypes.NewQueryClient(clientCtx),
		stateSource: stateSource,
		executor:    executor,
	}
}

// DebankBlock replays the full Cosmos block on its parent state and returns the
// DeBank block file, header, RLP-encoded state diff and validation hash, or only
// the block file in block-file-only mode. Exposed as the JSON-RPC method
// `trace_debankBlock`.
func (api *API) DebankBlock(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) (json.RawMessage, error) {
	output, err := api.DebankBlockRaw(ctx, blockNrOrHash)
	if err != nil {
		return nil, err
	}
	return encodeOutput(output)
}

// encodeOutput marshals with sonic instead of the RPC codec's encoding/json,
// which dominated CPU for large responses. ConfigStd keeps the bytes identical.
func encodeOutput(output *dtypes.DebankOutPut) (json.RawMessage, error) {
	if output.StateDiff == nil {
		return sonic.ConfigStd.Marshal(&struct {
			BlockFile *dtypes.BlockFile `json:"block_file"`
		}{output.BlockFile})
	}
	data, err := rlp.EncodeToBytes(output.StateDiff)
	if err != nil {
		return nil, err
	}
	return sonic.ConfigStd.Marshal(&dtypes.DebankOutPutJs{
		BlockFile:      output.BlockFile,
		Header:         output.Header,
		StateDiff:      data,
		ValidationHash: output.ValidationHash,
	})
}

func (api *API) DebankBlockRaw(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) (*dtypes.DebankOutPut, error) {
	return api.debankBlockRaw(ctx, blockNrOrHash, nil)
}

type replayBlockFunc func(context.Context, replay.Request) (*replay.Result, error)

func (api *API) debankBlockRaw(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash, run replayBlockFunc) (_ *dtypes.DebankOutPut, err error) {
	blockHeight, err := api.backend.BlockNumberFromTendermint(blockNrOrHash)
	if err != nil {
		return nil, err
	}
	if blockHeight == 0 {
		return nil, fmt.Errorf("can't trace block 0")
	}

	resBlock, err := api.backend.TendermintBlockByNumber(blockHeight)
	if err != nil {
		return nil, fmt.Errorf("load block %d: %w", blockHeight, err)
	}
	if resBlock == nil || resBlock.Block == nil {
		return nil, fmt.Errorf("cannot trace nil block")
	}

	requestedHeight := blockHeight
	blockHeight = rpctypes.BlockNumber(resBlock.Block.Height)
	hash := resBlock.Block.Hash()
	defer func() {
		if err != nil {
			err = fmt.Errorf("replay block %d/%X: %w", blockHeight, []byte(hash), err)
		}
	}()
	if requestedHeight > 0 && requestedHeight != blockHeight || !bytes.Equal(resBlock.BlockID.Hash, hash) || blockNrOrHash.BlockHash != nil && !bytes.Equal(blockNrOrHash.BlockHash.Bytes(), hash) {
		return nil, fmt.Errorf("block identity mismatch")
	}
	if api.executor == nil {
		return nil, fmt.Errorf("full Cosmos replay executor unavailable")
	}
	if err = api.executor.CheckReplayHeight(resBlock.Block.ChainID, int64(blockHeight)); err != nil {
		return nil, err
	}
	blockFileOnly, err := api.executor.BlockFileOnly()
	if err != nil {
		return nil, err
	}
	timing := newPhaseTimer()
	defer func() {
		if err == nil {
			api.logger.Debug("trace_debankBlock timing", timing.fields("height", int64(blockHeight))...)
		}
	}()

	blockRes, err := api.backend.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		api.logger.Debug("failed to fetch block result from Tendermint", "height", blockHeight, "error", err.Error())
		return nil, fmt.Errorf("fetch block results: %w", err)
	}
	if blockRes == nil || blockRes.Height != int64(blockHeight) || len(blockRes.TxsResults) != len(resBlock.Block.Txs) {
		return nil, fmt.Errorf("incomplete ABCI results")
	}
	timing.mark("results")
	messages, err := committedEVMMessages(api.clientCtx.TxConfig.TxDecoder(), resBlock.Block.Txs, blockRes.TxsResults)
	if err != nil {
		return nil, err
	}
	timing.mark("evm_msgs")
	if err = resBlock.Block.ValidateBasic(); err != nil {
		return nil, err
	}
	// Only a replay needs the parent validators; only a state diff needs the parent root.
	var req replay.Request
	var parentRoot []byte
	if len(messages) > 0 {
		req, parentRoot, err = api.replayRequest(ctx, resBlock.Block)
	} else if !blockFileOnly {
		var parent *tmtypes.Header
		if parent, err = api.parentHeader(ctx, resBlock.Block); err == nil {
			parentRoot = parent.AppHash
		}
	}
	if err != nil {
		return nil, err
	}
	timing.mark("request")
	result, err := api.replayIfNeeded(ctx, req, blockRes, len(messages), run)
	if err != nil {
		return nil, err
	}
	timing.mark("replay")
	// Only the next header attests the application root of this block.
	next, err := api.nextBlock(blockHeight)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(next.Block.LastBlockID.Hash, hash) {
		return nil, fmt.Errorf("next header parent hash mismatch")
	}
	if len(blockRes.AppHash) != common.HashLength || !bytes.Equal(next.Block.AppHash, blockRes.AppHash) {
		return nil, fmt.Errorf("next header AppHash mismatch")
	}
	timing.mark("next_header")
	block, err := api.backend.RPCBlockFromTendermintBlock(resBlock, blockRes, true)
	if err != nil {
		api.logger.Debug("RPCBlockFromTendermintBlock failed", "height", blockHeight, "error", err.Error())
		return nil, err
	}
	transactions, ok := block["transactions"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("malformed RPC block transactions")
	}
	timing.mark("rpc_block")
	stateHeader := dtracer.BuildPilelineBlockHeader(block)
	// London is active throughout v1.5; the keeper returns zero even if the fee market is disabled.
	if stateHeader.BaseFeePerGas == nil {
		return nil, fmt.Errorf("RPC block is missing baseFeePerGas")
	}
	baseFee := (*big.Int)(stateHeader.BaseFeePerGas)

	var observed []replay.Transaction
	if result != nil {
		if observed, err = canonicalObservations(messages, blockRes, result); err != nil {
			return nil, err
		}
	}
	blockFile, fromToAddress, err := assembleBlockFile(block, transactions, messages, baseFee, observed)
	if err != nil {
		return nil, err
	}
	timing.mark("assemble")

	evmDenom, err := api.evmDenom(blockHeight)
	if err != nil {
		return nil, err
	}
	// StorageContracts also include tx endpoints and native-CRO bank-event addresses.
	for addr := range bankdiff.CoinTouchedAddresses(blockRes, evmDenom) {
		fromToAddress[addr] = struct{}{}
	}
	blockFile.StorageContracts = mergeStorageContracts(fromToAddress, blockFile.StorageContracts)
	timing.mark("storage_contracts")
	if blockFileOnly {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &dtypes.DebankOutPut{BlockFile: blockFile}, nil
	}

	previousDenom, err := api.evmDenom(blockHeight - 1)
	if err != nil {
		return nil, err
	}
	if previousDenom != evmDenom {
		return nil, fmt.Errorf(
			"evm denom changed at block %d from %s to %s", blockHeight, previousDenom, evmDenom,
		)
	}
	canonical, err := statediff.CanonicalStateAt(api.stateSource, int64(blockHeight), evmDenom)
	if err != nil {
		return nil, fmt.Errorf("load canonical state diff for block %d: %w", blockHeight, err)
	}
	stateDiff := dtracer.BuildBlockStateDiff(common.BytesToHash(parentRoot), stateHeader.StateRoot, canonical)
	timing.mark("state_diff")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &dtypes.DebankOutPut{
		BlockFile:      blockFile,
		Header:         stateHeader,
		StateDiff:      &stateDiff,
		ValidationHash: blockFile.Validation().ValidationHash,
	}, nil
}

// phaseTimer records the elapsed milliseconds of consecutive request phases.
type phaseTimer struct {
	start, last time.Time
	phases      []any
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

func (p *phaseTimer) fields(prefix ...any) []any {
	return append(append(prefix, p.phases...), "total_ms", time.Since(p.start).Milliseconds())
}

// replayIfNeeded executes the block unless it has no committed EVM messages:
// such a block yields no traces, and its block file and state diff come from
// the block, its results and the committed state.
func (api *API) replayIfNeeded(ctx context.Context, req replay.Request, results *ctypes.ResultBlockResults, ethMsgs int, run replayBlockFunc) (*replay.Result, error) {
	if ethMsgs == 0 {
		return nil, nil
	}
	if run == nil {
		run = api.executor.ReplayBlock
	}
	result, err := run(ctx, req)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("missing replay result")
	}
	if err = replay.Verify(results, result.Finalize); err != nil {
		return nil, err
	}
	return result, nil
}

// nextBlock returns height+1, failing while it is not yet committed.
func (api *API) nextBlock(height rpctypes.BlockNumber) (*ctypes.ResultBlock, error) {
	head, err := api.backend.BlockNumber()
	if err != nil {
		return nil, err
	}
	if uint64(head) <= uint64(height) {
		return nil, fmt.Errorf("next block %d not committed yet", height+1)
	}
	next, err := api.backend.TendermintBlockByNumber(height + 1)
	if err != nil {
		return nil, err
	}
	if next == nil || next.Block == nil || next.Block.Height != int64(height+1) {
		return nil, fmt.Errorf("missing next block %d", height+1)
	}
	return next, nil
}

func assembleBlockFile(block map[string]interface{}, transactions []interface{}, messages []committedEVMMessage, baseFee *big.Int, observed []replay.Transaction) (*dtypes.BlockFile, map[common.Address]struct{}, error) {
	if len(messages) != len(transactions) || len(observed) != len(messages) {
		return nil, nil, fmt.Errorf("EVM count mismatch: %d messages, %d RPC transactions, %d observations", len(messages), len(transactions), len(observed))
	}
	blockFile := &dtypes.BlockFile{
		Block:            dtracer.BuildPipelineBlock(block),
		Events:           make([]dtypes.Event, 0),
		Txs:              make([]dtypes.Transaction, 0),
		Traces:           make([]dtypes.Trace, 0),
		ErrorEvents:      make([]dtypes.Event, 0),
		ErrorTraces:      make([]dtypes.Trace, 0),
		StorageContracts: make([]string, 0),
	}
	fromToAddress := make(map[common.Address]struct{})

	for i, obs := range observed {
		realTx := messages[i].tx
		rt, ok := transactions[i].(*rpctypes.RPCTransaction)
		if !ok || rt == nil || rt.Hash != realTx.Hash() || rt.TransactionIndex == nil || uint64(*rt.TransactionIndex) != uint64(i) {
			return nil, nil, fmt.Errorf("RPC transaction at index %d differs from signed message", i)
		}
		receipt := &messages[i].receipt
		traceResult, err := receiptTrace(obs, receipt)
		if err != nil {
			return nil, nil, fmt.Errorf("tx[%d]: %w", i, err)
		}
		tx := dtracer.BuildPipelineTransaction(realTx, int64(i), rt.From, new(big.Int).SetUint64(receipt.GasUsed), baseFee, receipt.Status)
		blockFile.Txs = append(blockFile.Txs, tx)
		fromToAddress[rt.From] = struct{}{}
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

func mergeStorageContracts(addresses map[common.Address]struct{}, storageChange []string) []string {
	storageChangeMap := make(map[common.Address]struct{})
	for _, address := range storageChange {
		storageChangeMap[common.HexToAddress(address)] = struct{}{}
	}
	for addr := range addresses {
		storageChangeMap[addr] = struct{}{}
	}

	resStorageChange := make([]string, 0, len(storageChangeMap))
	for address := range storageChangeMap {
		resStorageChange = append(resStorageChange, strings.ToLower(address.Hex()))
	}
	sort.Strings(resStorageChange)
	return resStorageChange
}

// evmDenom returns the EVM denom (native token) at the given height.
func (api *API) evmDenom(height rpctypes.BlockNumber) (string, error) {
	res, err := api.queryClient.Params(rpctypes.ContextWithHeight(int64(height)), &evmtypes.QueryParamsRequest{})
	if err != nil {
		return "", err
	}
	return res.Params.EvmDenom, nil
}

package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/evmos/ethermint/debank/replay"

	"cosmossdk.io/log/v2"
	"github.com/bytedance/sonic"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/go-ethereum/rlp"

	"github.com/evmos/ethermint/debank/bankdiff"
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

// DebankBlock replays block N at the N-1 state and returns the DeBank block
// file + header + RLP-encoded state diff + validation hash. Exposed as the
// JSON-RPC method `trace_debankBlock`.
func (api *API) DebankBlock(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) (json.RawMessage, error) {
	output, err := api.DebankBlockRaw(ctx, blockNrOrHash)
	if err != nil {
		return nil, err
	}
	data, err := rlp.EncodeToBytes(output.StateDiff)
	if err != nil {
		return nil, err
	}
	// Marshal here with sonic (SIMD/JIT) and return raw JSON, instead of letting
	// go-ethereum's RPC codec marshal the struct with reflection-based
	// encoding/json — the trace response (hundreds of traces/events) is the top
	// CPU consumer. ConfigStd keeps the output byte-compatible with the stdlib.
	return sonic.ConfigStd.Marshal(&dtypes.DebankOutPutJs{
		BlockFile:      output.BlockFile,
		Header:         output.Header,
		StateDiff:      data,
		ValidationHash: output.ValidationHash,
	})
}

func (api *API) DebankBlockRaw(ctx context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) (_ *dtypes.DebankOutPut, err error) {
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

	blockRes, err := api.backend.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		api.logger.Debug("failed to fetch block result from Tendermint", "height", blockHeight, "error", err.Error())
		return nil, fmt.Errorf("fetch block results: %w", err)
	}
	if blockRes == nil || blockRes.Height != int64(blockHeight) || len(blockRes.TxsResults) != len(resBlock.Block.Txs) {
		return nil, fmt.Errorf("incomplete ABCI results")
	}
	req, parentRoot, err := api.replayRequest(ctx, resBlock.Block)
	if err != nil {
		return nil, err
	}
	result, err := api.executor.ReplayBlock(ctx, req)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("missing replay result")
	}
	if err = replay.Verify(blockRes, result.Finalize); err != nil {
		return nil, err
	}
	// Only the next header attests the application root of this block.
	next, err := api.nextBlock(blockHeight)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(next.Block.LastBlockID.Hash, hash) || !bytes.Equal(next.Block.AppHash, result.Finalize.AppHash) {
		return nil, fmt.Errorf("next header AppHash or parent hash mismatch")
	}
	block, err := api.backend.RPCBlockFromTendermintBlock(resBlock, blockRes, true)
	if err != nil {
		api.logger.Debug("RPCBlockFromTendermintBlock failed", "height", blockHeight, "error", err.Error())
		return nil, err
	}
	transactions, ok := block["transactions"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("malformed RPC block transactions")
	}
	ethMsgs, err := api.backend.EthMsgsFromTendermintBlock(resBlock, blockRes)
	if err != nil {
		return nil, err
	}
	stateHeader := dtracer.BuildPilelineBlockHeader(block)
	var baseFee *big.Int
	if stateHeader.BaseFeePerGas != nil {
		baseFee = (*big.Int)(stateHeader.BaseFeePerGas)
	}

	consensus, err := api.loadConsensus(blockHeight)
	if err != nil {
		return nil, err
	}
	observed, err := api.canonicalObservations(resBlock, blockRes, result, consensus)
	if err != nil {
		return nil, err
	}
	blockFile, fromToAddress, err := assembleBlockFile(block, transactions, ethMsgs, baseFee, observed, consensus)
	if err != nil {
		return nil, err
	}

	evmDenom, err := api.evmDenom(blockHeight)
	if err != nil {
		return nil, err
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

	// Preserve the existing BlockFile.StorageContracts discovery from tx and
	// native-CRO bank-event addresses; it no longer supplies stateDiff accounts.
	for addr := range bankdiff.CoinTouchedAddresses(blockRes, evmDenom) {
		fromToAddress[addr] = struct{}{}
	}

	blockFile.StorageContracts = mergeStorageContracts(fromToAddress, blockFile.StorageContracts)

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
	if next == nil || next.Block == nil {
		return nil, fmt.Errorf("missing next block %d", height+1)
	}
	return next, nil
}

func assembleBlockFile(block map[string]interface{}, transactions []interface{}, ethMsgs []*evmtypes.MsgEthereumTx, baseFee *big.Int, observed []includedMessage, consensus map[string]*consensusReceipt) (*dtypes.BlockFile, map[common.Address]struct{}, error) {
	if len(ethMsgs) != len(transactions) || len(observed) != len(ethMsgs) || len(consensus) != len(ethMsgs) {
		return nil, nil, fmt.Errorf("EVM count mismatch: %d msgs, %d RPC txs, %d observations, %d receipts", len(ethMsgs), len(transactions), len(observed), len(consensus))
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
		realTx := ethMsgs[i].AsTransaction()
		rt, ok := transactions[i].(*rpctypes.RPCTransaction)
		if !ok || rt == nil || rt.Hash != obs.Hash || obs.Hash != realTx.Hash() {
			return nil, nil, fmt.Errorf("EVM tx[%d] hash mismatch", i)
		}
		receipt := consensus[strings.ToLower(obs.Hash.Hex())]
		if receipt == nil {
			return nil, nil, fmt.Errorf("tx[%d] missing receipt", i)
		}
		traceResult, err := obs.receiptTrace(receipt)
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

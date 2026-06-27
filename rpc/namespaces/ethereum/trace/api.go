package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"cosmossdk.io/log"
	"github.com/bytedance/sonic"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"

	"github.com/evmos/ethermint/debank/bankdiff"
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
}

// NewAPI creates the trace namespace API.
func NewAPI(
	ctx *server.Context,
	logger log.Logger,
	backend backend.EVMBackend,
	clientCtx client.Context,
) *API {
	return &API{
		ctx:         ctx,
		logger:      logger.With("module", "trace"),
		backend:     backend,
		clientCtx:   clientCtx,
		queryClient: rpctypes.NewQueryClient(clientCtx),
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

func (api *API) DebankBlockRaw(_ context.Context, blockNrOrHash rpctypes.BlockNumberOrHash) (*dtypes.DebankOutPut, error) {
	blockHeight, err := api.backend.BlockNumberFromTendermint(blockNrOrHash)
	if err != nil {
		return nil, err
	}
	if blockHeight == 0 {
		return nil, fmt.Errorf("can't trace block 0")
	}

	resBlock, err := api.backend.TendermintBlockByNumber(blockHeight)
	if err != nil {
		return nil, nil
	}
	if resBlock == nil || resBlock.Block == nil {
		return nil, fmt.Errorf("cannot trace nil block")
	}

	blockRes, err := api.backend.TendermintBlockResultByNumber(&resBlock.Block.Height)
	if err != nil {
		api.logger.Debug("failed to fetch block result from Tendermint", "height", blockHeight, "error", err.Error())
		return nil, fmt.Errorf("failed to fetch block result from Tendermint")
	}
	block, err := api.backend.RPCBlockFromTendermintBlock(resBlock, blockRes, true)
	if err != nil {
		api.logger.Debug("RPCBlockFromTendermintBlock failed", "height", blockHeight, "error", err.Error())
		return nil, err
	}
	if blockHeight == 1 {
		return api.onGenesisBlock(block)
	}

	// transactions[i], ethMsgs[i] and traceResults[i] are all produced by the
	// same EthMsgsFromTendermintBlock flattening/filtering, so they align 1:1.
	transactions, _ := block["transactions"].([]interface{})
	ethMsgs := api.backend.EthMsgsFromTendermintBlock(resBlock, blockRes)
	stateHeader := dtracer.BuildPilelineBlockHeader(block)
	parentHeader, err := api.backend.HeaderByNumber(blockHeight - 1)
	if err != nil {
		return nil, err
	}
	var baseFee *big.Int
	if stateHeader.BaseFeePerGas != nil {
		baseFee = (*big.Int)(stateHeader.BaseFeePerGas)
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

	// TraceReplay=true: tell the keeper this replays an already-included tx, so a
	// legacy gas-accounting mismatch on the upfront fee deduction doesn't abort the
	// trace (Cronos v1.7.8 / ethermint PR #997). Without it, affected historical txs
	// would error and get silently skipped below, losing their trace/event/state-diff.
	traceResults, err := api.backend.TraceBlock(blockHeight,
		&rpctypes.TraceConfig{TraceConfig: evmtypes.TraceConfig{Tracer: dtracer.Name, TraceReplay: true}}, resBlock)
	if err != nil {
		return nil, err
	}

	transactionStates := make([]dtypes.TransactionStateDiff, 0)
	fromToAddress := make(map[common.Address]struct{})
	for i, result := range traceResults {
		if result.Error != "" {
			api.logger.Error("trace result error", "error", result.Error)
			continue
		}
		decoded, err := json.Marshal(result.Result)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		var traceResult dtypes.TraceResult
		if err = json.Unmarshal(decoded, &traceResult); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("trace result parse error: %v", err))
		}

		// Build the per-tx Transaction here (not in the tracer): Cronos's standard
		// tracing.Hooks cannot supply nonce/gasPrice/status. The real tx provides
		// nonce/gasPrice/feeCap; from is the recovered sender; gasUsed/status come
		// from the tracer's root trace.
		if i < len(ethMsgs) {
			realTx := ethMsgs[i].AsTransaction()
			from := senderAt(transactions, i)
			gasUsed, success := rootGasAndStatus(traceResult)
			blockFile.Txs = append(blockFile.Txs,
				dtracer.BuildPipelineTransaction(realTx, int64(i), from, new(big.Int).SetUint64(gasUsed), baseFee, success))
			fromToAddress[from] = struct{}{}
			if to := realTx.To(); to != nil {
				fromToAddress[*to] = struct{}{}
			}
		}

		blockFile.Traces = append(blockFile.Traces, traceResult.Traces...)
		blockFile.Events = append(blockFile.Events, traceResult.Events...)
		blockFile.ErrorEvents = append(blockFile.ErrorEvents, traceResult.ErrorEvents...)
		blockFile.ErrorTraces = append(blockFile.ErrorTraces, traceResult.ErrorTraces...)
		blockFile.StorageContracts = append(blockFile.StorageContracts, traceResult.StorageContracts...)
		transactionStates = append(transactionStates, traceResult.StateDiff)
	}

	for i := range blockFile.Events {
		blockFile.Events[i].LogIndex = int64(i)
	}

	stateDiff := dtracer.BuildBlockStateDiff(parentHeader.Root, stateHeader.StateRoot, transactionStates)

	// Native CRO balance channel: bank events surface addresses the EVM tracer
	// never sees (gas/fee, plain transfers, CRC20 convert, IBC, module accounts).
	evmDenom, err := api.evmDenom(blockHeight)
	if err != nil {
		return nil, err
	}
	for addr := range bankdiff.CoinTouchedAddresses(blockRes, evmDenom) {
		fromToAddress[addr] = struct{}{}
	}

	newAccounts, storageContracts, err := api.fillAbsoluteState(fromToAddress, stateDiff.NewAccounts, blockFile.StorageContracts, blockHeight)
	if err != nil {
		return nil, err
	}
	stateDiff.NewAccounts = newAccounts
	blockFile.StorageContracts = storageContracts

	return &dtypes.DebankOutPut{
		BlockFile:      blockFile,
		Header:         stateHeader,
		StateDiff:      &stateDiff,
		ValidationHash: blockFile.Validation().ValidationHash,
	}, nil
}

// fillAbsoluteState overwrites/adds NewAccount entries with the authoritative
// post-N balance/nonce/code (via GetBalance/GetTransactionCount/GetCode at N)
// for every discovered address: tx from/to plus bank-event touched addresses.
// This is cosmos-evm's addGasUsedStateDiff generalized to the bank channel.
func (api *API) fillAbsoluteState(addresses map[common.Address]struct{}, newAccount []dtypes.NewAccount, storageChange []string, number rpctypes.BlockNumber) ([]dtypes.NewAccount, []string, error) {
	newAccountMap := make(map[common.Hash]dtypes.NewAccount)
	storageChangeMap := make(map[common.Address]struct{})
	for _, account := range newAccount {
		newAccountMap[account.Address] = account
	}
	for _, address := range storageChange {
		storageChangeMap[common.HexToAddress(address)] = struct{}{}
	}
	for addr := range addresses {
		addrHash := crypto.Keccak256Hash(addr.Bytes())
		balance, err := api.backend.GetBalance(addr, rpctypes.BlockNumberOrHash{BlockNumber: &number})
		if err != nil {
			return nil, nil, err
		}
		nonce, err := api.backend.GetTransactionCount(addr, number)
		if err != nil {
			return nil, nil, err
		}
		code, err := api.backend.GetCode(addr, rpctypes.BlockNumberOrHash{BlockNumber: &number})
		if err != nil {
			return nil, nil, err
		}
		newAccountMap[addrHash] = dtypes.NewAccount{
			Address:  addrHash,
			Balance:  uint256.MustFromBig((*big.Int)(balance)),
			Nonce:    uint64(*nonce),
			CodeHash: crypto.Keccak256Hash(code),
		}
		storageChangeMap[addr] = struct{}{}
	}

	resNewAccount := make([]dtypes.NewAccount, 0, len(newAccountMap))
	resStorageChange := make([]string, 0, len(storageChangeMap))
	for _, acc := range newAccountMap {
		resNewAccount = append(resNewAccount, acc)
	}
	for address := range storageChangeMap {
		resStorageChange = append(resStorageChange, strings.ToLower(address.Hex()))
	}
	return resNewAccount, resStorageChange, nil
}

// evmDenom returns the EVM denom (native token) at the given height.
func (api *API) evmDenom(height rpctypes.BlockNumber) (string, error) {
	res, err := api.queryClient.Params(rpctypes.ContextWithHeight(int64(height)), &evmtypes.QueryParamsRequest{})
	if err != nil {
		return "", err
	}
	return res.Params.EvmDenom, nil
}

// senderAt returns the recovered sender of the i-th block transaction.
func senderAt(transactions []interface{}, i int) common.Address {
	if i >= len(transactions) {
		return common.Address{}
	}
	if rt, ok := transactions[i].(*rpctypes.RPCTransaction); ok {
		return rt.From
	}
	return common.Address{}
}

// rootGasAndStatus extracts the tx-level gas used and success from the tracer's
// root trace (TraceAddress == []): a successful root lands in Traces, a reverted
// one in ErrorTraces.
func rootGasAndStatus(tr dtypes.TraceResult) (uint64, bool) {
	for i := range tr.Traces {
		if len(tr.Traces[i].TraceAddress) == 0 {
			return gasOf(tr.Traces[i]), true
		}
	}
	for i := range tr.ErrorTraces {
		if len(tr.ErrorTraces[i].TraceAddress) == 0 {
			return gasOf(tr.ErrorTraces[i]), false
		}
	}
	return 0, true
}

func gasOf(t dtypes.Trace) uint64 {
	if t.GasUsed == nil {
		return 0
	}
	return t.GasUsed.Uint64()
}

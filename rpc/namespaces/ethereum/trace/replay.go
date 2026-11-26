package trace

import (
	"bytes"
	"fmt"
	"sync"

	abci "github.com/cometbft/cometbft/abci/types"
	tmstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tmtypes "github.com/cometbft/cometbft/types"

	"github.com/evmos/ethermint/debank/replay"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

// ethMsg is an EVM message the current Cronos RPC reports: from a successful
// Cosmos transaction, or, as a failed transaction, from one that exceeded the
// block gas limit.
type ethMsg struct {
	cosmos, msg int
	tx          *evmtypes.MsgEthereumTx
}

func (api *API) ethMsgs(block *tmtypes.Block, results *tmstate.ABCIResponses) ([]ethMsg, bool, error) {
	var out []ethMsg
	var hasEVM bool
	for i, raw := range block.Txs {
		if results.DeliverTxs[i] == nil {
			return nil, false, fmt.Errorf("tx[%d]: missing archive result", i)
		}

		tx, err := api.txDecoder(raw)
		if err != nil {
			return nil, false, fmt.Errorf("tx[%d]: %w", i, err)
		}
		for j, m := range tx.GetMsgs() {
			if eth, ok := m.(*evmtypes.MsgEthereumTx); ok {
				hasEVM = true
				if !reported(results.DeliverTxs[i]) {
					continue
				}
				out = append(out, ethMsg{cosmos: i, msg: j, tx: eth})
			}
		}
	}
	return out, hasEVM, nil
}

func reported(r *abci.ResponseDeliverTx) bool {
	return r.Code == 0 || replay.ExceedsBlockGas(r)
}

// parentHeader returns header H-1 after checking that block H references it.
func (api *API) parentHeader(block *tmtypes.Block) (*tmtypes.Header, error) {
	header, err := api.chain.Header(block.Height - 1)
	if err != nil {
		return nil, fmt.Errorf("parent header: %w", err)
	}
	if !bytes.Equal(header.Hash(), block.LastBlockID.Hash) {
		return nil, fmt.Errorf("parent header hash mismatch")
	}
	return header, nil
}

// replayRequest builds BeginBlock as CometBFT v0.37 does in execBlockOnProxyApp.
func (api *API) replayRequest(block *tmtypes.Block) (replay.Request, error) {
	if block.Height < 2 {
		return replay.Request{}, fmt.Errorf("block %d has no parent block", block.Height)
	}
	header, err := api.parentHeader(block)
	if err != nil {
		return replay.Request{}, err
	}
	vals, err := api.parentValidators(block, header)
	if err != nil {
		return replay.Request{}, err
	}
	votes := make([]abci.VoteInfo, len(vals.Validators))
	for i, val := range vals.Validators {
		sig := block.LastCommit.Signatures[i]
		if !sig.Absent() && !bytes.Equal(sig.ValidatorAddress, val.Address) {
			return replay.Request{}, fmt.Errorf("parent validator order mismatch at %d", i)
		}
		votes[i] = abci.VoteInfo{
			Validator:       tmtypes.TM2PB.Validator(val),
			SignedLastBlock: !block.LastCommit.Signatures[i].Absent(),
		}
	}
	txs := make([][]byte, len(block.Txs))
	for i, tx := range block.Txs {
		txs[i] = tx
	}
	byzVals := make([]abci.Misbehavior, 0)
	for _, ev := range block.Evidence.Evidence {
		byzVals = append(byzVals, ev.ABCI()...)
	}
	return replay.Request{ChainID: block.ChainID, ParentHash: block.AppHash, BeginBlock: &abci.RequestBeginBlock{
		Hash:                block.Hash(),
		Header:              *block.Header.ToProto(),
		LastCommitInfo:      abci.CommitInfo{Round: block.LastCommit.Round, Votes: votes},
		ByzantineValidators: byzVals,
	}, Txs: txs}, nil
}

// validatorCache keeps the last verified validator set; sets rarely change
// between consecutive blocks and loading one replays proposer priorities.
type validatorCache struct {
	mu   sync.Mutex
	hash []byte
	vals *tmtypes.ValidatorSet
}

// parentValidators returns the H-1 validator set whose hash the parent header
// commits to. Proposer priorities are not part of the hash or the request.
func (api *API) parentValidators(block *tmtypes.Block, header *tmtypes.Header) (*tmtypes.ValidatorSet, error) {
	api.validators.mu.Lock()
	cached, hash := api.validators.vals, api.validators.hash
	api.validators.mu.Unlock()
	if cached != nil && bytes.Equal(hash, header.ValidatorsHash) && cached.Size() == block.LastCommit.Size() {
		return cached, nil
	}
	vals, err := api.chain.Validators(block.Height - 1)
	if err != nil {
		return nil, fmt.Errorf("validators at %d: %w", block.Height-1, err)
	}
	if !bytes.Equal(vals.Hash(), header.ValidatorsHash) {
		return nil, fmt.Errorf("parent validator hash mismatch")
	}
	if vals.Size() != block.LastCommit.Size() {
		return nil, fmt.Errorf("commit size %d differs from %d parent validators", block.LastCommit.Size(), vals.Size())
	}
	api.validators.mu.Lock()
	api.validators.hash, api.validators.vals = header.ValidatorsHash, vals
	api.validators.mu.Unlock()
	return vals, nil
}

// canonicalObservations matches each reported EVM message to its observation
// and checks the charged gas and status against the receipt. Messages of a
// failed Cosmos transaction are discarded with it, and so are messages that
// another message such as authz MsgExec executed, which the RPC does not list.
func canonicalObservations(msgs []ethMsg, results *tmstate.ABCIResponses, replayed *replay.Result, receipts []*receipt) ([]replay.Transaction, error) {
	type position struct{ cosmos, msg int }
	listed := make(map[position]bool, len(msgs))
	for _, m := range msgs {
		listed[position{m.cosmos, m.msg}] = true
	}
	var selected []replay.Transaction
	for _, obs := range replayed.Transactions {
		if obs.CosmosIndex < 0 || obs.CosmosIndex >= len(results.DeliverTxs) {
			return nil, fmt.Errorf("invalid observed Cosmos index %d", obs.CosmosIndex)
		}
		if listed[position{obs.CosmosIndex, obs.MessageIndex}] {
			selected = append(selected, obs)
		}
	}
	matched := make([]replay.Transaction, len(msgs))
	offset := 0
	for i, m := range msgs {
		hash := m.tx.AsTransaction().Hash()
		if offset == len(selected) || selected[offset].CosmosIndex != m.cosmos || selected[offset].MessageIndex != m.msg {
			if receipts[i].ExceededBlockGas {
				matched[i] = replay.Transaction{CosmosIndex: m.cosmos, MessageIndex: m.msg, Hash: hash, Failed: true}
				continue
			}
			return nil, fmt.Errorf("tx[%d]/msg[%d]: missing native observation", m.cosmos, m.msg)
		}
		obs := selected[offset]
		offset++
		matched[i] = obs
		if obs.Hash != hash || obs.Error != "" || len(obs.Trace) == 0 {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: invalid observation for %s: %s", m.cosmos, m.msg, hash, obs.Error)
		}
		rc := receipts[i]
		if rc.ExceededBlockGas {
			if !obs.Failed {
				return nil, fmt.Errorf("tx[%d]/msg[%d]: %s exceeded the block gas limit but was not observed as failed", m.cosmos, m.msg, hash)
			}
			continue
		}
		if obs.GasUsed != rc.ExecutionGas || obs.Failed == rc.Status {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: receipt mismatch for %s", m.cosmos, m.msg, hash)
		}
	}
	if offset != len(selected) {
		return nil, fmt.Errorf("unexpected native observations")
	}
	return matched, nil
}

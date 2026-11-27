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

func (api *API) ethMsgs(block *tmtypes.Block, results *tmstate.ABCIResponses) ([]ethMsg, error) {
	var out []ethMsg
	for i, raw := range block.Txs {
		if !reported(results.DeliverTxs[i]) {
			continue
		}
		tx, err := api.txDecoder(raw)
		if err != nil {
			return nil, fmt.Errorf("tx[%d]: %w", i, err)
		}
		for j, m := range tx.GetMsgs() {
			if eth, ok := m.(*evmtypes.MsgEthereumTx); ok {
				out = append(out, ethMsg{cosmos: i, msg: j, tx: eth})
			}
		}
	}
	return out, nil
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
		votes[i] = abci.VoteInfo{
			Validator:       tmtypes.TM2PB.Validator(val),
			SignedLastBlock: !block.LastCommit.Signatures[i].Absent(),
		}
	}
	txs := make([][]byte, len(block.Txs))
	for i, tx := range block.Txs {
		txs[i] = tx
	}
	return replay.Request{ChainID: block.ChainID, ParentHash: block.AppHash, BeginBlock: &abci.RequestBeginBlock{
		Hash:                block.Hash(),
		Header:              *block.Header.ToProto(),
		LastCommitInfo:      abci.CommitInfo{Round: block.LastCommit.Round, Votes: votes},
		ByzantineValidators: block.Evidence.Evidence.ToABCI(),
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

// withUnexecuted adds an empty failed observation for each listed message of a
// transaction that exceeded the block gas limit after its ante handler failed,
// so the VM never ran; the RPC still lists it as failed.
func withUnexecuted(msgs []ethMsg, observed []replay.Transaction, receipts []*receipt) []replay.Transaction {
	if len(observed) >= len(msgs) {
		return observed
	}
	out := make([]replay.Transaction, 0, len(msgs))
	j := 0
	for i, m := range msgs {
		if j < len(observed) && observed[j].CosmosIndex == m.cosmos && observed[j].MessageIndex == m.msg {
			out = append(out, observed[j])
			j++
		} else if receipts[i].ExceededBlockGas {
			out = append(out, replay.Transaction{CosmosIndex: m.cosmos, MessageIndex: m.msg, Hash: m.tx.AsTransaction().Hash(), Failed: true})
		}
	}
	return append(out, observed[j:]...)
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
	selected = withUnexecuted(msgs, selected, receipts)
	if len(selected) != len(msgs) {
		return nil, fmt.Errorf("%d observations for %d EVM messages", len(selected), len(msgs))
	}
	for i, m := range msgs {
		obs := selected[i]
		hash := m.tx.AsTransaction().Hash()
		if obs.CosmosIndex != m.cosmos || obs.MessageIndex != m.msg {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: missing observation", m.cosmos, m.msg)
		}
		rc := receipts[i]
		if obs.Hash != hash || obs.Error != "" || (len(obs.Trace) == 0 && !rc.ExceededBlockGas) {
			return nil, fmt.Errorf("tx[%d]/msg[%d]: invalid observation for %s: %s", m.cosmos, m.msg, hash, obs.Error)
		}
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
	return selected, nil
}

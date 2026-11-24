package replay

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
	sm "github.com/tendermint/tendermint/state"
)

// results-6346.json holds the archived results of mainnet H6346 and the
// LastResultsHash of H6347. cronosd fix-unlucky-tx rewrote tx 3, which
// exceeded the block gas limit after its messages were committed, to code 0
// with the re-execution's data; consensus recorded code 11 without data.
func mainnet6346(t *testing.T) (archived *tmstate.ABCIResponses, attested []byte) {
	raw, err := ioutil.ReadFile("testdata/results-6346.json")
	require.NoError(t, err)
	var f struct {
		Hash string `json:"next_last_results_hash"`
		Txs  []struct {
			Code      uint32
			Codespace string
			Data      string
			GasWanted string `json:"gas_wanted"`
			GasUsed   string `json:"gas_used"`
			Log       string
		}
	}
	require.NoError(t, json.Unmarshal(raw, &f))
	archived = &tmstate.ABCIResponses{BeginBlock: &abci.ResponseBeginBlock{}, EndBlock: &abci.ResponseEndBlock{}}
	for _, tx := range f.Txs {
		data, err := base64.StdEncoding.DecodeString(tx.Data)
		require.NoError(t, err)
		gw, _ := strconv.ParseInt(tx.GasWanted, 10, 64)
		gu, _ := strconv.ParseInt(tx.GasUsed, 10, 64)
		archived.DeliverTxs = append(archived.DeliverTxs, &abci.ResponseDeliverTx{Code: tx.Code, Codespace: tx.Codespace, Data: data, GasWanted: gw, GasUsed: gu, Log: tx.Log})
	}
	attested, err = hex.DecodeString(f.Hash)
	require.NoError(t, err)
	return archived, attested
}

func executed(archived *tmstate.ABCIResponses, i int) *tmstate.ABCIResponses {
	out := &tmstate.ABCIResponses{BeginBlock: archived.BeginBlock, EndBlock: archived.EndBlock}
	for _, r := range archived.DeliverTxs {
		c := *r
		out.DeliverTxs = append(out.DeliverTxs, &c)
	}
	row := out.DeliverTxs[i]
	*row = abci.ResponseDeliverTx{Code: 11, Codespace: "sdk", GasWanted: row.GasWanted, GasUsed: row.GasUsed,
		Log: "out of gas in location: block gas meter; gasWanted: 0, gasUsed: 11291736: out of gas"}
	return out
}

func TestFixUnluckyTxArchive(t *testing.T) {
	archived, attested := mainnet6346(t)
	require.False(t, bytes.Equal(attested, sm.ABCIResponsesResultsHash(archived)), "the rewritten archive is not what consensus attests")

	native := executed(archived, 3)
	require.Equal(t, attested, sm.ABCIResponsesResultsHash(native), "consensus attests the original code 11 row")
	require.True(t, ExceedsBlockGas(native.DeliverTxs[3]))
	require.NoError(t, Verify(archived, native))

	// At mainnet H441639 the re-execution used 800 gas less than consensus.
	other := executed(archived, 3)
	other.DeliverTxs[3].GasUsed += 800
	require.NoError(t, Verify(archived, other))

	// Only one transaction can exceed the block gas limit.
	other = executed(archived, 3)
	other.DeliverTxs[5] = other.DeliverTxs[3]
	require.ErrorContains(t, Verify(archived, other), "tx[5]: second transaction")

	// Only the block-gas failure after commit is excused.
	other = executed(archived, 3)
	other.DeliverTxs[3].Log = "out of gas in location: WriteFlat; gasWanted: 0"
	require.ErrorContains(t, Verify(archived, other), "tx[3] outcome")

	// A failure ante rejected before execution is never excused.
	require.False(t, ExceedsBlockGas(archived.DeliverTxs[4]))
}

// At mainnet H114868 the transaction that exceeded the block gas limit had
// failed ante, so fix-unlucky-tx wrote back its code 32 re-execution.
func TestFixUnluckyTxArchiveFailedRewrite(t *testing.T) {
	archived, _ := mainnet6346(t)
	row := archived.DeliverTxs[3]
	*row = abci.ResponseDeliverTx{Code: 32, Codespace: "sdk", GasWanted: row.GasWanted, GasUsed: row.GasUsed,
		Log: "account sequence mismatch, expected 27229, got 27244: incorrect account sequence"}
	require.NoError(t, Verify(archived, executed(archived, 3)))
}

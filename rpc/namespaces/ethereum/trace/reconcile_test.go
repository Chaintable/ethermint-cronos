package trace

import (
	"math/big"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	dtypes "github.com/evmos/ethermint/debank/types"
	"github.com/evmos/ethermint/debank/util"
)

var (
	addrA = common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	topic = common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
)

func mkTrace(id, parent string, ta []int64, pos int64, gasUsed uint64, errStr string) dtypes.Trace {
	return dtypes.Trace{
		ID: id, ParentTraceID: parent, TraceAddress: ta, PosInParentTrace: pos,
		GasUsed: new(big.Int).SetUint64(gasUsed), TxID: "0xtx", Error: errStr,
	}
}

func mkEvent(parent string, pos int64, data byte, topics ...common.Hash) dtypes.Event {
	ev := dtypes.Event{
		ID:            util.ToHash([]string{parent, strconv.FormatInt(pos, 10)}),
		Address:       "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ParentTraceID: parent, Position: pos,
		Data: []byte{data},
	}
	if len(topics) > 0 {
		ev.Selector = topics[0].Hex()
		for _, t := range topics[1:] {
			ev.Topics = append(ev.Topics, t.Hex())
		}
	}
	return ev
}

func mkLog(data byte, topics ...common.Hash) *ethtypes.Log {
	return &ethtypes.Log{Address: addrA, Topics: topics, Data: []byte{data}}
}

func TestCheckReceipt(t *testing.T) {
	root := mkTrace("root", "", nil, 0, 33, "")
	a, b := mkEvent("root", 0, 1, topic), mkEvent("root", 1, 2)
	logA, logB := mkLog(1, topic), mkLog(2)
	logA.Index = 9
	logB.Index = 12
	for _, tc := range []struct {
		name string
		tr   dtypes.TraceResult
		rc   consensusReceipt
		pass bool
	}{
		{"agree", dtypes.TraceResult{Traces: []dtypes.Trace{root}, Events: []dtypes.Event{a, b}}, consensusReceipt{true, 100, []*ethtypes.Log{logA, logB}}, true},
		{"receipt reverts", dtypes.TraceResult{Traces: []dtypes.Trace{root}}, consensusReceipt{}, false},
		{"replay reverts", dtypes.TraceResult{ErrorTraces: []dtypes.Trace{root}}, consensusReceipt{Status: true}, false},
		{"root missing", dtypes.TraceResult{}, consensusReceipt{Status: true}, false},
		{"duplicate root", dtypes.TraceResult{Traces: []dtypes.Trace{root, root}}, consensusReceipt{Status: true}, false},
		{"different content", dtypes.TraceResult{Traces: []dtypes.Trace{root}, Events: []dtypes.Event{a}}, consensusReceipt{true, 100, []*ethtypes.Log{logB}}, false},
		{"different order", dtypes.TraceResult{Traces: []dtypes.Trace{root}, Events: []dtypes.Event{a, b}}, consensusReceipt{true, 100, []*ethtypes.Log{logB, logA}}, false},
		{"missing event", dtypes.TraceResult{Traces: []dtypes.Trace{root}}, consensusReceipt{true, 100, []*ethtypes.Log{logA}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReceipt(&tc.tr, "0xtx", &tc.rc)
			if !tc.pass {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 33, tc.tr.Traces[0].GasUsed.Uint64())
			require.EqualValues(t, 9, tc.tr.Events[0].LogIndex)
			require.EqualValues(t, 12, tc.tr.Events[1].LogIndex)
			require.Equal(t, a.ParentTraceID, tc.tr.Events[0].ParentTraceID)
		})
	}
}

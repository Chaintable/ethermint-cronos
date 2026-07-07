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

func TestReconcileNoopWhenAgreeing(t *testing.T) {
	tr := &dtypes.TraceResult{
		Traces: []dtypes.Trace{mkTrace("root", "", nil, 0, 100, "")},
		Events: []dtypes.Event{mkEvent("root", 0, 0x01, topic)},
	}
	sum := reconcileTx(tr, &consensusReceipt{Status: true, GasUsed: 100, Logs: []*ethtypes.Log{mkLog(0x01, topic)}}, 500)
	require.False(t, sum.changed())
	require.Len(t, tr.Traces, 1)
	require.Len(t, tr.Events, 1)
	require.Equal(t, uint64(100), tr.Traces[0].GasUsed.Uint64())
}

func TestReconcileConsensusRevertReplaySuccess(t *testing.T) {
	tr := &dtypes.TraceResult{
		Traces: []dtypes.Trace{
			mkTrace("root", "", nil, 0, 90, ""),
			mkTrace("child", "root", []int64{0}, 0, 10, ""),
		},
		Events: []dtypes.Event{mkEvent("root", 1, 0x01, topic)},
	}
	// consensus: OOG (gasUsed == limit), no logs
	sum := reconcileTx(tr, &consensusReceipt{Status: false, GasUsed: 500, Logs: nil}, 500)
	require.True(t, sum.StatusFlipped)
	require.Empty(t, tr.Traces)
	require.Len(t, tr.ErrorTraces, 2)
	root := findRootIn(t, tr.ErrorTraces)
	require.Equal(t, "out of gas", root.Error)
	require.Equal(t, uint64(500), root.GasUsed.Uint64())
	require.Empty(t, tr.Events)
	require.Len(t, tr.ErrorEvents, 1)
	require.EqualValues(t, 0, tr.ErrorEvents[0].LogIndex)
}

func TestReconcileConsensusSuccessReplayRevert(t *testing.T) {
	tr := &dtypes.TraceResult{
		ErrorTraces: []dtypes.Trace{
			mkTrace("root", "", nil, 0, 480, "execution reverted"),
			mkTrace("child", "root", []int64{0}, 0, 10, "out of gas"), // 子帧错误保留
		},
		ErrorEvents: []dtypes.Event{mkEvent("root", 1, 0x01, topic)},
	}
	sum := reconcileTx(tr, &consensusReceipt{Status: true, GasUsed: 470, Logs: []*ethtypes.Log{mkLog(0x01, topic)}}, 500)
	require.True(t, sum.StatusFlipped)
	require.Empty(t, tr.ErrorTraces)
	require.Len(t, tr.Traces, 2)
	root := findRootIn(t, tr.Traces)
	require.Equal(t, "", root.Error)
	require.Equal(t, uint64(470), root.GasUsed.Uint64())
	child := childIn(t, tr.Traces)
	require.Equal(t, "out of gas", child.Error)
	// 事件从 ErrorEvents 恢复为正常事件, 内容与共识 log 一致
	require.Empty(t, tr.ErrorEvents)
	require.Len(t, tr.Events, 1)
	require.Equal(t, topic.Hex(), tr.Events[0].Selector)
}

func TestReconcileEventContentPositional(t *testing.T) {
	// 数量相等、内容不同(tokenId 偏移类): 按位配对, 内容取共识, 链接保留
	tr := &dtypes.TraceResult{
		Traces: []dtypes.Trace{mkTrace("root", "", nil, 0, 100, "")},
		Events: []dtypes.Event{mkEvent("root", 0, 0x01, topic), mkEvent("root", 1, 0x02, topic)},
	}
	otherTopic := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	logs := []*ethtypes.Log{mkLog(0x01, topic), mkLog(0x02, otherTopic)}
	sum := reconcileTx(tr, &consensusReceipt{Status: true, GasUsed: 100, Logs: logs}, 500)
	require.True(t, sum.EventsRebuilt)
	require.Zero(t, sum.DroppedEvents)
	require.Zero(t, sum.RootAttached)
	require.Len(t, tr.Events, 2)
	require.Equal(t, otherTopic.Hex(), tr.Events[1].Selector) // 内容修正
	require.Equal(t, "root", tr.Events[1].ParentTraceID)      // 链接保留
	require.EqualValues(t, 1, tr.Events[1].Position)
	require.Equal(t, util.ToHash([]string{"root", "1"}), tr.Events[1].ID)
}

func TestReconcileEventCountMismatch(t *testing.T) {
	// replay 多一个事件(丢弃), 共识多一个 replay 没有的(root 挂载)
	tr := &dtypes.TraceResult{
		Traces: []dtypes.Trace{
			mkTrace("root", "", nil, 0, 100, ""),
			mkTrace("child", "root", []int64{0}, 0, 10, ""), // 占 root pos 0
		},
		Events: []dtypes.Event{
			mkEvent("root", 1, 0x01, topic),
			mkEvent("root", 2, 0x99, topic), // 共识里不存在 -> 丢弃
		},
	}
	newTopic := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
	logs := []*ethtypes.Log{
		mkLog(0x01, topic),    // 内容匹配 -> 继承链接
		mkLog(0x42, newTopic), // replay 没有 -> root 挂载
		mkLog(0x43, newTopic), // replay 没有 -> root 挂载
	}
	sum := reconcileTx(tr, &consensusReceipt{Status: true, GasUsed: 100, Logs: logs}, 500)
	require.True(t, sum.EventsRebuilt)
	require.Equal(t, 1, sum.DroppedEvents) // 0x99 共识里不存在
	require.Equal(t, 2, sum.RootAttached)
	require.Len(t, tr.Events, 3)
	require.Equal(t, "root", tr.Events[0].ParentTraceID)
	require.EqualValues(t, 1, tr.Events[0].Position) // 继承
	att := tr.Events[1]
	require.Equal(t, "root", att.ParentTraceID)
	require.EqualValues(t, 3, att.Position) // 0(child)/1/2 已占 -> 3
	require.Equal(t, util.ToHash([]string{"root", "3"}), att.ID)
	require.Equal(t, newTopic.Hex(), att.Selector)
	require.EqualValues(t, 4, tr.Events[2].Position)
}

func TestZeroTopicSignature(t *testing.T) {
	ev := mkEvent("root", 0, 0x01) // 无 topics: Selector=="", Topics==nil
	lg := mkLog(0x01)
	require.Equal(t, logSig(lg), eventSig(&ev))
}

func findRootIn(t *testing.T, traces []dtypes.Trace) *dtypes.Trace {
	t.Helper()
	for i := range traces {
		if len(traces[i].TraceAddress) == 0 {
			return &traces[i]
		}
	}
	t.Fatal("no root trace")
	return nil
}

func childIn(t *testing.T, traces []dtypes.Trace) *dtypes.Trace {
	t.Helper()
	for i := range traces {
		if len(traces[i].TraceAddress) > 0 {
			return &traces[i]
		}
	}
	t.Fatal("no child trace")
	return nil
}

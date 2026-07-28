package trace

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestMergeStorageContractsPreservesAndAddsAddresses(t *testing.T) {
	first := common.HexToAddress("0xaAaAaAaaAaAaAaaAaAAAAAAAAaaaAaAaAaaAaaAa")
	second := common.HexToAddress("0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB")
	got := mergeStorageContracts(
		map[common.Address]struct{}{second: {}},
		[]string{first.Hex(), second.Hex()},
	)
	require.ElementsMatch(t, []string{
		strings.ToLower(first.Hex()), strings.ToLower(second.Hex()),
	}, got)
}

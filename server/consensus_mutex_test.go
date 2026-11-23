package server

import (
	"context"
	"testing"
	"time"

	abcicli "github.com/cometbft/cometbft/abci/client"
	abci "github.com/cometbft/cometbft/abci/types"
	cmtsync "github.com/cometbft/cometbft/libs/sync"
	"github.com/stretchr/testify/require"
)

func TestConsensusClientsShareEmitterLock(t *testing.T) {
	methods := map[string]func(abcicli.Client) error{
		"InitChain": func(c abcicli.Client) error {
			_, err := c.InitChain(context.Background(), &abci.RequestInitChain{})
			return err
		},
		"PrepareProposal": func(c abcicli.Client) error {
			_, err := c.PrepareProposal(context.Background(), &abci.RequestPrepareProposal{})
			return err
		},
		"ProcessProposal": func(c abcicli.Client) error {
			_, err := c.ProcessProposal(context.Background(), &abci.RequestProcessProposal{})
			return err
		},
		"FinalizeBlock": func(c abcicli.Client) error {
			_, err := c.FinalizeBlock(context.Background(), &abci.RequestFinalizeBlock{})
			return err
		},
		"ExtendVote": func(c abcicli.Client) error {
			_, err := c.ExtendVote(context.Background(), &abci.RequestExtendVote{})
			return err
		},
		"VerifyVoteExtension": func(c abcicli.Client) error {
			_, err := c.VerifyVoteExtension(context.Background(), &abci.RequestVerifyVoteExtension{})
			return err
		},
		"Commit": func(c abcicli.Client) error {
			_, err := c.Commit(context.Background(), &abci.RequestCommit{})
			return err
		},
	}
	for _, syncAll := range []bool{false, true} {
		for name, call := range methods {
			t.Run(name+map[bool]string{false: "/async", true: "/sync"}[syncAll], func(t *testing.T) {
				mtx := new(cmtsync.Mutex)
				creator := NewConsensusMutexClientCreator(mtx, abci.NewBaseApplication(), syncAll)
				client, err := creator.NewABCIClient()
				require.NoError(t, err)
				mtx.Lock()
				done := make(chan error, 1)
				go func() { done <- call(client) }()
				select {
				case <-done:
					mtx.Unlock()
					t.Fatal("consensus call bypassed the emitter lock")
				case <-time.After(20 * time.Millisecond):
				}
				mtx.Unlock()
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("consensus call did not resume after the emitter released the lock")
				}
			})
		}
	}
}

func TestAsyncCheckTxDoesNotTakeEmitterLock(t *testing.T) {
	mtx := new(cmtsync.Mutex)
	creator := NewConsensusMutexClientCreator(mtx, abci.NewBaseApplication(), false)
	client, err := creator.NewABCIClient()
	require.NoError(t, err)
	mtx.Lock()
	defer mtx.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := client.CheckTx(context.Background(), &abci.RequestCheckTx{})
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("async CheckTx blocked on the emitter lock")
	}
}

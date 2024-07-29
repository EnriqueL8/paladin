// Copyright 2019 Kaleido

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package blockindexer

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyperledger/firefly-signer/pkg/ethtypes"
	"github.com/hyperledger/firefly-signer/pkg/rpcbackend"
	"github.com/kaleido-io/paladin/kata/internal/confutil"
	"github.com/kaleido-io/paladin/kata/internal/persistence"
	"github.com/kaleido-io/paladin/kata/internal/persistence/mockpersistence"
	"github.com/kaleido-io/paladin/kata/internal/tls"
	"github.com/kaleido-io/paladin/kata/internal/types"
	"github.com/kaleido-io/paladin/kata/mocks/rpcbackendmocks"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func newTestBlockIndexer(t *testing.T) (context.Context, *blockIndexer, *rpcbackendmocks.WebSocketRPCClient, func()) {
	return newTestBlockIndexerConf(t, &Config{
		CommitBatchSize: confutil.P(1), // makes testing simpler
		FromBlock:       types.RawJSON(`0`),
	}, &RPCWSConnectConfig{})
}

func newTestBlockIndexerConf(t *testing.T, config *Config, wsConfig *RPCWSConnectConfig) (context.Context, *blockIndexer, *rpcbackendmocks.WebSocketRPCClient, func()) {
	logrus.SetLevel(logrus.DebugLevel)

	ctx, cancelCtx := context.WithCancel(context.Background())

	p, pDone, err := persistence.NewUnitTestPersistence(ctx)
	assert.NoError(t, err)

	blockListener, mRPC := newTestBlockListenerConf(t, ctx, config, wsConfig)
	bi, err := newBlockIndexer(ctx, config, p, blockListener)
	assert.NoError(t, err)
	bi.utBatchNotify = make(chan *blockWriterBatch)
	return ctx, bi, mRPC, func() {
		r := recover()
		if r != nil {
			panic(r)
		}
		bi.Stop()
		cancelCtx()
		pDone()
	}
}

func testBlockArray(l int) ([]*BlockInfoJSONRPC, map[string][]*TXReceiptJSONRPC) {
	blocks := make([]*BlockInfoJSONRPC, l)
	receipts := make(map[string][]*TXReceiptJSONRPC, l)
	for i := 0; i < l; i++ {
		blocks[i] = &BlockInfoJSONRPC{
			Number: ethtypes.HexUint64(i),
			Hash:   ethtypes.MustNewHexBytes0xPrefix(types.RandHex(32)),
		}
		receipts[blocks[i].Hash.String()] = []*TXReceiptJSONRPC{
			{
				TransactionHash: ethtypes.MustNewHexBytes0xPrefix(types.RandHex(32)),
				BlockNumber:     blocks[i].Number,
				BlockHash:       blocks[i].Hash,
				Logs: []*LogJSONRPC{
					{Topics: []ethtypes.HexBytes0xPrefix{
						ethtypes.MustNewHexBytes0xPrefix(types.RandHex(32)),
					}},
				},
			},
		}
		if i == 0 {
			blocks[i].ParentHash = ethtypes.MustNewHexBytes0xPrefix(types.RandHex(32))
		} else {
			blocks[i].ParentHash = blocks[i-1].Hash
		}
	}
	return blocks, receipts
}

func mockBlocksRPCCalls(mRPC *rpcbackendmocks.WebSocketRPCClient, blocks []*BlockInfoJSONRPC, receipts map[string][]*TXReceiptJSONRPC) {
	mockBlocksRPCCallsDynamic(mRPC, func(args mock.Arguments) ([]*BlockInfoJSONRPC, map[string][]*TXReceiptJSONRPC) {
		return blocks, receipts
	})
}

func mockBlocksRPCCallsDynamic(mRPC *rpcbackendmocks.WebSocketRPCClient, dynamic func(args mock.Arguments) ([]*BlockInfoJSONRPC, map[string][]*TXReceiptJSONRPC)) {
	byBlock := mRPC.On("CallRPC", mock.Anything, mock.Anything, "eth_getBlockByNumber", mock.Anything, false).Maybe()
	byBlock.Run(func(args mock.Arguments) {
		blocks, _ := dynamic(args)
		blockReturn := args[1].(*BlockInfoJSONRPC)
		blockNumber := int(args[3].(ethtypes.HexUint64))
		if blockNumber >= len(blocks) {
			byBlock.Return(&rpcbackend.RPCError{Message: "not found"})
		} else {
			*blockReturn = *blocks[blockNumber]
			byBlock.Return(nil)
		}
	})

	blockReceipts := mRPC.On("CallRPC", mock.Anything, mock.Anything, "eth_getBlockReceipts", mock.Anything).Maybe()
	blockReceipts.Run(func(args mock.Arguments) {
		_, receipts := dynamic(args)
		blockReturn := args[1].(*[]*TXReceiptJSONRPC)
		blockHash := args[3].(ethtypes.HexBytes0xPrefix)
		*blockReturn = receipts[blockHash.String()]
		if *blockReturn == nil {
			blockReceipts.Return(&rpcbackend.RPCError{Message: "not found"})
		} else {
			blockReceipts.Return(nil)
		}
	})
}

func TestNewBlockIndexerBadTLS(t *testing.T) {
	_, err := NewBlockIndexer(context.Background(), &Config{}, &RPCWSConnectConfig{
		TLS: tls.Config{
			Enabled: true,
			CAFile:  t.TempDir(),
		},
	}, nil)
	assert.Regexp(t, "PD010901", err)
}

func TestNewBlockIndexerRestoreCheckpointFail(t *testing.T) {
	p, err := mockpersistence.NewSQLMockProvider()
	assert.NoError(t, err)

	cancelledCtx, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()
	bi, err := NewBlockIndexer(cancelledCtx, &Config{}, &RPCWSConnectConfig{}, p.P)
	assert.NoError(t, err)

	// Start will get error, but return due to cancelled context
	bi.Start()
	assert.Nil(t, bi.(*blockIndexer).processorDone)

	assert.NoError(t, p.Mock.ExpectationsWereMet())
}

func TestBlockIndexerCatchUpToHeadFromZeroNoConfirmations(t *testing.T) {
	_, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	blocks, receipts := testBlockArray(10)
	mockBlocksRPCCalls(mRPC, blocks, receipts)

	bi.requiredConfirmations = 0
	bi.Start()

	for i := 0; i < len(blocks); i++ {
		b := <-bi.utBatchNotify
		assert.Len(t, b.blocks, 1) // We should get one block per batch
		assert.Equal(t, blocks[i], b.blocks[0])
	}
}

func TestBlockIndexerCatchUpToHeadFromZeroWithConfirmations(t *testing.T) {
	ctx, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	blocks, receipts := testBlockArray(15)
	mockBlocksRPCCalls(mRPC, blocks, receipts)

	bi.requiredConfirmations = 5
	bi.Start()

	for i := 0; i < len(blocks)-bi.requiredConfirmations; i++ {
		b := <-bi.utBatchNotify
		assert.Len(t, b.blocks, 1) // We should get one block per batch
		assert.Equal(t, blocks[i], b.blocks[0])

		// Get the block
		indexedBlock, err := bi.GetIndexedBlockByNumber(ctx, blocks[i].Number.Uint64())
		assert.NoError(t, err)
		assert.Equal(t, blocks[i].Hash.String(), indexedBlock.Hash.String())
	}

	// Get the first unconfirmed block
	indexedBlock, err := bi.GetIndexedBlockByNumber(ctx, blocks[len(blocks)-bi.requiredConfirmations+1].Number.Uint64())
	assert.NoError(t, err)
	assert.Nil(t, indexedBlock)

}

func TestBlockIndexerListenFromCurrentBlock(t *testing.T) {
	_, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	blocks, receipts := testBlockArray(15)
	mockBlocksRPCCalls(mRPC, blocks, receipts)

	bi.fromBlock = nil
	bi.nextBlock = nil
	bi.requiredConfirmations = 5

	bi.Start()

	// Notify starting at block 5
	for i := 5; i < len(blocks); i++ {
		bi.blockListener.notifyBlock(blocks[i])
	}

	// Randomly notify below that too, which will be ignored
	bi.blockListener.notifyBlock(blocks[1])

	for i := 5; i < len(blocks)-bi.requiredConfirmations; i++ {
		b := <-bi.utBatchNotify
		assert.Len(t, b.blocks, 1) // We should get one block per batch
		assert.Equal(t, blocks[i], b.blocks[0])
	}
}

func TestBatching(t *testing.T) {
	_, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	blocks, receipts := testBlockArray(10)
	mockBlocksRPCCalls(mRPC, blocks, receipts)

	bi.batchSize = 5

	bi.Start()

	// Notify starting at block 5
	for i := 5; i < len(blocks); i++ {
		bi.blockListener.notifyBlock(blocks[i])
	}

	// Randomly notify below that too, which will be ignored
	bi.blockListener.notifyBlock(blocks[1])

	for i := 0; i < len(blocks)-bi.requiredConfirmations; i += 5 {
		batch := <-bi.utBatchNotify
		assert.Len(t, batch.blocks, 5)
		for i2, b := range batch.blocks {
			assert.Equal(t, blocks[i+i2], b)
		}
	}
}

func TestBlockIndexerListenFromCurrentUsingCheckpointBlock(t *testing.T) {
	_, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	blocks, receipts := testBlockArray(15)
	mockBlocksRPCCalls(mRPC, blocks, receipts)

	bi.persistence.DB().Table("indexed_blocks").Create(&IndexedBlock{
		Number: 12345,
		Hash:   *types.MustParseHashID(types.RandHex(32)),
	})

	bi.Start()

	assert.Equal(t, ethtypes.HexUint64(12346), *bi.nextBlock)
}

func TestBlockIndexerHandleReorgInConfirmationWindow1(t *testing.T) {
	// test where the reorg happens at the edge of the confirmation window
	testBlockIndexerHandleReorgInConfirmationWindow(t,
		10, // blocks in chain before re-org
		5,  // blocks that remain from original chain after re-org
		5,  // required confirmations
	)
}

func TestBlockIndexerHandleReorgInConfirmationWindow2(t *testing.T) {
	// test where the reorg happens replacing some blocks
	// WE ALREADY CONFIRMED - meaning we dispatched them incorrectly
	// because the confirmations were not tuned correctly
	testBlockIndexerHandleReorgInConfirmationWindow(t,
		10, // blocks in chain before re-org
		0,  // blocks that remain from original chain after re-org
		5,  // required confirmations
	)
}

func TestBlockIndexerHandleReorgInConfirmationWindow3(t *testing.T) {
	// test without confirmations, so everything is a problem
	testBlockIndexerHandleReorgInConfirmationWindow(t,
		10, // blocks in chain before re-org
		0,  // blocks that remain from original chain after re-org
		0,  // required confirmations
	)
}

func TestBlockIndexerHandleReorgInConfirmationWindow4(t *testing.T) {
	// test of a re-org of one
	testBlockIndexerHandleReorgInConfirmationWindow(t,
		5, // blocks in chain before re-org
		4, // blocks that remain from original chain after re-org
		4, // required confirmations
	)
}

func checkBlocksSequential(t *testing.T, desc string, blocks []*BlockInfoJSONRPC, receipts map[string][]*TXReceiptJSONRPC) {
	blockSummaries := make([]string, len(blocks))
	var lastBlock *BlockInfoJSONRPC
	invalid := false
	for i, b := range blocks {
		assert.NotEmpty(t, b.Hash)
		blockSummaries[i] = fmt.Sprintf("%d/%s [rok=%t]", b.Number, b.Hash, receipts[b.Hash.String()] != nil)
		if i == 0 {
			assert.NotEmpty(t, b.ParentHash)
		} else if lastBlock.Hash.String() != b.ParentHash.String() {
			invalid = true
		}
		lastBlock = b
	}
	fmt.Printf("%s: %s\n", desc, strings.Join(blockSummaries, ",\n"))
	if invalid {
		panic("wrong sequence") // aid to writing tests that build sequences
	}
}

func testBlockIndexerHandleReorgInConfirmationWindow(t *testing.T, blockLenBeforeReorg, overlap, reqConf int) {
	_, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	bi.requiredConfirmations = reqConf

	blocksBeforeReorg, receipts := testBlockArray(blockLenBeforeReorg)
	blocksAfterReorg, receiptsAfterReorg := testBlockArray(blockLenBeforeReorg + overlap)
	dangerArea := len(blocksAfterReorg) - overlap
	for i := 0; i < len(blocksAfterReorg); i++ {
		receipts[blocksAfterReorg[i].Hash.String()] = receiptsAfterReorg[blocksAfterReorg[i].Hash.String()]
		if i < overlap {
			b := blocksBeforeReorg[i]
			// Copy the blocks over from the before-reorg chain
			blockCopy := *b
			blocksAfterReorg[i] = &blockCopy
		}
	}
	if overlap > 0 {
		// Re-wire the first forked block
		blocksAfterReorg[overlap].ParentHash = blocksAfterReorg[overlap-1].Hash
	}
	checkBlocksSequential(t, "before", blocksBeforeReorg, receipts)
	checkBlocksSequential(t, "after ", blocksAfterReorg, receipts)

	var isAfterReorg atomic.Bool
	notificationsDone := make(chan struct{})
	mockBlocksRPCCallsDynamic(mRPC, func(args mock.Arguments) ([]*BlockInfoJSONRPC, map[string][]*TXReceiptJSONRPC) {
		blockNumber := -1
		if args[2].(string) == "eth_getBlockByNumber" {
			blockNumber = int(args[3].(ethtypes.HexUint64))
		}
		if isAfterReorg.Load() {
			return blocksAfterReorg, receipts
		} else {
			// we instigate the re-org when we've returned all the blocks
			if blockNumber >= len(blocksBeforeReorg) {
				isAfterReorg.Store(true)
				go func() {
					defer close(notificationsDone)
					// Simulate the modified blocks only coming in with delays
					for i := overlap; i < len(blocksAfterReorg); i++ {
						time.Sleep(100 * time.Microsecond)
						bi.blockListener.notifyBlock(blocksAfterReorg[i])
					}
				}()
			}
			return blocksBeforeReorg, receipts
		}
	})

	bi.Start()

	for i := 0; i < len(blocksAfterReorg)-bi.requiredConfirmations; i++ {
		b := <-bi.utBatchNotify
		assert.Len(t, b.blocks, 1) // We should get one block per batch
		if i >= overlap && i < (dangerArea-reqConf) {
			// This would be a bad situation in reality, where a reorg crossed the confirmations
			// boundary. An indication someone incorrectly configured their confirmations
			assert.Equal(t, b.blocks[0].Hash.String(), blocksBeforeReorg[i].Hash.String())
			assert.Equal(t, b.blocks[0].Number, blocksBeforeReorg[i].Number)
		} else {
			assert.Equal(t, b.blocks[0].Hash.String(), blocksAfterReorg[i].Hash.String())
			assert.Equal(t, b.blocks[0].Number, blocksAfterReorg[i].Number)
		}
	}
	// Wait for the notifications to go through
	<-notificationsDone

}

func TestBlockIndexerHandleRandomConflictingBlockNotification(t *testing.T) {
	_, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	bi.requiredConfirmations = 5

	blocks, receipts := testBlockArray(50)

	randBlock := &BlockInfoJSONRPC{
		Number:     3,
		Hash:       ethtypes.MustNewHexBytes0xPrefix(types.RandHex(32)),
		ParentHash: ethtypes.MustNewHexBytes0xPrefix(types.RandHex(32)),
	}

	sentRandom := false
	mockBlocksRPCCallsDynamic(mRPC, func(args mock.Arguments) ([]*BlockInfoJSONRPC, map[string][]*TXReceiptJSONRPC) {
		if !sentRandom && args[3].(ethtypes.HexUint64) == 4 {
			sentRandom = true
			bi.blockListener.notifyBlock(randBlock)
			// Give notification handler likelihood to run before we continue the by-number getting
			time.Sleep(1 * time.Millisecond)
		}
		return blocks, receipts
	})

	bi.Start()

	for i := 0; i < len(blocks)-bi.requiredConfirmations; i++ {
		b := <-bi.utBatchNotify
		assert.Len(t, b.blocks, 1) // We should get one block per batch
		assert.Equal(t, blocks[i], b.blocks[0])
	}
}

func TestBlockIndexerResetsAfterHashLookupFail(t *testing.T) {
	_, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	blocks, receipts := testBlockArray(5)

	sentFail := false
	mockBlocksRPCCallsDynamic(mRPC, func(args mock.Arguments) ([]*BlockInfoJSONRPC, map[string][]*TXReceiptJSONRPC) {
		if !sentFail &&
			args[2].(string) == "eth_getBlockReceipts" &&
			args[3].(ethtypes.HexBytes0xPrefix).Equals(blocks[2].Hash) {
			sentFail = true
			// Send back a not found, to send us round the reset loop
			return []*BlockInfoJSONRPC{}, map[string][]*TXReceiptJSONRPC{}
		}
		return blocks, receipts
	})

	bi.Start()

	for i := 0; i < len(blocks); i++ {
		b := <-bi.utBatchNotify
		assert.Len(t, b.blocks, 1) // We should get one block per batch
		assert.Equal(t, blocks[i], b.blocks[0])
	}

	assert.True(t, sentFail)
}

func TestBlockIndexerDispatcherFallsBehindHead(t *testing.T) {
	_, bi, mRPC, blDone := newTestBlockIndexer(t)
	defer blDone()

	bi.requiredConfirmations = 5

	blocks, receipts := testBlockArray(30)
	mockBlocksRPCCalls(mRPC, blocks, receipts)

	bi.Start()

	// Notify all the blocks before we process any
	assert.True(t, bi.blockListener.unstableHeadLength > len(blocks))
	for _, b := range blocks {
		bi.blockListener.notifyBlock(b)
	}

	// The dispatches should have been added, until it got too far ahead
	// and then set to nil.
	for bi.newHeadToAdd != nil {
		time.Sleep(1 * time.Millisecond)
	}

	for i := 0; i < len(blocks)-bi.requiredConfirmations; i++ {
		b := <-bi.utBatchNotify
		assert.Len(t, b.blocks, 1) // We should get one block per batch
		assert.Equal(t, blocks[i], b.blocks[0])
	}

}

func TestBlockIndexerStartFromBlock(t *testing.T) {
	ctx, bl, _, done := newTestBlockListener(t)
	defer done()

	p, err := mockpersistence.NewSQLMockProvider()
	assert.NoError(t, err)

	_, err = newBlockIndexer(ctx, &Config{
		FromBlock: types.RawJSON(`"pending"`),
	}, p.P, bl)
	assert.Regexp(t, "PD011200.*pending", err)

	bi, err := newBlockIndexer(ctx, &Config{
		FromBlock: types.RawJSON(`"latest"`),
	}, p.P, bl)
	assert.NoError(t, err)
	assert.Nil(t, bi.fromBlock)

	bi, err = newBlockIndexer(ctx, &Config{
		FromBlock: types.RawJSON(`null`),
	}, p.P, bl)
	assert.NoError(t, err)
	assert.Nil(t, bi.fromBlock)

	bi, err = newBlockIndexer(ctx, &Config{}, p.P, bl)
	assert.NoError(t, err)
	assert.Nil(t, bi.fromBlock)

	bi, err = newBlockIndexer(ctx, &Config{
		FromBlock: types.RawJSON(`123`),
	}, p.P, bl)
	assert.NoError(t, err)
	assert.Equal(t, ethtypes.HexUint64(123), *bi.fromBlock)

	bi, err = newBlockIndexer(ctx, &Config{
		FromBlock: types.RawJSON(`"0x7b"`),
	}, p.P, bl)
	assert.NoError(t, err)
	assert.Equal(t, ethtypes.HexUint64(123), *bi.fromBlock)

	_, err = newBlockIndexer(ctx, &Config{
		FromBlock: types.RawJSON(`!!! bad JSON`),
	}, p.P, bl)
	assert.Regexp(t, "PD011200", err)

	_, err = newBlockIndexer(ctx, &Config{
		FromBlock: types.RawJSON(`false`),
	}, p.P, bl)
	assert.Regexp(t, "PD011200", err)
}

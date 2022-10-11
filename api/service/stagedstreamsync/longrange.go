package stagedstreamsync

import (
	"context"
	"fmt"
	"sync"
	"time"

	syncproto "github.com/harmony-one/harmony/p2p/stream/protocols/sync"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

// doLongRangeSync does the long range sync.
// One LongRangeSync consists of several iterations.
// For each iteration, estimate the current block number, then fetch block & insert to blockchain
func (d *Downloader) doLongRangeSync() (int, error) {
	var totalInserted int

	for {
		ctx, cancel := context.WithCancel(d.ctx)

		iter := &lrSyncIter{
			bc:     d.bc,
			p:      d.syncProtocol,
			d:      d,
			ctx:    ctx,
			config: d.config,
			logger: d.logger.With().Str("mode", "long range").Logger(),
		}
		if err := iter.doLongRangeSync(); err != nil {
			cancel()
			return totalInserted + iter.inserted, err
		}
		cancel()

		totalInserted += iter.inserted

		if iter.inserted < lastMileThres {
			return totalInserted, nil
		}
	}
}

// lrSyncIter run a single iteration of a full long range sync.
// First get a rough estimate of the current block height, and then sync to this
// block number
type lrSyncIter struct {
	bc blockChain
	p  syncProtocol
	d  *Downloader

	gbm      *getBlocksManager // initialized when finished get block number
	inserted int

	config Config
	ctx    context.Context
	logger zerolog.Logger
}

func (lsi *lrSyncIter) doLongRangeSync() error {
	if err := lsi.checkPrerequisites(); err != nil {
		return err
	}
	bn, err := lsi.estimateCurrentNumber()
	if err != nil {
		return err
	}
	if curBN := lsi.bc.CurrentBlock().NumberU64(); bn <= curBN {
		lsi.logger.Info().Uint64("current number", curBN).Uint64("target number", bn).
			Msg("early return of long range sync")
		return nil
	}

	lsi.d.startSyncing()
	defer lsi.d.finishSyncing()

	lsi.logger.Info().Uint64("target number", bn).Msg("estimated remote current number")
	lsi.d.status.setTargetBN(bn)

	return lsi.fetchAndInsertBlocks(bn)
}

func (lsi *lrSyncIter) checkPrerequisites() error {
	return lsi.checkHaveEnoughStreams()
}

// estimateCurrentNumber roughly estimate the current block number.
// The block number does not need to be exact, but just a temporary target of the iteration
func (lsi *lrSyncIter) estimateCurrentNumber() (uint64, error) {
	var (
		cnResults = make(map[sttypes.StreamID]uint64)
		lock      sync.Mutex
		wg        sync.WaitGroup
	)
	wg.Add(lsi.config.Concurrency)
	for i := 0; i != lsi.config.Concurrency; i++ {
		go func() {
			defer wg.Done()
			bn, stid, err := lsi.doGetCurrentNumberRequest()
			if err != nil {
				lsi.logger.Err(err).Str("streamID", string(stid)).
					Msg("getCurrentNumber request failed. Removing stream")
				if !errors.Is(err, context.Canceled) {
					lsi.p.RemoveStream(stid)
				}
				return
			}
			lock.Lock()
			cnResults[stid] = bn
			lock.Unlock()
		}()
	}
	wg.Wait()

	if len(cnResults) == 0 {
		select {
		case <-lsi.ctx.Done():
			return 0, lsi.ctx.Err()
		default:
		}
		return 0, errors.New("zero block number response from remote nodes")
	}
	bn := computeBlockNumberByMaxVote(cnResults)
	return bn, nil
}

func (lsi *lrSyncIter) doGetCurrentNumberRequest() (uint64, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(lsi.ctx, 10*time.Second)
	defer cancel()

	bn, stid, err := lsi.p.GetCurrentBlockNumber(ctx, syncproto.WithHighPriority())
	if err != nil {
		return 0, stid, err
	}
	return bn, stid, nil
}

// fetchAndInsertBlocks use the pipeline pattern to boost the performance of inserting blocks.
// TODO: For resharding, use the pipeline to do fast sync (epoch loop, header loop, body loop)
func (lsi *lrSyncIter) fetchAndInsertBlocks(targetBN uint64) error {
	gbm := newGetBlocksManager(lsi.bc, targetBN, lsi.logger)
	lsi.gbm = gbm

	// Setup workers to fetch blocks from remote node
	for i := 0; i != lsi.config.Concurrency; i++ {
		worker := &getBlocksWorker{
			gbm:      gbm,
			protocol: lsi.p,
			ctx:      lsi.ctx,
		}
		go worker.workLoop(nil)
	}

	// insert the blocks to chain. Return when the target block number is reached.
	lsi.insertChainLoop(targetBN)

	select {
	case <-lsi.ctx.Done():
		return lsi.ctx.Err()
	default:
	}
	return nil
}

func (lsi *lrSyncIter) insertChainLoop(targetBN uint64) {
	var (
		gbm     = lsi.gbm
		t       = time.NewTicker(100 * time.Millisecond)
		resultC = make(chan struct{}, 1)
	)
	defer t.Stop()

	trigger := func() {
		select {
		case resultC <- struct{}{}:
		default:
		}
	}

	for {
		select {
		case <-lsi.ctx.Done():
			return

		case <-t.C:
			// Redundancy, periodically check whether there is blocks that can be processed
			trigger()

		case <-gbm.resultC:
			// New block arrive in resultQueue
			trigger()

		case <-resultC:
			blockResults := gbm.PullContinuousBlocks(blocksPerInsert)
			if len(blockResults) > 0 {
				lsi.processBlocks(blockResults, targetBN)
				// more blocks is expected being able to be pulled from queue
				trigger()
			}
			if lsi.bc.CurrentBlock().NumberU64() >= targetBN {
				return
			}
		}
	}
}

func (lsi *lrSyncIter) processBlocks(results []*blockResult, targetBN uint64) {
	blocks := blockResultsToBlocks(results)

	for i, block := range blocks {
		if err := verifyAndInsertBlock(lsi.bc, block); err != nil {
			lsi.logger.Warn().Err(err).Uint64("target block", targetBN).
				Uint64("block number", block.NumberU64()).
				Msg("insert blocks failed in long range")
			pl := lsi.d.promLabels()
			pl["error"] = err.Error()
			longRangeFailInsertedBlockCounterVec.With(pl).Inc()

			lsi.p.RemoveStream(results[i].stid)
			lsi.gbm.HandleInsertError(results, i)
			return
		}

		lsi.inserted++
		longRangeSyncedBlockCounterVec.With(lsi.d.promLabels()).Inc()
	}
	lsi.gbm.HandleInsertResult(results)
}

func (lsi *lrSyncIter) checkHaveEnoughStreams() error {
	numStreams := lsi.p.NumStreams()
	if numStreams < lsi.config.MinStreams {
		return fmt.Errorf("number of streams smaller than minimum: %v < %v",
			numStreams, lsi.config.MinStreams)
	}
	return nil
}

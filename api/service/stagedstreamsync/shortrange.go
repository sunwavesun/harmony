package stagedstreamsync

import (
	"context"

	"github.com/harmony-one/harmony/core"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/harmony-one/harmony/shard"
	"github.com/pkg/errors"
)

// doShortRangeSync does the short range sync.
// Compared with long range sync, short range sync is more focused on syncing to the latest block.
// It consist of 3 steps:
// 1. Obtain the block hashes and ompute the longest hash chain..
// 2. Get blocks by hashes from computed hash chain.
// 3. Insert the blocks to blockchain.
func (d *Downloader) doShortRangeSync() (int, error) {
	if _, ok := d.bc.(*core.EpochChain); ok {
		return d.doShortRangeSyncForEpochSync()
	}
	numShortRangeCounterVec.With(d.promLabels()).Inc()

	srCtx, cancel := context.WithTimeout(d.ctx, shortRangeTimeout)
	defer cancel()

	sh := &srHelper{
		syncProtocol: d.syncProtocol,
		ctx:          srCtx,
		config:       d.config,
		logger:       d.logger.With().Str("mode", "short range").Logger(),
	}

	if err := sh.checkPrerequisites(); err != nil {
		return 0, errors.Wrap(err, "prerequisite")
	}
	curBN := d.bc.CurrentBlock().NumberU64()
	hashChain, whitelist, err := sh.getHashChain(sh.prepareBlockHashNumbers(curBN))
	if err != nil {
		return 0, errors.Wrap(err, "getHashChain")
	}
	if len(hashChain) == 0 {
		// short circuit for no sync is needed
		return 0, nil
	}

	expEndBN := curBN + uint64(len(hashChain))
	d.logger.Info().Uint64("current number", curBN).
		Uint64("target number", expEndBN).
		Interface("hashChain", hashChain).
		Msg("short range start syncing")
	d.startSyncing()
	d.status.setTargetBN(expEndBN)
	defer func() {
		d.logger.Info().Msg("short range finished syncing")
		d.finishSyncing()
	}()

	blocks, stids, err := sh.getBlocksByHashes(hashChain, whitelist)
	if err != nil {
		d.logger.Warn().Err(err).Msg("getBlocksByHashes failed")
		if !errors.Is(err, context.Canceled) {
			sh.removeStreams(whitelist) // Remote nodes cannot provide blocks with target hashes
		}
		return 0, errors.Wrap(err, "getBlocksByHashes")
	}
	d.logger.Info().Int("num blocks", len(blocks)).Msg("getBlockByHashes result")

	n, err := verifyAndInsertBlocks(d.bc, blocks)
	numBlocksInsertedShortRangeHistogramVec.With(d.promLabels()).Observe(float64(n))
	if err != nil {
		d.logger.Warn().Err(err).Int("blocks inserted", n).Msg("Insert block failed")
		if sh.blameAllStreams(blocks, n, err) {
			sh.removeStreams(whitelist) // Data provided by remote nodes is corrupted
		} else {
			// It is the last block gives a wrong commit sig. Blame the provider of the last block.
			st2Blame := stids[len(stids)-1]
			sh.removeStreams([]sttypes.StreamID{st2Blame})
		}
		return n, err
	}
	d.logger.Info().Err(err).Int("blocks inserted", n).Msg("Insert block success")

	return len(blocks), nil
}

func (d *Downloader) doShortRangeSyncForEpochSync() (int, error) {
	numShortRangeCounterVec.With(d.promLabels()).Inc()

	srCtx, cancel := context.WithTimeout(d.ctx, shortRangeTimeout)
	defer cancel()

	sh := &srHelper{
		syncProtocol: d.syncProtocol,
		ctx:          srCtx,
		config:       d.config,
		logger:       d.logger.With().Str("mode", "short range").Logger(),
	}

	if err := sh.checkPrerequisites(); err != nil {
		return 0, errors.Wrap(err, "prerequisite")
	}
	curBN := d.bc.CurrentBlock().NumberU64()
	bns := make([]uint64, 0, numBlocksByNumPerRequest)
	loopEpoch := d.bc.CurrentHeader().Epoch().Uint64() //+ 1
	for len(bns) < numBlocksByNumPerRequest {
		blockNum := shard.Schedule.EpochLastBlock(loopEpoch)
		if blockNum > curBN {
			bns = append(bns, blockNum)
		}
		loopEpoch = loopEpoch + 1
	}

	if len(bns) == 0 {
		return 0, nil
	}

	blocks, streamID, err := sh.getBlocksChain(bns)
	if err != nil {
		return 0, errors.Wrap(err, "getHashChain")
	}
	if len(blocks) == 0 {
		// short circuit for no sync is needed
		return 0, nil
	}
	n, err := d.bc.InsertChain(blocks, true)
	numBlocksInsertedShortRangeHistogramVec.With(d.promLabels()).Observe(float64(n))
	if err != nil {
		sh.removeStreams([]sttypes.StreamID{streamID}) // Data provided by remote nodes is corrupted
		return n, err
	}
	d.logger.Info().Err(err).Int("blocks inserted", n).Msg("Insert block success")

	return len(blocks), nil
}

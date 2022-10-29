package stagedstreamsync

import (
	"context"
	"fmt"
	"time"

	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type StageStates struct {
	configs StageStatesCfg
}
type StageStatesCfg struct {
	ctx         context.Context
	bc          core.BlockChain
	db          kv.RwDB
	logger      zerolog.Logger
	logProgress bool
}

func NewStageStates(cfg StageStatesCfg) *StageStates {
	return &StageStates{
		configs: cfg,
	}
}

func NewStageStatesCfg(ctx context.Context,
	bc core.BlockChain,
	db kv.RwDB,
	logger zerolog.Logger,
	logProgress bool) StageStatesCfg {

	return StageStatesCfg{
		ctx:         ctx,
		bc:          bc,
		db:          db,
		logger:      logger,
		logProgress: logProgress,
	}
}

func (stg *StageStates) SetStageContext(ctx context.Context) {
	stg.configs.ctx = ctx
}

// Exec progresses States stage in the forward direction
func (stg *StageStates) Exec(firstCycle bool, invalidBlockRevert bool, s *StageState, reverter Reverter, tx kv.RwTx) (err error) {

	// for short range sync, skip this step
	if !s.state.initSync {
		return nil
	}

	maxHeight := s.state.status.targetBN
	currentHead := stg.configs.bc.CurrentBlock().NumberU64()
	if currentHead >= maxHeight {
		return nil
	}
	currProgress := stg.configs.bc.CurrentBlock().NumberU64()
	targetHeight := s.state.currentCycle.TargetHeight
	if currProgress >= targetHeight {
		return nil
	}
	useInternalTx := tx == nil
	if useInternalTx {
		var err error
		tx, err = stg.configs.db.BeginRw(stg.configs.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	// blocksBucketName := GetBucketName(DownloadedBlocksBucket, s.state.isBeacon)
	// isLastCycle := targetHeight >= maxHeight
	// startTime := time.Now()
	// startBlock := currProgress

	if stg.configs.logProgress {
		fmt.Print("\033[s") // save the cursor position
	}

	// insert blocks
	// insert the blocks to chain. Return when the target block number is reached.
	// blockResults := s.state.gbm.PullContinuousBlocks(blocksPerInsert)
	// s.state.inserted = 0
	// if len(blockResults) > 0 {
	// 	lbls := s.state.promLabels()
	// 	nInserted := stg.processBlocks(blockResults, s.state.gbm, s.state.protocol, lbls, targetHeight)
	// 	s.state.inserted = nInserted
	// }

	stg.insertChainLoop(s.state.gbm, s.state.protocol, s.state.promLabels(), targetHeight)
	select {
	case <-s.state.ctx.Done():
		return s.state.ctx.Err()
	default:
	}

	if useInternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

func (stg *StageStates) insertChainLoop(gbm *getBlocksManager,
	protocol syncProtocol,
	lbls prometheus.Labels,
	targetBN uint64) {

	var (
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
		case <-stg.configs.ctx.Done():
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
				stg.processBlocks(blockResults, gbm, protocol, lbls, targetBN)
				// more blocks is expected being able to be pulled from queue
				trigger()
			}
			if stg.configs.bc.CurrentBlock().NumberU64() >= targetBN {
				return
			}
		}
	}
}

func (stg *StageStates) processBlocks(results []*blockResult,
	gbm *getBlocksManager,
	protocol syncProtocol,
	pl prometheus.Labels,
	targetBN uint64) int {

	blocks := blockResultsToBlocks(results)
	var nInserted int

	for i, block := range blocks {
		if err := verifyAndInsertBlock(stg.configs.bc, block); err != nil {
			stg.configs.logger.Warn().Err(err).Uint64("target block", targetBN).
				Uint64("block number", block.NumberU64()).
				Msg("insert blocks failed in long range")
			pl["error"] = err.Error()
			longRangeFailInsertedBlockCounterVec.With(pl).Inc()

			protocol.RemoveStream(results[i].stid)
			gbm.HandleInsertError(results, i)
			return nInserted
		}
		nInserted++
		longRangeSyncedBlockCounterVec.With(pl).Inc()
	}
	gbm.HandleInsertResult(results)
	return nInserted
}

func (stg *StageStates) saveProgress(s *StageState, tx kv.RwTx) (err error) {

	useInternalTx := tx == nil
	if useInternalTx {
		var err error
		tx, err = stg.configs.db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	// save progress
	if err = s.Update(tx, stg.configs.bc.CurrentBlock().NumberU64()); err != nil {
		utils.Logger().Error().
			Err(err).
			Msgf("[STAGED_SYNC] saving progress for block States stage failed")
		return ErrSaveStateProgressFail
	}

	if useInternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (stg *StageStates) Revert(firstCycle bool, u *RevertState, s *StageState, tx kv.RwTx) (err error) {
	useInternalTx := tx == nil
	if useInternalTx {
		tx, err = stg.configs.db.BeginRw(stg.configs.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	if err = u.Done(tx); err != nil {
		return err
	}

	if useInternalTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (stg *StageStates) CleanUp(firstCycle bool, p *CleanUpState, tx kv.RwTx) (err error) {
	useInternalTx := tx == nil
	if useInternalTx {
		tx, err = stg.configs.db.BeginRw(stg.configs.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	if useInternalTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

package stagedstreamsync

import (
	"context"
	"fmt"
	"sync"

	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/ledgerwatch/erigon-lib/kv"
)

type StageBodies struct {
	configs StageBodiesCfg
}
type StageBodiesCfg struct {
	ctx         context.Context
	bc          core.BlockChain
	db          kv.RwDB
	isBeacon    bool
	logProgress bool
}

func NewStageBodies(cfg StageBodiesCfg) *StageBodies {
	return &StageBodies{
		configs: cfg,
	}
}

func NewStageBodiesCfg(ctx context.Context, bc core.BlockChain, db kv.RwDB, isBeacon bool, logProgress bool) StageBodiesCfg {
	return StageBodiesCfg{
		ctx:         ctx,
		bc:          bc,
		db:          db,
		isBeacon:    isBeacon,
		logProgress: logProgress,
	}
}

func (b *StageBodies) SetStageContext(ctx context.Context) {
	b.configs.ctx = ctx
}

// Exec progresses Bodies stage in the forward direction
func (b *StageBodies) Exec(firstCycle bool, invalidBlockRevert bool, s *StageState, reverter Reverter, tx kv.RwTx) (err error) {

	// for short range sync, skip this stage
	if !s.state.initSync {
		return nil
	}

	maxHeight := s.state.status.targetBN
	currentHead := b.configs.bc.CurrentBlock().NumberU64()
	if currentHead >= maxHeight {
		return nil
	}
	currProgress := uint64(0)
	targetHeight := s.state.currentCycle.TargetHeight
	// isBeacon := s.state.isBeacon
	// isLastCycle := targetHeight >= maxHeight

	if errV := CreateView(b.configs.ctx, b.configs.db, tx, func(etx kv.Tx) error {
		if currProgress, err = s.CurrentStageProgress(etx); err != nil {
			return err
		}
		return nil
	}); errV != nil {
		return errV
	}

	if currProgress == 0 {
		if err := b.clearBlocksBucket(tx, s.state.isBeacon); err != nil {
			return err
		}
		currProgress = currentHead
	}

	if currProgress >= targetHeight {
		return nil
	}

	// size := uint64(0)
	// startTime := time.Now()
	// startBlock := currProgress
	if b.configs.logProgress {
		fmt.Print("\033[s") // save the cursor position
	}

	// Fetch blocks from neighbors
	s.state.gbm = newGetBlocksManager(b.configs.bc, targetHeight, s.state.logger)

	// Setup workers to fetch blocks from remote node
	var wg sync.WaitGroup

	for i := 0; i != s.state.config.Concurrency; i++ {
		worker := &getBlocksWorker{
			gbm:      s.state.gbm,
			protocol: s.state.protocol,
			ctx:      b.configs.ctx,
		}
		wg.Add(1)
		go worker.workLoop(&wg)
	}

	wg.Wait()

	return nil
}

func (b *StageBodies) clearBlocksBucket(tx kv.RwTx, isBeacon bool) error {
	useInternalTx := tx == nil
	if useInternalTx {
		var err error
		tx, err = b.configs.db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
	bucketName := GetBucketName(DownloadedBlocksBucket, isBeacon)
	if err := tx.ClearBucket(bucketName); err != nil {
		return err
	}

	if useInternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (b *StageBodies) saveProgress(s *StageState, progress uint64, tx kv.RwTx) (err error) {
	useInternalTx := tx == nil
	if useInternalTx {
		var err error
		tx, err = b.configs.db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	// save progress
	if err = s.Update(tx, progress); err != nil {
		utils.Logger().Error().
			Err(err).
			Msgf("[STAGED_SYNC] saving progress for block bodies stage failed")
		return ErrSavingBodiesProgressFail
	}

	if useInternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (b *StageBodies) Revert(firstCycle bool, u *RevertState, s *StageState, tx kv.RwTx) (err error) {
	useInternalTx := tx == nil
	if useInternalTx {
		tx, err = b.configs.db.BeginRw(b.configs.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	// clean block hashes db
	blocksBucketName := GetBucketName(DownloadedBlocksBucket, b.configs.isBeacon)
	if err = tx.ClearBucket(blocksBucketName); err != nil {
		utils.Logger().Error().
			Err(err).
			Msgf("[STAGED_SYNC] clear blocks bucket after revert failed")
		return err
	}

	// save progress
	currentHead := b.configs.bc.CurrentBlock().NumberU64()
	if err = s.Update(tx, currentHead); err != nil {
		utils.Logger().Error().
			Err(err).
			Msgf("[STAGED_SYNC] saving progress for block bodies stage after revert failed")
		return err
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

func (b *StageBodies) CleanUp(firstCycle bool, p *CleanUpState, tx kv.RwTx) (err error) {
	useInternalTx := tx == nil
	if useInternalTx {
		tx, err = b.configs.db.BeginRw(b.configs.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	blocksBucketName := GetBucketName(DownloadedBlocksBucket, b.configs.isBeacon)
	tx.ClearBucket(blocksBucketName)

	if useInternalTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

package stagedstreamsync

import (
	"context"
	"time"

	"github.com/harmony-one/harmony/consensus"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/node/worker"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/erigon-lib/kv/memdb"
	"github.com/ledgerwatch/log/v3"
	"github.com/rs/zerolog"
)

const (
	BlockHashesBucket            = "BlockHashes"
	BeaconBlockHashesBucket      = "BeaconBlockHashes"
	DownloadedBlocksBucket       = "BlockBodies"
	BeaconDownloadedBlocksBucket = "BeaconBlockBodies" // Beacon Block bodies are downloaded, TxHash and UncleHash are getting verified
	LastMileBlocksBucket         = "LastMileBlocks"    // last mile blocks to catch up with the consensus
	StageProgressBucket          = "StageProgress"

	// cache db keys
	LastBlockHeight = "LastBlockHeight"
	LastBlockHash   = "LastBlockHash"
)

var Buckets = []string{
	BlockHashesBucket,
	BeaconBlockHashesBucket,
	DownloadedBlocksBucket,
	BeaconDownloadedBlocksBucket,
	LastMileBlocksBucket,
	StageProgressBucket,
}

// CreateStagedSync creates an instance of staged sync
func CreateStagedSync(
	bc core.BlockChain,
	UseMemDB bool,
	protocol syncProtocol,
	status status,
	config Config,
	logger zerolog.Logger,
	logProgress bool,
) (*StagedStreamSync, error) {

	ctx := context.Background()
	isBeacon := bc.ShardID() == bc.Engine().Beaconchain().ShardID()

	var db kv.RwDB
	if UseMemDB {
		db = memdb.New()
	} else {
		if isBeacon {
			db = mdbx.NewMDBX(log.New()).Path("cache_beacon_db").MustOpen()
		} else {
			db = mdbx.NewMDBX(log.New()).Path("cache_shard_db").MustOpen()
		}
	}

	if errInitDB := initDB(ctx, db); errInitDB != nil {
		return nil, errInitDB
	}

	stageHeadsCfg := NewStageHeadersCfg(ctx, bc, db)
	stageShortRangeCfg := NewStageShortRangeCfg(ctx, bc, db)
	stageSyncEpochCfg := NewStageEpochCfg(ctx, bc, db)
	stageBodiesCfg := NewStageBodiesCfg(ctx, bc, db, isBeacon, logProgress)
	stageStatesCfg := NewStageStatesCfg(ctx, bc, db, logger, logProgress)
	stageFinishCfg := NewStageFinishCfg(ctx, db)

	stages := DefaultStages(ctx,
		stageHeadsCfg,
		stageShortRangeCfg,
		stageSyncEpochCfg,
		stageBodiesCfg,
		stageStatesCfg,
		stageFinishCfg,
	)

	return New(ctx,
		bc,
		db,
		stages,
		isBeacon,
		protocol,
		nil,
		status,
		config,
		logger,
	), nil
}

// init sync loop main database and create buckets
func initDB(ctx context.Context, db kv.RwDB) error {
	tx, errRW := db.BeginRw(ctx)
	if errRW != nil {
		return errRW
	}
	defer tx.Rollback()
	for _, name := range Buckets {
		// create bucket
		if err := tx.CreateBucket(GetStageName(name, false, false)); err != nil {
			return err
		}
		// create bucket for beacon
		if err := tx.CreateBucket(GetStageName(name, true, false)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// SyncLoop will keep syncing with peers until catches up
func (s *StagedStreamSync) SyncLoop(bc core.BlockChain, worker *worker.Worker, isBeacon bool, consensus *consensus.Consensus, loopMinTime time.Duration) {

	utils.Logger().Info().
		Uint64("current height", bc.CurrentBlock().NumberU64()).
		Msgf("staged sync is executing ... ")

	startTime := time.Now()

	if loopMinTime != 0 {
		waitTime := loopMinTime - time.Since(startTime)
		utils.Logger().Debug().
			Bool("isBeacon", isBeacon).
			Uint32("shard", bc.ShardID()).
			Interface("duration", waitTime).
			Msgf("[STAGED SYNC] Node is syncing ..., it's waiting a few seconds until next loop")
		c := time.After(waitTime)
		select {
		case <-s.Context().Done():
			return
		case <-c:
		}
	}

	utils.Logger().Info().
		Uint64("new height", bc.CurrentBlock().NumberU64()).
		Msgf("staged sync is executed")
	return
}

// runSyncCycle will run one cycle of staged syncing
func (s *StagedStreamSync) runSyncCycle(bc core.BlockChain, worker *worker.Worker, isBeacon bool, consensus *consensus.Consensus, maxPeersHeight uint64) error {
	canRunCycleInOneTransaction := false
	var tx kv.RwTx
	if canRunCycleInOneTransaction {
		var err error
		if tx, err = s.DB().BeginRw(context.Background()); err != nil {
			return err
		}
		defer tx.Rollback()
	}
	// Do one cycle of staged sync

	if tx != nil {
		errTx := tx.Commit()
		if errTx != nil {
			return errTx
		}
	}
	return nil
}

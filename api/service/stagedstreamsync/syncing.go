package stagedstreamsync

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/harmony-one/harmony/consensus"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/core/rawdb"
	"github.com/harmony-one/harmony/core/types"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/harmony-one/harmony/shard"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/log/v3"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

const (
	BlocksBucket          = "BlockBodies"
	BlockHashesBucket     = "BlockHashes"
	BlockSignaturesBucket = "BlockSignatures"
	StageProgressBucket   = "StageProgress"

	// cache db keys
	LastBlockHeight = "LastBlockHeight"
	LastBlockHash   = "LastBlockHash"
)

var Buckets = []string{
	BlocksBucket,
	BlockHashesBucket,
	BlockSignaturesBucket,
	StageProgressBucket,
}

// CreateStagedSync creates an instance of staged sync
func CreateStagedSync(ctx context.Context,
	bc core.BlockChain,
	nodeConfig *nodeconfig.ConfigType,
	consensus *consensus.Consensus,
	dbDir string,
	protocol syncProtocol,
	config Config,
	isBeaconNode bool,
	logger zerolog.Logger,
) (*StagedStreamSync, error) {

	logger.Info().
		Uint32("shard", bc.ShardID()).
		Bool("isBeaconNode", isBeaconNode).
		Bool("memdb", config.UseMemDB).
		Str("dbDir", dbDir).
		Bool("serverOnly", config.ServerOnly).
		Int("minStreams", config.MinStreams).
		Msg(WrapStagedSyncMsg("creating staged sync"))

	isExplorer := nodeConfig.Role() == nodeconfig.ExplorerNode
	isValidator := nodeConfig.Role() == nodeconfig.Validator
	isBeaconShard := bc.ShardID() == shard.BeaconChainShardID
	isEpochChain := !isBeaconNode && isBeaconShard
	isBeaconValidator := isBeaconNode && isValidator
	isBeaconExplorer := isBeaconNode && isExplorer
	joinConsensus := false
	if isValidator {
		joinConsensus = true
	}

	var mainDB kv.RwDB
	dbs := make([]kv.RwDB, config.Concurrency)
	beacon := isBeaconValidator || isBeaconExplorer
	if config.UseMemDB {
		mdbPath := getBlockDbPath(bc.ShardID(), beacon, -1, dbDir)
		logger.Info().
			Str("path", mdbPath).
			Msg(WrapStagedSyncMsg("creating main db in memory"))
		mainDB = mdbx.NewMDBX(log.New()).InMem(mdbPath).MustOpen()
		for i := 0; i < config.Concurrency; i++ {
			dbPath := getBlockDbPath(bc.ShardID(), beacon, i, dbDir)
			logger.Info().
				Str("path", dbPath).
				Msg(WrapStagedSyncMsg("creating blocks db in memory"))
			dbs[i] = mdbx.NewMDBX(log.New()).InMem(dbPath).MustOpen()
		}
	} else {
		mdbPath := getBlockDbPath(bc.ShardID(), beacon, -1, dbDir)
		logger.Info().
			Str("path", mdbPath).
			Msg(WrapStagedSyncMsg("creating main db in disk"))
		mainDB = mdbx.NewMDBX(log.New()).Path(mdbPath).MustOpen()
		for i := 0; i < config.Concurrency; i++ {
			dbPath := getBlockDbPath(bc.ShardID(), beacon, i, dbDir)
			logger.Info().
				Str("path", dbPath).
				Msg(WrapStagedSyncMsg("creating blocks db in disk"))
			dbs[i] = mdbx.NewMDBX(log.New()).Path(dbPath).MustOpen()
		}
	}

	if errInitDB := initDB(ctx, mainDB, dbs); errInitDB != nil {
		logger.Error().Err(errInitDB).Msg("create staged sync instance failed")
		return nil, errInitDB
	}

	extractReceiptHashes := config.SyncMode == FastSync || config.SyncMode == SnapSync
	stageHeadsCfg := NewStageHeadersCfg(bc, mainDB, logger)
	stageShortRangeCfg := NewStageShortRangeCfg(bc, mainDB, logger)
	stageSyncEpochCfg := NewStageEpochCfg(bc, mainDB, logger)
	stageBodiesCfg := NewStageBodiesCfg(bc, mainDB, dbs, config.Concurrency, protocol, extractReceiptHashes, logger, config.LogProgress)
	stageHashesCfg := NewStageBlockHashesCfg(bc, mainDB, config.Concurrency, protocol, logger, config.LogProgress)
	stageStatesCfg := NewStageStatesCfg(bc, mainDB, dbs, config.Concurrency, logger, config.LogProgress)
	stageStateSyncCfg := NewStageStateSyncCfg(bc, mainDB, config.Concurrency, protocol, logger, config.LogProgress)
	stageFullStateSyncCfg := NewStageFullStateSyncCfg(bc, mainDB, config.Concurrency, protocol, logger, config.LogProgress)
	stageReceiptsCfg := NewStageReceiptsCfg(bc, mainDB, dbs, config.Concurrency, protocol, logger, config.LogProgress)
	stageFinishCfg := NewStageFinishCfg(mainDB, logger)

	// init stages order based on sync mode
	initStagesOrder(config.SyncMode)

	defaultStages := DefaultStages(ctx,
		stageHeadsCfg,
		stageSyncEpochCfg,
		stageShortRangeCfg,
		stageHashesCfg,
		stageBodiesCfg,
		stageStateSyncCfg,
		stageFullStateSyncCfg,
		stageStatesCfg,
		stageReceiptsCfg,
		stageFinishCfg,
	)

	logger.Info().
		Uint32("shard", bc.ShardID()).
		Bool("isEpochChain", isEpochChain).
		Bool("isExplorer", isExplorer).
		Bool("isValidator", isValidator).
		Bool("isBeaconShard", isBeaconShard).
		Bool("isBeaconValidator", isBeaconValidator).
		Bool("joinConsensus", joinConsensus).
		Bool("memdb", config.UseMemDB).
		Str("dbDir", dbDir).
		Str("SyncMode", config.SyncMode.String()).
		Bool("serverOnly", config.ServerOnly).
		Int("minStreams", config.MinStreams).
		Str("dbDir", dbDir).
		Msg(WrapStagedSyncMsg("staged stream sync created successfully"))

	return New(
		bc,
		consensus,
		mainDB,
		defaultStages,
		protocol,
		isEpochChain,
		isBeaconShard,
		isBeaconValidator,
		isExplorer,
		isValidator,
		joinConsensus,
		config,
		logger,
	), nil
}

// initDB inits the sync loop main database and create buckets
func initDB(ctx context.Context, mainDB kv.RwDB, dbs []kv.RwDB) error {

	// create buckets for mainDB
	tx, errRW := mainDB.BeginRw(ctx)
	if errRW != nil {
		return errRW
	}
	defer tx.Rollback()

	for _, bucketName := range Buckets {
		if err := tx.CreateBucket(bucketName); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	createBlockBuckets := func(db kv.RwDB) error {
		tx, err := db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := tx.CreateBucket(BlocksBucket); err != nil {
			return err
		}
		if err := tx.CreateBucket(BlockSignaturesBucket); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}

	// create buckets for block cache DBs
	for _, db := range dbs {
		if err := createBlockBuckets(db); err != nil {
			return err
		}
	}

	return nil
}

// getBlockDbPath returns the path of the cache database which stores blocks
func getBlockDbPath(shardID uint32, beacon bool, workerID int, dbDir string) string {
	if beacon {
		if workerID >= 0 {
			return fmt.Sprintf("%s_%d", filepath.Join(dbDir, "cache/beacon_blocks_db"), workerID)
		} else {
			return filepath.Join(dbDir, "cache/beacon_blocks_db_main")
		}
	} else {
		if workerID >= 0 {
			return fmt.Sprintf("%s_%d_%d", filepath.Join(dbDir, "cache/blocks_db"), shardID, workerID)
		} else {
			return fmt.Sprintf("%s_%d", filepath.Join(dbDir, "cache/blocks_db_main"), shardID)
		}
	}
}

func (s *StagedStreamSync) Debug(source string, msg interface{}) {
	// only log the msg in debug mode
	if !s.config.DebugMode {
		return
	}
	pc, _, _, _ := runtime.Caller(1)
	caller := runtime.FuncForPC(pc).Name()
	callerParts := strings.Split(caller, "/")
	if len(callerParts) > 0 {
		caller = callerParts[len(callerParts)-1]
	}
	src := source
	if src == "" {
		src = "message"
	}
	// SSSD: STAGED STREAM SYNC DEBUG
	if msg == nil {
		fmt.Printf("[SSSD:%s] %s: nil or no error\n", caller, src)
	} else if err, ok := msg.(error); ok {
		fmt.Printf("[SSSD:%s] %s: %s\n", caller, src, err.Error())
	} else if str, ok := msg.(string); ok {
		fmt.Printf("[SSSD:%s] %s: %s\n", caller, src, str)
	} else {
		fmt.Printf("[SSSD:%s] %s: %v\n", caller, src, msg)
	}
}

// checkPivot checks pivot block and returns pivot block and cycle Sync mode
func (s *StagedStreamSync) checkPivot(ctx context.Context, estimatedHeight uint64) (*types.Block, SyncMode, error) {

	if s.config.SyncMode == FullSync {
		return nil, FullSync, nil
	}

	// do full sync if chain is at early stage
	if s.initSync && estimatedHeight < MaxPivotDistanceToHead {
		return nil, FullSync, nil
	}

	pivotBlockNumber := uint64(0)
	var curPivot *uint64
	if curPivot = rawdb.ReadLastPivotNumber(s.bc.ChainDb()); curPivot != nil {
		// if head is behind pivot, that means it is still on fast/snap sync mode
		if head := s.CurrentBlockNumber(); head < *curPivot {
			pivotBlockNumber = *curPivot
			// pivot could be moved forward if it is far from head
			if pivotBlockNumber < estimatedHeight-MaxPivotDistanceToHead {
				pivotBlockNumber = estimatedHeight - MinPivotDistanceToHead
			}
		}
	} else {
		if head := s.CurrentBlockNumber(); s.config.SyncMode == FastSync && head <= 1 {
			pivotBlockNumber = estimatedHeight - MinPivotDistanceToHead
			if err := rawdb.WriteLastPivotNumber(s.bc.ChainDb(), pivotBlockNumber); err != nil {
				s.logger.Warn().Err(err).
					Uint64("new pivot number", pivotBlockNumber).
					Msg(WrapStagedSyncMsg("update pivot number failed"))
			}
		}
	}
	if pivotBlockNumber > 0 {
		if block, err := s.queryAllPeersForBlockByNumber(ctx, pivotBlockNumber); err != nil {
			s.logger.Error().Err(err).
				Uint64("pivot", pivotBlockNumber).
				Msg(WrapStagedSyncMsg("query peers for pivot block failed"))
			return block, FastSync, err
		} else {
			if curPivot == nil || pivotBlockNumber != *curPivot {
				if err := rawdb.WriteLastPivotNumber(s.bc.ChainDb(), pivotBlockNumber); err != nil {
					s.logger.Warn().Err(err).
						Uint64("new pivot number", pivotBlockNumber).
						Msg(WrapStagedSyncMsg("update pivot number failed"))
					return block, FastSync, err
				}
			}
			s.status.SetPivotBlock(block)
			s.logger.Info().
				Uint64("estimatedHeight", estimatedHeight).
				Uint64("pivot number", pivotBlockNumber).
				Msg(WrapStagedSyncMsg("fast/snap sync mode, pivot is set successfully"))
			return block, FastSync, nil
		}
	}
	return nil, FullSync, nil
}

// doSync does the long range sync.
// One LongRangeSync consists of several iterations.
// For each iteration, estimate the current block number, then fetch block & insert to blockchain
func (s *StagedStreamSync) doSync(downloaderContext context.Context) (uint64, int, error) {

	startedNumber := s.bc.CurrentBlock().NumberU64()

	var totalInserted int

	if err := s.checkPrerequisites(); err != nil {
		return 0, 0, err
	}

	var estimatedHeight uint64
	if s.initSync {
		if h, err := s.estimateCurrentNumber(downloaderContext); err != nil {
			return 0, 0, err
		} else {
			estimatedHeight = h
			//TODO: use directly currentCycle var
			s.status.SetTargetBN(estimatedHeight)
		}
		if curBN := s.CurrentBlockNumber(); estimatedHeight <= curBN {
			s.logger.Info().Uint64("current number", curBN).Uint64("target number", estimatedHeight).
				Msg(WrapStagedSyncMsg("early return of long range sync (chain is already ahead of target height)"))
			return estimatedHeight, 0, nil
		} else if curBN < estimatedHeight && s.consensus != nil {
			s.consensus.BlocksNotSynchronized("StagedStreamSync.doSync")
		}
	}

	// We are probably in full sync, but we might have rewound to before the
	// fast/snap sync pivot, check if we should reenable
	if pivotBlock, cycleSyncMode, err := s.checkPivot(downloaderContext, estimatedHeight); err != nil {
		s.logger.Error().Err(err).Msg(WrapStagedSyncMsg("check pivot failed"))
		return 0, 0, err
	} else {
		s.status.SetCycleSyncMode(cycleSyncMode)
		s.status.SetPivotBlock(pivotBlock)
	}

	s.startSyncing()
	defer s.finishSyncing()

	ctx, cancel := context.WithCancel(downloaderContext)
	defer cancel()

	for {

		n, err := s.doSyncCycle(ctx)
		if err != nil {
			s.logger.Error().
				Err(err).
				Bool("initSync", s.initSync).
				Bool("isValidator", s.isValidator).
				Bool("isExplorer", s.isExplorer).
				Uint32("shard", s.bc.ShardID()).
				Msg(WrapStagedSyncMsg("sync cycle failed"))

			pl := s.promLabels()
			pl["error"] = err.Error()
			numFailedDownloadCounterVec.With(pl).Inc()

			//cancel()
			return estimatedHeight, totalInserted + n, err
		}
		//cancel()

		totalInserted += n

		// if there is no more blocks to add, skip loop
		if n == 0 {
			break
		}
	}

	if totalInserted > 0 {
		s.logger.Info().
			Bool("initSync", s.initSync).
			Bool("isBeaconShard", s.isBeaconShard).
			Uint32("shard", s.bc.ShardID()).
			Int("blocks", totalInserted).
			Uint64("startedNumber", startedNumber).
			Uint64("currentNumber", s.bc.CurrentBlock().NumberU64()).
			Msg(WrapStagedSyncMsg("sync cycle blocks inserted successfully"))
	}

	if s.consensus != nil && !s.isEpochChain {
		// add consensus last mile blocks
		if hashes, err := s.addConsensusLastMile(s.Blockchain(), s.consensus); err != nil {
			s.logger.Error().Err(err).
				Msg("[STAGED_STREAM_SYNC] Add consensus last mile failed")
			s.RollbackLastMileBlocks(downloaderContext, hashes)
			return estimatedHeight, totalInserted, err
		} else {
			totalInserted += len(hashes)
		}

		// TODO: move this to explorer handler code.
		if s.isExplorer {
			s.consensus.UpdateConsensusInformation("stream sync is explorer")
		}

		if s.initSync && totalInserted > 0 {
			s.consensus.BlocksSynchronized("StagedStreamSync block synchronized")
		}
	}

	return estimatedHeight, totalInserted, nil
}

func (s *StagedStreamSync) doSyncCycle(ctx context.Context) (int, error) {

	// TODO: initSync=true means currentCycleNumber==0, so we can remove initSync

	var totalInserted int

	s.inserted = 0
	startHead := s.CurrentBlockNumber()
	canRunCycleInOneTransaction := false

	var tx kv.RwTx
	if canRunCycleInOneTransaction {
		var err error
		if tx, err = s.DB().BeginRw(ctx); err != nil {
			return totalInserted, err
		}
		defer tx.Rollback()
	}

	startTime := time.Now()

	// Do one cycle of staged sync
	initialCycle := s.currentCycle.GetCycleNumber() == 0
	if err := s.Run(ctx, s.DB(), tx, initialCycle); err != nil {
		s.logger.Error().
			Err(err).
			Bool("isBeaconShard", s.isBeaconShard).
			Uint32("shard", s.bc.ShardID()).
			Uint64("currentHeight", startHead).
			Msg(WrapStagedSyncMsg("sync cycle failed"))
		return totalInserted, err
	}

	totalInserted += s.inserted

	s.currentCycle.AddCycleNumber(1)

	// calculating sync speed (blocks/second)
	if s.config.LogProgress && s.inserted > 0 {
		dt := time.Since(startTime).Seconds()
		speed := float64(0)
		if dt > 0 {
			speed = float64(s.inserted) / dt
		}
		syncSpeed := fmt.Sprintf("%.2f", speed)
		fmt.Println("sync speed:", syncSpeed, "blocks/s")
	}

	return totalInserted, nil
}

func (s *StagedStreamSync) startSyncing() {
	s.status.StartSyncing()
	if s.evtDownloadStartedSubscribed {
		s.evtDownloadStarted.Send(struct{}{})
	}
}

func (s *StagedStreamSync) finishSyncing() {
	s.status.FinishSyncing()
	if s.evtDownloadFinishedSubscribed {
		s.evtDownloadFinished.Send(struct{}{})
	}
}

func (s *StagedStreamSync) checkPrerequisites() error {
	return s.checkHaveEnoughStreams()
}

func (s *StagedStreamSync) CurrentBlockNumber() uint64 {
	// if current head is ahead of pivot block, return chain head regardless of sync mode
	if s.status.HasPivotBlock() && s.bc.CurrentBlock().NumberU64() >= s.status.GetPivotBlockNumber() {
		return s.bc.CurrentBlock().NumberU64()
	}

	if s.status.HasPivotBlock() && s.bc.CurrentFastBlock().NumberU64() >= s.status.GetPivotBlockNumber() {
		return s.bc.CurrentFastBlock().NumberU64()
	}

	current := uint64(0)
	switch s.config.SyncMode {
	case FullSync:
		current = s.bc.CurrentBlock().NumberU64()
	case FastSync:
		current = s.bc.CurrentFastBlock().NumberU64()
	case SnapSync:
		current = s.bc.CurrentHeader().Number().Uint64()
	}
	return current
}

func (s *StagedStreamSync) stateSyncStage() bool {
	switch s.config.SyncMode {
	case FullSync:
		return false
	case FastSync:
		return s.status.HasPivotBlock() && s.bc.CurrentFastBlock().NumberU64() == s.status.GetPivotBlockNumber()-1
	case SnapSync:
		return false
	}
	return false
}

// estimateCurrentNumber roughly estimates the current block number.
// The block number does not need to be exact, but just a temporary target of the iteration.
func (s *StagedStreamSync) estimateCurrentNumber(ctx context.Context) (uint64, error) {
	var (
		cnResults = make(map[sttypes.StreamID]uint64)
		lock      sync.Mutex
		wg        sync.WaitGroup
	)

	// Create a channel to propagate errors back if needed.
	errChan := make(chan error, s.config.Concurrency)

	numStreams := s.protocol.NumStreams()
	wg.Add(numStreams)

	// ask all streams for height
	for i := 0; i != numStreams; i++ {
		go func() {
			defer wg.Done()

			bn, stid, err := s.doGetCurrentNumberRequest(ctx)
			if err != nil {
				s.logger.Err(err).Str("streamID", string(stid)).
					Msg(WrapStagedSyncMsg("getCurrentNumber request failed"))

				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					// Mark stream failure if it's not due to context cancelation or deadline
					s.protocol.StreamFailed(stid, "getCurrentNumber request failed")
				} else {
					s.protocol.RemoveStream(stid, "getCurrentNumber request failed many times")
				}
				return
			}

			// Safely update the cnResults map
			lock.Lock()
			cnResults[stid] = bn
			lock.Unlock()
		}()
	}

	// Wait for all goroutines to finish
	wg.Wait()
	close(errChan) // Close the error channel when done

	// Check if the context was canceled and return an error accordingly
	select {
	case <-ctx.Done():
		s.logger.Error().Err(ctx.Err()).Msg(WrapStagedSyncMsg("context canceled or timed out"))
		return 0, ctx.Err()
	default:
	}

	// If no valid block number results were received, return an error
	if len(cnResults) == 0 {
		return 0, ErrZeroBlockResponse
	}

	// Compute block number by majority vote
	bn := computeBlockNumberByMaxVote(cnResults)
	return bn, nil
}

// queryAllPeersForBlockByNumber queries all connected streams for a block by its number.
func (s *StagedStreamSync) queryAllPeersForBlockByNumber(ctx context.Context, bn uint64) (*types.Block, error) {
	var (
		blkResults []*types.Block
		lock       sync.Mutex
		wg         sync.WaitGroup
	)
	wg.Add(s.config.Concurrency)
	for i := 0; i != s.config.Concurrency; i++ {
		go func() {
			defer wg.Done()
			block, stid, err := s.doGetBlockByNumberRequest(ctx, bn)
			if err != nil {
				s.logger.Err(err).Str("streamID", string(stid)).
					Msg(WrapStagedSyncMsg("getBlockByNumber request failed"))
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					s.protocol.StreamFailed(stid, "getBlockByNumber request failed")
				}
				return
			}
			lock.Lock()
			blkResults = append(blkResults, block)
			lock.Unlock()
		}()
	}
	wg.Wait()

	if len(blkResults) == 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		return nil, ErrZeroBlockResponse
	}
	block, err := getBlockByMaxVote(blkResults)
	if err != nil {
		return nil, err
	}
	return block, nil
}

package stagedstreamsync

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/consensus"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	syncproto "github.com/harmony-one/harmony/p2p/stream/protocols/sync"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type StagedStreamSync struct {
	ctx        context.Context
	bc         core.BlockChain
	isBeacon   bool
	isExplorer bool
	db         kv.RwDB
	protocol   syncProtocol
	gbm        *getBlocksManager // initialized when finished get block number
	inserted   int
	config     Config
	logger     zerolog.Logger
	status     status
	initSync   bool // if sets to true, node start long range syncing

	revertPoint     *uint64 // used to run stages
	prevRevertPoint *uint64 // used to get value from outside of staged sync after cycle (for example to notify RPCDaemon)
	invalidBlock    common.Hash
	currentStage    uint
	LogProgress     bool
	currentCycle    SyncCycle // current cycle
	stages          []*Stage
	revertOrder     []*Stage
	pruningOrder    []*Stage
	timings         []Timing
	logPrefixes     []string

	evtDownloadFinished           event.Feed // channel for each download task finished
	evtDownloadFinishedSubscribed bool
	evtDownloadStarted            event.Feed // channel for each download has started
	evtDownloadStartedSubscribed  bool
}

// BlockWithSig the serialization structure for request DownloaderRequest_BLOCKWITHSIG
// The block is encoded as block + commit signature
type BlockWithSig struct {
	Block              *types.Block
	CommitSigAndBitmap []byte
}

type Timing struct {
	isRevert  bool
	isCleanUp bool
	stage     SyncStageID
	took      time.Duration
}

type SyncCycle struct {
	Number       uint64
	StartHash    []byte
	TargetHeight uint64
	lock         sync.RWMutex
}

func (s *StagedStreamSync) Len() int                    { return len(s.stages) }
func (s *StagedStreamSync) Context() context.Context    { return s.ctx }
func (s *StagedStreamSync) Blockchain() core.BlockChain { return s.bc }
func (s *StagedStreamSync) DB() kv.RwDB                 { return s.db }
func (s *StagedStreamSync) IsBeacon() bool              { return s.isBeacon }
func (s *StagedStreamSync) IsExplorer() bool            { return s.isExplorer }
func (s *StagedStreamSync) LogPrefix() string {
	if s == nil {
		return ""
	}
	return s.logPrefixes[s.currentStage]
}
func (s *StagedStreamSync) PrevRevertPoint() *uint64 { return s.prevRevertPoint }

func (s *StagedStreamSync) NewRevertState(id SyncStageID, revertPoint, currentProgress uint64) *RevertState {
	return &RevertState{id, revertPoint, currentProgress, common.Hash{}, s}
}

func (s *StagedStreamSync) CleanUpStageState(id SyncStageID, forwardProgress uint64, tx kv.Tx, db kv.RwDB) (*CleanUpState, error) {
	var pruneProgress uint64
	var err error

	if errV := CreateView(context.Background(), db, tx, func(tx kv.Tx) error {
		pruneProgress, err = GetStageCleanUpProgress(tx, id, s.isBeacon)
		if err != nil {
			return err
		}
		return nil
	}); errV != nil {
		return nil, errV
	}

	return &CleanUpState{id, forwardProgress, pruneProgress, s}, nil
}

func (s *StagedStreamSync) NextStage() {
	if s == nil {
		return
	}
	s.currentStage++
}

// IsBefore returns true if stage1 goes before stage2 in staged sync
func (s *StagedStreamSync) IsBefore(stage1, stage2 SyncStageID) bool {
	idx1 := -1
	idx2 := -1
	for i, stage := range s.stages {
		if stage.ID == stage1 {
			idx1 = i
		}

		if stage.ID == stage2 {
			idx2 = i
		}
	}

	return idx1 < idx2
}

// IsAfter returns true if stage1 goes after stage2 in staged sync
func (s *StagedStreamSync) IsAfter(stage1, stage2 SyncStageID) bool {
	idx1 := -1
	idx2 := -1
	for i, stage := range s.stages {
		if stage.ID == stage1 {
			idx1 = i
		}

		if stage.ID == stage2 {
			idx2 = i
		}
	}

	return idx1 > idx2
}

func (s *StagedStreamSync) RevertTo(revertPoint uint64, invalidBlock common.Hash) {
	utils.Logger().Info().
		Interface("invalidBlock", invalidBlock).
		Uint64("revertPoint", revertPoint).
		Msgf("[STAGED_SYNC] Reverting blocks")
	s.revertPoint = &revertPoint
	s.invalidBlock = invalidBlock
}

func (s *StagedStreamSync) Done() {
	s.currentStage = uint(len(s.stages))
	s.revertPoint = nil
}

func (s *StagedStreamSync) IsDone() bool {
	return s.currentStage >= uint(len(s.stages)) && s.revertPoint == nil
}

func (s *StagedStreamSync) SetCurrentStage(id SyncStageID) error {
	for i, stage := range s.stages {
		if stage.ID == id {
			s.currentStage = uint(i)
			return nil
		}
	}
	utils.Logger().Error().
		Interface("stage id", id).
		Msgf("[STAGED_SYNC] stage not found")

	return ErrStageNotFound
}

func (s *StagedStreamSync) StageState(stage SyncStageID, tx kv.Tx, db kv.RwDB) (*StageState, error) {
	var blockNum uint64
	var err error
	if errV := CreateView(context.Background(), db, tx, func(rtx kv.Tx) error {
		blockNum, err = GetStageProgress(rtx, stage, s.isBeacon)
		if err != nil {
			return err
		}
		return nil
	}); errV != nil {
		return nil, errV
	}

	return &StageState{s, stage, blockNum}, nil
}

func (s *StagedStreamSync) cleanUp(fromStage int, db kv.RwDB, tx kv.RwTx, firstCycle bool) error {
	found := false
	for i := 0; i < len(s.pruningOrder); i++ {
		if s.pruningOrder[i].ID == s.stages[fromStage].ID {
			found = true
		}
		if !found || s.pruningOrder[i] == nil || s.pruningOrder[i].Disabled {
			continue
		}
		if err := s.pruneStage(firstCycle, s.pruningOrder[i], db, tx); err != nil {
			panic(err)
		}
	}
	return nil
}

func New(ctx context.Context,
	bc core.BlockChain,
	db kv.RwDB,
	stagesList []*Stage,
	isBeacon bool,
	protocol syncProtocol,
	gbm *getBlocksManager,
	status status,
	config Config,
	logger zerolog.Logger,
) *StagedStreamSync {

	logPrefixes := make([]string, len(stagesList))
	for i := range stagesList {
		logPrefixes[i] = fmt.Sprintf("%d/%d %s", i+1, len(stagesList), stagesList[i].ID)
	}

	return &StagedStreamSync{
		ctx:         ctx,
		bc:          bc,
		isBeacon:    isBeacon,
		db:          db,
		protocol:    protocol,
		gbm:         gbm,
		status:      status,
		inserted:    0,
		config:      config,
		logger:      logger,
		stages:      stagesList,
		logPrefixes: logPrefixes,
	}
}

func (s *StagedStreamSync) startSyncing() {
	s.status.startSyncing()
	if s.evtDownloadStartedSubscribed {
		s.evtDownloadStarted.Send(struct{}{})
	}
}

func (s *StagedStreamSync) finishSyncing() {
	s.status.finishSyncing()
	if s.evtDownloadFinishedSubscribed {
		s.evtDownloadFinished.Send(struct{}{})
	}
}

func (s *StagedStreamSync) doSync(initSync bool) error {

	s.initSync = initSync
	// if initSync {
	// 	d.logger.Info().Uint64("current number", d.bc.CurrentBlock().NumberU64()).
	// 		Uint32("shard ID", d.bc.ShardID()).Msg("start long range sync")

	// 	n, err = d.doLongRangeSync()
	// } else {
	// 	d.logger.Info().Uint64("current number", d.bc.CurrentBlock().NumberU64()).
	// 		Uint32("shard ID", d.bc.ShardID()).Msg("start short range sync")

	// 	n, err = d.doShortRangeSync()
	// }

	// if err != nil {
	// 	pl := d.promLabels()
	// 	pl["error"] = err.Error()
	// 	numFailedDownloadCounterVec.With(pl).Inc()
	// 	return
	// }

	//////////////////////////////////////////////////////////
	// My Code                                              //
	//////////////////////////////////////////////////////////
	if err := s.checkPrerequisites(); err != nil {
		return err
	}
	bn, err := s.estimateCurrentNumber()
	if err != nil {
		return err
	}

	utils.Logger().Info().
		Uint64("current height", s.bc.CurrentBlock().NumberU64()).
		Uint64("target height", bn).
		Msgf("staged sync is executing ... ")

	s.status.setTargetBN(bn)

	s.startSyncing()
	defer s.finishSyncing()

	var totalInserted int

	for {
		startHead := s.bc.CurrentBlock().NumberU64()
		canRunCycleInOneTransaction := false

		var tx kv.RwTx
		if canRunCycleInOneTransaction {
			var err error
			if tx, err = s.DB().BeginRw(context.Background()); err != nil {
				return err
			}
			defer tx.Rollback()
		}

		startTime := time.Now()

		// Do one cycle of staged sync
		initialCycle := s.currentCycle.Number == 0
		if err := s.Run(s.DB(), tx, initialCycle); err != nil {
			utils.Logger().Error().
				Err(err).
				Bool("isBeacon", s.isBeacon).
				Uint32("shard", s.bc.ShardID()).
				Uint64("currentHeight", startHead).
				Msgf("[STAGED_SYNC] sync cycle failed")
			break
		}

		// calculating sync speed (blocks/second)
		currHead := s.bc.CurrentBlock().NumberU64()
		if s.LogProgress && currHead-startHead > 0 {
			dt := time.Now().Sub(startTime).Seconds()
			speed := float64(0)
			if dt > 0 {
				speed = float64(currHead-startHead) / dt
			}
			syncSpeed := fmt.Sprintf("%.2f", speed)
			fmt.Println("sync speed:", syncSpeed, "blocks/s")
		}

		totalInserted += int(currHead - startHead)

		s.currentCycle.lock.Lock()
		s.currentCycle.Number++
		s.currentCycle.lock.Unlock()

		if currHead == startHead {
			break
		}
	}

	// for long range set the inserted (for short range it will be set by method itself)
	if initSync {
		s.inserted = totalInserted
	}

	//////////////////////////////////////////////////////////
	// Long Range Sync                                      //
	//////////////////////////////////////////////////////////
	/*
		if err := s.checkPrerequisites(); err != nil {
			return err
		}
		bn, err := s.estimateCurrentNumber()
		if err != nil {
			return err
		}
		if curBN := s.bc.CurrentBlock().NumberU64(); bn <= curBN {
			s.logger.Info().Uint64("current number", curBN).Uint64("target number", bn).
				Msg("early return of long range sync")
			return nil
		}

		s.downloader.startSyncing()
		defer s.downloader.finishSyncing()

		s.logger.Info().Uint64("target number", bn).Msg("estimated remote current number")
		s.status.setTargetBN(bn)
	*/

	//return s.fetchAndInsertBlocks(bn)
	return nil
}

func (s *StagedStreamSync) checkPrerequisites() error {
	return s.checkHaveEnoughStreams()
}

// estimateCurrentNumber roughly estimate the current block number.
// The block number does not need to be exact, but just a temporary target of the iteration
func (s *StagedStreamSync) estimateCurrentNumber() (uint64, error) {
	var (
		cnResults = make(map[sttypes.StreamID]uint64)
		lock      sync.Mutex
		wg        sync.WaitGroup
	)
	wg.Add(s.config.Concurrency)
	for i := 0; i != s.config.Concurrency; i++ {
		go func() {
			defer wg.Done()
			bn, stid, err := s.doGetCurrentNumberRequest()
			if err != nil {
				s.logger.Err(err).Str("streamID", string(stid)).
					Msg("getCurrentNumber request failed. Removing stream")
				if !errors.Is(err, context.Canceled) {
					s.protocol.RemoveStream(stid)
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
		case <-s.ctx.Done():
			return 0, s.ctx.Err()
		default:
		}
		return 0, errors.New("zero block number response from remote nodes")
	}
	bn := computeBlockNumberByMaxVote(cnResults)
	return bn, nil
}

func (s *StagedStreamSync) doGetCurrentNumberRequest() (uint64, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	bn, stid, err := s.protocol.GetCurrentBlockNumber(ctx, syncproto.WithHighPriority())
	if err != nil {
		return 0, stid, err
	}
	return bn, stid, nil
}

// fetchAndInsertBlocks use the pipeline pattern to boost the performance of inserting blocks.
// TODO: For resharding, use the pipeline to do fast sync (epoch loop, header loop, body loop)
func (s *StagedStreamSync) fetchAndInsertBlocks(targetBN uint64) error {
	gbm := newGetBlocksManager(s.bc, targetBN, s.logger)
	s.gbm = gbm

	// Setup workers to fetch blocks from remote node
	for i := 0; i != s.config.Concurrency; i++ {
		worker := &getBlocksWorker{
			gbm:      gbm,
			protocol: s.protocol,
			ctx:      s.ctx,
		}
		go worker.workLoop(nil)
	}

	// insert the blocks to chain. Return when the target block number is reached.
	s.insertChainLoop(targetBN)

	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
	}
	return nil
}

func (s *StagedStreamSync) insertChainLoop(targetBN uint64) {
	var (
		gbm     = s.gbm
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
		case <-s.ctx.Done():
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
				s.processBlocks(blockResults, targetBN)
				// more blocks is expected being able to be pulled from queue
				trigger()
			}
			if s.bc.CurrentBlock().NumberU64() >= targetBN {
				return
			}
		}
	}
}

func (s *StagedStreamSync) processBlocks(results []*blockResult, targetBN uint64) {
	blocks := blockResultsToBlocks(results)

	for i, block := range blocks {
		if err := verifyAndInsertBlock(s.bc, block); err != nil {
			s.logger.Warn().Err(err).Uint64("target block", targetBN).
				Uint64("block number", block.NumberU64()).
				Msg("insert blocks failed in long range")
			pl := s.promLabels()
			pl["error"] = err.Error()
			longRangeFailInsertedBlockCounterVec.With(pl).Inc()

			s.protocol.RemoveStream(results[i].stid)
			s.gbm.HandleInsertError(results, i)
			return
		}

		s.inserted++
		longRangeSyncedBlockCounterVec.With(s.promLabels()).Inc()
	}
	s.gbm.HandleInsertResult(results)
}

func (s *StagedStreamSync) promLabels() prometheus.Labels {
	sid := s.bc.ShardID()
	return prometheus.Labels{"ShardID": fmt.Sprintf("%d", sid)}
}

func (s *StagedStreamSync) checkHaveEnoughStreams() error {
	numStreams := s.protocol.NumStreams()
	if numStreams < s.config.MinStreams {
		return fmt.Errorf("number of streams smaller than minimum: %v < %v",
			numStreams, s.config.MinStreams)
	}
	return nil
}

func (s *StagedStreamSync) Run(db kv.RwDB, tx kv.RwTx, firstCycle bool) error {
	s.prevRevertPoint = nil
	s.timings = s.timings[:0]

	for !s.IsDone() {
		var invalidBlockRevert bool
		if s.revertPoint != nil {
			for j := 0; j < len(s.revertOrder); j++ {
				if s.revertOrder[j] == nil || s.revertOrder[j].Disabled {
					continue
				}
				if err := s.revertStage(firstCycle, s.revertOrder[j], db, tx); err != nil {
					return err
				}
			}
			s.prevRevertPoint = s.revertPoint
			s.revertPoint = nil
			if s.invalidBlock != (common.Hash{}) {
				invalidBlockRevert = true
			}
			s.invalidBlock = common.Hash{}
			if err := s.SetCurrentStage(s.stages[0].ID); err != nil {
				return err
			}
			firstCycle = false
		}

		stage := s.stages[s.currentStage]

		if stage.Disabled {
			utils.Logger().Trace().
				Msgf("[STAGED_SYNC] %s disabled. %s", stage.ID, stage.DisabledDescription)

			s.NextStage()
			continue
		}

		if err := s.runStage(stage, db, tx, firstCycle, invalidBlockRevert); err != nil {
			return err
		}

		s.NextStage()
	}

	if err := s.cleanUp(0, db, tx, firstCycle); err != nil {
		return err
	}
	if err := s.SetCurrentStage(s.stages[0].ID); err != nil {
		return err
	}
	if err := printLogs(tx, s.timings); err != nil {
		return err
	}
	s.currentStage = 0
	return nil
}

func CreateView(ctx context.Context, db kv.RwDB, tx kv.Tx, f func(tx kv.Tx) error) error {
	if tx != nil {
		return f(tx)
	}
	return db.View(context.Background(), func(etx kv.Tx) error {
		return f(etx)
	})
}

func ByteCount(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB",
		float64(b)/float64(div), "KMGTPE"[exp])
}

func printLogs(tx kv.RwTx, timings []Timing) error {
	var logCtx []interface{}
	count := 0
	for i := range timings {
		if timings[i].took < 50*time.Millisecond {
			continue
		}
		count++
		if count == 50 {
			break
		}
		if timings[i].isRevert {
			logCtx = append(logCtx, "Revert "+string(timings[i].stage), timings[i].took.Truncate(time.Millisecond).String())
		} else if timings[i].isCleanUp {
			logCtx = append(logCtx, "CleanUp "+string(timings[i].stage), timings[i].took.Truncate(time.Millisecond).String())
		} else {
			logCtx = append(logCtx, string(timings[i].stage), timings[i].took.Truncate(time.Millisecond).String())
		}
	}
	if len(logCtx) > 0 {
		utils.Logger().Info().
			Msgf("[STAGED_SYNC] Timings (slower than 50ms) %v", logCtx...)
	}

	if tx == nil {
		return nil
	}

	if len(logCtx) > 0 { // also don't print this logs if everything is fast
		buckets := Buckets
		bucketSizes := make([]interface{}, 0, 2*len(buckets))
		for _, bucket := range buckets {
			sz, err1 := tx.BucketSize(bucket)
			if err1 != nil {
				return err1
			}
			bucketSizes = append(bucketSizes, bucket, ByteCount(sz))
		}
		utils.Logger().Info().
			Msgf("[STAGED_SYNC] Tables %v", bucketSizes...)
	}
	tx.CollectMetrics()
	return nil
}

func (s *StagedStreamSync) runStage(stage *Stage, db kv.RwDB, tx kv.RwTx, firstCycle bool, invalidBlockRevert bool) (err error) {
	start := time.Now()
	stageState, err := s.StageState(stage.ID, tx, db)
	if err != nil {
		return err
	}

	if err = stage.Handler.Exec(firstCycle, invalidBlockRevert, stageState, s, tx); err != nil {
		utils.Logger().Error().
			Err(err).
			Interface("stage id", stage.ID).
			Msgf("[STAGED_SYNC] stage failed")
		return fmt.Errorf("[%s] %w", s.LogPrefix(), err)
	}
	utils.Logger().Info().
		Msgf("[STAGED_SYNC] stage %s executed successfully", stage.ID)

	took := time.Since(start)
	if took > 60*time.Second {
		logPrefix := s.LogPrefix()
		utils.Logger().Info().
			Msgf("[STAGED_SYNC] [%s] DONE in %d", logPrefix, took)

	}
	s.timings = append(s.timings, Timing{stage: stage.ID, took: took})
	return nil
}

func (s *StagedStreamSync) revertStage(firstCycle bool, stage *Stage, db kv.RwDB, tx kv.RwTx) error {
	start := time.Now()
	utils.Logger().Trace().
		Msgf("[STAGED_SYNC] Revert... stage %s", stage.ID)
	stageState, err := s.StageState(stage.ID, tx, db)
	if err != nil {
		return err
	}

	revert := s.NewRevertState(stage.ID, *s.revertPoint, stageState.BlockNumber)
	revert.InvalidBlock = s.invalidBlock

	if stageState.BlockNumber <= revert.RevertPoint {
		return nil
	}

	if err = s.SetCurrentStage(stage.ID); err != nil {
		return err
	}

	err = stage.Handler.Revert(firstCycle, revert, stageState, tx)
	if err != nil {
		return fmt.Errorf("[%s] %w", s.LogPrefix(), err)
	}

	took := time.Since(start)
	if took > 60*time.Second {
		logPrefix := s.LogPrefix()
		utils.Logger().Info().
			Msgf("[STAGED_SYNC] [%s] Revert done in %d", logPrefix, took)
	}
	s.timings = append(s.timings, Timing{isRevert: true, stage: stage.ID, took: took})
	return nil
}

func (s *StagedStreamSync) pruneStage(firstCycle bool, stage *Stage, db kv.RwDB, tx kv.RwTx) error {
	start := time.Now()
	utils.Logger().Info().
		Msgf("[STAGED_SYNC] CleanUp... stage %s", stage.ID)

	stageState, err := s.StageState(stage.ID, tx, db)
	if err != nil {
		return err
	}

	prune, err := s.CleanUpStageState(stage.ID, stageState.BlockNumber, tx, db)
	if err != nil {
		return err
	}
	if err = s.SetCurrentStage(stage.ID); err != nil {
		return err
	}

	err = stage.Handler.CleanUp(firstCycle, prune, tx)
	if err != nil {
		return fmt.Errorf("[%s] %w", s.LogPrefix(), err)
	}

	took := time.Since(start)
	if took > 60*time.Second {
		logPrefix := s.LogPrefix()
		utils.Logger().Trace().
			Msgf("[STAGED_SYNC] [%s] CleanUp done in %d", logPrefix, took)

		utils.Logger().Info().
			Msgf("[STAGED_SYNC] [%s] CleanUp done in %d", logPrefix, took)
	}
	s.timings = append(s.timings, Timing{isCleanUp: true, stage: stage.ID, took: took})
	return nil
}

// DisableAllStages - including their reverts
func (s *StagedStreamSync) DisableAllStages() []SyncStageID {
	var backupEnabledIds []SyncStageID
	for i := range s.stages {
		if !s.stages[i].Disabled {
			backupEnabledIds = append(backupEnabledIds, s.stages[i].ID)
		}
	}
	for i := range s.stages {
		s.stages[i].Disabled = true
	}
	return backupEnabledIds
}

func (s *StagedStreamSync) DisableStages(ids ...SyncStageID) {
	for i := range s.stages {
		for _, id := range ids {
			if s.stages[i].ID != id {
				continue
			}
			s.stages[i].Disabled = true
		}
	}
}

func (s *StagedStreamSync) EnableStages(ids ...SyncStageID) {
	for i := range s.stages {
		for _, id := range ids {
			if s.stages[i].ID != id {
				continue
			}
			s.stages[i].Disabled = false
		}
	}
}

// checkPeersDuplicity checks whether there are duplicates in p2p.Peer
func checkPeersDuplicity(ps []p2p.Peer) error {
	type peerDupID struct {
		ip   string
		port string
	}
	m := make(map[peerDupID]struct{})
	for _, p := range ps {
		dip := peerDupID{p.IP, p.Port}
		if _, ok := m[dip]; ok {
			return fmt.Errorf("duplicate peer [%v:%v]", p.IP, p.Port)
		}
		m[dip] = struct{}{}
	}
	return nil
}

// GetActivePeerNumber returns the number of active peers
func (ss *StagedStreamSync) GetActiveStreams() int {
	//TODO: return active streams
	return 0
}

// RlpDecodeBlockOrBlockWithSig decode payload to types.Block or BlockWithSig.
// Return the block with commitSig if set.
func RlpDecodeBlockOrBlockWithSig(payload []byte) (*types.Block, error) {
	var block *types.Block
	if err := rlp.DecodeBytes(payload, &block); err == nil {
		// received payload as *types.Block
		return block, nil
	}

	var bws BlockWithSig
	if err := rlp.DecodeBytes(payload, &bws); err == nil {
		block := bws.Block
		block.SetCurrentCommitSig(bws.CommitSigAndBitmap)
		return block, nil
	}
	return nil, errors.New("failed to decode to either types.Block or BlockWithSig")
}

// CompareBlockByHash compares two block by hash, it will be used in sort the blocks
func CompareBlockByHash(a *types.Block, b *types.Block) int {
	ha := a.Hash()
	hb := b.Hash()
	return bytes.Compare(ha[:], hb[:])
}

// GetHowManyMaxConsensus will get the most common blocks and the first such blockID
func GetHowManyMaxConsensus(blocks []*types.Block) (int, int) {
	// As all peers are sorted by their blockHashes, all equal blockHashes should come together and consecutively.
	curCount := 0
	curFirstID := -1
	maxCount := 0
	maxFirstID := -1
	for i := range blocks {
		if curFirstID == -1 || CompareBlockByHash(blocks[curFirstID], blocks[i]) != 0 {
			curCount = 1
			curFirstID = i
		} else {
			curCount++
		}
		if curCount > maxCount {
			maxCount = curCount
			maxFirstID = curFirstID
		}
	}
	return maxFirstID, maxCount
}

func (ss *StagedStreamSync) addConsensusLastMile(bc core.BlockChain, consensus *consensus.Consensus) error {
	curNumber := bc.CurrentBlock().NumberU64()
	blockIter, err := consensus.GetLastMileBlockIter(curNumber + 1)
	if err != nil {
		return err
	}
	for {
		block := blockIter.Next()
		if block == nil {
			break
		}
		if _, err := bc.InsertChain(types.Blocks{block}, true); err != nil {
			return errors.Wrap(err, "failed to InsertChain")
		}
	}
	return nil
}

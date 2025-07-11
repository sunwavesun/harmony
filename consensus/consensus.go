package consensus

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harmony-one/abool"
	bls_core "github.com/harmony-one/bls/ffi/go/bls"
	"github.com/harmony-one/harmony/consensus/engine"
	"github.com/harmony-one/harmony/consensus/quorum"
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/crypto/bls"
	bls_cosi "github.com/harmony-one/harmony/crypto/bls"
	"github.com/harmony-one/harmony/internal/registry"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/multibls"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/shard"
	"github.com/harmony-one/harmony/shard/committee"
	"github.com/harmony-one/harmony/staking/slash"
	"github.com/pkg/errors"
)

const (
	vdFAndProofSize = 516 // size of VDF and Proof
	vdfAndSeedSize  = 548 // size of VDF/Proof and Seed
)

var errLeaderPriKeyNotFound = errors.New("leader private key not found locally")

type Proposal struct {
	Type     ProposalType
	Caller   string
	blockNum uint64
}

// NewProposal creates a new proposal
func NewProposal(t ProposalType, blockNum uint64) Proposal {
	return Proposal{
		Type:     t,
		Caller:   utils.GetCallStackInfo(2),
		blockNum: blockNum,
	}
}

// ProposalType is to indicate the type of signal for new block proposal
type ProposalType byte

// Constant of the type of new block proposal
const (
	SyncProposal ProposalType = iota
	AsyncProposal
)

type DownloadAsync interface {
	DownloadAsync()
}

// Consensus is the main struct with all states and data related to consensus process.
type Consensus struct {
	decider quorum.Decider
	// FBFTLog stores the pbft messages and blocks during FBFT process
	fBFTLog *FBFTLog
	// current indicates what state a node is in
	current State
	// isBackup declarative the node is in backup mode
	isBackup bool
	// 2 types of timeouts: normal and viewchange
	consensusTimeout map[TimeoutType]*utils.Timeout
	// Commits collected from validators.
	aggregatedPrepareSig *bls_core.Sign
	aggregatedCommitSig  *bls_core.Sign
	prepareBitmap        *bls_cosi.Mask
	commitBitmap         *bls_cosi.Mask

	multiSigBitmap *bls_cosi.Mask // Bitmap for parsing multisig bitmap from validators

	pendingCXReceipts map[utils.CXKey]*types.CXReceiptsProof // All the receipts received but not yet processed for Consensus
	// Registry for services.
	registry *registry.Registry
	// Minimal number of peers in the shard
	// If the number of validators is less than minPeers, the consensus won't start
	MinPeers int
	// private/public keys of current node
	priKey multibls.PrivateKeys

	// Shard Id which this node belongs to
	ShardID uint32
	// IgnoreViewIDCheck determines whether to ignore viewID check
	IgnoreViewIDCheck *abool.AtomicBool
	// consensus mutex
	mutex *sync.RWMutex
	// ViewChange struct
	vc *viewChange
	// Signal channel for proposing a new block and start new consensus
	readySignal chan Proposal
	// Channel to send full commit signatures to finish new block proposal
	commitSigChannel chan []byte
	// verified block to state sync broadcast
	VerifiedNewBlock chan *types.Block
	// Channel for DRG protocol to send pRnd (preimage of randomness resulting from combined vrf
	// randomnesses) to consensus. The first 32 bytes are randomness, the rest is for bitmap.
	PRndChannel chan []byte
	// Channel for DRG protocol to send VDF. The first 516 bytes are the VDF/Proof and the last 32
	// bytes are the seed for deriving VDF
	RndChannel  chan [vdfAndSeedSize]byte
	pendingRnds [][vdfAndSeedSize]byte // A list of pending randomness
	// The p2p host used to send/receive p2p messages
	host p2p.Host
	// MessageSender takes are of sending consensus message and the corresponding retry logic.
	msgSender *MessageSender
	// Have a dedicated reader thread pull from this chan, like in node
	SlashChan chan slash.Record
	// How long in second the leader needs to wait to propose a new block.
	BlockPeriod time.Duration
	// The time due for next block proposal
	NextBlockDue time.Time
	// Temporary flag to control whether aggregate signature signing is enabled
	AggregateSig bool

	// TODO (leo): an new metrics system to keep track of the consensus/viewchange
	// finality of previous consensus in the unit of milliseconds
	finality int64
	// finalityCounter keep tracks of the finality time
	finalityCounter atomic.Value //int64

	dHelper DownloadAsync

	// Both flags only for initialization state.
	start           bool
	isInitialLeader bool

	// value receives from
	lastKnownSignPower int64
	lastKnowViewChange int64

	transitions struct {
		finalCommit bool
	}
}

// Blockchain returns the blockchain.
func (consensus *Consensus) Blockchain() core.BlockChain {
	return consensus.registry.GetBlockchain()
}

func (consensus *Consensus) FBFTLog() FBFT {
	return threadsafeFBFTLog{
		log: consensus.fBFTLog,
		mu:  consensus.mutex,
	}
}

// ChainReader returns the chain reader.
// This is mostly the same as Blockchain, but it returns only read methods, so we assume it's safe for concurrent use.
func (consensus *Consensus) ChainReader() engine.ChainReader {
	return consensus.Blockchain()
}

func (consensus *Consensus) ReadySignal(p Proposal, signalSource string, signalReason string) {
	consensus.GetLogger().Info().
		Str("signalSource", signalSource).
		Str("signalReason", signalReason).
		Msg("ReadySignal is called to propose new block")
	consensus.readySignal <- p
}

func (consensus *Consensus) GetReadySignal() chan Proposal {
	return consensus.readySignal
}

func (consensus *Consensus) GetCommitSigChannel() chan []byte {
	return consensus.commitSigChannel
}

// Beaconchain returns the beaconchain.
func (consensus *Consensus) Beaconchain() core.BlockChain {
	return consensus.registry.GetBeaconchain()
}

// verifyBlock is a function used to verify the block and keep trace of verified blocks.
func (consensus *Consensus) verifyBlock(block *types.Block) error {
	if !consensus.fBFTLog.IsBlockVerified(block.Hash()) {
		if err := consensus.BlockVerifier(block); err != nil {
			return errors.Errorf("Block verification failed: %s", err)
		}
		consensus.fBFTLog.MarkBlockVerified(block)
	}
	return nil
}

// BlocksSynchronized lets the main loop know that block synchronization finished
// thus the blockchain is likely to be up to date.
func (consensus *Consensus) BlocksSynchronized(reason string) {
	err := consensus.AddConsensusLastMile()
	if err != nil {
		consensus.GetLogger().Error().Err(err).Msg("add last mile failed")
	}
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	if !consensus.transitions.finalCommit {
		consensus.syncReadyChan(reason)
	}
}

// BlocksNotSynchronized lets the main loop know that block is not synchronized
func (consensus *Consensus) BlocksNotSynchronized(reason string) {
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	consensus.syncNotReadyChan(reason)
}

// VdfSeedSize returns the number of VRFs for VDF computation
func (consensus *Consensus) VdfSeedSize() int {
	consensus.mutex.RLock()
	defer consensus.mutex.RUnlock()
	return int(consensus.decider.ParticipantsCount()) * 2 / 3
}

// GetPublicKeys returns the public keys
func (consensus *Consensus) GetPublicKeys() multibls.PublicKeys {
	return consensus.getPublicKeys()
}

func (consensus *Consensus) getPublicKeys() multibls.PublicKeys {
	return consensus.priKey.GetPublicKeys()
}

func (consensus *Consensus) GetLeaderPubKey() *bls_cosi.PublicKeyWrapper {
	return consensus.getLeaderPubKey()
}

func (consensus *Consensus) getLeaderPubKey() *bls_cosi.PublicKeyWrapper {
	return consensus.current.getLeaderPubKey()
}

func (consensus *Consensus) SetLeaderPubKey(pub *bls_cosi.PublicKeyWrapper) {
	consensus.setLeaderPubKey(pub)
}

func (consensus *Consensus) setLeaderPubKey(pub *bls_cosi.PublicKeyWrapper) {
	consensus.current.setLeaderPubKey(pub)
}

func (consensus *Consensus) GetPrivateKeys() multibls.PrivateKeys {
	return consensus.priKey
}

// GetLeaderPrivateKey returns leader private key if node is the leader
func (consensus *Consensus) getLeaderPrivateKey(leaderKey *bls_core.PublicKey) (*bls.PrivateKeyWrapper, error) {
	for i, key := range consensus.priKey {
		if key.Pub.Object.IsEqual(leaderKey) {
			return &consensus.priKey[i], nil
		}
	}
	return nil, errors.Wrap(errLeaderPriKeyNotFound, leaderKey.SerializeToHexStr())
}

// getConsensusLeaderPrivateKey returns consensus leader private key if node is the leader
func (consensus *Consensus) getConsensusLeaderPrivateKey() (*bls.PrivateKeyWrapper, error) {
	return consensus.getLeaderPrivateKey(consensus.getLeaderPubKey().Object)
}

func (consensus *Consensus) IsBackup() bool {
	consensus.mutex.RLock()
	defer consensus.mutex.RUnlock()
	return consensus.isBackup
}

func (consensus *Consensus) BlockNum() uint64 {
	return consensus.getBlockNum()
}

func (consensus *Consensus) getBlockNum() uint64 {
	return atomic.LoadUint64(&consensus.current.blockNum)
}

// New create a new Consensus record
func New(
	host p2p.Host, shard uint32, multiBLSPriKey multibls.PrivateKeys,
	registry *registry.Registry,
	Decider quorum.Decider, minPeers int, aggregateSig bool,
) (*Consensus, error) {
	consensus := Consensus{
		mutex:        &sync.RWMutex{},
		ShardID:      shard,
		fBFTLog:      NewFBFTLog(),
		current:      NewState(Normal, shard),
		decider:      Decider,
		registry:     registry,
		MinPeers:     minPeers,
		AggregateSig: aggregateSig,
		host:         host,
		msgSender:    NewMessageSender(host),
		// FBFT timeout
		consensusTimeout:  createTimeout(),
		dHelper:           downloadAsync{},
		pendingCXReceipts: make(map[utils.CXKey]*types.CXReceiptsProof), // All the receipts received but not yet processed for Consensus
	}

	if multiBLSPriKey != nil {
		consensus.priKey = multiBLSPriKey
		consensus.getLogger().Info().
			Str("publicKey", consensus.GetPublicKeys().SerializeToHexStr()).Msg("My Public Key")
	} else {
		consensus.getLogger().Error().Msg("the bls key is nil")
		return nil, fmt.Errorf("nil bls key, aborting")
	}

	// viewID has to be initialized as the height of
	// the blockchain during initialization as it was
	// displayed on explorer as Height right now
	consensus.setCurBlockViewID(0)
	consensus.SlashChan = make(chan slash.Record)
	consensus.readySignal = make(chan Proposal)
	consensus.commitSigChannel = make(chan []byte)
	// channel for receiving newly generated VDF
	consensus.RndChannel = make(chan [vdfAndSeedSize]byte)
	consensus.IgnoreViewIDCheck = abool.NewBool(false)
	// Make Sure Verifier is not null
	consensus.vc = newViewChange()
	// init prometheus metrics
	initMetrics()
	consensus.AddPubkeyMetrics()

	return &consensus, nil
}

func (consensus *Consensus) GetHost() p2p.Host {
	return consensus.host
}

func (consensus *Consensus) Registry() *registry.Registry {
	return consensus.registry
}

func (consensus *Consensus) Decider() quorum.Decider {
	return quorum.NewThreadSafeDecider(consensus.decider, consensus.mutex)
}

// InitConsensusWithValidators initialize shard state
// from latest epoch and update committee pub
// keys for consensus
func (consensus *Consensus) InitConsensusWithValidators() error {
	consensus.SetMode(Listening)
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()
	err := consensus.initConsensusWithValidators(consensus.Blockchain())
	return err
}

func (consensus *Consensus) initConsensusWithValidators(bc core.BlockChain) (err error) {
	shardID := consensus.ShardID
	currentBlock := bc.CurrentBlock()
	blockNum := currentBlock.NumberU64()

	epoch := currentBlock.Epoch()
	consensus.getLogger().Info().
		Uint64("blockNum", blockNum).
		Uint32("shardID", shardID).
		Uint64("epoch", epoch.Uint64()).
		Msg("[InitConsensusWithValidators] Try To Get PublicKeys")
	shardState, err := committee.WithStakingEnabled.Compute(
		epoch, consensus.Blockchain(),
	)
	if err != nil {
		consensus.getLogger().Err(err).
			Uint64("blockNum", blockNum).
			Uint32("shardID", shardID).
			Uint64("epoch", epoch.Uint64()).
			Msg("[InitConsensusWithValidators] Failed getting shard state")
		return err
	}
	subComm, err := shardState.FindCommitteeByID(shardID)
	if err != nil {
		consensus.getLogger().Err(err).
			Interface("shardState", shardState).
			Msg("[InitConsensusWithValidators] Find CommitteeByID")
		return err
	}
	pubKeys, err := subComm.BLSPublicKeys()
	if err != nil {
		consensus.getLogger().Error().
			Uint32("shardID", shardID).
			Uint64("blockNum", blockNum).
			Msg("[InitConsensusWithValidators] PublicKeys is Empty, Cannot update public keys")
		return errors.Wrapf(
			err,
			"[InitConsensusWithValidators] PublicKeys is Empty, Cannot update public keys",
		)
	}

	for _, key := range pubKeys {
		if consensus.getPublicKeys().Contains(key.Object) {
			consensus.getLogger().Info().
				Uint64("blockNum", blockNum).
				Int("numPubKeys", len(pubKeys)).
				Str("mode", consensus.Mode().String()).
				Msg("[InitConsensusWithValidators] Successfully updated public keys")
			consensus.updatePublicKeys(pubKeys, shard.Schedule.InstanceForEpoch(epoch).ExternalAllowlist())
			consensus.setMode(Normal)
			return nil
		}
	}
	return nil
}

func (consensus *Consensus) SetLastKnownSignPower(signPower, viewChange int64) {
	atomic.StoreInt64(&consensus.lastKnownSignPower, signPower)
	atomic.StoreInt64(&consensus.lastKnowViewChange, viewChange)
}

func (consensus *Consensus) GetLastKnownSignPower() int64 {
	if consensus.IsViewChangingMode() {
		return atomic.LoadInt64(&consensus.lastKnowViewChange)
	}
	return atomic.LoadInt64(&consensus.lastKnownSignPower)
}

type downloadAsync struct {
}

func (a downloadAsync) DownloadAsync() {
}

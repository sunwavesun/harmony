package consensus

// GetConsensusPhase returns the current phase of the consensus.
func (consensus *Consensus) GetConsensusPhase() FBFTPhase {
	return consensus.getConsensusPhase()
}

// GetConsensusPhase returns the current phase of the consensus.
func (consensus *Consensus) getConsensusPhase() FBFTPhase {
	return consensus.current.phase.Load().(FBFTPhase)
}

// GetConsensusMode returns the current mode of the consensus
func (consensus *Consensus) GetConsensusMode() string {
	return consensus.current.Mode().String()
}

// GetCurBlockViewID returns the current view ID of the consensus
// Method is thread safe.
func (consensus *Consensus) GetCurBlockViewID() uint64 {
	return consensus.getCurBlockViewID()
}

// GetCurBlockViewID returns the current view ID of the consensus
func (consensus *Consensus) getCurBlockViewID() uint64 {
	return consensus.current.GetCurBlockViewID()
}

// GetViewChangingID returns the current view changing ID of the consensus.
// Method is thread safe.
func (consensus *Consensus) GetViewChangingID() uint64 {
	return consensus.current.GetViewChangingID()
}

// GetViewChangingID returns the current view changing ID of the consensus
func (consensus *Consensus) getViewChangingID() uint64 {
	return consensus.current.GetViewChangingID()
}

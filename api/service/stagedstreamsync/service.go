package stagedstreamsync

import (
	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/p2p"
)

// StagedStreamSyncService is simply a adapter of Downloaders, which support block synchronization
type StagedStreamSyncService struct {
	Downloaders *Downloaders
}

// NewService creates the a new downloader service
func NewService(host p2p.Host, bcs []core.BlockChain, config Config) *StagedStreamSyncService {
	return &StagedStreamSyncService{
		Downloaders: NewDownloaders(host, bcs, config),
	}
}

// Start start the service
func (s *StagedStreamSyncService) Start() error {
	s.Downloaders.Start()
	return nil
}

// Stop stop the service
func (s *StagedStreamSyncService) Stop() error {
	s.Downloaders.Close()
	return nil
}

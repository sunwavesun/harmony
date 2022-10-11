package stagedstreamsync

import (
	"context"
)

type ForwardOrder []SyncStageID
type RevertOrder []SyncStageID
type CleanUpOrder []SyncStageID

var DefaultForwardOrder = ForwardOrder{
	Heads,
	BlockHashes,
	BlockBodies,
	// Stages below don't use Internet
	States,
	LastMile,
	Finish,
}

var DefaultRevertOrder = RevertOrder{
	Finish,
	LastMile,
	States,
	BlockBodies,
	BlockHashes,
	Heads,
}

var DefaultCleanUpOrder = CleanUpOrder{
	Finish,
	LastMile,
	States,
	BlockBodies,
	BlockHashes,
	Heads,
}

func DefaultStages(ctx context.Context,
	headsCfg StageHeadsCfg,
	) []*Stage {

	handlerStageHeads := NewStageHeads(headsCfg)

	return []*Stage{
		{
			ID:          Heads,
			Description: "Retrieve Chain Heads",
			Handler:     handlerStageHeads,
		},
	}
}

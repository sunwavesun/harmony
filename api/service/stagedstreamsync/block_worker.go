package stagedstreamsync

import (
	"context"
	"sync"
	"time"

	"github.com/harmony-one/harmony/core/types"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/pkg/errors"
)

// getBlocksWorker does the request job
type getBlocksWorker struct {
	gbm      *getBlocksManager
	protocol syncProtocol

	ctx context.Context
}

func (w *getBlocksWorker) workLoop(wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}
		batch := w.gbm.GetNextBatch()
		if len(batch) == 0 {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		blocks, stid, err := w.doBatch(batch)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				w.protocol.RemoveStream(stid)
			}
			err = errors.Wrap(err, "request error")
			w.gbm.HandleRequestError(batch, err, stid)
		} else {
			w.gbm.HandleRequestResult(batch, blocks, stid)
		}
	}
}

func (w *getBlocksWorker) doBatch(bns []uint64) ([]*types.Block, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()

	blocks, stid, err := w.protocol.GetBlocksByNumber(ctx, bns)
	if err != nil {
		return nil, stid, err
	}
	if err := validateGetBlocksResult(bns, blocks); err != nil {
		return nil, stid, err
	}
	return blocks, stid, nil
}

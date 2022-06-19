// Copyright 2020 The Erigon Authors
// This file is part of the Erigon library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Get Max Heights
// Build Sync Matrix
// ProcessStateSync
//
// Consensus Last Mile
//

package stagedsync

import (
	"encoding/binary"
	"fmt"

	"github.com/ledgerwatch/erigon-lib/kv"
)

// SyncStage represents the stages of syncronisation in the Mode.StagedSync mode
// It is used to persist the information about the stage state into the database.
// It should not be empty and should be unique.
type SyncStage string

var (
	Headers     SyncStage = "Headers"     // Headers are downloaded
	BlockHashes SyncStage = "BlockHashes" // Headers hashes are downloaded from peers
	TasksQueue  SyncStage = "TasksQueue"  // Generate Tasks Queue 
	Bodies      SyncStage = "Bodies"      // Block bodies are downloaded, TxHash and UncleHash are getting verified
	Finish      SyncStage = "Finish"      // Nominal stage after all other stages
)

var AllStages = []SyncStage{
	Headers,
	BlockHashes,
	TasksQueue,
	Bodies,
	Finish,
}

// GetStageProgress retrieves saved progress of given sync stage from the database
func GetStageProgress(db kv.Getter, stage SyncStage) (uint64, error) {
	v, err := db.GetOne(kv.SyncStageProgress, []byte(stage))
	if err != nil {
		return 0, err
	}
	return unmarshalData(v)
}

func SaveStageProgress(db kv.Putter, stage SyncStage, progress uint64) error {
	return db.Put(kv.SyncStageProgress, []byte(stage), marshalData(progress))
}

// GetStagePruneProgress retrieves saved progress of given sync stage from the database
func GetStagePruneProgress(db kv.Getter, stage SyncStage) (uint64, error) {
	v, err := db.GetOne(kv.SyncStageProgress, []byte("prune_"+stage))
	if err != nil {
		return 0, err
	}
	return unmarshalData(v)
}

func SaveStagePruneProgress(db kv.Putter, stage SyncStage, progress uint64) error {
	return db.Put(kv.SyncStageProgress, []byte("prune_"+stage), marshalData(progress))
}

func marshalData(blockNumber uint64) []byte {
	return encodeBigEndian(blockNumber)
}

func unmarshalData(data []byte) (uint64, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if len(data) < 8 {
		return 0, fmt.Errorf("value must be at least 8 bytes, got %d", len(data))
	}
	return binary.BigEndian.Uint64(data[:8]), nil
}

func encodeBigEndian(n uint64) []byte {
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], n)
	return v[:]
}
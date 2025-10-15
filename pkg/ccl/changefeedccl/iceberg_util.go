// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
)

// computeFileHash returns SHA-256 over a canonical JSON encoding of
// {path,size,record_count,partition} as a byte slice suitable for the proto.
func computeFileHash(path string, size int64, recordCount int64, partition map[string]string) []byte {
	payload := struct {
		Path        string            `json:"path"`
		Size        int64             `json:"size"`
		RecordCount int64             `json:"record_count"`
		Partition   map[string]string `json:"partition"`
	}{Path: path, Size: size, RecordCount: recordCount, Partition: partition}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	out := make([]byte, len(sum))
	copy(out, sum[:])
	return out
}

// marshalFileClosed constructs and marshals execinfrapb.IcebergFileClosed.
func marshalFileClosed(path string, epoch hlc.Timestamp, tableFQN string, workerID int32, schemaID uint32, specID int32, recordCount int64, fileSize int64, partition map[string]string) []byte {
	// execinfrapb imports util/hlc/timestamp.proto which maps to util/hlc.Timestamp in Go.
	// Use the value directly rather than a pointer to satisfy the generated field type.
	epochCopy := epoch
	fc := &execinfrapb.IcebergFileClosed{
		Path:          path,
		EpochHlc:      epochCopy,
		TableFqn:      tableFQN,
		WorkerId:      workerID,
		SchemaId:      schemaID,
		SpecId:        specID,
		Kind:          execinfrapb.IcebergFileClosed_DATA,
		RecordCount:   recordCount,
		FileSizeBytes: fileSize,
		Partition:     partition,
	}
	fc.FileHash = computeFileHash(path, fileSize, recordCount, partition)
	b, _ := protoutil.Marshal(fc)
	return b
}

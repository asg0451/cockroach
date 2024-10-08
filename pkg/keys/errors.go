// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package keys

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
)

// InvalidRangeMetaKeyError indicates that a Range Metadata key is somehow
// invalid.
type InvalidRangeMetaKeyError struct {
	Msg string
	Key roachpb.Key
}

// NewInvalidRangeMetaKeyError returns a new InvalidRangeMetaKeyError
func NewInvalidRangeMetaKeyError(msg string, k []byte) *InvalidRangeMetaKeyError {
	return &InvalidRangeMetaKeyError{Msg: msg, Key: k}
}

// Error formats error string.
func (i *InvalidRangeMetaKeyError) Error() string {
	return fmt.Sprintf("%q is not valid range metadata key: %s", string(i.Key), i.Msg)
}

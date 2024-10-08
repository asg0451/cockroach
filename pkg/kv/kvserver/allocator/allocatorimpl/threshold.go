// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package allocatorimpl

import (
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/allocator"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/allocator/load"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/errors"
)

const (
	// disableRebalancingThreshold declares a rebalancing threshold that
	// tolerates significant imbalance.
	disableRebalancingThreshold = 0
)

func getLoadThreshold(dim load.Dimension, sv *settings.Values) float64 {
	switch dim {
	case load.Queries:
		return allocator.QPSRebalanceThreshold.Get(sv)
	case load.CPU:
		return allocator.CPURebalanceThreshold.Get(sv)
	default:
		panic(errors.AssertionFailedf("Unkown load dimension %d", dim))
	}
}

// LoadThresholds returns the load threshold values for the passed in
// dimensions.
func LoadThresholds(sv *settings.Values, dims ...load.Dimension) load.Load {
	thresholds := load.Vector{}
	// Initially set each threshold to be disabled.
	for i := range thresholds {
		thresholds[i] = disableRebalancingThreshold
	}

	for _, dim := range dims {
		thresholds[dim] = getLoadThreshold(dim, sv)
	}
	return thresholds
}

func getLoadMinThreshold(dim load.Dimension) float64 {
	switch dim {
	case load.Queries:
		return allocator.MinQPSThresholdDifference
	case load.CPU:
		return allocator.MinCPUThresholdDifference
	default:
		panic(errors.AssertionFailedf("Unkown load dimension %d", dim))
	}
}

// LoadMinThresholds returns the minimum absoute load amounts by which a
// candidate must differ from the mean before it is considered under or
// overfull.
func LoadMinThresholds(dims ...load.Dimension) load.Load {
	diffs := load.Vector{}
	// Initially set each threshold to be disabled.
	for i := range diffs {
		diffs[i] = disableRebalancingThreshold
	}

	for _, dim := range dims {
		diffs[dim] = getLoadMinThreshold(dim)
	}
	return diffs
}

func getLoadRebalanceMinRequiredDiff(dim load.Dimension, sv *settings.Values) float64 {
	switch dim {
	case load.Queries:
		return allocator.MinQPSDifferenceForTransfers.Get(sv)
	case load.CPU:
		return allocator.MinCPUDifferenceForTransfers
	default:
		panic(errors.AssertionFailedf("Unkown load dimension %d", dim))
	}
}

// LoadRebalanceRequiredMinDiff returns the minimum absolute difference between
// an existing (store) and another candidate that must be met before a
// rebalance or transfer is allowed. This setting is purposed to prevent
// thrashing.
func LoadRebalanceRequiredMinDiff(sv *settings.Values, dims ...load.Dimension) load.Load {
	diffs := load.Vector{}
	// Initially set each threshold to be disabled.
	for i := range diffs {
		diffs[i] = disableRebalancingThreshold
	}

	for _, dim := range dims {
		diffs[dim] = getLoadRebalanceMinRequiredDiff(dim, sv)
	}
	return diffs
}

// OverfullLoadThresholds returns the overfull load threshold for each load
// dimension.
func OverfullLoadThresholds(means, thresholds, minThresholds load.Load) load.Load {
	return load.Add(means, load.Max(load.ElementWiseProduct(means, thresholds), minThresholds))
}

// UnderfullLoadThresholds returns the underfull load threshold for each load
// dimension.
func UnderfullLoadThresholds(means, thresholds, minThresholds load.Load) load.Load {
	return load.Sub(means, load.Max(load.ElementWiseProduct(means, thresholds), minThresholds))
}

// MakeQPSOnlyDim returns a load dimension with only QPS filled in with the
// value given.
func MakeQPSOnlyDim(v float64) load.Load {
	dims := load.Vector{}
	dims[load.Queries] = v
	return dims
}

// WithAllDims returns a load vector with all dimensions filled in with the
// value given.
func WithAllDims(v float64) load.Load {
	dims := load.Vector{}
	for i := range dims {
		dims[i] = v
	}
	return dims
}

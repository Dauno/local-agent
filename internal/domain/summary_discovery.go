package domain

import (
	"errors"
	"math"
)

// SummaryDiscoveryTargetFloor is the floor of a reserved
// target_ordinal range for a durable summary job whose real covered range is
// not yet known at schedule time. A job whose stored target_ordinal is at or
// above this floor is a discovery job: the worker that claims it resolves
// the real target itself (from closed-turn history) rather than trusting
// the stored value as a concrete turn ordinal.
//
// internal/usecase/contextsummary and internal/adapter/sqlite both need this
// exact value: the former decides whether to resolve a claimed job as
// discovery, the latter decides whether a target_ordinal collision is
// eligible for the discovery-marker workaround (FIND-134) instead of a plain
// failure. Neither package may import the other (the architecture layering
// runs domain -> port -> usecase -> adapter), so this constant lives here,
// the lowest package both already import, instead of being copied into each
// with a keep-in-sync comment (FIND-135).
//
// The value leaves room below math.MaxInt64 to hand out fresh discovery
// markers (see NextSummaryDiscoveryMarker) for the lifetime of any real
// session, and room above any real turn ordinal that the two ranges can
// never meet.
const SummaryDiscoveryTargetFloor = int64(math.MaxInt64 / 4)

// IsSummaryDiscoveryTarget reports whether ordinal names a discovery job
// rather than a concrete turn ordinal a caller already resolved.
func IsSummaryDiscoveryTarget(ordinal int64) bool {
	return ordinal >= SummaryDiscoveryTargetFloor
}

// ErrSummaryDiscoveryMarkersExhausted is returned by
// NextSummaryDiscoveryMarker when the highest target_ordinal a session has
// ever used already sits at math.MaxInt64, so no incremented marker exists.
var ErrSummaryDiscoveryMarkersExhausted = errors.New("summary discovery markers exhausted")

// NextSummaryDiscoveryMarker returns the marker for a fresh discovery job
// row: one past maxUsed, the highest target_ordinal a session's durable job
// history has ever used (across every status, not only pending/failed). It
// fails closed instead of letting maxUsed+1 wrap past math.MaxInt64 into a
// negative value, which would either violate the jobs table's
// target_ordinal > 0 CHECK constraint or, worse, silently collide with a
// low, concrete ordinal (FIND-135).
func NextSummaryDiscoveryMarker(maxUsed int64) (int64, error) {
	if maxUsed == math.MaxInt64 {
		return 0, ErrSummaryDiscoveryMarkersExhausted
	}
	return maxUsed + 1, nil
}

package attestations

import (
	"sort"

	"github.com/pkg/errors"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/go-bitfield"
	stateTrie "github.com/prysmaticlabs/prysm/beacon-chain/state"
	"github.com/prysmaticlabs/prysm/shared/aggregation"
	"github.com/prysmaticlabs/prysm/shared/bls"
)

// MaxCoverAttestationAggregation relies on Maximum Coverage greedy algorithm for aggregation.
// Aggregation occurs in many rounds, up until no more aggregation is possible (all attestations
// are overlapping).
func MaxCoverAttestationAggregation(atts []*ethpb.Attestation) ([]*ethpb.Attestation, error) {
	if len(atts) < 2 {
		return atts, nil
	}

	aggregated := attList(make([]*ethpb.Attestation, 0, len(atts)))
	unaggregated := attList(atts)

	if err := unaggregated.validate(); err != nil {
		if errors.Is(err, aggregation.ErrBitsDifferentLen) {
			return unaggregated, nil
		}
		return nil, err
	}

	// Aggregation over n/2 rounds is enough to find all aggregatable items (exits earlier if there
	// are many items that can be aggregated).
	for i := 0; i < len(atts)/2; i++ {
		if len(unaggregated) < 2 {
			break
		}

		// Find maximum non-overlapping coverage.
		maxCover := NewMaxCover(unaggregated)
		solution, err := maxCover.Cover(len(atts), false /* allowOverlaps */)
		if err != nil {
			return aggregated.merge(unaggregated), err
		}

		// Exit earlier, if possible cover does not allow aggregation (less than two items).
		if len(solution.Keys) < 2 {
			break
		}

		// Create aggregated attestation and update solution lists.
		if !aggregated.hasCoverage(solution.Coverage) {
			att, err := unaggregated.selectUsingKeys(solution.Keys).aggregate(solution.Coverage)
			if err != nil {
				return aggregated.merge(unaggregated), err
			}
			aggregated = append(aggregated, att)
		}
		unaggregated = unaggregated.selectComplementUsingKeys(solution.Keys)
	}

	return aggregated.merge(unaggregated.filterContained()), nil
}

// optMaxCoverAttestationAggregation relies on Maximum Coverage greedy algorithm for aggregation.
// Aggregation occurs in many rounds, up until no more aggregation is possible (all attestations
// are overlapping).
// NB: this method will replace the MaxCoverAttestationAggregation() above (and will be renamed to it).
func optMaxCoverAttestationAggregation(atts []*ethpb.Attestation) ([]*ethpb.Attestation, error) {
	if len(atts) < 2 {
		return atts, nil
	}

	if err := validateAttestations(atts); err != nil {
		if errors.Is(err, aggregation.ErrBitsDifferentLen) {
			return atts, nil
		}
		return nil, err
	}

	// In the future this conversion will be redundant, as attestation bitlist will be of a Bitlist64
	// type, so incoming `atts` parameters can be used as candidates list directly.
	candidates := make([]*bitfield.Bitlist64, len(atts))
	for i := 0; i < len(atts); i++ {
		candidates[i] = bitfield.NewBitlist64FromBytes(atts[i].AggregationBits.Bytes())
	}
	coveredBitsSoFar := bitfield.NewBitlist64(candidates[0].Len())

	// In order not to re-allocate anything we rely on the very same underlying array, which
	// can only shrink (while the `aggregated` slice length can increase).
	// The `aggregated` slice grows by combining individual attestations and appending to that slice.
	// Both aggregated and non-aggregated slices operate on the very same underlying array.
	aggregated := atts[:0]
	unaggregated := atts[:]

	// Aggregation over n/2 rounds is enough to find all aggregatable items (exits earlier if there
	// are many items that can be aggregated).
	for i := 0; i < len(atts)/2; i++ {
		if len(unaggregated) < 2 {
			break
		}

		// Find maximum non-overlapping coverage.
		selectedKeys, coverage, err := aggregation.MaxCover(candidates, len(candidates), false /* allowOverlaps */)
		if err != nil {
			// Return aggregated attestations, and attestations that couldn't be aggregated.
			return append(aggregated, unaggregated...), err
		}

		// Exit earlier, if possible cover does not allow aggregation (less than two items).
		if selectedKeys.Count() < 2 {
			break
		}

		// Create aggregated attestation and update solution lists. Process aggregates only if they
		// feature at least one unknown bit i.e. can increase the overall coverage.
		if coveredBitsSoFar.XorCount(coverage) > 0 {
			aggIdx, err := aggregateAttestations(atts, selectedKeys, coverage)
			if err != nil {
				return append(aggregated, unaggregated...), err
			}

			// Unless we are already at the right position, swap aggregation and the first non-aggregated item.
			idx0 := len(aggregated)
			if idx0 < aggIdx {
				atts[idx0], atts[aggIdx] = atts[aggIdx], atts[idx0]
				candidates[idx0], candidates[aggIdx] = candidates[aggIdx], candidates[idx0]
				aggregated = atts[:idx0+1] // Expand to the newly added aggregate.
			}

			// Shift the starting point of the slice right.
			unaggregated = unaggregated[1:]
			candidates = candidates[1:]

			// Update covered bits map.
			coveredBitsSoFar.NoAllocOr(coverage, coveredBitsSoFar)
			selectedKeys.SetBitAt(uint64(aggIdx), false)
		}

		// Remove processed attestations.
		processedKeys := selectedKeys.BitIndices()
		rearrangeProcessedAttestations(atts, candidates, processedKeys)
		unaggregated = unaggregated[:len(unaggregated)-len(processedKeys)]
		candidates = candidates[:len(unaggregated)-len(processedKeys)]
	}

	return append(aggregated, filterContainedAttestations(unaggregated)...), nil
}

// NewMaxCover returns initialized Maximum Coverage problem for attestations aggregation.
func NewMaxCover(atts []*ethpb.Attestation) *aggregation.MaxCoverProblem {
	candidates := make([]*aggregation.MaxCoverCandidate, len(atts))
	for i := 0; i < len(atts); i++ {
		candidates[i] = aggregation.NewMaxCoverCandidate(i, &atts[i].AggregationBits)
	}
	return &aggregation.MaxCoverProblem{Candidates: candidates}
}

// aggregate returns list as an aggregated attestation.
func (al attList) aggregate(coverage bitfield.Bitlist) (*ethpb.Attestation, error) {
	if len(al) < 2 {
		return nil, errors.Wrap(ErrInvalidAttestationCount, "cannot aggregate")
	}
	signs := make([]bls.Signature, len(al))
	for i := 0; i < len(al); i++ {
		sig, err := signatureFromBytes(al[i].Signature)
		if err != nil {
			return nil, err
		}
		signs[i] = sig
	}
	return &ethpb.Attestation{
		AggregationBits: coverage,
		Data:            stateTrie.CopyAttestationData(al[0].Data),
		Signature:       aggregateSignatures(signs).Marshal(),
	}, nil
}

// aggregateAttestations combines signatures of selected attestations into a single aggregate attestation, and
// pushes that aggregated attestation into the position of the first of selected attestations.
func aggregateAttestations(
	atts []*ethpb.Attestation, selectedKeys, coverage *bitfield.Bitlist64,
) (targetIdx int, err error) {
	if selectedKeys.Count() < 2 {
		return targetIdx, errors.Wrap(ErrInvalidAttestationCount, "cannot aggregate")
	}

	var data *ethpb.AttestationData
	signs := make([]bls.Signature, 0, selectedKeys.Count())
	for _, idx := range selectedKeys.BitIndices() {
		sig, err := signatureFromBytes(atts[idx].Signature)
		if err != nil {
			return targetIdx, err
		}
		signs = append(signs, sig)
		if data == nil {
			data = stateTrie.CopyAttestationData(atts[idx].Data)
			targetIdx = idx
		}
	}
	// Put aggregated attestation at a position of the first selected attestation.
	atts[targetIdx] = &ethpb.Attestation{
		AggregationBits: coverage.Bytes(),
		Data:            data,
		Signature:       aggregateSignatures(signs).Marshal(),
	}
	return
}

// rearrangeProcessedAttestations pushes processed attestations to the end of the slice, returning
// the number of items re-arranged (so that caller can cut the slice, and allow processed items to be
// garbage collected).
func rearrangeProcessedAttestations(atts []*ethpb.Attestation, candidates []*bitfield.Bitlist64, processedKeys []int) {
	if atts == nil || candidates == nil || processedKeys == nil {
		return
	}
	// Set all selected keys to nil.
	for _, idx := range processedKeys {
		atts[idx] = nil
		candidates[idx] = nil
	}
	// Re-arrange nil items, move them to end of slice.
	for _, idx0 := range processedKeys {
		idx1 := len(atts) - 1
		// Make sure that nil items are swapped for non-nil items only.
		for idx1 > idx0 && atts[idx1] == nil {
			idx1--
		}
		if idx0 == idx1 {
			continue
		}
		atts[idx0], atts[idx1] = atts[idx1], atts[idx0]
		candidates[idx0], candidates[idx1] = candidates[idx1], candidates[idx0]
	}
}

// merge combines two attestation lists into one.
func (al attList) merge(al1 attList) attList {
	return append(al, al1...)
}

// selectUsingKeys returns only items with specified keys.
func (al attList) selectUsingKeys(keys []int) attList {
	filtered := make([]*ethpb.Attestation, len(keys))
	for i, key := range keys {
		filtered[i] = al[key]
	}
	return filtered
}

// selectComplementUsingKeys returns only items with keys that are NOT specified.
func (al attList) selectComplementUsingKeys(keys []int) attList {
	foundInKeys := func(key int) bool {
		for i := 0; i < len(keys); i++ {
			if keys[i] == key {
				keys[i] = keys[len(keys)-1]
				keys = keys[:len(keys)-1]
				return true
			}
		}
		return false
	}
	filtered := al[:0]
	for i, att := range al {
		if !foundInKeys(i) {
			filtered = append(filtered, att)
		}
	}
	return filtered
}

// hasCoverage returns true if a given coverage is found in attestations list.
func (al attList) hasCoverage(coverage bitfield.Bitlist) bool {
	for _, att := range al {
		if att.AggregationBits.Xor(coverage).Count() == 0 {
			return true
		}
	}
	return false
}

// filterContained removes attestations that are contained within other attestations.
func (al attList) filterContained() attList {
	if len(al) < 2 {
		return al
	}
	sort.Slice(al, func(i, j int) bool {
		return al[i].AggregationBits.Count() > al[j].AggregationBits.Count()
	})
	filtered := al[:0]
	filtered = append(filtered, al[0])
	for i := 1; i < len(al); i++ {
		if filtered[len(filtered)-1].AggregationBits.Contains(al[i].AggregationBits) {
			continue
		}
		filtered = append(filtered, al[i])
	}
	return filtered
}

// filterContainedAttestations removes attestations that are contained within other attestations.
func filterContainedAttestations(atts []*ethpb.Attestation) []*ethpb.Attestation {
	return attList(atts).filterContained()
}

// validate checks attestation list for validity (equal bitlength, non-nil bitlist etc).
func (al attList) validate() error {
	if al == nil {
		return errors.New("nil list")
	}
	if len(al) == 0 {
		return errors.Wrap(aggregation.ErrInvalidMaxCoverProblem, "empty list")
	}
	if al[0].AggregationBits == nil || al[0].AggregationBits.Len() == 0 {
		return errors.Wrap(aggregation.ErrInvalidMaxCoverProblem, "bitlist cannot be nil or empty")
	}
	bitlistLen := al[0].AggregationBits.Len()
	for i := 1; i < len(al); i++ {
		if al[i].AggregationBits == nil || bitlistLen != al[i].AggregationBits.Len() {
			return aggregation.ErrBitsDifferentLen
		}
	}
	return nil
}

// validateAttestations checks attestation list for validity (equal bitlength, non-nil bitlist etc).
func validateAttestations(atts []*ethpb.Attestation) error {
	return attList(atts).validate()
}

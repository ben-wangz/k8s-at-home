package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"registry-hub/internal/route"
	"sort"
)

func updateReferrerDescriptors(current []json.RawMessage, descriptor json.RawMessage, remove bool) ([]json.RawMessage, bool, error) {
	desired := make(map[string]json.RawMessage, len(current)+1)
	for _, raw := range current {
		var item referrerDescriptor
		if json.Unmarshal(raw, &item) != nil || !route.ValidDigest(item.Digest) {
			return nil, false, invalidReferrer("fallback referrers descriptor is invalid")
		}
		canonical, err := canonicalDescriptor(raw)
		if err != nil {
			return nil, false, invalidReferrer("fallback referrers descriptor is invalid")
		}
		if existing, found := desired[item.Digest]; found && !bytes.Equal(existing, canonical) {
			return nil, false, invalidReferrer("fallback referrers index contains conflicting duplicate descriptors")
		}
		desired[item.Digest] = canonical
	}
	var target referrerDescriptor
	if len(descriptor) > 0 && (json.Unmarshal(descriptor, &target) != nil || !route.ValidDigest(target.Digest)) {
		return nil, false, invalidReferrer("new fallback referrer descriptor is invalid")
	}
	if remove {
		if len(descriptor) == 0 {
			return nil, false, invalidReferrer("remove operation requires a referrer digest")
		}
		if _, found := desired[target.Digest]; !found {
			return sortedReferrerDescriptors(desired), false, nil
		}
		delete(desired, target.Digest)
		return sortedReferrerDescriptors(desired), true, nil
	}
	descriptor, err := canonicalDescriptor(descriptor)
	if err != nil {
		return nil, false, invalidReferrer("new fallback referrer descriptor is invalid")
	}
	if existing, found := desired[target.Digest]; found {
		if !bytes.Equal(existing, descriptor) {
			return nil, false, invalidReferrer("fallback referrer digest already has conflicting metadata")
		}
		return sortedReferrerDescriptors(desired), false, nil
	}
	desired[target.Digest] = descriptor
	return sortedReferrerDescriptors(desired), true, nil
}

func canonicalDescriptor(raw json.RawMessage) (json.RawMessage, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("referrer descriptor JSON is invalid")
	}
	canonical, err := json.Marshal(value)
	return canonical, err
}

func sortedReferrerDescriptors(values map[string]json.RawMessage) []json.RawMessage {
	digests := make([]string, 0, len(values))
	for digest := range values {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	result := make([]json.RawMessage, 0, len(digests))
	for _, digest := range digests {
		result = append(result, values[digest])
	}
	return result
}

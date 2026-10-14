package app

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

func renewLeaseWithInterval(ctx context.Context, cancel context.CancelFunc, manager *lease.Manager, current lease.Lease, interval time.Duration, done chan<- error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			renewed, err := manager.Renew(ctx, current, objectLeaseTTL)
			if err != nil {
				if ctx.Err() != nil {
					done <- nil
					return
				}
				cancel()
				done <- err
				return
			}
			current = renewed
		}
	}
}

func (g *closure) addObject(object contentRef, limits config.Limits) error {
	g.objects++
	g.bytes += object.size
	if g.objects > limits.ClosureObjects || g.bytes > limits.ClosureBytes {
		return permanent("closure_limit", errors.New("artifact closure exceeds configured limits"))
	}
	return nil
}

func copyDescriptors(descriptor *artifact.Descriptor) []artifact.Descriptor {
	if descriptor == nil {
		return nil
	}
	return []artifact.Descriptor{*descriptor}
}

func descriptorRef(descriptor artifact.Descriptor) contentRef {
	return contentRef{
		digest: descriptor.Digest, mediaType: descriptor.MediaType, size: descriptor.Size,
		artifactType: descriptor.ArtifactType, annotations: cloneAnnotations(descriptor.Annotations),
		platform: append(json.RawMessage(nil), descriptor.Platform...), urls: append([]string(nil), descriptor.URLs...),
	}
}

func cloneAnnotations(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func hasEmbeddedData(data json.RawMessage) bool { return len(data) > 0 && string(data) != "null" }

func looksLikeManifest(data []byte) bool {
	var value struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	return json.Unmarshal(data, &value) == nil && value.SchemaVersion == 2
}

func sameMediaType(left, right string) bool {
	leftType, _, leftErr := mime.ParseMediaType(left)
	rightType, _, rightErr := mime.ParseMediaType(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(leftType, rightType)
}

func artifactDigest(value string) error {
	_, err := artifact.Hasher(value)
	return err
}

func objectLeaseKey(authority, repository, digest string) string {
	return artifact.CalculateSHA256([]byte(authority + "\x00" + repository + "\x00" + digest))[7:]
}

func (s *solidifier) internalName(authority, repository string) string {
	name, _ := route.InternalName(authority + "/" + repository)
	return name
}

func permanent(class string, err error) error {
	return &tasks.Failure{Class: class, Permanent: true, Err: err}
}
func temporary(class string, err error) error { return &tasks.Failure{Class: class, Err: err} }

var _ = url.Values{}

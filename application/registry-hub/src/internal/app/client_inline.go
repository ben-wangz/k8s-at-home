package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/route"
	"sort"
)

type inlineManifest struct {
	digest    string
	mediaType string
	data      []byte
	parsed    artifact.Manifest
}

type inlineObjects struct {
	blobs           map[string]artifact.Descriptor
	manifests       []inlineManifest
	identities      map[string]inlineIdentity
	active          map[string]bool
	seenManifests   map[string]inlineManifest
	descriptorCount int
	embeddedBytes   int64
}

type inlineIdentity struct {
	kind      string
	mediaType string
	size      int64
}

func (s *Service) publishEmbeddedObjects(ctx context.Context, routed route.Route, objects *inlineObjects) ([]func() error, error) {
	if len(objects.blobs) == 0 && len(objects.manifests) == 0 {
		return nil, nil
	}

	digests := make([]string, 0, len(objects.blobs)+len(objects.manifests))
	for digest := range objects.blobs {
		digests = append(digests, digest)
	}
	for _, manifest := range objects.manifests {
		digests = append(digests, manifest.digest)
	}
	sort.Strings(digests)

	authority, repository := routeStorageIdentity(routed)
	releases := make([]func() error, 0, len(digests))
	leaseContexts := make(map[string]context.Context, len(digests))
	complete := false
	defer func() {
		if !complete {
			releaseContentLeases(releases)
		}
	}()
	for _, digest := range digests {
		objectCtx, release, err := acquireContentLease(ctx, s.tasks.Leases(), authority, repository, digest, requestID())
		if err != nil {
			return nil, err
		}
		releases = append(releases, release)
		leaseContexts[digest] = objectCtx
	}
	for _, digest := range sortedDescriptorDigests(objects.blobs) {
		descriptor := objects.blobs[digest]
		beforeWrite := func() error {
			_, err := s.advanceGeneration(leaseContexts[digest], authority, repository, digest, false)
			return err
		}
		if err := s.ensureEmbeddedBlob(leaseContexts[digest], routed.Internal, descriptor, beforeWrite); err != nil {
			return nil, err
		}
	}
	for _, manifest := range objects.manifests {
		beforeWrite := func() error {
			_, err := s.advanceGeneration(leaseContexts[manifest.digest], authority, repository, manifest.digest, false)
			return err
		}
		if err := s.ensureEmbeddedManifest(leaseContexts[manifest.digest], routed, manifest, beforeWrite); err != nil {
			return nil, err
		}
	}
	complete = true
	return releases, nil
}

func collectInlineObjects(rootDigest string, root artifact.Manifest, limits config.Limits) (*inlineObjects, error) {
	objects := &inlineObjects{
		blobs:         make(map[string]artifact.Descriptor),
		identities:    make(map[string]inlineIdentity),
		active:        map[string]bool{rootDigest: true},
		seenManifests: make(map[string]inlineManifest),
	}
	var visit func(artifact.Manifest, int) error
	visit = func(manifest artifact.Manifest, depth int) error {
		if depth > limits.ClosureDepth {
			return errors.New("inline manifest recursion depth exceeds limit")
		}
		count := len(manifest.Layers)
		if manifest.Config != nil {
			count++
		}
		if manifest.Manifests != nil {
			count += len(*manifest.Manifests)
		}
		if manifest.Subject != nil {
			count++
		}
		objects.descriptorCount += count
		if objects.descriptorCount > limits.MaxDescriptors {
			return errors.New("inline manifest descriptor count exceeds limit")
		}
		accountData := func(data []byte) error {
			objects.embeddedBytes += int64(len(data))
			if objects.embeddedBytes > limits.DescriptorDataBytes {
				return errors.New("inline manifest descriptor.data exceeds limit")
			}
			return nil
		}
		if manifest.Config != nil {
			if err := registerInlineIdentity(objects, *manifest.Config, "blob"); err != nil {
				return err
			}
			if hasEmbeddedData(manifest.Config.Data) {
				if err := accountData(manifest.Config.Embedded); err != nil {
					return err
				}
				objects.blobs[manifest.Config.Digest] = *manifest.Config
			}
		}
		for _, descriptor := range manifest.Layers {
			if err := registerInlineIdentity(objects, descriptor, "blob"); err != nil {
				return err
			}
			if hasEmbeddedData(descriptor.Data) {
				if err := accountData(descriptor.Embedded); err != nil {
					return err
				}
				objects.blobs[descriptor.Digest] = descriptor
			}
		}
		if manifest.Subject != nil && hasEmbeddedData(manifest.Subject.Data) {
			if err := accountData(manifest.Subject.Embedded); err != nil {
				return err
			}
		}
		if manifest.Manifests == nil {
			return nil
		}
		for _, descriptor := range *manifest.Manifests {
			if err := registerInlineIdentity(objects, descriptor, "manifest"); err != nil {
				return err
			}
			if !hasEmbeddedData(descriptor.Data) {
				continue
			}
			if err := accountData(descriptor.Embedded); err != nil {
				return err
			}
			if int64(len(descriptor.Embedded)) > limits.ManifestBytes || !looksLikeManifest(descriptor.Embedded) {
				return errors.New("inline child descriptor.data is not a valid manifest")
			}
			if objects.active[descriptor.Digest] {
				return errors.New("inline manifest graph contains a cycle")
			}
			if existing, ok := objects.seenManifests[descriptor.Digest]; ok {
				if !sameMediaType(existing.mediaType, descriptor.MediaType) || int64(len(existing.data)) != descriptor.Size || !bytes.Equal(existing.data, descriptor.Embedded) {
					return errors.New("inline manifest descriptor metadata conflicts")
				}
				continue
			}
			child, err := artifact.ParseManifest(descriptor.Embedded, descriptor.MediaType, limits.MaxDescriptors, limits.DescriptorDataBytes)
			if err != nil {
				return fmt.Errorf("inline child manifest failed validation: %w", err)
			}
			objects.active[descriptor.Digest] = true
			if err := visit(child, depth+1); err != nil {
				delete(objects.active, descriptor.Digest)
				return err
			}
			delete(objects.active, descriptor.Digest)
			node := inlineManifest{digest: descriptor.Digest, mediaType: descriptor.MediaType, data: append([]byte(nil), descriptor.Embedded...), parsed: child}
			objects.seenManifests[descriptor.Digest] = node
			objects.manifests = append(objects.manifests, node)
		}
		return nil
	}
	if err := visit(root, 0); err != nil {
		return nil, err
	}
	return objects, nil
}

func registerInlineIdentity(objects *inlineObjects, descriptor artifact.Descriptor, kind string) error {
	identity := inlineIdentity{kind: kind, mediaType: descriptor.MediaType, size: descriptor.Size}
	if existing, ok := objects.identities[descriptor.Digest]; ok {
		if existing.kind != identity.kind || existing.size != identity.size || !sameMediaType(existing.mediaType, identity.mediaType) {
			return errors.New("inline manifest descriptor metadata conflicts")
		}
		return nil
	}
	objects.identities[descriptor.Digest] = identity
	return nil
}

func sortedDescriptorDigests(descriptors map[string]artifact.Descriptor) []string {
	digests := make([]string, 0, len(descriptors))
	for digest := range descriptors {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests
}

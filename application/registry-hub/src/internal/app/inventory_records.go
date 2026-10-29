package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"time"
)

type inventoryProof struct {
	SchemaVersion int       `json:"schemaVersion"`
	Generation    uint64    `json:"generation"`
	Epoch         uint64    `json:"epoch"`
	State         string    `json:"state"`
	ObservedAt    time.Time `json:"observedAt"`
}

type inventoryWork struct {
	Identity          inventory.Identity `json:"identity"`
	Digest            string             `json:"digest"`
	Generation        uint64             `json:"generation"`
	ContentGeneration uint64             `json:"contentGeneration"`
	CreatedAt         time.Time          `json:"createdAt"`
	UpdatedAt         time.Time          `json:"updatedAt"`
	Work              inventory.Work     `json:"work"`
}

func inventoryIdentity(authority, repository, reference string) inventory.Identity {
	return inventory.Identity{Authority: authority, Repository: repository, Reference: reference}
}

func inventoryDescriptor(d artifact.Descriptor, role string) inventory.ObjectRef {
	result := inventory.ObjectRef{Role: role, Digest: d.Digest, MediaType: d.MediaType, Size: d.Size, ArtifactType: d.ArtifactType}
	if len(d.Platform) > 0 {
		result.PlatformUnknown = true
		var p inventory.Platform
		if json.Unmarshal(d.Platform, &p) == nil && p.OS != "" && p.OS != "unknown" && p.Architecture != "" && p.Architecture != "unknown" {
			result.Platform = &p
			result.PlatformUnknown = false
		}
	}
	return result
}

func (s *Service) inventoryFailure(err error) {
	if err == nil {
		return
	}
	s.inventoryDirty.Store(true)
	if s.metrics != nil {
		s.metrics.recordEvent("inventory_projection_error")
	}
	if s.logger != nil {
		s.logger.Warn("inventory projection deferred", "error_class", "inventory", "error", err)
	}
}

func (s *Service) inventoryContent(ctx context.Context, identity inventory.Identity, digest, mediaType string, data []byte) (inventory.Content, error) {
	parsed, err := artifact.ParseManifest(data, mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil {
		return inventory.Content{}, err
	}
	generation, err := s.generation(ctx, identity.Authority, identity.Repository, digest)
	if err != nil {
		return inventory.Content{}, err
	}
	content := inventory.Content{Identity: inventoryIdentity(identity.Authority, identity.Repository, ""), ObjectRef: inventory.ObjectRef{Role: "manifest", Digest: digest, MediaType: mediaType, Size: int64(len(data)), ArtifactType: parsed.ArtifactType}, Generation: generation.Generation, Children: []inventory.ObjectRef{}, ClosureKnown: true, UpdatedAt: time.Now().UTC()}
	content.Source = "manifest"
	if parsed.ArtifactType == "" {
		content.Platform = s.inventoryConfigPlatform(ctx, identity, parsed.Config)
	}
	if parsed.Config != nil {
		content.Children = append(content.Children, inventoryDescriptor(*parsed.Config, "config"))
	}
	for _, d := range parsed.Layers {
		content.Children = append(content.Children, inventoryDescriptor(d, "layer"))
	}
	if parsed.Manifests != nil {
		for _, d := range *parsed.Manifests {
			content.Children = append(content.Children, inventoryDescriptor(d, "manifest"))
		}
	}
	if parsed.Subject != nil {
		content.Subject = parsed.Subject.Digest
	}
	err = s.inventory.SaveContent(ctx, content)
	if pending, ok := ctx.Value(inventoryLinkCapture{}).(*[]inventory.Content); ok {
		if err == nil {
			*pending = append(*pending, content)
		}
		return content, err
	}
	if err == nil {
		err = s.linkInventoryContent(ctx, content)
	}
	if err == nil {
		err = s.inventoryChildDescriptors(ctx, content)
	}
	return content, err
}

func (s *Service) inventoryChildDescriptors(ctx context.Context, parent inventory.Content) error {
	for _, child := range parent.Children {
		if child.Role != "manifest" {
			continue
		}
		generation, err := s.generation(ctx, parent.Authority, parent.Repository, child.Digest)
		if err != nil {
			return err
		}
		key := inventory.Prefix + "contents/" + parent.ContentID(child.Digest) + ".json"
		if err := inventory.Update(ctx, s.store, key, func(value *inventory.Content) error {
			if value.SchemaVersion == 0 || value.Generation < generation.Generation {
				*value = inventory.Content{SchemaVersion: 1, Identity: parent.Identity, ObjectRef: child, Generation: generation.Generation, Children: []inventory.ObjectRef{}, UpdatedAt: parent.UpdatedAt}
			} else if value.Generation == generation.Generation {
				if child.PlatformUnknown || child.Platform != nil && value.Platform != nil && *child.Platform != *value.Platform {
					value.Platform = nil
					value.PlatformUnknown = true
				} else if value.Platform == nil && !value.PlatformUnknown {
					value.Platform = child.Platform
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) linkInventoryContent(ctx context.Context, content inventory.Content) error {
	objects := append([]inventory.ObjectRef{content.ObjectRef}, content.Children...)
	for _, object := range objects {
		key := inventory.Prefix + "object-links/" + content.ContentID(object.Digest) + "/" + content.ContentID(content.Digest) + ".json"
		if err := inventory.Update(ctx, s.store, key, func(value *string) error { *value = content.Digest; return nil }); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) inventoryBinding(ctx context.Context, identity inventory.Identity, digest, source string, generation uint64, confirmed time.Time, knownNew bool) (inventory.Binding, error) {
	contentGeneration, err := s.generation(ctx, identity.Authority, identity.Repository, digest)
	if err != nil {
		return inventory.Binding{}, err
	}
	binding, err := s.inventory.Bind(ctx, inventory.Binding{Identity: identity, Digest: digest, Generation: generation, ContentGeneration: contentGeneration.Generation, Source: source, ConfirmedAt: confirmed})
	if err != nil {
		return binding, err
	}
	version := inventory.LifecycleVersion(identity.ID(), binding.Revision, binding.ContentGeneration)
	return binding, s.inventory.Register(ctx, version, knownNew)
}

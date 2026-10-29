package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type inventoryImageProgress struct {
	Image    inventory.Image           `json:"image"`
	Content  inventory.Content         `json:"content"`
	Closure  inventoryClosureProgress  `json:"closure"`
	Objects  []inventory.Object        `json:"objects"`
	Phase    int                       `json:"phase"`
	Offset   int                       `json:"offset"`
	Epoch    uint64                    `json:"epoch"`
	Anchored bool                      `json:"anchored"`
	Invalid  bool                      `json:"invalid"`
	Work     inventoryWorkProgress     `json:"work"`
	Capture  inventory.CaptureProgress `json:"capture"`
	Audit    *inventoryAuditProgress   `json:"audit,omitempty"`
}

func (s *Service) newInventoryImageProgress(ctx context.Context, image inventory.Image, content inventory.Content) (*inventoryImageProgress, error) {
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return nil, err
	}
	image.MediaType, image.ArtifactType, image.Platform = content.MediaType, content.ArtifactType, content.Platform
	if content.PlatformUnknown {
		image.Platform = nil
	}
	image.Subject, image.Kind = content.Subject, "manifest"
	if content.ArtifactType != "" {
		image.Kind = "artifact"
	}
	if content.MediaType == "application/vnd.oci.image.index.v1+json" || content.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		image.Kind = "index"
	}
	content.Children = nil
	return &inventoryImageProgress{Image: image, Content: content, Epoch: gate.Epoch, Closure: newInventoryClosureProgress(image.ResolvedDigest), Objects: []inventory.Object{}}, nil
}

func (s *Service) advanceInventoryImage(ctx context.Context, writer *inventory.Writer, p *inventoryImageProgress) (bool, error) {
	image := &p.Image
	switch p.Phase {
	case 0:
		if len(p.Closure.Frames) != 0 {
			return false, s.advanceInventoryClosure(ctx, image.Identity, &p.Closure)
		}
		image.Evidence = inventory.Evidence{ClosureComplete: p.Closure.Complete, CheckedAt: time.Now().UTC(), Freshness: "fresh", Reasons: p.Closure.Reasons, GCEpoch: p.Epoch}
		p.Closure.Seen, p.Closure.Active, p.Closure.Required = nil, nil, nil
		if image.ResolvedDigest == "" {
			image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "binding_unresolved")
		}
		p.Phase++
	case 1:
		if p.Audit != nil {
			return false, s.advanceInventoryImageAudit(ctx, p)
		}
		if p.Offset < len(p.Closure.Objects) {
			object, _, err := s.inspectInventoryObject(context.WithValue(ctx, inventoryAuditCapture{}, &p.Audit), image.Identity, p.Closure.Objects[p.Offset], p.Epoch)
			if err != nil {
				return false, err
			}
			if p.Audit != nil {
				return false, nil
			}
			object.Associated = true
			p.Objects = append(p.Objects, object)
			for _, reason := range object.Reasons {
				image.Evidence.Reasons = appendReason(image.Evidence.Reasons, reason)
			}
			p.Offset++
			return false, nil
		}
		image.Cache, image.Availability, p.Anchored = summarizeInventoryObjects(p.Objects, p.Closure.Complete)
		p.Closure.Objects = nil
		p.Offset, p.Phase = 0, 2
	case 2:
		return false, s.advanceInventoryAnchor(ctx, p)
	case 3:
		version := inventory.LifecycleVersion(image.ID, image.BindingRevision, image.ContentGeneration)
		lifecycle, err := s.inventory.Lifecycle(ctx, version)
		if err != nil {
			return false, err
		}
		image.Solidification.Lifecycle = lifecycle
		if image.Solidification.Started == nil {
			image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "legacy_start_unknown")
		}
		p.Phase++
	case 4:
		done, err := s.advanceInventoryImageWork(ctx, p)
		if err != nil || !done {
			return false, err
		}
		p.Phase++
	case 5:
		return false, s.advanceInventoryRecheck(ctx, p)
	case 6:
		return false, s.advanceInventoryRequirements(ctx, writer, p)
	case 7:
		return writer.CaptureImage(ctx, image, p.Objects, p.Closure.Platforms, &p.Capture)
	default:
		return false, storage.ErrCorruptObject
	}
	return false, nil
}

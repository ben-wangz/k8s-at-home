package app

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func inventoryInternalName(identity inventory.Identity) (string, error) {
	name := identity.Repository
	if identity.Authority != "" {
		name = identity.Authority + "/" + name
	}
	return route.InternalName(name)
}

func (s *Service) inventoryEpoch(ctx context.Context) (gateRecord, error) {
	gate, _, err := inventory.Read[gateRecord](ctx, s.store, "system/gc/state.json")
	if err == nil && (gate.SchemaVersion != gateSchemaCurrent || gate.Epoch == 0) {
		err = storage.ErrCorruptObject
	}
	return gate, err
}

func (s *Service) auditInventoryObject(ctx context.Context, object inventory.Object, path string, epoch uint64) error {
	ctx, cancel := context.WithTimeout(ctx, min(5*time.Second, s.inventoryConfig().BatchTimeout/4))
	defer cancel()
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return err
	}
	if gate.Epoch != epoch || gate.Exclusive || gate.State != "open" || gate.RecoveryBarrier {
		return storage.ErrConditionFailed
	}
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != object.Size || response.Header.Get("Docker-Content-Digest") != object.Digest {
		return storage.ErrCorruptObject
	}
	hasher, err := artifact.Hasher(object.Digest)
	if err != nil {
		return err
	}
	size, err := io.Copy(hasher, io.LimitReader(response.Body, object.Size+1))
	if err != nil {
		return err
	}
	if size != object.Size || strings.SplitN(object.Digest, ":", 2)[0]+":"+hex.EncodeToString(hasher.Sum(nil)) != object.Digest {
		return storage.ErrCorruptObject
	}
	gate, err = s.inventoryEpoch(ctx)
	if err != nil {
		return err
	}
	generation, err := s.generation(ctx, object.Authority, object.Repository, object.Digest)
	if err != nil {
		return err
	}
	if gate.Epoch != epoch || gate.Exclusive || gate.State != "open" || gate.RecoveryBarrier || generation.Tombstone || generation.Generation != object.Generation {
		return storage.ErrConditionFailed
	}
	proof := inventoryProof{SchemaVersion: 1, Generation: object.Generation, Epoch: epoch, State: "verified", ObservedAt: time.Now().UTC()}
	return s.saveInventoryProof(ctx, object.ID, proof)
}

func (s *Service) saveInventoryProof(ctx context.Context, id string, proof inventoryProof) error {
	return inventory.Update(ctx, s.store, inventory.Prefix+"proofs/"+id+".json", func(old *inventoryProof) error {
		if old.Generation > proof.Generation || old.Generation == proof.Generation && old.Epoch > proof.Epoch {
			return nil
		}
		if old.Generation == proof.Generation && old.Epoch == proof.Epoch && old.State == "anchored" && proof.State != "anchored" {
			return nil
		}
		*old = proof
		return nil
	})
}

func (s *Service) anchorInventoryPublication(ctx context.Context, image inventory.Image, objects []inventory.Object) (bool, error) {
	if !image.Evidence.ClosureComplete || len(objects) == 0 || image.Scope != "reference" || image.Reference == image.ResolvedDigest {
		return false, nil
	}
	for _, object := range objects {
		if !object.DistributionVerified {
			return false, nil
		}
	}
	published, err := s.inventoryTagPublished(ctx, image)
	if err != nil || !published {
		return false, err
	}
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return false, err
	}
	if gate.Epoch != image.Evidence.GCEpoch || gate.Exclusive || gate.State != "open" || gate.RecoveryBarrier {
		return false, nil
	}
	for index := range objects {
		object := &objects[index]
		generation, err := s.generation(ctx, object.Authority, object.Repository, object.Digest)
		if err != nil {
			return false, err
		}
		if generation.Tombstone || generation.Generation != object.Generation {
			return false, nil
		}
		if err := s.saveInventoryProof(ctx, object.ID, inventoryProof{SchemaVersion: 1, Generation: object.Generation, Epoch: gate.Epoch, State: "anchored", ObservedAt: object.CheckedAt}); err != nil {
			return false, err
		}
		object.Anchored = true
	}
	gate, err = s.inventoryEpoch(ctx)
	return err == nil && gate.Epoch == image.Evidence.GCEpoch && gate.State == "open" && !gate.Exclusive && !gate.RecoveryBarrier, err
}

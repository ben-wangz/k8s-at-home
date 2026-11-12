package app

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
)

type protectionRemoteCheckpoint struct {
	Write       *protectionRemoteWrite `json:"write,omitempty"`
	Offset      int64                  `json:"offset,omitempty"`
	Hash        []byte                 `json:"hash,omitempty"`
	Verified    bool                   `json:"verified,omitempty"`
	Absent      bool                   `json:"absent,omitempty"`
	Query       string                 `json:"query,omitempty"`
	Pages       []string               `json:"pages,omitempty"`
	Fallback    bool                   `json:"fallback,omitempty"`
	HeadChecked bool                   `json:"headChecked,omitempty"`
}

func (r *protectionRecovery) remoteStep(ctx context.Context) error {
	c := &r.checkpoint
	if c.Index >= len(c.SessionIDs) {
		r.next("representation_indexes")
		return nil
	}
	id := c.SessionIDs[c.Index]
	session, active, err := r.service.gate.protectionSession(ctx, id)
	if err != nil {
		return err
	}
	if !active {
		return storage.ErrConditionFailed
	}
	if session.PendingOperations == 0 && !session.UncertainRemoteResult && session.RemoteWrite == nil {
		c.Index++
		c.Step, c.Remote = "", protectionRemoteCheckpoint{}
		return nil
	}
	if session.UncertainRemoteResult {
		if session.RemoteWrite == nil {
			return r.service.blockUnknownProtectionWrite(ctx, id, errors.New("remote write identity was not persisted"))
		}
		if c.Remote.Write == nil {
			write := *session.RemoteWrite
			c.Remote.Write = &write
			return nil
		}
		if *c.Remote.Write != *session.RemoteWrite {
			return storage.ErrConditionFailed
		}
		if !c.Remote.Verified && !c.Remote.Absent {
			return r.verifyRemoteStep(ctx)
		}
		if c.Step != "observed" {
			// A confirmed repository miss can still leave unlinked upload data.
			if err := r.service.gate.MarkRecoveredProtectionContentObserved(ctx, id); err != nil {
				return err
			}
			c.Step = "observed"
			return nil
		}
	}
	if err := r.service.gate.CompleteProtectionStartupRecovery(ctx, id, c.Remote.Write); err != nil {
		return err
	}
	c.Index++
	c.Step, c.Remote = "", protectionRemoteCheckpoint{}
	return nil
}

func (r *protectionRecovery) verifyRemoteStep(ctx context.Context) error {
	remote := &r.checkpoint.Remote
	write := *remote.Write
	if write.Kind == "blob" {
		return r.verifyRemoteBlobChunk(ctx)
	}
	if write.Kind == "referrer" {
		return r.verifyRemoteReferrerPage(ctx)
	}
	verified, err := r.service.verifyProtectionRemoteWrite(ctx, write)
	if err != nil {
		return err
	}
	remote.Verified, remote.Absent = verified, !verified
	return nil
}

func (r *protectionRecovery) verifyRemoteBlobChunk(ctx context.Context) error {
	remote := &r.checkpoint.Remote
	write := *remote.Write
	if !remote.HeadChecked {
		response, err := r.service.registry.Do(ctx, http.MethodHead, write.Path, nil, make(http.Header), nil, 0)
		if err != nil {
			return err
		}
		miss := confirmedMiss(response, http.MethodHead, route.EndpointBlob)
		_ = response.Body.Close()
		if miss {
			remote.Absent = true
			return nil
		}
		if response.StatusCode != http.StatusOK || response.ContentLength != write.Size || response.Header.Get("Docker-Content-Digest") != write.Digest {
			return ErrRecoveryBarrier
		}
		remote.HeadChecked = true
		return nil
	}
	hasher, err := artifact.Hasher(write.Digest)
	if err != nil {
		return err
	}
	if remote.Offset < 0 || remote.Offset > write.Size {
		return storage.ErrCorruptObject
	}
	if len(remote.Hash) != 0 {
		if err := hasher.(encoding.BinaryUnmarshaler).UnmarshalBinary(remote.Hash); err != nil {
			return storage.ErrCorruptObject
		}
	}
	if remote.Offset == write.Size {
		if artifact.DigestFromHasher(write.Digest, hasher) != write.Digest {
			return artifact.ErrDigestMismatch
		}
		remote.Verified = true
		return nil
	}
	end := remote.Offset + min(int64(4<<20), write.Size-remote.Offset) - 1
	headers := http.Header{"Range": []string{fmt.Sprintf("bytes=%d-%d", remote.Offset, end)}}
	response, err := r.service.registry.Do(ctx, http.MethodGet, write.Path, nil, headers, nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	expected := fmt.Sprintf("bytes %d-%d/%d", remote.Offset, end, write.Size)
	if response.StatusCode != http.StatusPartialContent || response.Header.Get("Content-Range") != expected {
		// A small object may be returned in full by a server that ignores Range.
		if response.StatusCode != http.StatusOK || remote.Offset != 0 || end != write.Size-1 {
			return errors.New("Distribution recovery requires a valid bounded blob range")
		}
	}
	if digest := response.Header.Get("Docker-Content-Digest"); digest != "" && digest != write.Digest {
		return artifact.ErrDigestMismatch
	}
	size := end - remote.Offset + 1
	if response.ContentLength >= 0 && response.ContentLength != size {
		return storage.ErrCorruptObject
	}
	count, err := io.Copy(hasher, io.LimitReader(response.Body, size+1))
	if err != nil {
		return err
	}
	if count != size {
		return storage.ErrCorruptObject
	}
	remote.Hash, err = hasher.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	remote.Offset += count
	return nil
}

func (r *protectionRecovery) verifyRemoteReferrerPage(ctx context.Context) error {
	remote := &r.checkpoint.Remote
	write := *remote.Write
	path := write.Path
	query, err := url.ParseQuery(remote.Query)
	if err != nil {
		return storage.ErrCorruptObject
	}
	if remote.Fallback {
		path, query = write.FallbackPath, nil
	}
	response, err := r.service.registry.Do(ctx, http.MethodGet, path, query, http.Header{"Accept": []string{referrerIndexMediaType}}, nil, 0)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusNotFound {
		endpoint := route.EndpointReferrers
		if remote.Fallback {
			endpoint = route.EndpointManifest
		}
		miss := confirmedMiss(response, http.MethodGet, endpoint)
		_ = response.Body.Close()
		if !miss {
			return ErrRecoveryBarrier
		}
		if remote.Fallback {
			remote.Absent = true
			return nil
		}
		remote.Fallback = true
		return nil
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return errors.New("Distribution referrer recovery request failed")
	}
	links := response.Header.Values("Link")
	index, err := r.service.readReferrerIndex(response)
	if err != nil {
		return err
	}
	if referrerIndexContains(index, write.Digest) {
		remote.Verified = true
		return nil
	}
	if remote.Fallback {
		remote.Absent = true
		return nil
	}
	next, found, err := nextReferrerPage(links, write.Path)
	if err != nil {
		return err
	}
	if !found {
		remote.Fallback = true
		return nil
	}
	if len(remote.Pages)+1 >= r.service.cfg.Limits.MaxReferrerPages {
		return errors.New("referrer recovery page limit exceeded")
	}
	for _, page := range remote.Pages {
		if page == next.Encode() {
			return errors.New("referrer recovery pagination loop")
		}
	}
	remote.Query = next.Encode()
	remote.Pages = append(remote.Pages, remote.Query)
	return nil
}

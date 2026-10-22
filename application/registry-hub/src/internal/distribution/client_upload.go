package distribution

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

const (
	maxUploadCancelAttempts  = 3
	maxUploadStatusRefreshes = maxUploadCancelAttempts - 1
)

func (c *Client) CancelUpload(ctx context.Context, location, internalRepository string) error {
	uploadPath, query, err := c.ParseLocation(location, internalRepository)
	if err != nil {
		return err
	}
	query.Del("digest")

	refreshes := 0
	for attempt := 0; attempt < maxUploadCancelAttempts; attempt++ {
		response, err := c.Do(ctx, http.MethodDelete, uploadPath, query, make(http.Header), nil, 0)
		if err != nil {
			closeResponse(response)
			return fmt.Errorf("send Distribution upload cancellation: %w", err)
		}
		status := response.StatusCode
		closeResponse(response)
		switch status {
		case http.StatusNoContent, http.StatusNotFound:
			return nil
		case http.StatusBadRequest, http.StatusRequestedRangeNotSatisfiable:
			if refreshes >= maxUploadStatusRefreshes {
				return fmt.Errorf("Distribution upload cancellation returned status %d", status)
			}
		default:
			return fmt.Errorf("Distribution upload cancellation returned status %d", status)
		}

		refreshedQuery, gone, err := c.refreshUploadState(ctx, uploadPath, internalRepository)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}
		query = refreshedQuery
		refreshes++
	}
	return errors.New("Distribution upload cancellation attempts exhausted")
}

func (c *Client) refreshUploadState(ctx context.Context, uploadPath, internalRepository string) (url.Values, bool, error) {
	response, err := c.Do(ctx, http.MethodGet, uploadPath, nil, make(http.Header), nil, 0)
	if err != nil {
		closeResponse(response)
		return nil, false, fmt.Errorf("get Distribution upload status: %w", err)
	}
	status := response.StatusCode
	location := response.Header.Get("Location")
	closeResponse(response)
	switch status {
	case http.StatusNotFound:
		return nil, true, nil
	case http.StatusNoContent:
		refreshedPath, query, err := c.ParseLocation(location, internalRepository)
		if err != nil {
			return nil, false, errors.New("Distribution returned an invalid upload status Location")
		}
		if refreshedPath != uploadPath {
			return nil, false, errors.New("Distribution upload status Location changed upload identity")
		}
		query.Del("digest")
		return query, false, nil
	default:
		return nil, false, fmt.Errorf("Distribution upload status returned status %d", status)
	}
}

func closeResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

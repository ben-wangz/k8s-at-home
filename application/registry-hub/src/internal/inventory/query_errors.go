package inventory

import (
	"errors"
	"registry-hub/internal/storage"
)

func snapshotPageError(err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return storage.ErrCorruptObject
	}
	return err
}

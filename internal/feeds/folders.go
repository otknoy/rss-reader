package feeds

import (
	"errors"
	"strings"
)

// Folder groups subscriptions at one level.
type Folder struct {
	ID   int64
	Name string
}

// ErrFolderName indicates an empty folder name.
var ErrFolderName = errors.New("フォルダー名を入力してください")

var (
	// ErrFolderNotFound indicates that the referenced folder does not exist.
	ErrFolderNotFound = errors.New("folder not found")
	// ErrFolderNameConflict indicates that the folder name is already in use.
	ErrFolderNameConflict = errors.New("folder name already in use")
	// ErrInvalidFolderOrder indicates that the requested order contains invalid folder IDs.
	ErrInvalidFolderOrder = errors.New("invalid folder order")
)

// ErrFolderOrderChanged indicates the caller's displayed order is stale.
var ErrFolderOrderChanged = errors.New("folder order changed")

// ValidateFolderOrder checks that the new order contains the same unique folder IDs.
func ValidateFolderOrder(previous, ids []int64) error {
	if len(previous) == 0 || len(previous) != len(ids) {
		return ErrInvalidFolderOrder
	}
	remaining := make(map[int64]bool, len(previous))
	for _, id := range previous {
		if id <= 0 || remaining[id] {
			return ErrInvalidFolderOrder
		}
		remaining[id] = true
	}
	for _, id := range ids {
		if !remaining[id] {
			return ErrInvalidFolderOrder
		}
		delete(remaining, id)
	}
	return nil
}

// NormalizeFolderName trims the name and rejects empty names.
func NormalizeFolderName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrFolderName
	}
	return name, nil
}

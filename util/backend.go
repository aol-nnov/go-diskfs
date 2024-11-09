package util

import (
	"io/fs"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/backend/raw"
)

// Returns backend.Storage or creates Raw backend around provided fs.File
func BackendFromFile(f fs.File, readOnly bool) backend.Storage {
	if theBackend, storageLike := f.(backend.Storage); storageLike {
		return theBackend
	}
	return raw.New(f, readOnly)
}

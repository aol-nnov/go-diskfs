package raw

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/diskfs/go-diskfs/backend"
)

type rawBackend struct {
	storage  fs.File
	readonly bool
}

func New(f fs.File, isReadonly bool) backend.Storage {
	return rawBackend{
		storage:  f,
		readonly: isReadonly,
	}
}

// Create a backend.Storage from a path to a device
// Should pass a path to a block device e.g. /dev/sda or a path to a file /tmp/foo.img
// The provided device must not exist at the time you call CreateFromPath()
func CreateFromPath(pathName string, size int64) (backend.Storage, error) {
	if pathName == "" {
		return nil, errors.New("must pass device name")
	}
	if size <= 0 {
		return nil, errors.New("must pass valid device size to create")
	}
	f, err := os.OpenFile(pathName, os.O_RDWR|os.O_EXCL|os.O_CREATE, 0o666)
	if err != nil {
		return nil, fmt.Errorf("could not create device %s: %w", pathName, err)
	}
	err = os.Truncate(pathName, size)
	if err != nil {
		return nil, fmt.Errorf("could not expand device %s to size %d: %w", pathName, size, err)
	}

	return rawBackend{
		storage:  f,
		readonly: false,
	}, nil
}

// backend.Storage interface guard
var _ backend.Storage = (*rawBackend)(nil)

// OS-stecific file for ioctl calls via fd
func (f rawBackend) Sys() (*os.File, error) {
	if osFile, ok := f.storage.(*os.File); ok {
		return osFile, nil
	}
	return nil, backend.ErrNotSuitable
}

// file for read-write operations
func (f rawBackend) Writable() (backend.WritableFile, error) {
	if rwFile, ok := f.storage.(backend.WritableFile); ok {
		if !f.readonly {
			return rwFile, nil
		}

		return nil, backend.ErrIncorrectOpenMode
	}
	return nil, backend.ErrNotSuitable
}

func (f rawBackend) Stat() (fs.FileInfo, error) {
	return f.storage.Stat()
}

func (f rawBackend) Read(b []byte) (int, error) {
	return f.storage.Read(b)
}

func (f rawBackend) Close() error {
	return f.storage.Close()
}

func (f rawBackend) ReadAt(p []byte, off int64) (n int, err error) {
	if readerAt, ok := f.storage.(io.ReaderAt); ok {
		return readerAt.ReadAt(p, off)
	}
	return -1, backend.ErrNotSuitable
}

func (f rawBackend) Seek(offset int64, whence int) (int64, error) {
	if seeker, ok := f.storage.(io.Seeker); ok {
		return seeker.Seek(offset, whence)
	}
	return -1, backend.ErrNotSuitable
}

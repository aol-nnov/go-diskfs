package qcow2

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"

	"github.com/diskfs/go-diskfs/backend"
)

const (
	// DefaultBlocksize for qcow2 is 64KB
	DefaultBlocksize int64 = 64 * 1024
)

// qcow2Backend a qcow2 disk
type qcow2Backend struct {
	file          *os.File
	size          int64
	start         int64
	blocksize     int64
	header        *header
	compressor    Compressor
	encryptor     Encryptor
	l1Table       *l1Table
	refcountTable *refcountTable
	readonly      bool
}

func New(f fs.File, isReadonly bool) backend.Storage {
	readerAt, ok := f.(io.ReaderAt)
	if !ok {
		log.Fatal(backend.ErrNotSuitable)
	}

	// we were asked to use an existing one, so have to see if we can read it as qcow2
	theBackend, err := readImage(readerAt, 0)
	if err != nil {

		log.Fatal(err)
	}

	theBackend.readonly = isReadonly

	return theBackend
}

func CreateFromPath(pathName string, size int64) (backend.Storage, error) {
	var blocksize int64
	if blocksize == 0 {
		blocksize = DefaultBlocksize
	}
	// create the header
	h := &header{
		version:         3,
		clusterSize:     uint32(blocksize),
		fileSize:        uint64(size),
		compressionType: compressionZlib,
	}

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

	b := h.toBytes()
	n, err := f.WriteAt(b, 0)
	if err != nil {
		return nil, fmt.Errorf("could not write qcow2 header for new qcow2 file: %v", err)
	}
	if n != len(b) {
		return nil, fmt.Errorf("wrote qcow2 header of %d bytes instead of expected %d bytes", n, len(b))
	}
	return &qcow2Backend{
		file:          f,
		size:          size,
		start:         0,
		blocksize:     blocksize,
		header:        h,
		compressor:    nil,
		encryptor:     nil,
		l1Table:       &l1Table{},
		refcountTable: &refcountTable{},
		readonly:      false,
	}, nil
}

// backend.Storage interface guard
var _ backend.Storage = (*qcow2Backend)(nil)

// OS-stecific file for ioctl calls via fd
func (f qcow2Backend) Sys() (*os.File, error) {
	return f.file, nil
}

// file for read-write operations
func (f qcow2Backend) Writable() (backend.WritableFile, error) {
	if !f.readonly {
		return f, nil
	}

	return nil, backend.ErrIncorrectOpenMode
}

func (f qcow2Backend) Stat() (fs.FileInfo, error) {
	return f.file.Stat()
}

func (f qcow2Backend) Read(b []byte) (int, error) {
	return f.ReadAt(b, 0)
}

func (f qcow2Backend) Close() error {
	return f.file.Close()
}

func (f qcow2Backend) Seek(offset int64, whence int) (int64, error) {
	return -1, backend.ErrNotSuitable
}

// ReadAt read into the provided []byte at the given offset. Translates into
// the proper clusetr in the underlying qcow2 image.
func (q qcow2Backend) ReadAt(b []byte, offset int64) (int, error) {
	clusterSize := int(q.header.clusterSize)
	inClusterOffset := offset % int64(clusterSize)
	// the data could stretch over more than one cluster
	for remainder := len(b); remainder > 0; {
		// find the cluster location
		clusterLocation, err := q.getClusterLocation(offset+int64(len(b)-remainder), false)
		if err != nil {
			return 0, err
		}
		// how much data do we read from to this cluster?
		size := remainder
		if remainder > clusterSize {
			size = clusterSize
		}
		size = remainder - int(inClusterOffset)
		// if the cluster was unallocated, just add empty bytes
		if clusterLocation == 0 {
			b2 := make([]byte, size)
			copy(b[remainder:remainder+size], b2)
		} else {
			location := clusterLocation + inClusterOffset
			if _, err := q.file.ReadAt(b[remainder:remainder+size], location); err != nil {
				return 0, fmt.Errorf("error reading from cluster at %d in-cluster offset %d: %v", clusterLocation, inClusterOffset, err)
			}
		}
		// for all subsequent clusters, our inClusterOffset should be 0
		inClusterOffset = 0
		// find out where our offset would be for the next cluster
		remainder -= size
	}
	return len(b), nil
}

func (q qcow2Backend) WriteAt(b []byte, offset int64) (int, error) {
	clusterSize := int(q.header.clusterSize)
	inClusterOffset := offset % int64(clusterSize)
	// the data could stretch over more than one cluster
	for remainder := len(b); remainder > 0; {
		// find the cluster location
		clusterLocation, err := q.getClusterLocation(offset+int64(len(b)-remainder), true)
		if err != nil {
			return 0, err
		}
		// how much data do we read write to this cluster?
		size := remainder
		if remainder > clusterSize {
			size = clusterSize
		}
		size = remainder - int(inClusterOffset)
		location := clusterLocation + inClusterOffset
		if _, err := q.file.WriteAt(b[remainder:remainder+size], location); err != nil {
			return 0, fmt.Errorf("error writing to cluster at %d in-cluster offset %d: %v", clusterLocation, inClusterOffset, err)
		}
		// for all subsequent clusters, our inClusterOffset should be 0
		inClusterOffset = 0
		// find out where our offset would be for the next cluster
		remainder -= size
	}
	return len(b), nil
}

// TODO:
// When creating, be sure to have the preallocation options: none, metadata, falloc, full

package qcow2

import (
	"fmt"
	"io"
)

// readImage read an existing qcow2 disk to get a usable Qcow2 Driver
func readImage(file io.ReaderAt, start int64) (*qcow2Backend, error) {
	var (
		read int
		err  error
	)

	// load the information from the disk

	// read the header
	b := make([]byte, header2Size)
	read, err = file.ReadAt(b, start)
	if err != nil {
		return nil, fmt.Errorf("Unable to read bytes for header: %v", err)
	}
	if int64(read) != header2Size {
		return nil, fmt.Errorf("Read %d bytes instead of expected %d for header", read, header2Size)
	}

	// parse header first run. The purpose here is just to get the cluster size,
	// since we really should parse the first cluster in its entirety
	h, err := parseHeader(b)
	if err != nil {
		return nil, fmt.Errorf("Error parsing %d minimal header: %v", header2Size, err)
	}
	b = make([]byte, h.clusterSize)
	read, err = file.ReadAt(b, start)
	if err != nil {
		return nil, fmt.Errorf("Unable to read bytes for header: %v", err)
	}
	if read != int(h.clusterSize) {
		return nil, fmt.Errorf("Read %d bytes instead of expected cluster %d for header", read, h.clusterSize)
	}
	h, err = parseHeader(b)
	if err != nil {
		return nil, fmt.Errorf("Error parsing %d full cluster header: %v", h.clusterSize, err)
	}

	compress, err := newCompressor(h.compressionType)
	if err != nil {
		return nil, fmt.Errorf("error getting compression: %v", err)
	}
	encrypt, err := newEncryptor(h.encryptMethod)
	if err != nil {
		return nil, fmt.Errorf("error getting encryptor: %v", err)
	}
	if encrypt.hasHeader() {
		for _, extension := range h.extensions {
			extEncrypt, ok := extension.(headerExtensionFullDiskEncryption)
			if !ok {
				continue
			}
			b = make([]byte, extEncrypt.length)
			read, err = file.ReadAt(b, start+int64(extEncrypt.offset))
			if err != nil {
				return nil, fmt.Errorf("Unable to read bytes for full disk encryption header header: %v", err)
			}
			if uint64(read) != extEncrypt.length {
				return nil, fmt.Errorf("Read %d bytes instead of expected %d for full disk encryption header header", read, extEncrypt.length)
			}
			if err := extEncrypt.parseEncryptionHeader(b); err != nil {
				return nil, fmt.Errorf("Error reading full disk encryption header: %v", err)
			}
		}
	}

	refcountTableSize := h.refCountClusters * h.clusterSize
	refcountTableBytes := make([]byte, int(refcountTableSize))
	pos := start + int64(h.refCountOffset)
	n, err := file.ReadAt(refcountTableBytes, pos)
	if err != nil {
		return nil, fmt.Errorf("error reading refcount table bytes at position %d: %v", pos, err)
	}
	if len(refcountTableBytes) != n {
		return nil, fmt.Errorf("reading refcount table read %d bytes instead of expected %d at position %d", n, len(refcountTableBytes), pos)
	}
	refcountTable, err := parseRefcountTable(refcountTableBytes)
	if err != nil {
		return nil, fmt.Errorf("error parsing refcount table from bytes: %v", err)
	}

	l1TableBytes := make([]byte, int(h.l1Size))
	pos = start + int64(h.l1Offset)
	n, err = file.ReadAt(l1TableBytes, pos)
	if err != nil {
		return nil, fmt.Errorf("error reading L1 table bytes at position %d: %v", pos, err)
	}
	if len(l1TableBytes) != n {
		return nil, fmt.Errorf("reading L1 table read %d bytes instead of expected %d at position %d", n, len(l1TableBytes), pos)
	}
	l1Table, err := parseL1Table(l1TableBytes)
	if err != nil {
		return nil, fmt.Errorf("error parsing L1 table from bytes: %v", err)
	}

	return &qcow2Backend{
		start:         start,
		header:        h,
		blocksize:     int64(h.clusterSize),
		compressor:    compress,
		encryptor:     encrypt,
		l1Table:       l1Table,
		refcountTable: refcountTable,
	}, nil
}

// writeL1Table writes an updated L1 table to disk
func (q qcow2Backend) writeL1Table() error {
	b := q.l1Table.toBytes()
	targetSize := int(q.header.l1Size)
	if targetSize != 0 && len(b) != targetSize {
		return fmt.Errorf("mismatched L1 table size when writing to disk, actual %d, expected %d", len(b), targetSize)
	}
	var pos int64 = q.start + int64(q.header.l1Offset)
	if _, err := q.file.WriteAt(b, pos); err != nil {
		return fmt.Errorf("error writing L1 table at %d: %v", pos, err)
	}
	return nil
}

// writeRefcountTable writes an updated refcount table to disk
func (q qcow2Backend) writeRefcountTable() error {
	b := q.refcountTable.toBytes()
	targetSize := int(q.header.refCountClusters * q.header.clusterSize)
	if targetSize != 0 && len(b) != targetSize {
		return fmt.Errorf("mismatched refcount table size when writing to disk, actual %d, expected %d", len(b), targetSize)
	}
	var pos int64 = q.start + int64(q.header.refCountOffset)
	if _, err := q.file.WriteAt(b, pos); err != nil {
		return fmt.Errorf("error writing refcount table at %d: %v", pos, err)
	}
	return nil
}

// getClusterLocation given an offset into the virtual disk, find the location of
// the start of the cluster in the qcow2 image.
func (q qcow2Backend) getClusterLocation(offset int64, create bool) (location int64, err error) {
	clusterSize := int64(q.header.clusterSize)
	l2entries := clusterSize / 8 // 8 = sizeof uint64
	l2index := (offset / clusterSize) % l2entries
	l1index := (offset / clusterSize) / l2entries
	l1Entry := q.l1Table.entries[l1index]

	// if the l2table referenced (and its clusters) have not been allocated yet?
	switch {
	case l1Entry.allocated:
		// allocated, just get the location and return it
		l2tableData, err := q.readCluster(int64(l1Entry.offset))
		if err != nil {
			return 0, err
		}
		l2table, err := parseL2Table(l2tableData, q.header.clusterBits, q.header.extendedL2)
		if err != nil {
			return 0, err
		}
		location = int64(l2table.entries[l2index].offset)
	case create:
		// not allocated, but we were told to create it, so create the l2table and then the data cluster at the end of the disk
		// be sure to align to cluster boundaries
		l2t := &l2Table{}
		b := l2t.toBytes()
		l2Offset, err := q.writeCluster(b, 0)
		if err != nil {
			return 0, fmt.Errorf("error writing l2 table at %d: %v", l1Entry.offset, err)
		}
		// update the l1Entry
		q.l1Table.entries[l1index] = l1TableEntry{
			offset:    uint64(l2Offset),
			allocated: true,
			active:    true,
		}
		// write the l1Table back out
		if err := q.writeL1Table(); err != nil {
			return 0, fmt.Errorf("error writing l1 table: %v", err)
		}
		b = make([]byte, q.blocksize)
		location, err = q.writeCluster(b, 0)
		if err != nil {
			return 0, fmt.Errorf("error writing data cluster: %v", err)
		}
	default:
		// not allocated, not told to create it, so return empty data
		location = 0
	}
	return location, nil
}

// readCluster read the contents of an individual cluster
func (q qcow2Backend) readCluster(offset int64) ([]byte, error) {
	b := make([]byte, q.header.clusterSize)
	pos := q.start + offset
	n, err := q.file.ReadAt(b, pos)
	if err != nil {
		return nil, fmt.Errorf("error reading cluster at position %d: %v", pos, err)
	}
	if n != len(b) {
		return nil, fmt.Errorf("at position %d, read %d bytes instead of expected %d", pos, n, len(b))
	}
	return b, nil
}

// writeCluster write the contents of an individual cluster. If the offset is 0, it will append it to the end
// of the file, as nothing should be writing to 0.
func (q qcow2Backend) writeCluster(b []byte, offset int64) (pos int64, err error) {
	if len(b) != int(q.header.clusterSize) {
		return pos, fmt.Errorf("mismatched sizes, actual %d, expected %d", len(b), q.header.clusterSize)
	}
	if offset == 0 {
		pos, err = q.file.Seek(0, 2)
	} else {
		pos, err = q.file.Seek(q.start+offset, 0)
	}
	if err != nil {
		return pos, fmt.Errorf("error seeking to file %d: %v", pos, err)
	}
	if _, err := q.file.Write(b); err != nil {
		return pos, fmt.Errorf("error writing cluster: %v", err)
	}
	return pos, nil
}

// getClusterRefcount given an offset into the virtual disk, find the refcount
// of the cluster
func (q qcow2Backend) getClusterRefcount(offset int64, create bool) (refcount int64, err error) {
	clusterSize := int64(q.header.clusterSize)
	refcountBlockEntries := (clusterSize * 8) / int64(q.header.refcountBits) // 8 = sizeof uint64
	refcountBlockIndex := (offset / clusterSize) % refcountBlockEntries
	refcountTableIndex := (offset / clusterSize) / refcountBlockEntries
	refcountTableValue := q.refcountTable.entries[refcountTableIndex]

	// if the refcount block referenced has not been allocated yet?
	switch {
	case refcountTableValue != 0:
		// allocated, just get the location and return it
		refcountBlockData, err := q.readCluster(int64(refcountTableValue))
		if err != nil {
			return 0, err
		}
		refcountBlock, err := parseRefcountBlock(refcountBlockData, q.header.clusterBits)
		if err != nil {
			return 0, err
		}
		refcount = int64(refcountBlock.entries[refcountBlockIndex])
	case create:
		// not allocated, but we were told to create it, so create the l2table and then the data cluster at the end of the disk
		// be sure to align to cluster boundaries
		refcountBlock := &refcountBlock{}
		b := refcountBlock.toBytes()
		refcountBlockOffset, err := q.writeCluster(b, 0)
		if err != nil {
			return 0, fmt.Errorf("error writing refcount block at %d: %v", refcountBlockOffset, err)
		}
		// update the refcount table entry
		q.refcountTable.entries[refcountTableIndex] = refcountTableEntry(refcountBlockOffset)
		// write the refcount table back out
		if err := q.writeRefcountTable(); err != nil {
			return 0, fmt.Errorf("error writing refcount table: %v", err)
		}
		refcount = 1
	default:
		// not allocated, not told to create it, so return empty data
		refcount = 0
	}
	return refcount, nil
}

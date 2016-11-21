package qcow2

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// Guest allows access to the blocks of a qcow2 file as a guest OS sees them
type Guest interface {
	Open(header header, refcounts refcounts, l1 int64, size int64)

	ReaderWriterAt
	Size() int64
}

const (
	noCow      uint64 = 1 << 63
	compressed uint64 = 1 << 62
	zeroBit    uint64 = 1
	l1Valid    uint64 = noCow | (1<<56-1)&^0x1ff
	l2Valid    uint64 = l1Valid | compressed | zeroBit
)

type guestImpl struct {
	header     header
	refcounts  refcounts
	l1Position int64
	size       int64
	sync.RWMutex
}

type mapEntry uint64

func (e mapEntry) compressed() bool {
	return uint64(e)&compressed != 0
}
func (e mapEntry) zero() bool { // For L2 only
	return uint64(e)&zeroBit != 0
}
func (e mapEntry) nil() bool {
	return e.offset() == 0
}
func (e mapEntry) hasOffset() bool {
	return !e.zero() && !e.nil()
}
func (e mapEntry) offset() int64 {
	return int64(uint64(e) &^ (noCow | compressed))
}
func (e mapEntry) cow() bool {
	return uint64(e)&noCow == 0 && uint64(e)&noCow != 0
}
func (e mapEntry) writable() bool {
	return e.hasOffset() && !e.cow()
}

func (g *guestImpl) Open(header header, refcounts refcounts, l1 int64, size int64) {
	g.header = header
	g.refcounts = refcounts
	g.l1Position = l1
	g.size = size
}

func (g *guestImpl) Close() {

}

func (g *guestImpl) io() *ioAt {
	return g.header.io()
}

func (g *guestImpl) clusterSize() int {
	return g.header.clusterSize()
}

func (g *guestImpl) l2Entries() int64 {
	return int64(g.clusterSize() / 8)
}

func (g *guestImpl) validateL1(e mapEntry) error {
	if e.offset()%int64(g.clusterSize()) != 0 {
		return errors.New("Misaligned mapping entry")
	}
	return nil
}

func (g *guestImpl) validateL2(e mapEntry) error {
	if e.zero() {
		return nil
	}
	if e.compressed() {
		return errors.New("Compression not supported")
	}
	return g.validateL1(e)
}

type entryValidator func(mapEntry) error
type offsetFinder func(int64) (int64, error)

func (g *guestImpl) getEntry(validator entryValidator, off int64, writable bool) (mapEntry, error) {
	// Read the current entry
	v, err := g.io().read64(off)
	if err != nil {
		return 0, err
	}
	e := mapEntry(v)
	if err = validator(e); err != nil {
		return 0, err
	}
	if !writable || e.writable() {
		return e, nil
	}

	// Need to make it writable, so allocate a new block
	allocIdx, err := g.refcounts.allocate(1)
	if err != nil {
		return 0, err
	}
	alloc := allocIdx * int64(g.clusterSize())

	// Initialize the new block
	if e.hasOffset() {
		err = g.io().copy(alloc, e.offset(), g.clusterSize())
	} else {
		err = g.io().fill(alloc, g.clusterSize(), 0)
	}

	// Write it to the parent
	if err = g.io().write64(off, uint64(alloc)); err != nil {
		return 0, err
	}

	// Deref the old value
	if e.hasOffset() {
		if _, err = g.refcounts.decrement(e.offset() / int64(g.clusterSize())); err != nil {
			return 0, err
		}
	}

	return mapEntry(uint64(alloc) | noCow), nil
}

func (g *guestImpl) getL1(idx int64, writable bool) (mapEntry, error) {
	off := g.l1Position + (idx/g.l2Entries())*8
	return g.getEntry(g.validateL1, off, writable)
}

func (g *guestImpl) getL2(idx int64, writable bool) (mapEntry, error) {
	// Get the L1
	l1, err := g.getL1(idx, writable)
	if err != nil || l1.nil() {
		return 0, err
	}

	// Read the current L2
	off := l1.offset() + idx%g.l2Entries()*8
	return g.getEntry(g.validateL2, off, writable)
}

func zeroFill(p []byte) {
	for i := 0; i < len(p); i++ {
		p[i] = 0
	}
}

func (g *guestImpl) readByL2(p []byte, l2 mapEntry, off int) error {
	if l2.nil() || l2.zero() {
		zeroFill(p)
	} else {
		if _, err := g.io().ReadAt(p, l2.offset()+int64(off)); err != nil {
			return err
		}
	}
	return nil
}

func (g *guestImpl) readCluster(p []byte, idx int64, off int) error {
	l2, err := g.getL2(idx, false)
	if err != nil {
		return err
	}
	return g.readByL2(p, l2, off)
}

func (g *guestImpl) writeCluster(p []byte, idx int64, off int) error {
	if err := g.header.autoclear(); err != nil {
		return err
	}

	l2, err := g.lookupCluster(idx)
	if err != nil {
		return err
	}

	// Are there any changes?
	orig := make([]byte, len(p))
	if err = g.readClusterByL2(orig, l2, off); err != nil {
		return err
	}
	if bytes.Compare(orig, p) == 0 {
		// If no changes, no need to do anything!
		return nil
	}

	var pos int64
	if l2 == 0 {
		pos, err = g.allocate(idx)
	} else {
		pos, err = g.l2ToOffset(l2)
	}
	if err != nil {
		return err
	}

	if _, err := g.io().WriteAt(p, pos); err != nil {
		return err
	}
	return nil
}

type clusterFunc func(g *guestImpl, p []byte, idx int64, off int) error

func (g *guestImpl) perCluster(p []byte, off int64, f clusterFunc) (n int, err error) {
	if off+int64(len(p)) > g.size {
		return 0, io.ErrUnexpectedEOF
	}

	idx := int64(off / int64(g.clusterSize()))
	offset := int(off % int64(g.clusterSize()))
	n = 0
	for len(p) > 0 {
		length := g.clusterSize() - offset
		if length > len(p) {
			length = len(p)
		}
		if err = f(g, p[:length], idx, offset); err != nil {
			return
		}
		p = p[length:]
		idx++
		offset = 0
		n += length
	}
	return n, nil
}

func (g *guestImpl) ReadAt(p []byte, off int64) (n int, err error) {
	return g.perCluster(p, off, (*guestImpl).readCluster)
}

func (g *guestImpl) WriteAt(p []byte, off int64) (n int, err error) {
	return g.perCluster(p, off, (*guestImpl).writeCluster)
}

func (g *guestImpl) Size() int64 {
	return g.size
}

// Package uring provides a minimal io_uring wrapper for batched file I/O.
// Pure Go, no CGO. Uses unsafe for kernel struct layouts and ring pointer arithmetic.
// Only supports basic submit-and-wait — no SQPOLL, no fixed files, no SQE chaining.
package uring

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// Mmap offsets for io_uring_setup.
	offSQRing = 0
	offCQRing = 0x8000000
	offSQEs   = 0x10000000

	// io_uring_enter flags.
	enterGetEvents = 1

	// io_uring_params features.
	featSingleMmap = 1 << 0
)

// sqringOffsets matches struct io_sqring_offsets from linux/io_uring.h.
type sqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	UserAddr    uint64
}

// cqringOffsets matches struct io_cqring_offsets from linux/io_uring.h.
type cqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	CQEs        uint32
	Flags       uint32
	Resv1       uint32
	UserAddr    uint64
}

// params matches struct io_uring_params from linux/io_uring.h.
type params struct {
	SQEntries    uint32
	CQEntries    uint32
	Flags        uint32
	SQThreadCPU  uint32
	SQThreadIdle uint32
	Features     uint32
	WQFd         uint32
	Resv         [3]uint32
	SQOff        sqringOffsets
	CQOff        cqringOffsets
}

// SQE is a 64-byte submission queue entry matching struct io_uring_sqe.
type SQE struct {
	Opcode      uint8
	Flags       uint8
	Ioprio      uint16
	Fd          int32
	Off         uint64 // file offset or addr2
	Addr        uint64 // buffer address or pathname
	Len         uint32 // buffer length
	OpcodeFlags uint32 // union: rw_flags, open_flags, statx_flags, etc.
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFdIn  int32
	Addr3       uint64
	_pad2       [1]uint64
}

// CQE is a 16-byte completion queue entry matching struct io_uring_cqe.
type CQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

// Statx matches the kernel struct statx layout (256 bytes).
// We only read stx_size but the kernel writes the full struct.
type Statx struct {
	Mask           uint32
	Blksize        uint32
	Attributes     uint64
	Nlink          uint32
	UID            uint32
	GID            uint32
	Mode           uint16
	_spare0        [1]uint16
	Ino            uint64
	Size           uint64
	Blocks         uint64
	AttributesMask uint64
	// timestamps: atime, btime, ctime, mtime — each 16 bytes
	_timestamps [4][16]byte
	// rdev_major, rdev_minor, dev_major, dev_minor
	_devs [4]uint32
	// mnt_id, dio_mem_align, dio_offset_align
	_tail [3]uint64
	// pad to 256 bytes total
	_pad [12]uint64
}

// Ring is a minimal io_uring instance.
type Ring struct {
	fd      int
	sqMem   []byte // mmap'd SQ ring
	cqMem   []byte // mmap'd CQ ring (may be same as sqMem with SINGLE_MMAP)
	sqesMem []byte // mmap'd SQE array

	// SQ ring pointers (into mmap'd memory)
	sqHead  *uint32
	sqTail  *uint32
	sqMask  uint32
	sqArray unsafe.Pointer // base of uint32 indirection array

	// CQ ring pointers (into mmap'd memory)
	cqHead *uint32
	cqTail *uint32
	cqMask uint32
	cqes   unsafe.Pointer // base of CQE array

	sqes    unsafe.Pointer // base of SQE array
	entries uint32
}

// NewRing creates an io_uring instance with the given number of entries.
// entries must be a power of 2 (the kernel will round up if not).
func NewRing(entries uint32) (*Ring, error) {
	var p params
	fd, _, errno := syscall.RawSyscall(unix.SYS_IO_URING_SETUP,
		uintptr(entries), uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}

	r := &Ring{
		fd:      int(fd),
		entries: p.SQEntries,
	}

	if err := r.mmapRings(&p); err != nil {
		unix.Close(r.fd)
		return nil, err
	}

	return r, nil
}

func (r *Ring) mmapRings(p *params) error {
	// Map SQ ring
	sqRingSize := p.SQOff.Array + p.SQEntries*4 // array of uint32
	sqMem, err := syscall.Mmap(r.fd, offSQRing, int(sqRingSize),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sq ring: %w", err)
	}
	r.sqMem = sqMem

	// Map CQ ring (may be same region with SINGLE_MMAP)
	if p.Features&featSingleMmap != 0 {
		r.cqMem = sqMem
	} else {
		cqRingSize := p.CQOff.CQEs + p.CQEntries*uint32(unsafe.Sizeof(CQE{}))
		cqMem, err := syscall.Mmap(r.fd, offCQRing, int(cqRingSize),
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
		if err != nil {
			syscall.Munmap(sqMem)
			return fmt.Errorf("mmap cq ring: %w", err)
		}
		r.cqMem = cqMem
	}

	// Map SQE array
	sqeSize := p.SQEntries * uint32(unsafe.Sizeof(SQE{}))
	sqesMem, err := syscall.Mmap(r.fd, offSQEs, int(sqeSize),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		if r.cqMem != nil && &r.cqMem[0] != &r.sqMem[0] {
			syscall.Munmap(r.cqMem)
		}
		syscall.Munmap(r.sqMem)
		return fmt.Errorf("mmap sqes: %w", err)
	}
	r.sqesMem = sqesMem

	// Set up SQ pointers
	base := unsafe.Pointer(&sqMem[0])
	r.sqHead = (*uint32)(unsafe.Add(base, p.SQOff.Head))
	r.sqTail = (*uint32)(unsafe.Add(base, p.SQOff.Tail))
	r.sqMask = *(*uint32)(unsafe.Add(base, p.SQOff.RingMask))
	r.sqArray = unsafe.Add(base, p.SQOff.Array)

	// Set up CQ pointers
	cqBase := unsafe.Pointer(&r.cqMem[0])
	r.cqHead = (*uint32)(unsafe.Add(cqBase, p.CQOff.Head))
	r.cqTail = (*uint32)(unsafe.Add(cqBase, p.CQOff.Tail))
	r.cqMask = *(*uint32)(unsafe.Add(cqBase, p.CQOff.RingMask))
	r.cqes = unsafe.Add(cqBase, p.CQOff.CQEs)

	// SQE array base
	r.sqes = unsafe.Pointer(&sqesMem[0])

	return nil
}

// Close releases all resources.
func (r *Ring) Close() {
	if r.sqesMem != nil {
		syscall.Munmap(r.sqesMem)
	}
	if r.cqMem != nil && (r.sqMem == nil || &r.cqMem[0] != &r.sqMem[0]) {
		syscall.Munmap(r.cqMem)
	}
	if r.sqMem != nil {
		syscall.Munmap(r.sqMem)
	}
	unix.Close(r.fd)
}

// GetSQE returns a pointer to the SQE at the given index (modulo ring size).
func (r *Ring) GetSQE(index uint32) *SQE {
	idx := index & r.sqMask
	return (*SQE)(unsafe.Add(r.sqes, uintptr(idx)*unsafe.Sizeof(SQE{})))
}

// SubmitAndWait submits count SQEs, waits for all completions, and processes
// CQEs via the callback. Drains all available CQEs to prevent leakage.
func (r *Ring) SubmitAndWait(count uint32, fn func(cqe *CQE)) error {
	if count == 0 {
		return nil
	}

	// Set up SQ array: SQ[slot] = SQE index (callers fill SQEs 0..count-1)
	tail := atomic.LoadUint32(r.sqTail)
	for i := range count {
		slot := (tail + i) & r.sqMask
		*(*uint32)(unsafe.Add(r.sqArray, uintptr(slot)*4)) = i
	}

	// Advance SQ tail (release semantics — kernel reads this)
	atomic.StoreUint32(r.sqTail, tail+count)

	// Submit and wait for all completions
	_, _, errno := syscall.Syscall6(unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), uintptr(count), uintptr(count),
		enterGetEvents, 0, 0)
	if errno != 0 {
		return fmt.Errorf("io_uring_enter: %w", errno)
	}

	// Drain all available CQEs (at least count, possibly more)
	head := atomic.LoadUint32(r.cqHead)
	cqTail := atomic.LoadUint32(r.cqTail)
	for head != cqTail {
		idx := head & r.cqMask
		cqe := (*CQE)(unsafe.Add(r.cqes, uintptr(idx)*unsafe.Sizeof(CQE{})))
		fn(cqe)
		head++
	}
	atomic.StoreUint32(r.cqHead, head)

	return nil
}

// Entries returns the ring size.
func (r *Ring) Entries() uint32 {
	return r.entries
}

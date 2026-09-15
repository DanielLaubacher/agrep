package walker

import "unsafe"

// Linux dirent64 structure layout:
//
//	struct linux_dirent64 {
//	    ino64_t        d_ino;    /* 64-bit inode number */
//	    off64_t        d_off;    /* 64-bit offset to next structure */
//	    unsigned short d_reclen; /* Size of this dirent */
//	    unsigned char  d_type;   /* File type */
//	    char           d_name[]; /* Filename (null-terminated) */
//	};

// File type constants from dirent.h
const (
	DT_UNKNOWN = 0
	DT_FIFO    = 1
	DT_CHR     = 2
	DT_DIR     = 4
	DT_BLK     = 6
	DT_REG     = 8
	DT_LNK     = 10
	DT_SOCK    = 12
)

// Dirent represents a parsed Linux directory entry.
type Dirent struct {
	Name string
	Type uint8
}

// ParseDirents parses raw getdents64 output into Dirent structs, appending
// them onto dst (which is reused to avoid per-call slice allocation; pass
// nil on first call). Unlike a reset-then-fill parse, appending lets a
// caller accumulate several getdents64 batches for one directory into a
// single slice without an extra copy — callers wanting one batch's worth
// alone must pass dst[:0] themselves.
func ParseDirents(buf []byte, n int, dst []Dirent) []Dirent {
	entries := dst
	offset := 0

	for offset < n {
		// Ensure we have at least the fixed header (19 bytes minimum)
		if offset+19 > n {
			break
		}

		// Parse fields from the raw buffer (skip d_ino at offset+0, d_off at offset+8)
		reclen := *(*uint16)(unsafe.Pointer(&buf[offset+16]))
		dtype := buf[offset+18]

		if reclen == 0 {
			break // prevent infinite loop
		}

		// d_name starts at offset+19, null-terminated
		nameStart := offset + 19
		nameEnd := min(offset+int(reclen), n)

		// Find the null terminator
		nameBytes := buf[nameStart:nameEnd]
		nameLen := 0
		for nameLen < len(nameBytes) && nameBytes[nameLen] != 0 {
			nameLen++
		}
		name := string(nameBytes[:nameLen])

		// Skip . and ..
		if name != "." && name != ".." {
			entries = append(entries, Dirent{
				Name: name,
				Type: dtype,
			})
		}

		offset += int(reclen)
	}

	return entries
}

package walker

import (
	"bytes"
	"strings"
)

// IsBinary checks if data appears to be binary by scanning for NUL bytes
// in the first 8KB, matching GNU grep behavior.
func IsBinary(data []byte) bool {
	limit := min(len(data), 8192)
	return bytes.IndexByte(data[:limit], 0) >= 0
}

// IsBinaryExtension returns true if the filename has an extension known to be
// a binary format. Skipping these avoids opening + reading files that would be
// discarded by IsBinary anyway, saving syscalls on trees like /usr/lib.
// Also handles versioned shared libs like "libfoo.so.1.2.3".
func IsBinaryExtension(name string) bool {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return false
	}
	ext := name[dot:]
	// Two-stage check: single character for .a/.o/.z, then map for the rest.
	if len(ext) == 2 {
		switch ext[1] {
		case 'a', 'o', 'z':
			return true
		}
	}
	_, ok := binaryExts[ext]
	if ok {
		return true
	}
	// Handle versioned shared libraries: libfoo.so.1, libfoo.so.1.2.3
	if strings.Contains(name, ".so.") {
		return true
	}
	return false
}

// binaryExts is the set of file extensions known to be binary.
// Covers: compiled objects, shared libs, archives, images, audio, video,
// fonts, executables, compressed, databases, and other common binary formats.
var binaryExts = map[string]struct{}{
	// Compiled / linked
	".so":    {},
	".dylib": {},
	".dll":   {},
	".exe":   {},
	".bin":   {},
	".elf":   {},
	".class": {},
	".pyc":   {},
	".pyo":   {},
	".wasm":  {},
	// Archives / compressed
	".gz":  {},
	".bz2": {},
	".xz":  {},
	".zst": {},
	".lz4": {},
	".lzo": {},
	".zip": {},
	".tar": {},
	".rar": {},
	".7z":  {},
	".cab": {},
	".deb": {},
	".rpm": {},
	".jar": {},
	".war": {},
	// Images
	".png":  {},
	".jpg":  {},
	".jpeg": {},
	".gif":  {},
	".bmp":  {},
	".ico":  {},
	".tif":  {},
	".tiff": {},
	".webp": {},
	".svg":  {}, // technically text, but rarely grepped
	".psd":  {},
	".xcf":  {},
	// Audio / video
	".mp3":  {},
	".mp4":  {},
	".ogg":  {},
	".flac": {},
	".wav":  {},
	".avi":  {},
	".mkv":  {},
	".webm": {},
	".mov":  {},
	".wmv":  {},
	// Fonts
	".ttf":   {},
	".otf":   {},
	".woff":  {},
	".woff2": {},
	".eot":   {},
	// Documents (binary formats)
	".pdf":  {},
	".doc":  {},
	".docx": {},
	".xls":  {},
	".xlsx": {},
	".ppt":  {},
	".pptx": {},
	".odt":  {},
	// Databases
	".db":     {},
	".sqlite": {},
	".mdb":    {},
	// Misc binary
	".swp":      {},
	".swo":      {},
	".DS_Store": {},
}

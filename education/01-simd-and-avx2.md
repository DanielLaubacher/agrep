# SIMD and AVX2 Acceleration in agrep

This document is a comprehensive reference on how agrep uses SIMD (Single Instruction, Multiple Data) instructions -- specifically Intel AVX2 -- to accelerate byte-level pattern matching. It covers the hardware fundamentals, Go 1.26's `simd/archsimd` package, every SIMD technique used in the codebase, and the practical lessons learned about when custom SIMD helps and when it does not.

---

## Table of Contents

1. [What is SIMD?](#1-what-is-simd)
2. [x86-64 Vector Register Hierarchy](#2-x86-64-vector-register-hierarchy)
3. [The Four Core SIMD Operations](#3-the-four-core-simd-operations)
4. [Go 1.26's simd/archsimd Package](#4-go-126s-simdarchsimd-package)
5. [AVX-SSE Transition Penalties and VZEROUPPER](#5-avx-sse-transition-penalties-and-vzeroupper)
6. [Technique: Single-Byte Search (IndexByte)](#6-technique-single-byte-search-indexbyte)
7. [Technique: Backward Search (LastIndexByte)](#7-technique-backward-search-lastindexbyte)
8. [Technique: SIMD Byte Counting (Count)](#8-technique-simd-byte-counting-count)
9. [Technique: AVX2 ASCII Case Conversion (ToLowerASCII)](#9-technique-avx2-ascii-case-conversion-tolowerascii)
10. [Technique: First+Last Byte Prefilter -- Horspool-style SIMD](#10-technique-firstlast-byte-prefilter----horspool-style-simd)
11. [Technique: Bitmask Iteration (Kernighan's Trick)](#11-technique-bitmask-iteration-kernighans-trick)
12. [Technique: Non-Overlapping Match Collection with SIMD Skip](#12-technique-non-overlapping-match-collection-with-simd-skip)
13. [Why bytes.Index Already Uses AVX2](#13-why-bytesindex-already-uses-avx2)
14. [Integration: From SIMD Primitives to Full-Buffer Search](#14-integration-from-simd-primitives-to-full-buffer-search)
15. [Stack Buffer Escape Avoidance in IndexAll](#15-stack-buffer-escape-avoidance-in-indexall)
16. [Performance Results and Lessons](#16-performance-results-and-lessons)
17. [Cross-references](#17-cross-references)

---

## 1. What is SIMD?

SIMD stands for **Single Instruction, Multiple Data**. It is a class of CPU instructions that perform the same operation on multiple data points simultaneously, using wide registers that hold several values packed together.

Consider the task of searching for the byte `0x0A` (newline) in a 1024-byte buffer:

**Scalar approach** (one byte at a time):
```
for each byte in buffer:
    if byte == 0x0A: found it
```
This requires 1024 comparison instructions in the worst case.

**SIMD approach** (32 bytes at a time with AVX2):
```
broadcast 0x0A into all 32 lanes of a 256-bit register
for each 32-byte chunk of buffer:
    load 32 bytes into another 256-bit register
    compare all 32 bytes simultaneously (one instruction)
    extract result bitmask (one instruction)
    if any bit set: found it
```
This requires only 32 iterations of the main loop -- a 32x reduction in the number of comparison operations.

The fundamental insight is that modern CPUs have wide data paths (256 bits for AVX2, 512 bits for AVX-512) but scalar code only uses 8 bits at a time when processing bytes. SIMD fills the entire data path width on every cycle.

---

## 2. x86-64 Vector Register Hierarchy

x86-64 processors provide three tiers of vector registers, each an extension of the previous:

```
ZMM0  [================ 512 bits (64 bytes) ================]   AVX-512
       YMM0  [======== 256 bits (32 bytes) ========]            AVX / AVX2
              XMM0  [==== 128 bits (16 bytes) ====]             SSE
```

| Register | Width    | Bytes | Instruction Set | Year Introduced |
|----------|----------|-------|-----------------|-----------------|
| XMM0-15  | 128-bit  | 16    | SSE2            | 2001 (Pentium 4) |
| YMM0-15  | 256-bit  | 32    | AVX / AVX2      | 2011/2013 (Sandy Bridge / Haswell) |
| ZMM0-31  | 512-bit  | 64    | AVX-512         | 2017 (Skylake-X) |

**YMM registers** are the workhorses of agrep's SIMD code. Each YMM register holds 32 bytes, and AVX2 provides integer operations on all 32 bytes simultaneously. This is the sweet spot for several reasons:

- AVX2 is universally available on x86-64 processors from 2013 onward (nearly all current server and desktop hardware).
- AVX-512 is not universally available, may cause frequency throttling on some CPUs, and provides diminishing returns for byte-level search patterns.
- 32 bytes per iteration is enough to process data at memory bandwidth speeds for most search workloads.

The key AVX2 instructions that agrep relies on (mapped to their `archsimd` equivalents):

| x86 Instruction | What it does | archsimd equivalent |
|-----------------|-------------|---------------------|
| `vpbroadcastb`  | Fill all 32 byte lanes with one value | `BroadcastUint8x32(c)` |
| `vmovdqu`       | Load/store 32 bytes unaligned | `LoadUint8x32Slice(data)` / `.StoreSlice(dst)` |
| `vpcmpeqb`      | Compare 32 byte pairs for equality | `.Equal(other)` |
| `vpmovmskb`     | Extract MSB of each byte into 32-bit int | `.ToBits()` |
| `vzeroupper`    | Zero upper 128 bits of all YMM registers | `ClearAVXUpperBits()` |
| `vpaddb`        | Add 32 byte pairs | `.Add(other)` |
| `vpand`         | Bitwise AND of 256-bit registers | `.And(other)` |
| `vpor`          | Bitwise OR of 256-bit registers | `.Or(other)` |
| `vpmaxub` / `vpminub` | Unsigned byte max/min (used for >= / <=) | `.GreaterEqual(other)` / `.LessEqual(other)` |

---

## 3. The Four Core SIMD Operations

Every SIMD search algorithm in agrep is built from four fundamental operations. Understanding these four operations is sufficient to understand all the SIMD code.

### 3.1 Broadcast

**Fill all 32 lanes of a YMM register with the same byte value.**

```
Input:  byte = 0x41 ('A')
Output: YMM = [0x41, 0x41, 0x41, ..., 0x41]  (32 copies)
```

This creates a "template" register. If you want to search for the letter 'A' in a buffer, you first broadcast 'A' into a register so you can compare it against 32 data bytes at once.

```go
needle := archsimd.BroadcastUint8x32('A')
```

### 3.2 Load

**Read 32 consecutive bytes from memory into a YMM register.**

```
Memory: [0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x20, 0x57, 0x6F, ...]  ("Hello Wo...")
Output: YMM = [0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x20, 0x57, 0x6F, ...]
```

The `vmovdqu` instruction handles unaligned loads, so the data pointer does not need 32-byte alignment (though aligned loads are slightly faster on some microarchitectures).

```go
chunk := archsimd.LoadUint8x32Slice(data[i:])
```

### 3.3 Compare (Equal)

**Compare each of the 32 byte pairs, producing 0xFF for match and 0x00 for mismatch.**

```
Register A (data):   [0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x20, ...]
Register B (needle): [0x6C, 0x6C, 0x6C, 0x6C, 0x6C, 0x6C, ...]   (all 'l')
Result:              [0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00, ...]
                                   ^     ^
                              'l' matches at positions 2 and 3
```

```go
mask := chunk.Equal(needle)
```

### 3.4 Mask Extraction (ToBits)

**Extract the most significant bit (MSB) of each byte into a 32-bit integer.**

Since the comparison result is 0xFF (all bits set) for a match and 0x00 for no match, the MSB is 1 for matches and 0 for non-matches. This gives us a compact 32-bit bitmask where each bit corresponds to one byte position.

```
Compare result: [0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00, ...]
ToBits output:  0b00000000_00000000_00000000_00001100
                                                ^^
                                         positions 2 and 3
```

```go
b := mask.ToBits()  // b == 0b00001100 == 12
```

Once you have this bitmask, standard integer operations (`bits.TrailingZeros32`, `bits.OnesCount32`, Kernighan's trick) let you efficiently extract match positions without examining all 32 bytes individually.

---

## 4. Go 1.26's simd/archsimd Package

Go 1.26 introduced the experimental `simd/archsimd` package, which provides type-safe access to SIMD intrinsics without writing assembly or using CGO. It requires the `GOEXPERIMENT=simd` environment variable at build time.

### Enabling SIMD

All `go` commands that compile agrep code must set the experiment flag:

```bash
GOEXPERIMENT=simd go build ./...
GOEXPERIMENT=simd go test -race ./...
GOEXPERIMENT=simd go test -bench=. -benchmem ./internal/simd/
```

The agrep Makefile sets this automatically.

### Core Type: `archsimd.Uint8x32`

This type represents 32 unsigned bytes packed into a single 256-bit YMM register. It is a value type (not a pointer), so it can live in registers without heap allocation.

### Function Reference

**Construction:**

| Function | x86 Instruction | Description |
|----------|-----------------|-------------|
| `archsimd.BroadcastUint8x32(c byte) Uint8x32` | `vpbroadcastb` | Fill all 32 lanes with byte `c` |
| `archsimd.LoadUint8x32Slice(data []byte) Uint8x32` | `vmovdqu` | Load 32 bytes from a slice into a register |

**Comparison:**

| Method | x86 Instruction | Description |
|--------|-----------------|-------------|
| `.Equal(other Uint8x32) Uint8x32` | `vpcmpeqb` | Per-byte equality: 0xFF if equal, 0x00 otherwise |
| `.GreaterEqual(other Uint8x32) Uint8x32` | `vpmaxub` + `vpcmpeqb` | Per-byte unsigned >= comparison |
| `.LessEqual(other Uint8x32) Uint8x32` | `vpminub` + `vpcmpeqb` | Per-byte unsigned <= comparison |

**Extraction:**

| Method | x86 Instruction | Description |
|--------|-----------------|-------------|
| `.ToBits() uint32` | `vpmovmskb` | Extract MSB of each byte into a 32-bit integer |

**Arithmetic and Logic:**

| Method | x86 Instruction | Description |
|--------|-----------------|-------------|
| `.Add(other Uint8x32) Uint8x32` | `vpaddb` | Per-byte addition (wrapping) |
| `.And(other Uint8x32) Uint8x32` | `vpand` | Bitwise AND of all 256 bits |
| `.Or(other Uint8x32) Uint8x32` | `vpor` | Bitwise OR of all 256 bits |
| `.Masked(mask Uint8x32) Uint8x32` | `vpand` | Zero lanes where mask is 0x00, keep where 0xFF |

**Store:**

| Method | x86 Instruction | Description |
|--------|-----------------|-------------|
| `.StoreSlice(dst []byte)` | `vmovdqu` | Write 32 bytes from register to memory |

**Cleanup:**

| Function | x86 Instruction | Description |
|----------|-----------------|-------------|
| `archsimd.ClearAVXUpperBits()` | `vzeroupper` | Zero upper 128 bits of all YMM registers |

---

## 5. AVX-SSE Transition Penalties and VZEROUPPER

This is one of the most important and most commonly misunderstood aspects of writing AVX2 code. Getting this wrong causes silent, severe performance degradation.

### The Problem

The XMM registers (128-bit, used by SSE) are the lower half of the YMM registers (256-bit, used by AVX2). When you use an AVX2 instruction, it writes to the full 256-bit YMM register. When you later execute an SSE instruction (which only reads/writes the lower 128-bit XMM portion), the CPU faces a dilemma: the upper 128 bits of the YMM register still contain data from the AVX2 operation.

On Intel CPUs (pre-Ice Lake), the processor must save the upper 128 bits to an internal buffer before executing the SSE instruction, then restore them afterward. This save/restore incurs a penalty of approximately **70 clock cycles per transition** on older microarchitectures.

This matters because:
1. The Go runtime uses SSE instructions internally (floating point, some string operations).
2. The OS kernel may use SSE for signal handlers or context switches.
3. Any function you call after your SIMD code might use SSE.

### The Solution: VZEROUPPER

The `VZEROUPPER` instruction explicitly zeros the upper 128 bits of **all** YMM registers. This tells the CPU "I am done with AVX2; you do not need to save anything." It executes in a single cycle and eliminates the transition penalty entirely.

```go
archsimd.ClearAVXUpperBits()  // Emits VZEROUPPER
```

### Rule: VZEROUPPER at Every Exit Point

In agrep, `ClearAVXUpperBits()` is called at **every code path that exits a function using AVX2 instructions**. This includes:
- Early returns (match found)
- Loop exits (no match)
- Error/edge case returns

Looking at `internal/simd/simd.go`, here is every location where `ClearAVXUpperBits()` is called:

```
Line 27: return inside SIMD loop of IndexByte (match found)
Line 36: return inside scalar tail of IndexByte (match found in tail)
Line 41: return at end of IndexByte (no match, -1)
Line 59: return inside scalar tail of LastIndexByte (match in tail)
Line 70: return inside SIMD loop of LastIndexByte (match found)
Line 76: return at end of LastIndexByte (no match, -1)
Line 107: return at end of Count
Line 141: return at end of ToLowerASCII
```

File: `internal/simd/simd.go`

And in `internal/simd/index.go`:

```
Line 109: return inside SIMD loop of indexAllByte (via implicit ClearAVXUpperBits before return)
Line 163: return inside SIMD loop of IndexCaseInsensitive (match found)
Line 175: return inside scalar tail of IndexCaseInsensitive (match found)
Line 180: return at end of IndexCaseInsensitive (no match)
Line 260: return at end of IndexAllCaseInsensitive
```

File: `internal/simd/index.go`

The pattern is: **if any `archsimd` function was called before this return statement, `ClearAVXUpperBits()` must precede it.** Missing even one call site can cause intermittent performance degradation that is very difficult to diagnose, because it depends on what SSE instructions the caller (or Go runtime) happens to execute next.

---

## 6. Technique: Single-Byte Search (IndexByte)

The simplest SIMD search: find the first occurrence of a single byte in a buffer.

File: `internal/simd/simd.go`, lines 13-43

```go
// IndexByte returns the index of the first occurrence of c in data, or -1 if not present.
// Uses AVX2 VPCMPEQB to compare 32 bytes per iteration.
func IndexByte(data []byte, c byte) int {
    n := len(data)
    if n == 0 {
        return -1
    }

    needle := archsimd.BroadcastUint8x32(c)
    i := 0

    for i+32 <= n {
        chunk := archsimd.LoadUint8x32Slice(data[i:])
        mask := chunk.Equal(needle)
        b := mask.ToBits()
        if b != 0 {
            archsimd.ClearAVXUpperBits()
            return i + bits.TrailingZeros32(b)
        }
        i += 32
    }

    // Scalar tail
    for ; i < n; i++ {
        if data[i] == c {
            archsimd.ClearAVXUpperBits()
            return i
        }
    }

    archsimd.ClearAVXUpperBits()
    return -1
}
```

### Step-by-step Walkthrough

**Step 1: Broadcast the target byte**
```go
needle := archsimd.BroadcastUint8x32(c)
```
If `c = '\n'` (0x0A), the YMM register `needle` now contains:
```
[0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A,
 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A,
 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A,
 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A, 0x0A]
```
This is done once, outside the loop. The register will be reused for every chunk comparison.

**Step 2: Load 32 bytes from the data buffer**
```go
chunk := archsimd.LoadUint8x32Slice(data[i:])
```
This reads 32 consecutive bytes starting at position `i`. For example, if the data is `"Hello World\nGoodbye\n..."`:
```
chunk = [0x48, 0x65, 0x6C, 0x6C, 0x6F, 0x20, 0x57, 0x6F,
         0x72, 0x6C, 0x64, 0x0A, 0x47, 0x6F, 0x6F, 0x64,
         0x62, 0x79, 0x65, 0x0A, ...]
                      ^^^^                ^^^^
                      '\n' at position 11   '\n' at position 19
```

**Step 3: Compare all 32 bytes simultaneously**
```go
mask := chunk.Equal(needle)
```
This executes a single `vpcmpeqb` instruction. For each of the 32 byte positions:
- If `chunk[j] == needle[j]`: result byte is `0xFF` (all bits set)
- If `chunk[j] != needle[j]`: result byte is `0x00` (all bits clear)

```
mask = [0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
        0x00, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00, 0x00,
        0x00, 0x00, 0x00, 0xFF, ...]
                         ^^^^                     ^^^^
                   match at 11              match at 19
```

**Step 4: Extract the bitmask**
```go
b := mask.ToBits()
```
This executes `vpmovmskb`, which takes the most significant bit (bit 7) of each of the 32 bytes and packs them into a 32-bit integer:

```
b = 0b00000000_00001000_00000000_10000000_00000000  (simplified)
    = 0x00080800
```
Actually, for our example: bit 11 and bit 19 are set:
```
b = (1 << 11) | (1 << 19) = 0x00082800
```

**Step 5: Find the first match position**
```go
if b != 0 {
    archsimd.ClearAVXUpperBits()
    return i + bits.TrailingZeros32(b)
}
```
`bits.TrailingZeros32(b)` counts the number of trailing zero bits, which gives the position of the lowest set bit -- i.e., the position of the first match within the 32-byte chunk.

For `b = (1 << 11) | (1 << 19)`:
- `bits.TrailingZeros32(b)` = 11
- Return value: `i + 11` = position of the first `'\n'` in the data

**Step 6: Scalar tail**
```go
for ; i < n; i++ {
    if data[i] == c {
        archsimd.ClearAVXUpperBits()
        return i
    }
}
```
After the main SIMD loop, there may be fewer than 32 bytes remaining. These are checked one at a time with scalar code. This tail is at most 31 bytes, so the scalar overhead is negligible for any buffer larger than a few dozen bytes.

### Why the Scalar Tail is Necessary

If the buffer length is not a multiple of 32, reading 32 bytes from position `i` when `i + 32 > n` would read past the end of the slice, causing a bounds violation. The scalar tail handles this remainder safely.

An alternative technique (not used here) is to do a final overlapping read: load 32 bytes ending at position `n` (i.e., from `n-32` to `n`), which may re-examine some bytes already checked. This avoids the scalar tail but requires careful deduplication of results.

---

## 7. Technique: Backward Search (LastIndexByte)

The same principle as `IndexByte`, but scanning from the end of the buffer toward the start. This finds the **last** occurrence of a byte.

File: `internal/simd/simd.go`, lines 47-78

```go
// LastIndexByte returns the index of the last occurrence of c in data, or -1 if not present.
// Uses AVX2 scanning from the end.
func LastIndexByte(data []byte, c byte) int {
    n := len(data)
    if n == 0 {
        return -1
    }

    needle := archsimd.BroadcastUint8x32(c)

    // Scalar tail for bytes that don't fill a full 32-byte chunk
    tail := n % 32
    for i := n - 1; i >= n-tail; i-- {
        if data[i] == c {
            archsimd.ClearAVXUpperBits()
            return i
        }
    }

    // SIMD scan from end, 32 bytes at a time
    for i := n - tail - 32; i >= 0; i -= 32 {
        chunk := archsimd.LoadUint8x32Slice(data[i:])
        mask := chunk.Equal(needle)
        b := mask.ToBits()
        if b != 0 {
            archsimd.ClearAVXUpperBits()
            // Highest set bit = last match in this chunk
            return i + 31 - bits.LeadingZeros32(b)
        }
    }

    archsimd.ClearAVXUpperBits()
    return -1
}
```

### Key Difference: LeadingZeros Instead of TrailingZeros

When scanning forward, we want the **first** (lowest) set bit, so we use `bits.TrailingZeros32(b)`.

When scanning backward, we process chunks from right to left. Within each chunk, the bytes are still in left-to-right order (position 0 is the leftmost byte in the chunk). The **last** match in a chunk is the **highest** set bit. So we use:

```go
return i + 31 - bits.LeadingZeros32(b)
```

`bits.LeadingZeros32(b)` returns the number of leading zeros (counting from bit 31 down). Subtracting from 31 gives the bit position of the highest set bit, which corresponds to the byte position of the last match.

Example:
```
b = 0b00000000_00001000_10000000_00000000
     bit 31 .......... bit 19 ... bit 11 .............. bit 0

bits.LeadingZeros32(b) = 12   (bits 31..20 are zero)
31 - 12 = 19                   (position 19 is the last match in this chunk)
```

### Scalar Tail Comes First

Notice that the scalar tail is at the **beginning** of the function (checking the rightmost bytes), not at the end. When scanning backward, the "remainder" bytes that do not fill a full 32-byte chunk are at the **right** end of the buffer. These are checked first with scalar code, then the SIMD loop processes the rest in 32-byte chunks from right to left.

---

## 8. Technique: SIMD Byte Counting (Count)

Counting occurrences of a byte in a buffer is a variation of searching where we do not stop at the first match -- instead, we accumulate the total number of matches across all chunks.

File: `internal/simd/simd.go`, lines 82-109

```go
// Count returns the number of occurrences of c in data.
// Uses AVX2 to compare 32 bytes per iteration and popcount to sum matches.
func Count(data []byte, c byte) int {
    n := len(data)
    if n == 0 {
        return 0
    }

    needle := archsimd.BroadcastUint8x32(c)
    count := 0
    i := 0

    for i+32 <= n {
        chunk := archsimd.LoadUint8x32Slice(data[i:])
        mask := chunk.Equal(needle)
        b := mask.ToBits()
        count += bits.OnesCount32(b)
        i += 32
    }

    // Scalar tail
    for ; i < n; i++ {
        if data[i] == c {
            count++
        }
    }

    archsimd.ClearAVXUpperBits()
    return count
}
```

### The Key: popcount (OnesCount32)

Instead of iterating over set bits to find positions, we use `bits.OnesCount32(b)` which returns the **number** of set bits in the 32-bit mask. This maps to the x86 `POPCNT` instruction, which executes in a single cycle on modern hardware.

For each 32-byte chunk:
1. Compare all 32 bytes against the target (one instruction).
2. Extract the bitmask (one instruction).
3. Count set bits (one instruction).
4. Add to running total.

This is three instructions per 32 bytes of data, plus a load -- roughly 8 bytes per clock cycle, approaching memory bandwidth limits.

### Use Case in agrep

The `Count` function is used to count newline characters (`'\n'`) in data buffers, which is needed for computing line numbers. When the matcher finds match offsets in a buffer, it needs to know what line number each offset falls on. Counting newlines up to each offset provides this.

---

## 9. Technique: AVX2 ASCII Case Conversion (ToLowerASCII)

Converting ASCII uppercase letters to lowercase is a common need for case-insensitive search. The SIMD approach converts 32 bytes simultaneously without branching.

File: `internal/simd/simd.go`, lines 113-142

```go
// ToLowerASCII lowercases ASCII bytes from src into dst using AVX2.
// dst must be at least len(src) bytes. Non-ASCII bytes are copied unchanged.
func ToLowerASCII(dst, src []byte) {
    n := len(src)
    if n == 0 {
        return
    }

    vecA := archsimd.BroadcastUint8x32('A')
    vecZ := archsimd.BroadcastUint8x32('Z')
    vec32 := archsimd.BroadcastUint8x32(0x20)
    i := 0

    for i+32 <= n {
        chunk := archsimd.LoadUint8x32Slice(src[i:])
        isUpper := chunk.GreaterEqual(vecA).And(chunk.LessEqual(vecZ))
        lowered := chunk.Add(vec32.Masked(isUpper))
        lowered.StoreSlice(dst[i:])
        i += 32
    }

    // Scalar tail
    for ; i < n; i++ {
        b := src[i]
        if b >= 'A' && b <= 'Z' {
            b += 0x20
        }
        dst[i] = b
    }

    archsimd.ClearAVXUpperBits()
}
```

### Understanding ASCII Case Layout

In ASCII, uppercase and lowercase letters differ by exactly one bit:

```
'A' = 0x41 = 0100_0001
'a' = 0x61 = 0110_0001
              ^
              bit 5 (0x20) is the only difference
```

Adding `0x20` to any uppercase letter converts it to its lowercase equivalent. The challenge is doing this **conditionally** -- only bytes in the range `'A'` (0x41) through `'Z'` (0x5A) should be modified.

### Step-by-Step

**Step 1: Create broadcast vectors for comparison bounds and the offset**
```go
vecA := archsimd.BroadcastUint8x32('A')    // 32 copies of 0x41
vecZ := archsimd.BroadcastUint8x32('Z')    // 32 copies of 0x5A
vec32 := archsimd.BroadcastUint8x32(0x20)  // 32 copies of 0x20
```

**Step 2: Load 32 bytes of source data**
```go
chunk := archsimd.LoadUint8x32Slice(src[i:])
```

**Step 3: Create a mask of which bytes are uppercase letters**
```go
isUpper := chunk.GreaterEqual(vecA).And(chunk.LessEqual(vecZ))
```

This computes two comparisons and ANDs them:
- `chunk.GreaterEqual(vecA)`: 0xFF where byte >= 'A', 0x00 otherwise
- `chunk.LessEqual(vecZ)`: 0xFF where byte <= 'Z', 0x00 otherwise
- `.And(...)`: 0xFF only where **both** conditions hold (byte is in range A-Z)

For example:
```
chunk:              ['H', 'e', 'l', 'l', 'o', ' ', 'W', '3', ...]
GreaterEqual('A'):  [0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0xFF, 0x00, ...]
LessEqual('Z'):     [0xFF, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, ...]
AND:                [0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0x00, ...]
                       ^                                    ^
                      'H' is uppercase                     'W' is uppercase
```

**Step 4: Conditionally add 0x20**
```go
lowered := chunk.Add(vec32.Masked(isUpper))
```

`vec32.Masked(isUpper)` zeroes out the 0x20 value in lanes where `isUpper` is 0x00, keeping 0x20 only where the byte is uppercase:
```
vec32:            [0x20, 0x20, 0x20, 0x20, 0x20, 0x20, 0x20, 0x20, ...]
isUpper:          [0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0x00, ...]
Masked result:    [0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x20, 0x00, ...]
```

Then `chunk.Add(...)` adds this to the original data:
```
chunk:   ['H',  'e',  'l',  'l',  'o',  ' ',  'W',  '3', ...]
add:     [0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x20, 0x00, ...]
result:  ['h',  'e',  'l',  'l',  'o',  ' ',  'w',  '3', ...]
```

Non-ASCII bytes (>= 0x80) pass through unchanged because they do not satisfy the `GreaterEqual('A').And(LessEqual('Z'))` check. Digits, punctuation, and whitespace are similarly unaffected.

**Step 5: Store result**
```go
lowered.StoreSlice(dst[i:])
```

This writes the 32 lowercased bytes to the destination buffer.

### Performance Characteristics

This approach is entirely branchless within the SIMD loop. There are no conditional jumps -- every byte in every chunk takes the same code path. This is ideal for pipelined execution on modern CPUs, which suffer significant penalties from branch misprediction.

---

## 10. Technique: First+Last Byte Prefilter -- Horspool-style SIMD

This is the most important SIMD technique in agrep and the primary source of SIMD-derived performance advantage. It accelerates **multi-byte pattern search** (especially case-insensitive) by using a two-point prefilter inspired by the Horspool string search algorithm.

File: `internal/simd/index.go`, lines 123-182

### The Core Insight

When searching for a pattern like `"error"` (length 5) in a buffer:
- A naive approach checks all 5 bytes at every position.
- A smarter approach first checks just the **first byte** (`'e'`) and only verifies the remaining bytes when the first matches.
- An even smarter approach checks **both the first AND last byte** simultaneously.

The probability of a random byte matching the first byte is 1/256 (for case-sensitive) or 2/256 (for case-insensitive, matching both cases). The probability of **both** the first and last byte matching is:

```
Case-sensitive:    (1/256) * (1/256)  =  1/65,536   = 0.0015%
Case-insensitive:  (2/256) * (2/256)  =  4/65,536   = 0.006%
```

This means that for every 65,536 positions checked (case-sensitive) or ~16,384 positions (case-insensitive), only about one will be a candidate requiring full verification. Since the SIMD prefilter checks 32 positions per iteration, you might go thousands of iterations before finding a single candidate.

### The Algorithm

For a pattern of length P:
1. Broadcast the first byte of the pattern (both cases for case-insensitive).
2. Broadcast the last byte of the pattern (both cases for case-insensitive).
3. For each 32-byte window starting at position `i`:
   a. Load 32 bytes from position `i` (first-byte candidates).
   b. Load 32 bytes from position `i + P - 1` (last-byte candidates).
   c. Compare first-byte block against broadcast first byte; OR the case variants.
   d. Compare last-byte block against broadcast last byte; OR the case variants.
   e. AND the first-byte mask with the last-byte mask.
   f. For each set bit in the combined mask: verify the full pattern at that position.

### Full Code

```go
// IndexCaseInsensitive returns the index of the first case-insensitive occurrence
// of pattern in data. Pattern must be pre-lowered. Only handles ASCII case folding.
func IndexCaseInsensitive(data, patternLower []byte) int {
    plen := len(patternLower)
    switch {
    case plen == 0:
        return 0
    case plen > len(data):
        return -1
    }

    // For case-insensitive, we need to check both cases of first/last byte
    firstLo := patternLower[0]
    firstHi := toUpperASCII(firstLo)
    lastLo := patternLower[plen-1]
    lastHi := toUpperASCII(lastLo)

    bFirstLo := archsimd.BroadcastUint8x32(firstLo)
    bFirstHi := archsimd.BroadcastUint8x32(firstHi)
    bLastLo := archsimd.BroadcastUint8x32(lastLo)
    bLastHi := archsimd.BroadcastUint8x32(lastHi)

    i := 0
    limit := len(data) - plen + 1

    for i+32 <= limit {
        blockFirst := archsimd.LoadUint8x32Slice(data[i:])
        blockLast := archsimd.LoadUint8x32Slice(data[i+plen-1:])

        mFirstLo := blockFirst.Equal(bFirstLo)
        mFirstHi := blockFirst.Equal(bFirstHi)
        mFirst := mFirstLo.Or(mFirstHi)

        mLastLo := blockLast.Equal(bLastLo)
        mLastHi := blockLast.Equal(bLastHi)
        mLast := mLastLo.Or(mLastHi)

        b := mFirst.And(mLast).ToBits()

        for b != 0 {
            j := bits.TrailingZeros32(b)
            if matchCaseInsensitive(data[i+j:i+j+plen], patternLower) {
                archsimd.ClearAVXUpperBits()
                return i + j
            }
            b &= b - 1
        }

        i += 32
    }

    // Scalar tail
    for ; i < limit; i++ {
        if matchCaseInsensitive(data[i:i+plen], patternLower) {
            archsimd.ClearAVXUpperBits()
            return i
        }
    }

    archsimd.ClearAVXUpperBits()
    return -1
}
```

### Visual Walkthrough

Suppose we are searching for `"error"` (pattern length 5) in a buffer, and we are at position `i`:

```
Buffer positions:  i    i+1  i+2  i+3  ... i+31
                   |    |    |    |        |
First-byte load:   [b0,  b1,  b2,  b3, ..., b31]   <- 32 bytes from data[i:]
Last-byte load:    [b4,  b5,  b6,  b7, ..., b35]   <- 32 bytes from data[i+4:]
                    ^                         ^
                    These are 4 positions apart (plen-1 = 4)
```

For position `j` within this 32-byte window:
- `blockFirst[j]` is the byte at `data[i+j]` -- the candidate first byte
- `blockLast[j]` is the byte at `data[i+j+4]` -- the candidate last byte

If both match their respective targets, position `i+j` is a candidate for the full pattern `"error"`.

The two loads are **offset by `plen-1`** (4 in this case). This is the key geometric trick: position `j` in the first-byte block aligns with position `j` in the last-byte block, but they reference data positions that are `plen-1` bytes apart -- exactly the distance between the first and last character of the pattern.

### Case-Insensitive Matching

For case-insensitive search, each byte has two variants. The first byte of `"error"` could be `'e'` or `'E'`. So we do two comparisons and OR them:

```go
mFirstLo := blockFirst.Equal(bFirstLo)   // matches lowercase 'e'
mFirstHi := blockFirst.Equal(bFirstHi)   // matches uppercase 'E'
mFirst := mFirstLo.Or(mFirstHi)          // matches either case
```

Same for the last byte (`'r'` or `'R'`).

The combined mask `mFirst.And(mLast)` identifies positions where both the first and last bytes match in any case combination. Only these positions proceed to full verification:

```go
func matchCaseInsensitive(data, patternLower []byte) bool {
    for i, b := range data {
        if toLowerASCII(b) != patternLower[i] {
            return false
        }
    }
    return true
}
```

File: `internal/simd/index.go`, lines 272-279

The full verification is scalar (byte-by-byte), but it is called so rarely that its cost is amortized to nearly zero. The SIMD prefilter eliminates >99.99% of positions.

### Why This is the Key SIMD Win

For **case-sensitive** search, Go's `bytes.Index` already uses similar SIMD tricks internally (see [Section 13](#13-why-bytesindex-already-uses-avx2)). There is no point reimplementing it.

For **case-insensitive** search, there is no stdlib equivalent. The naive approach (`bytes.Contains(bytes.ToLower(data), patternLower)`) requires allocating a lowercased copy of the entire buffer. The Horspool-style prefilter avoids this allocation entirely: it checks the original mixed-case data against both case variants, performing at most `plen` scalar case conversions per candidate position.

---

## 11. Technique: Bitmask Iteration (Kernighan's Trick)

Once the SIMD comparison produces a 32-bit bitmask, we need to iterate over the set bits to process each match position. The naive approach (checking all 32 bit positions) wastes time on zero bits. Kernighan's trick iterates only over **set** bits.

### The Trick

```go
for b != 0 {
    j := bits.TrailingZeros32(b)   // position of lowest set bit
    // ... process position j ...
    b &= b - 1                     // clear the lowest set bit
}
```

**How `b &= b - 1` works:**

Subtracting 1 from a binary number flips the lowest set bit and all bits below it:
```
b     = 0b10100100
b - 1 = 0b10100011
              ^^^^ these bits all flipped
```

ANDing `b` with `b - 1` keeps all the higher bits unchanged but clears the lowest set bit:
```
b         = 0b10100100
b - 1     = 0b10100011
b & (b-1) = 0b10100000    <- lowest set bit (bit 2) is now clear
```

### Full Example

```
Iteration 1:
  b = 0b10100100
  TrailingZeros32 = 2         -> process position 2
  b &= b - 1 -> 0b10100000

Iteration 2:
  b = 0b10100000
  TrailingZeros32 = 5         -> process position 5
  b &= b - 1 -> 0b10000000

Iteration 3:
  b = 0b10000000
  TrailingZeros32 = 7         -> process position 7
  b &= b - 1 -> 0b00000000

Loop exits: b == 0
```

Three iterations for three set bits, with no wasted iterations on zero bits. This is optimal: the loop body executes exactly once per set bit.

### Historical Note

This technique is attributed to Brian Kernighan (of K&R C fame) for counting set bits. In SIMD search code, it is adapted for **position extraction**: instead of just counting, we use `TrailingZeros32` to get the position before clearing the bit.

### Where It Appears in agrep

- `IndexCaseInsensitive`: iterates over candidate positions from the first+last byte prefilter
  (`internal/simd/index.go`, line 160-167)
- `IndexAllCaseInsensitive`: same, with additional skip logic for non-overlapping matches
  (`internal/simd/index.go`, lines 215-239)
- `indexAllByte`: iterates over all matching byte positions in a chunk
  (`internal/simd/index.go`, lines 77-89)

---

## 12. Technique: Non-Overlapping Match Collection with SIMD Skip

When collecting **all** matches (not just the first), we need to handle non-overlapping semantics: after finding a match at position `j`, the next match cannot start until position `j + patternLen`.

File: `internal/simd/index.go`, lines 185-270

### The Problem

In `IndexAllCaseInsensitive`, we process a 32-byte chunk and get a bitmask of candidate positions. If we find a valid match at position `j`, we need to skip the next `patternLen - 1` positions (they would overlap with the current match). But these positions might have bits set in the current bitmask.

### The Solution: Bitmask Shifting

```go
for b != 0 {
    j := bits.TrailingZeros32(b)
    pos := i + j
    if matchCaseInsensitive(data[pos:pos+plen], patternLower) {
        // ... record the match ...

        // Skip overlapping positions in the current bitmask
        skipTo := j + plen
        if skipTo < 32 {
            b >>= skipTo
            b <<= skipTo
        } else {
            b = 0
        }
        continue
    }
    b &= b - 1
}
```

### How the Skip Works

Suppose `plen = 5` and we found a match at bit position `j = 10`. The next valid position is `j + 5 = 15`. We need to clear bits 10-14 from the bitmask.

```
Before:  b = ...1_1110_1100_0100_0000_0000
              bit 15 is set, bits 10-12 are set

skipTo = 10 + 5 = 15

b >>= 15:    ...0_0000_0000_0000_001  (shift right, clearing low bits)
b <<= 15:    ...1_0000_0000_0000_000  (shift left, putting remaining bits back)

After:   b = ...1_0000_0000_0000_0000
              Only bit 15 remains; bits 10-14 are cleared
```

The right-shift discards the lowest `skipTo` bits. The left-shift restores the remaining bits to their correct positions. The net effect is zeroing out bits 0 through `skipTo - 1`.

If `skipTo >= 32`, the entire bitmask is consumed (the match spans beyond this 32-byte chunk), so we set `b = 0` to exit the inner loop immediately.

### Why Two Paths (Match vs. No-Match)

Notice the two different bit-clearing strategies:

```go
if matchCaseInsensitive(...) {
    // Match found: skip forward by pattern length
    skipTo := j + plen
    b >>= skipTo; b <<= skipTo   // or b = 0
    continue
}
b &= b - 1   // No match: just clear this one candidate bit
```

When the full verification **fails** (the first+last bytes matched but the middle bytes did not), we only clear the current bit using Kernighan's trick and try the next candidate. When verification **succeeds**, we skip forward by the full pattern length to enforce non-overlapping semantics.

---

## 13. Why bytes.Index Already Uses AVX2

An important lesson learned during agrep development: **Go's standard library already uses SIMD assembly for common byte operations.**

### What the Stdlib Optimizes

Go's `bytes` and `strings` packages include hand-written assembly (in `internal/bytealg`) for:
- `IndexByte` / `IndexByteString` -- single-byte search using SSE2/AVX2
- `Index` / `IndexString` -- multi-byte substring search using a combination of techniques
- `Equal` / `Compare` -- byte slice comparison
- `Count` -- byte counting

These implementations use `PCMPESTRI` (SSE4.2), `VPCMPEQB` (AVX2), and other SIMD instructions depending on the CPU capabilities detected at startup.

### agrep's Delegation Strategy

For case-sensitive multi-byte search, agrep delegates directly to `bytes.Index`:

File: `internal/simd/index.go`, lines 12-14

```go
// Index returns the index of the first occurrence of pattern in data, or -1 if not present.
// Delegates to bytes.Index which uses optimized AVX2 assembly internally.
func Index(data, pattern []byte) int {
    return bytes.Index(data, pattern)
}
```

For case-sensitive `IndexAll` (finding all occurrences), agrep loops over `bytes.Index`:

File: `internal/simd/index.go`, lines 18-63

```go
func IndexAll(data, pattern []byte) []int {
    plen := len(pattern)
    switch {
    case plen == 0:
        return nil
    case plen == 1:
        return indexAllByte(data, pattern[0])
    case plen > len(data):
        return nil
    }

    var stackBuf [16]int
    n := 0
    var overflow []int
    i := 0

    for {
        idx := bytes.Index(data[i:], pattern)
        if idx < 0 {
            break
        }
        if n < len(stackBuf) {
            stackBuf[n] = i + idx
        } else {
            if overflow == nil {
                overflow = make([]int, 0, 64)
                overflow = append(overflow, stackBuf[:]...)
            }
            overflow = append(overflow, i+idx)
        }
        n++
        i += idx + plen
    }

    if n == 0 {
        return nil
    }
    if overflow != nil {
        return overflow
    }
    result := make([]int, n)
    copy(result, stackBuf[:n])
    return result
}
```

### The Lesson

**Always benchmark against the stdlib before writing custom SIMD.** The Go standard library team has invested significant effort into optimizing common operations with hand-tuned assembly. For single-byte operations like `IndexByte`, agrep's custom AVX2 implementation performs roughly the same as the stdlib:

```
BenchmarkIndexByte_SIMD:    ~same throughput
BenchmarkIndexByte_Stdlib:  ~same throughput
```

Custom SIMD only provides an advantage where the stdlib does **not** have an equivalent:
- **Case-insensitive substring search**: No stdlib function for this. The Horspool-style first+last byte prefilter is the main SIMD win.
- **Batch case conversion (ToLowerASCII)**: `bytes.ToLower` allocates a new buffer; the SIMD version writes to a pre-allocated destination.

---

## 14. Integration: From SIMD Primitives to Full-Buffer Search

The SIMD functions in `internal/simd/` are low-level building blocks. The matcher layer in `internal/matcher/` composes them into complete search strategies.

### Matcher Selection

File: `internal/matcher/factory.go`

```
Pattern type          -> Matcher              -> SIMD function used
-----------              -------                 -------------------
Fixed + 1 pattern     -> BoyerMooreMatcher    -> simd.IndexAll / simd.IndexAllCaseInsensitive
Fixed + N patterns    -> AhoCorasickMatcher   -> (Aho-Corasick automaton, not SIMD)
Regex (not literal)   -> RegexMatcher         -> regexp.FindAllIndex (RE2, no SIMD)
PCRE flag             -> PCREMatcher          -> pcre.FindAllIndex (PCRE2, no SIMD)
```

The `BoyerMooreMatcher` is the primary consumer of SIMD acceleration.

### BoyerMooreMatcher: Search-then-Split

File: `internal/matcher/boyermoore.go`

The BoyerMooreMatcher uses a "search-then-split" strategy:

1. **Search the whole buffer**: Call `simd.IndexAll()` or `simd.IndexAllCaseInsensitive()` on the entire file content. This returns a list of byte offsets where the pattern occurs.
2. **Split into lines**: For each offset, extract the surrounding line boundaries and build a `Match` struct.

```go
func (m *BoyerMooreMatcher) FindAll(data []byte) MatchSet {
    if m.invert {
        return m.findAllInvert(data)
    }

    var offsets []int
    if m.ignoreCase {
        offsets = simd.IndexAllCaseInsensitive(data, m.patternLow)
    } else {
        offsets = simd.IndexAll(data, m.patternLow)
    }
    return matchSetFromOffsets(data, offsets, len(m.patternLow), m.maxCols, m.needLineNums)
}
```

This is more efficient than the alternative "split-then-search" approach (splitting the buffer into lines first, then searching each line), because:
- The SIMD search processes the buffer in large contiguous chunks, maximizing cache utilization and SIMD throughput.
- Line-splitting overhead is only incurred for lines that actually contain matches.
- For typical grep workloads (sparse matches), the vast majority of lines are skipped entirely.

The `matchSetFromOffsets` function in `internal/matcher/lineindex.go` converts raw byte offsets into structured `Match` objects with line numbers and highlight positions.

### Contrast: FixedMatcher (No SIMD)

File: `internal/matcher/fixed.go`

The `FixedMatcher` uses the split-then-search approach with `bytes.Index`. For case-insensitive mode, it calls `bytes.ToLower(line)` on every line, allocating a new lowercased copy. This allocation is the primary cost that the SIMD path avoids.

---

## 15. Stack Buffer Escape Avoidance in IndexAll

The `IndexAll` and `IndexAllCaseInsensitive` functions use a technique to avoid heap allocation in the common case of few or zero matches.

File: `internal/simd/index.go`

```go
var stackBuf [16]int
n := 0
var overflow []int

// ... for each match at position pos:
if n < len(stackBuf) {
    stackBuf[n] = pos
} else {
    if overflow == nil {
        overflow = make([]int, 0, 64)
        overflow = append(overflow, stackBuf[:]...)
    }
    overflow = append(overflow, pos)
}
n++

// At the end:
if n == 0 {
    return nil
}
if overflow != nil {
    return overflow
}
result := make([]int, n)
copy(result, stackBuf[:n])
return result
```

### How It Works

1. A fixed-size array `stackBuf [16]int` is declared on the stack (128 bytes on 64-bit systems).
2. The first 16 matches are stored in this stack buffer. Because `stackBuf` is a fixed-size array (not a slice), the Go compiler can prove it does not escape to the heap.
3. If there are more than 16 matches, a heap-allocated `overflow` slice is created and all results (including the first 16) are moved to it.
4. At the end, if all matches fit in `stackBuf`, a new slice is allocated and the results are copied from the stack buffer.

### Why This Matters

The most common case in grep workloads is **no match** (the pattern does not appear in the current file buffer). In this case:
- `n` remains 0
- `overflow` is never allocated
- The function returns `nil`
- **Zero heap allocations occurred**

The second most common case is **sparse matches** (fewer than 16 matches per file). In this case:
- Only the final `result := make([]int, n)` allocates
- The intermediate stack buffer avoided repeated `append` growth allocations

This optimization is particularly valuable because `IndexAll` is called once per file in the search-then-split architecture. Eliminating allocations on the no-match path means that scanning 37,000 files (a typical agrep workload) with most files not matching produces nearly zero GC pressure from the match collection code.

---

## 16. Performance Results and Lessons

### Benchmark Data

From the SIMD primitive benchmarks (`internal/simd/simd_test.go` and `internal/simd/index_test.go`):

**Single-byte search (`IndexByte`):**
```
BenchmarkIndexByte_SIMD:       ~equivalent to stdlib
BenchmarkIndexByte_Stdlib:     ~equivalent to SIMD
BenchmarkIndexByte_SIMD_Far:   ~equivalent to stdlib (worst case, match at end)
BenchmarkIndexByte_Stdlib_Far: ~equivalent to SIMD
```
Lesson: The Go stdlib already uses AVX2 assembly for `bytes.IndexByte`. Custom SIMD provides no advantage.

**Multi-byte search (`Index`):**
```
BenchmarkIndex_SIMD_Short:   ~equivalent to stdlib (delegates to bytes.Index)
BenchmarkIndex_Stdlib_Short: ~equivalent to SIMD
BenchmarkIndex_SIMD_NoMatch: ~equivalent to stdlib
```
Lesson: Same as above. `bytes.Index` is already highly optimized. agrep correctly delegates to it.

**Case-insensitive search (the SIMD win):**
```
BenchmarkIndexCaseInsensitive_SIMD: significantly faster than any non-SIMD approach
```
This is the primary payoff. There is no stdlib `IndexCaseInsensitive`.

### End-to-End Performance (from project benchmarks)

On a 440KB file with 10,000 lines:
```
NoMatch pattern:   9.7 GB/s throughput
SparseMatch:       7.5 GB/s throughput
DenseMatch:        ~190 MB/s (allocation-bound from per-line Match structs)
```

The NoMatch and SparseMatch numbers approach memory bandwidth. The SIMD code processes data faster than the memory subsystem can deliver it to the CPU in many configurations.

### End-to-End Against ripgrep (37K files)

```
agrep:  ~304ms (--no-ignore --hidden -l)
rg:      ~301ms (same flags)
```

Performance is essentially tied with ripgrep (written in Rust with hand-tuned SIMD via the `memchr` crate), demonstrating that Go + `archsimd` can match native SIMD performance.

For case-insensitive search:
```
agrep:  ~144ms (-i -l on /usr/include for "define")
rg:      ~180ms (same)
```

agrep is 1.25x faster for case-insensitive fixed-string search, which is the workload where the custom Horspool-style SIMD prefilter has the most impact.

### Summary of Lessons

1. **Do not reimplement what the stdlib already optimizes.** `bytes.IndexByte` and `bytes.Index` already use AVX2. Benchmark before writing custom SIMD.

2. **SIMD wins when the stdlib has no equivalent.** Case-insensitive substring search, batch ASCII case conversion, and custom prefilter strategies are where custom SIMD provides real value.

3. **The prefilter selectivity is everything.** The first+last byte Horspool prefilter eliminates >99.99% of candidate positions before any scalar verification occurs. The quality of the prefilter determines throughput.

4. **VZEROUPPER discipline is non-negotiable.** Missing a single `ClearAVXUpperBits()` call causes a silent ~70-cycle penalty that is extremely difficult to diagnose.

5. **Minimize allocations around SIMD code.** The stack buffer escape technique in `IndexAll` ensures that the no-match fast path (the most common case) is allocation-free.

6. **Search-then-split beats split-then-search.** Processing the entire buffer with SIMD before splitting into lines maximizes SIMD throughput and minimizes the overhead of line boundary detection.

---

## 17. Cross-references

- **`06-gc-and-allocation-optimization.md`**: Covers the stack buffer escape pattern used in `IndexAll` and `IndexAllCaseInsensitive`, the pointer-free `Match` struct design, and how `MatchSet` minimizes GC scanning overhead.

- **`04-string-search-algorithms.md`**: Covers how the SIMD Horspool prefilter fits into the broader landscape of string search algorithms (Boyer-Moore, Aho-Corasick, regex), and the search-then-split architecture in detail.

- **`07-benchmarking-and-profiling.md`**: Covers how to benchmark SIMD code correctly (`GOEXPERIMENT=simd`, `b.SetBytes`, `b.Loop()`), how to profile for AVX-SSE transition penalties, and how to compare against stdlib baselines.

---

## Appendix: File Index

All source files referenced in this document:

| File | Description |
|------|-------------|
| `internal/simd/simd.go` | IndexByte, LastIndexByte, Count, ToLowerASCII |
| `internal/simd/index.go` | Index, IndexAll, IndexCaseInsensitive, IndexAllCaseInsensitive, indexAllByte |
| `internal/simd/simd_test.go` | Benchmarks for single-byte operations |
| `internal/simd/index_test.go` | Benchmarks for multi-byte and case-insensitive operations |
| `internal/matcher/match.go` | Matcher interface, Match/MatchSet types |
| `internal/matcher/boyermoore.go` | BoyerMooreMatcher (primary SIMD consumer) |
| `internal/matcher/fixed.go` | FixedMatcher (non-SIMD baseline for comparison) |
| `internal/matcher/lineindex.go` | matchSetFromOffsets (search-then-split line extraction) |
| `internal/matcher/factory.go` | Matcher selection logic |

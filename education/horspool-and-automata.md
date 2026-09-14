# The Horspool Algorithm and Regex Automata

This article covers two foundational algorithms from first principles: the Boyer-Moore-Horspool algorithm for fixed-string search, and finite automata (NFA, DFA, lazy DFA) for regex matching. Both are central to how grep tools work. The goal is to build intuition for *why* these algorithms are fast, not just *what* they do -- covering the shift tables, state transitions, proofs of correctness, and the fundamental trade-offs that determine performance.

This article focuses on the algorithms themselves. For how agrep implements them with SIMD and AVX2, see [01: SIMD and AVX2](01-simd-and-avx2.md). For the code-level walkthrough of agrep's matchers, see [04: String Search Algorithms](04-string-search-algorithms.md). For competitive benchmarks against ripgrep and the prefilter cascade, see [08: Regex Engines and Prefilters](08-regex-engines-and-prefilters.md).

---

## Table of Contents

1. [Naive String Search and Its Cost](#naive-string-search-and-its-cost)
2. [Boyer-Moore: The Right-to-Left Insight](#boyer-moore-the-right-to-left-insight)
3. [The Bad Character Rule](#the-bad-character-rule)
4. [The Good Suffix Rule](#the-good-suffix-rule)
5. [Horspool: Simplifying Boyer-Moore](#horspool-simplifying-boyer-moore)
6. [Building the Horspool Shift Table](#building-the-horspool-shift-table)
7. [Horspool: Worked Examples](#horspool-worked-examples)
8. [Horspool Complexity Analysis](#horspool-complexity-analysis)
9. [From Horspool to SIMD Prefilter](#from-horspool-to-simd-prefilter)
10. [Regular Expressions and Languages](#regular-expressions-and-languages)
11. [Nondeterministic Finite Automata (NFA)](#nondeterministic-finite-automata-nfa)
12. [Thompson's Construction: Regex to NFA](#thompsons-construction-regex-to-nfa)
13. [NFA Simulation: The Thompson Algorithm](#nfa-simulation-the-thompson-algorithm)
14. [Deterministic Finite Automata (DFA)](#deterministic-finite-automata-dfa)
15. [Subset Construction: NFA to DFA](#subset-construction-nfa-to-dfa)
16. [The State Explosion Problem](#the-state-explosion-problem)
17. [Lazy DFA: On-Demand Construction](#lazy-dfa-on-demand-construction)
18. [Backtracking Engines: A Different Trade-off](#backtracking-engines-a-different-trade-off)
19. [Unanchored Search: The Invisible Loop](#unanchored-search-the-invisible-loop)
20. [Putting It All Together: Why Grep Is Hard](#putting-it-all-together-why-grep-is-hard)
21. [Cross-References](#cross-references)

---

## Naive String Search and Its Cost

The simplest approach to finding a pattern `P` of length `m` in a text `T` of length `n`: try every position.

```
NaiveSearch(T, P):
    for i = 0 to n - m:
        j = 0
        while j < m and T[i + j] == P[j]:
            j = j + 1
        if j == m:
            report match at position i
```

At each position `i`, compare `P[0..m-1]` with `T[i..i+m-1]` left to right, stopping on the first mismatch. Then advance `i` by 1 and try again.

**Best case**: O(n). If `P[0]` never appears in `T`, every attempt fails on the first comparison. Each position costs O(1), and there are O(n) positions.

**Worst case**: O(nm). Searching for `"aaa...ab"` (m-1 `a`s followed by `b`) in `"aaa...aaa"` (all `a`s). At every position, the algorithm matches m-1 characters before failing on the `b`. That's O(m) work per position, O(n) positions, O(nm) total.

**The key observation**: on a mismatch, the naive algorithm has already gathered information -- it knows which characters matched. But it throws that information away and advances by just 1 position. Better algorithms exploit mismatch information to skip ahead.

---

## Boyer-Moore: The Right-to-Left Insight

Boyer and Moore (1977) observed that comparing **right to left** within the pattern allows larger skips on mismatch.

Consider searching for `"EXAMPLE"` in text. When the algorithm aligns the pattern with a window of text:

```
Text:     ... H E R E _ I S _ A _ S I M P L E ...
Pattern:      E X A M P L E
Compare:                    ^  (start at rightmost character)
```

Compare `P[6] = 'E'` with `T[6] = 'E'`. Match. Move left.
Compare `P[5] = 'L'` with `T[5] = 'L'`. Match. Move left.
Compare `P[4] = 'P'` with `T[4] = 'P'`. Match. Move left.
Compare `P[3] = 'M'` with `T[3] = 'M'`. Match. ... and so on.

Now consider a mismatch case:

```
Text:     ... H E R E _ I S _ A _ S I M P L E ...
Pattern:      E X A M P L E
Compare:                    ^  Start at P[6]='E' vs T[6]='S'
```

Wait, let me use a clearer example:

```
Text:     ... S I M P L E _ S T R I N G ...
Pattern:      E X A M P L E
              0 1 2 3 4 5 6
```

Compare right to left. `P[6] = 'E'` vs `T[6] = '_'`. Mismatch.

Now the question is: how far can we safely shift the pattern to the right?

The character `'_'` (underscore) does not appear anywhere in the pattern `"EXAMPLE"`. This means that no alignment of the pattern with this window can produce a match at any position that would include this underscore. We can safely shift the pattern by its entire length (7 positions), jumping completely past the mismatch.

This is the power of right-to-left comparison: a single mismatch at the **rightmost** position can skip the entire pattern length. With left-to-right comparison, a mismatch at the first position only allows a 1-position advance.

---

## The Bad Character Rule

The **bad character rule** formalizes the skip logic for the mismatched character.

When comparing right to left, suppose we mismatch at pattern position `j`: `P[j] != T[i + j]`. The mismatched text character is `c = T[i + j]`.

**Case 1: `c` does not appear in `P`.**
No alignment of the pattern can match at any position where `c` would overlap with the pattern. Shift the pattern to align position 0 just past the mismatched character:

```
Shift = j + 1
```

**Case 2: `c` appears in `P`, with its rightmost occurrence at position `k` (where `k < j`).**
Shift the pattern to align `P[k]` with the mismatched position:

```
Shift = j - k
```

This ensures the text character `c` is aligned with a position in the pattern where `c` actually occurs, which is the earliest position that could possibly lead to a match.

**Case 3: `c` appears in `P` at position `k >= j`.**
The bad character rule suggests a zero or negative shift (we'd be shifting the pattern backward). In this case, the shift is 1 (minimum advance). This case is handled by the good suffix rule (next section) or simply clamped to 1.

### Precomputing the Bad Character Table

For each possible byte value `c` (0-255), precompute the rightmost position of `c` in the pattern:

```
BadChar(c):
    for j = m - 1 down to 0:
        if P[j] == c:
            return j
    return -1  (character not in pattern)
```

In practice, build a 256-entry lookup table in O(m) time:

```
BuildBadCharTable(P):
    table[0..255] = -1
    for j = 0 to m - 1:
        table[P[j]] = j
    return table
```

The shift on mismatch at position `j` with text character `c` is:

```
shift = max(1, j - table[c])
```

---

## The Good Suffix Rule

The **good suffix rule** handles the case where some suffix of the pattern matched before the mismatch occurred.

Suppose we matched `P[j+1..m-1]` (call this suffix `s`), then mismatched at `P[j]`. The good suffix rule asks: is there another occurrence of the suffix `s` in the pattern, preceded by a different character?

**Case 1: `s` appears elsewhere in `P`, preceded by a different character.**
Shift to align this other occurrence with the matched portion:

```
Text:     ... X A B C A B C Y ...
Pattern:      C A B C A B C
              0 1 2 3 4 5 6

Mismatch at j=0: P[0]='C' vs T[0]='X'. Matched suffix = "ABCABC"[1:] = "ABCABC"
Wait -- let me use the suffix that matched: P[1..6] = "ABCABC"
```

Actually, the good suffix rule is subtle enough that a smaller example is clearer:

```
Pattern:  A B C X X A B C
          0 1 2 3 4 5 6 7

Matched suffix: "ABC" (positions 5,6,7 matched)
Mismatch at position 4.

The suffix "ABC" also appears at positions 0,1,2.
The character before that occurrence (P[-1], i.e., nothing -- it's at the start) is different from P[4] = 'X'.

Shift: align positions 0,1,2 with where positions 5,6,7 were.
Shift amount = 5 (shift right by 5 positions).
```

**Case 2: No other occurrence of `s`, but a prefix of `P` matches a suffix of `s`.**
Shift to align that prefix:

```
Pattern:  B C A B C
          0 1 2 3 4

Matched suffix: "ABC" (positions 2,3,4)
No other "ABC" in pattern.
But prefix "BC" (positions 0,1) matches suffix of the matched portion "BC" (from "ABC").
Shift: align this prefix with the end of the text window.
Shift amount = 3.
```

**Case 3: Neither case applies.**
Shift by the full pattern length `m`.

### Why It's Rarely Implemented

The good suffix rule requires O(m) preprocessing to build a shift table (using the pattern's suffix structure, similar to KMP's failure function). The implementation is complex and error-prone. More importantly:

- For short patterns (typical in grep: 3-20 bytes), the bad character rule alone provides excellent skips.
- The good suffix rule provides additional benefit only when patterns have repeated internal structure, which is uncommon in grep usage.
- The simpler Horspool algorithm (next section) achieves nearly the same performance with much simpler code.

---

## Horspool: Simplifying Boyer-Moore

Horspool (1980) made a crucial simplification: instead of using the bad character rule at the *mismatch position*, always use the text character aligned with the *last position* of the pattern.

```
HorspoolSearch(T, P):
    m = len(P)
    n = len(T)
    Build shift table (see next section)

    i = 0
    while i <= n - m:
        j = m - 1
        while j >= 0 and P[j] == T[i + j]:
            j = j - 1
        if j < 0:
            report match at position i
            i = i + shift[T[i + m - 1]]  // shift using last-aligned character
        else:
            i = i + shift[T[i + m - 1]]  // same shift regardless of where mismatch occurred
```

The key difference from Boyer-Moore: the shift amount depends only on `T[i + m - 1]` (the text character currently aligned with the **last** pattern character), regardless of where the mismatch happened within the pattern.

This seems like it would lose information -- after all, Boyer-Moore's bad character rule uses the mismatch position to compute a larger shift in some cases. But in practice:

1. The character at the last position is the one we check first (right-to-left comparison), so on an immediate mismatch (most common case), both algorithms compute the same shift.

2. When a partial match occurs (some suffix matches before mismatch), Horspool's shift might be smaller than Boyer-Moore's. But partial matches are rare for typical search patterns and text.

3. The simpler control flow means fewer branches, better branch prediction, and better instruction-level parallelism. The constant-factor speedup from simpler code often outweighs the occasional extra skip from the good suffix rule.

---

## Building the Horspool Shift Table

The shift table maps each byte value to the distance the pattern should shift when that byte appears at the last-aligned position.

```
BuildHorspoolTable(P):
    m = len(P)
    shift[0..255] = m    // default: shift by full pattern length

    for j = 0 to m - 2:  // NOTE: excludes the last character
        shift[P[j]] = m - 1 - j

    return shift
```

**Why exclude the last character?** The table is indexed by the text character aligned with `P[m-1]`. If `P[m-1]` itself were included, the shift for that character would be 0 (it appears at position `m-1`, so shift = `m - 1 - (m-1)` = 0). A shift of 0 means no progress, causing an infinite loop. By excluding it, if the last character only appears at position `m-1` in the pattern, its shift defaults to `m` (full pattern length), which is correct: the only way this character aligns with the end of the pattern is the current position, which has already been checked.

**Exception**: If the last character also appears earlier in the pattern (at position `k < m-1`), the loop sets `shift[P[m-1]] = m - 1 - k` for the earlier occurrence. This is the correct smaller shift: there's another position in the pattern where this character could align.

### Example: Pattern "EXAMPLE"

```
Pattern:  E  X  A  M  P  L  E
Position: 0  1  2  3  4  5  6
Length m = 7

Initialize: shift[all] = 7

j=0: P[0]='E', shift['E'] = 7 - 1 - 0 = 6
j=1: P[1]='X', shift['X'] = 7 - 1 - 1 = 5
j=2: P[2]='A', shift['A'] = 7 - 1 - 2 = 4
j=3: P[3]='M', shift['M'] = 7 - 1 - 3 = 3
j=4: P[4]='P', shift['P'] = 7 - 1 - 4 = 2
j=5: P[5]='L', shift['L'] = 7 - 1 - 5 = 1
(j=6 is excluded: P[6]='E' is the last character)
```

Final table (non-default entries):

| Byte | Shift |
|---|---|
| 'E' | 6 |
| 'X' | 5 |
| 'A' | 4 |
| 'M' | 3 |
| 'P' | 2 |
| 'L' | 1 |
| all others | 7 |

Note that 'E' has shift 6 (from position 0), not 0 (from position 6, which was excluded). This means: if the text character aligned with the pattern's last position is 'E', the pattern should shift by 6 to align the first 'E' (at position 0) with this text position.

---

## Horspool: Worked Examples

### Example 1: No match, maximum skipping

```
Text:     T H I S _ I S _ A _ S I M P L E _ T E S T
Pattern:  E X A M P L E

Step 1: Align at position 0
Text:     T H I S _ I S _ A _ S I M P L E _ T E S T
Pattern:  E X A M P L E
                      ^  Check T[6]='S'. shift['S']=7. Advance by 7.

Step 2: Align at position 7
Text:     T H I S _ I S _ A _ S I M P L E _ T E S T
Pattern:          E X A M P L E
                              ^  Check T[13]='P'. shift['P']=2. Advance by 2.

Step 3: Align at position 9
Text:     T H I S _ I S _ A _ S I M P L E _ T E S T
Pattern:              E X A M P L E
                                  ^  Check T[15]='E'. shift['E']=6. Advance by 6.

Step 4: Align at position 15
Text:     T H I S _ I S _ A _ S I M P L E _ T E S T
Pattern:                          E X A M P L E
                                              ^  Check T[21]: past end. Done.
```

Total: 4 alignments, 4 character comparisons for a 21-character text with a 7-character pattern. That's roughly n/m = 21/7 = 3 iterations -- sublinear.

### Example 2: Match found

```
Text:     A N _ E X A M P L E _ H E R E
Pattern:  E X A M P L E

Step 1: Align at position 0
Text:     A N _ E X A M P L E _ H E R E
Pattern:  E X A M P L E
                      ^  Check T[6]='M'. shift['M']=3. Advance by 3.

Step 2: Align at position 3
Text:     A N _ E X A M P L E _ H E R E
Pattern:        E X A M P L E
                            ^  Check T[9]='E'. shift['E']=6.

But wait -- before shifting, we check right to left for a match:
T[9]='E' == P[6]='E'. Match. Continue left.
T[8]='L' == P[5]='L'. Match. Continue left.
T[7]='P' == P[4]='P'. Match. Continue left.
T[6]='M' == P[3]='M'. Match. Continue left.
T[5]='A' == P[2]='A'. Match. Continue left.
T[4]='X' == P[1]='X'. Match. Continue left.
T[3]='E' == P[0]='E'. Match!

Report match at position 3.
Advance by shift['E']=6. New position: 9.

Step 3: Align at position 9
Text:     A N _ E X A M P L E _ H E R E
Pattern:              E X A M P L E
                                  ^  Check T[15]: past end. Done.
```

Total: 3 alignments, ~10 comparisons. The match was found after examining only a fraction of the text.

### Example 3: Pattern with repeated characters

```
Pattern:  A B A B
Shift table:
  j=0: P[0]='A', shift['A'] = 4 - 1 - 0 = 3
  j=1: P[1]='B', shift['B'] = 4 - 1 - 1 = 2
  j=2: P[2]='A', shift['A'] = 4 - 1 - 2 = 1  (overwrites previous 3)
  (j=3 excluded)

  'A' -> 1, 'B' -> 2, others -> 4

Text:     C A B A B A C
Pattern:  A B A B
                ^  Check T[3]='A'. shift['A']=1. Advance by 1.

Pattern:    A B A B
                  ^  Check T[4]='B'. shift['B']=2.
Check right to left: T[4]='B'==P[3]='B'. T[3]='A'==P[2]='A'. T[2]='B'==P[1]='B'. T[1]='A'==P[0]='A'.
Match at position 1! Advance by 2.

Pattern:        A B A B
                      ^  Check T[6]='C'. shift['C']=4. Past end. Done.
```

With repeated characters in the pattern, the shift values are smaller (shift['A'] = 1 instead of 3), because the algorithm must be conservative: there could be a valid alignment just 1 position ahead. This is the pathological case for Horspool -- patterns with heavy character repetition produce small shifts and approach O(nm) behavior.

---

## Horspool Complexity Analysis

### Best Case: O(n/m)

When the text character at the last-aligned position never appears in the pattern, the shift is `m` (full pattern length). Each alignment examines exactly 1 character and advances by `m` positions. Total comparisons: `n/m`.

This happens when the pattern consists of characters that are rare in the text. For example, searching for a pattern containing 'Q' in English text -- 'Q' appears in less than 0.1% of characters, so almost every alignment will shift by the full pattern length.

### Average Case: O(n/m) for random text

For an alphabet of size `σ` (256 for bytes) and a pattern of length `m`, the expected shift when the last-aligned character is not the pattern's last character is:

```
E[shift] = Σ_{c != P[m-1]} (m / σ) + Σ_{c == P[m-1]} (shift_value / σ)
```

For random text, the probability that the last-aligned character appears nowhere in the pattern (triggering a full-length shift) is approximately `((σ - m) / σ)^1 ≈ 1 - m/σ`. For m=7 and σ=256, this is about 97%. So 97% of the time, we shift by the full pattern length, and the average shift is close to `m`.

Expected comparisons: approximately `n / E[shift] ≈ n/m`, which is sublinear -- we examine fewer characters than exist in the text.

### Worst Case: O(nm)

Pattern: `"aaaa"` (all same character). Text: `"aaaa...aaaa"` (all same character).

The shift table has `shift['a'] = 1` (the last character 'a' also appears at position `m-2`, so the shift is `m - 1 - (m-2) = 1`). Every alignment shifts by only 1 position. Each alignment requires `m` comparisons (the entire pattern matches or matches m-1 characters before failing). Total: O(nm).

This worst case requires both:
- A pattern where the last character appears in every position (or near-every position).
- A text where that character dominates.

In practice, this pathological combination is extremely rare for natural text and typical search patterns.

### Comparison with Boyer-Moore Full

| | Best | Average | Worst |
|---|---|---|---|
| Horspool | O(n/m) | O(n/m) | O(nm) |
| Boyer-Moore (full) | O(n/m) | O(n/m) | O(n) with good suffix |

Boyer-Moore's good suffix rule guarantees that the worst case is O(n) (or O(n + m) for preprocessing). But in practice, the O(nm) worst case of Horspool almost never occurs, and the simpler constant factors of Horspool make it faster for real-world grep workloads.

---

## From Horspool to SIMD Prefilter

Classical Horspool processes one text position at a time. agrep's SIMD prefilter adapts the core idea to process 32 positions simultaneously.

The connection: Horspool checks `T[i + m - 1]` (the last-aligned character) to decide whether position `i` is worth checking. This is a **two-byte probe** -- checking the first and last bytes of the potential match window.

The SIMD version does exactly this, but for 32 consecutive positions at once:

```
Classical Horspool:
    Check T[i + m - 1] against shift table.
    If shift > 0: skip.
    If shift == 0: verify full pattern at position i.

SIMD Prefilter:
    Load T[i..i+31]        (first bytes of 32 consecutive windows)
    Load T[i+m-1..i+m+30]  (last bytes of 32 consecutive windows)
    Compare all 32 first bytes against P[0].
    Compare all 32 last bytes against P[m-1].
    AND the results: positions where both match are candidates.
    Verify full pattern only at candidate positions.
```

The classical Horspool shift table is replaced by a simpler first+last byte check. This loses the variable-shift optimization (Horspool can shift by more than 1 when the last byte matches but at a non-terminal position), but gains massive parallelism: 32 positions checked per SIMD iteration instead of 1.

For a pattern where the first byte appears in ~1/256 of text bytes and the last byte appears in ~1/256, the combined probability of both matching is ~1/65536. Out of 32 positions per iteration, the expected number of candidates is 32/65536 ≈ 0.0005. In other words, almost every SIMD iteration produces zero candidates, and the algorithm advances by 32 positions with just a few SIMD instructions.

This is why the SIMD first+last byte prefilter achieves throughputs of 10+ GB/s -- it processes 32 bytes of text per CPU cycle in the scanning loop, and the verification function (which is O(m) per candidate) is called so rarely that it's effectively free.

See [01: SIMD and AVX2, Section 10](01-simd-and-avx2.md) for the full AVX2 implementation with `archsimd` intrinsics.

---

## Regular Expressions and Languages

A regular expression defines a **language** -- a (possibly infinite) set of strings. The expression `a(b|c)*d` defines the set:

```
{ad, abd, acd, abbd, abcd, acbd, accd, abbbd, ...}
```

These are all strings that start with `a`, end with `d`, and have zero or more `b`s or `c`s in between.

### The Three Core Operations

Every regular expression is built from three operations applied to characters:

1. **Concatenation**: `ab` matches `"a"` followed by `"b"`. The language is `{"ab"}`.

2. **Alternation**: `a|b` matches either `"a"` or `"b"`. The language is `{"a", "b"}`.

3. **Kleene star**: `a*` matches zero or more `"a"`s. The language is `{"", "a", "aa", "aaa", ...}`.

Every standard regex feature can be desugared into these three primitives plus the empty string `ε`:

| Syntax | Desugaring |
|---|---|
| `a+` | `a(a*)` |
| `a?` | `(a\|ε)` |
| `a{3}` | `aaa` |
| `a{2,4}` | `aa(a\|ε)(a\|ε)` |
| `[abc]` | `(a\|b\|c)` |
| `.` | `(any_char_0\|any_char_1\|...\|any_char_255)` |

This means any regex, no matter how complex, can be reduced to concatenation, alternation, and Kleene star. This is the foundation for converting regex to automata.

### Character Classes

In practice, character classes like `[a-z]` and `.` are handled specially rather than desugared into massive alternations. The automaton representation uses **character class transitions** -- a single transition labeled with a set of characters, rather than 256 individual transitions. This is an optimization, not a fundamental change to the model.

---

## Nondeterministic Finite Automata (NFA)

An NFA is a state machine defined by:

- A finite set of **states** `Q`
- A start state `q0 ∈ Q`
- A set of **accept states** `F ⊆ Q`
- A **transition function** `δ(q, c) → {set of states}` -- given a state and an input character, returns zero or more next states
- **Epsilon transitions** `ε(q) → {set of states}` -- transitions that consume no input

The "nondeterministic" means that from a given state on a given input, there can be **multiple** possible next states. The machine "guesses" the right path -- formally, it accepts if **any** possible path through the states leads to an accept state.

### Example: NFA for `a(b|c)*d`

```
        ε          ε
(q0) -----> (q1) -----> (q2)
  |          |    ε       |
  |  a       | b or c    | d
  v          v    ε       v
(q3) -----> (q4) -----> (q5 accept)
        ε          ε

Actually, let me draw this more carefully:

      a           ε          b          ε          d
(q0) ----> (q1) ----> (q2) ----> (q3) ----> (q1) ----> (q5 accept)
                 |                           ^
                 |           c               |
                 +----> (q4) -----> (q5) ----+
                               ε         ε

Hmm, let me just draw the states plainly:
```

Let me use a cleaner representation:

```
States: q0, q1, q2, q3 (accept)

Transitions:
  q0 --a--> q1
  q1 --b--> q1
  q1 --c--> q1
  q1 --d--> q2 (not accept -- wait, wrong)
```

Actually, for `a(b|c)*d` the simplest NFA:

```
  q0 --a--> q1 --d--> q2 (accept)
              |  ^
              b  |
              c  |
              +--+
              (self-loop)
```

States: {q0, q1, q2}
Start: q0
Accept: {q2}
Transitions:
- δ(q0, 'a') = {q1}
- δ(q1, 'b') = {q1}
- δ(q1, 'c') = {q1}
- δ(q1, 'd') = {q2}
- All other transitions = {} (empty set, meaning the machine "dies")

This NFA has no epsilon transitions because the regex is simple enough. Thompson's construction (next section) produces NFAs with epsilon transitions for more complex patterns.

### Processing Input: "abcd"

```
Step 0: Current states = {q0}
Read 'a': δ(q0, 'a') = {q1}. New states = {q1}.
Read 'b': δ(q1, 'b') = {q1}. New states = {q1}.
Read 'c': δ(q1, 'c') = {q1}. New states = {q1}.
Read 'd': δ(q1, 'd') = {q2}. New states = {q2}.

q2 is an accept state. Match!
```

### Processing Input: "abd"

```
Step 0: Current states = {q0}
Read 'a': δ(q0, 'a') = {q1}. New states = {q1}.
Read 'b': δ(q1, 'b') = {q1}. New states = {q1}.
Read 'd': δ(q1, 'd') = {q2}. New states = {q2}.

Accept!
```

### Processing Input: "aed"

```
Step 0: Current states = {q0}
Read 'a': δ(q0, 'a') = {q1}. New states = {q1}.
Read 'e': δ(q1, 'e') = {}. New states = {}.

No active states. Reject.
```

---

## Thompson's Construction: Regex to NFA

Ken Thompson (1968) gave an elegant algorithm for converting any regular expression into an NFA. The construction is recursive, following the structure of the regex.

### Base Case: Single Character

For the regex `a`:

```
(start) --a--> (accept)
```

Two states, one transition. The start state has one outgoing edge labeled `a` to the accept state.

### Concatenation: `rs`

Given NFAs for `r` (with start `r0`, accept `ra`) and `s` (with start `s0`, accept `sa`):

```
(r0) --...--> (ra) --ε--> (s0) --...--> (sa)
```

Connect `r`'s accept state to `s`'s start state with an epsilon transition. The new start state is `r0` and the new accept state is `sa`. This forces the machine to match `r` first, then `s`.

### Alternation: `r|s`

```
          ε --> (r0) --...--> (ra) --ε
         /                            \
(start) -                              -> (accept)
         \                            /
          ε --> (s0) --...--> (sa) --ε
```

Create a new start state that branches to both `r`'s and `s`'s start states via epsilon transitions. Both accept states connect to a new shared accept state via epsilon transitions. The machine nondeterministically "chooses" which branch to take.

### Kleene Star: `r*`

```
          ε
(start) ------> (accept)
    |                ^
    | ε              | ε
    v                |
   (r0) --...--> (ra)
         ^         |
         |   ε     |
         +---------+
```

- Epsilon from start to accept (matches zero repetitions).
- Epsilon from start to `r0` (enters the repetition).
- Epsilon from `ra` back to `r0` (loops for more repetitions).
- Epsilon from `ra` to accept (exits after one or more repetitions).

### Properties of Thompson NFAs

1. **Exactly 2 states per operator**: Each step adds at most 2 states. A regex of length `m` produces an NFA with at most `2m` states.

2. **At most 2 outgoing edges per state**: Each state has at most one labeled transition and at most two epsilon transitions. This bounded fan-out keeps the NFA compact.

3. **No cycles of epsilon transitions**: Well, the Kleene star introduces epsilon cycles (`ra → r0`), but these are controlled by the structure and don't cause infinite loops in simulation.

4. **Single accept state**: The construction always produces exactly one accept state, simplifying composition.

---

## NFA Simulation: The Thompson Algorithm

Given a Thompson NFA and an input string, we can determine if the string matches by tracking the **set of all possible states** the NFA could be in at each step.

```
ThompsonSimulate(nfa, input):
    current = epsilon_closure({nfa.start})  // all states reachable from start via ε

    for each character c in input:
        next = {}
        for each state q in current:
            next = next ∪ δ(q, c)       // follow character transitions
        current = epsilon_closure(next)  // follow ε transitions from new states

    return (current ∩ nfa.accept_states) ≠ {}  // any accept state reached?
```

### Epsilon Closure

The **epsilon closure** of a set of states `S` is the set of all states reachable from any state in `S` by following zero or more epsilon transitions:

```
epsilon_closure(S):
    result = S
    stack = S.to_list()
    while stack is not empty:
        q = stack.pop()
        for each state r in ε(q):
            if r not in result:
                result.add(r)
                stack.push(r)
    return result
```

This is a simple graph reachability computation (BFS or DFS).

### Example: `a(b|c)*d` on Input "abcd"

Using the NFA from earlier (no epsilon transitions needed for this simple case):

```
States: {q0, q1, q2}

Step 0: current = ε_closure({q0}) = {q0}
Read 'a': next = δ(q0,'a') = {q1}. current = ε_closure({q1}) = {q1}.
Read 'b': next = δ(q1,'b') = {q1}. current = ε_closure({q1}) = {q1}.
Read 'c': next = δ(q1,'c') = {q1}. current = ε_closure({q1}) = {q1}.
Read 'd': next = δ(q1,'d') = {q2}. current = ε_closure({q2}) = {q2}.

q2 ∈ accept_states. Match!
```

### Complexity

At each input character, we iterate over every state in `current` and compute transitions. The maximum size of `current` is `|Q|` (the number of NFA states). Computing transitions and epsilon closure is at most O(|Q|) work per step (with appropriate data structures).

**Total time: O(n * |Q|)** where n is the input length and |Q| is the number of NFA states. For a Thompson NFA from a regex of length m, |Q| ≤ 2m, so the time is **O(n * m)**.

**Space: O(|Q|)** for the current state set.

This is the algorithm Go's `regexp` package implements. Its key guarantee: **linear time in the input length** (for a fixed pattern). No pathological input can cause exponential blowup.

---

## Deterministic Finite Automata (DFA)

A DFA is like an NFA but with a crucial restriction: from any state, on any input character, there is **exactly one** next state. No nondeterminism, no epsilon transitions.

Formally:
- Transition function `δ(q, c) → q'` returns a single state (not a set).
- No epsilon transitions.

### The Speed Advantage

Processing one input character in a DFA requires exactly one operation: look up `δ(current_state, input_char)` in a table. If the table is an array indexed by `(state, char)`, this is a single memory load.

```
DFAMatch(dfa, input):
    state = dfa.start
    for each character c in input:
        state = dfa.transition[state][c]  // single array lookup
    return state ∈ dfa.accept_states
```

**Time per character: O(1).** No iteration over states, no epsilon closures. Just one table lookup.

**Total time: O(n)** for input of length n.

Compare with NFA simulation: O(n * m) where m is the pattern length. For a pattern of length 20, the DFA is 20x faster per character.

### Example: DFA for `a(b|c)*d`

```
States: {S0, S1, S2, DEAD}
Start: S0
Accept: {S2}

Transition table:
         a     b     c     d     other
S0:      S1    DEAD  DEAD  DEAD  DEAD
S1:      DEAD  S1    S1    S2    DEAD
S2:      DEAD  DEAD  DEAD  DEAD  DEAD
DEAD:    DEAD  DEAD  DEAD  DEAD  DEAD
```

Every state has exactly one transition per character. The DEAD state is a sink -- once the automaton enters it, it stays there permanently.

Processing "abcd":
```
state = S0
'a': transition[S0]['a'] = S1
'b': transition[S1]['b'] = S1
'c': transition[S1]['c'] = S1
'd': transition[S1]['d'] = S2

S2 is accept. Match!
```

Four table lookups. That's it.

---

## Subset Construction: NFA to DFA

The **subset construction** (also called the **powerset construction**) converts any NFA to an equivalent DFA. The key idea: each DFA state represents a **set of NFA states**.

```
SubsetConstruction(nfa):
    dfa_start = epsilon_closure({nfa.start})
    queue = [dfa_start]
    dfa_states = {dfa_start}
    dfa_transitions = {}

    while queue is not empty:
        S = queue.pop()              // S is a set of NFA states
        for each character c:
            T = {}
            for each state q in S:
                T = T ∪ nfa.δ(q, c)
            T = epsilon_closure(T)

            if T not in dfa_states:
                dfa_states.add(T)
                queue.append(T)

            dfa_transitions[S][c] = T

    dfa_accept = {S ∈ dfa_states : S ∩ nfa.accept_states ≠ {}}
    return DFA(dfa_start, dfa_accept, dfa_transitions)
```

Each DFA state is a subset of NFA states. The DFA transition from set `S` on character `c` leads to the set of all NFA states reachable from any state in `S` on `c`, followed by epsilon closure.

### Why It's Correct

The DFA simulates the NFA's behavior. At any point during input processing, the DFA's current state (a set of NFA states) is exactly the set of NFA states the NFA simulation would have in its `current` set. Since the DFA transition function is deterministic (the next state set is uniquely determined by the current state set and the input character), the DFA is deterministic.

### Example: NFA to DFA for `(a|b)*abb`

This is a classic textbook example. The NFA (from Thompson's construction) has states numbered 0-10.

Rather than trace the full construction (which produces 5 DFA states), the key observation is:

- NFA start with epsilon closure: {0, 1, 2, 4, 7} -- call this DFA state A.
- On input 'a' from A: reach NFA states {1, 2, 3, 4, 6, 7, 8} -- DFA state B.
- On input 'b' from A: reach NFA states {1, 2, 4, 5, 6, 7} -- DFA state C.
- On input 'b' from B: reach NFA states {1, 2, 4, 5, 6, 7, 9} -- DFA state D.
- On input 'b' from D: reach NFA states {1, 2, 4, 5, 6, 7, 10} -- DFA state E (accept, since 10 is NFA's accept).

The DFA has 5 states where the NFA has 11. Each DFA state encodes the full "knowledge" of which NFA states are possible.

---

## The State Explosion Problem

The subset construction can produce a DFA with **exponentially more states** than the NFA. An NFA with `|Q|` states can produce a DFA with up to `2^|Q|` states (one for each possible subset of NFA states).

### The Classic Example

The regex `.*a.{n}` ("match `a` followed by exactly `n` characters, with anything before"):

- The NFA has about `n + 3` states.
- The DFA must "remember" whether it saw `a` in each of the last `n+1` positions. This requires `2^(n+1)` DFA states, because each combination of "saw `a` k positions ago" is a distinct DFA state.

For `n = 20`, the NFA has ~23 states, but the DFA has over 2 million states. The transition table would consume `2^21 * 256 * sizeof(state_id) ≈ 4 GB`.

### Practical Impact

Most real-world regex patterns produce manageable DFA sizes. Simple patterns like `error|warning|fatal` produce a DFA with roughly as many states as the total pattern length. Character classes like `[a-z]+` add minimal states.

Pathological patterns include:
- `.{n}a` for large `n` (forces the DFA to track `a`'s position)
- `(a|b){20}(a|b){20}` (combinatorial explosion in the intersection)
- Complex lookahead patterns (not applicable to NFAs, but relevant to PCRE)

### Why It Matters for Grep

A grep tool cannot afford to build a 4GB DFA for a user's regex. This is why:
- Go's `regexp` uses NFA simulation (no DFA construction, no explosion risk).
- Rust's `regex` uses a **lazy DFA** (next section) that avoids building the full DFA.
- PCRE uses backtracking (no DFA at all, but risks exponential time instead of exponential space).

---

## Lazy DFA: On-Demand Construction

The lazy DFA (also called **on-demand DFA** or **JIT DFA**) combines the speed of DFA matching with the bounded memory of NFA simulation.

### The Idea

Instead of building the entire DFA transition table upfront (which might require exponential space), build transitions **as they are needed** during matching. Cache the computed transitions for reuse.

```
LazyDFAMatch(nfa, input):
    cache = {}
    start = freeze(epsilon_closure({nfa.start}))  // canonical representation
    state = start
    cache[start] = {}

    for each character c in input:
        if c in cache[state]:
            state = cache[state][c]        // cache hit: O(1)
        else:
            // cache miss: compute transition via NFA simulation
            next_nfa_states = {}
            for each q in unfreeze(state):
                next_nfa_states = next_nfa_states ∪ nfa.δ(q, c)
            next = freeze(epsilon_closure(next_nfa_states))

            cache[state][c] = next         // store for reuse
            if len(cache) > MAX_CACHE:
                cache.clear()              // flush and restart
                cache[start] = {}

            state = next

    return any state in unfreeze(state) is an accept state
```

### Key Properties

**Cache hits are O(1)**: Just a hash table lookup. With a good hash function and small state representations, this approaches the speed of a precomputed DFA transition table.

**Cache misses are O(|Q|)**: The NFA simulation runs for one step. This is the same cost as one step of Thompson's algorithm. But cache misses are rare after the first few hundred input bytes.

**Bounded memory**: The cache has a maximum size. When it fills up, it is flushed entirely. This means the lazy DFA never uses more than `MAX_CACHE * 256 * sizeof(state)` memory, regardless of how complex the regex is.

**Steady state**: For typical inputs, the lazy DFA reaches a "steady state" after processing a small fraction of the input. In this steady state, all transitions are cached and every character is processed in O(1). The cache needs at most as many entries as there are distinct (state, character) pairs actually encountered in the input -- typically far fewer than the full DFA.

### Why It's Faster Than Pure NFA

Consider matching `[a-z]{3}` against English text:

- **NFA simulation**: At each byte, iterate over all active NFA states (up to 4 states for this pattern) and compute transitions. That's ~4 hash lookups per byte.

- **Lazy DFA**: After processing a few hundred bytes, the cache contains all transitions between the ~10 distinct DFA states that arise in English text. Each byte requires 1 hash lookup.

The lazy DFA is 4x faster per byte in this case. For more complex patterns with more NFA states, the advantage grows.

### Why It's Slower Than Full DFA

A precomputed DFA uses a flat array for transitions: `transition[state * 256 + c]`. This is a single array index -- no hashing, no hash collision handling, no cache-miss checks. The lazy DFA uses a hash table, which is ~2-5x slower per lookup than a flat array.

For patterns that produce small DFAs (< 1000 states), it would be faster to build the full DFA once. But the lazy DFA is a safe default because it works for all patterns, including those that would produce millions of DFA states.

---

## Backtracking Engines: A Different Trade-off

PCRE, Perl, Python, Java, and most scripting languages use a **backtracking** regex engine. This is a fundamentally different approach from NFA simulation.

### How Backtracking Works

The engine tries to match the regex at each position by exploring one choice at a time. When it reaches a dead end, it **backtracks** -- undoes the last choice and tries the next alternative.

```
BacktrackMatch(regex, input, pos):
    if regex is empty:
        return true (match)

    if regex is "a":
        return input[pos] == 'a' and advance pos

    if regex is "r|s":
        save checkpoint
        if BacktrackMatch(r, input, pos):
            return true
        restore checkpoint
        return BacktrackMatch(s, input, pos)

    if regex is "r*":
        save checkpoint
        // Try matching r, then recursing
        if BacktrackMatch(r, input, pos):
            if BacktrackMatch(r*, input, new_pos):
                return true
        restore checkpoint
        // Try matching zero times
        return true

    if regex is "rs":
        if BacktrackMatch(r, input, pos):
            return BacktrackMatch(s, input, new_pos)
        return false
```

### Why Backtracking Exists

Backtracking engines support features that cannot be expressed as finite automata:

- **Backreferences**: `(a+)\1` matches `"aa"`, `"aaaa"`, `"aaaaaa"` (the second half must equal the first). This requires remembering what the first group captured -- impossible for a finite automaton (it would need infinite states to remember arbitrary-length captures).

- **Lookahead/lookbehind**: `(?=foo)bar` or `(?<=foo)bar`. These require the engine to "peek" at surrounding context without consuming input.

- **Atomic groups / possessive quantifiers**: `(?>a+)` matches greedily and refuses to backtrack. These affect the backtracking strategy.

None of these features can be implemented by NFA/DFA simulation. This is why PCRE and Perl use backtracking.

### The Catastrophic Backtracking Problem

The backtracking engine explores one path at a time. For the regex `(a+)+b` on the input `"aaaaaaaaaaac"`:

1. The outer `(a+)+` tries to match all `a`s in one group: `"aaaaaaaaaaaa"`. Then `b` fails against `c`.
2. Backtrack: try the outer `+` matching twice: `"aaaaaaaaaaa"` + `"a"`. Then `b` fails.
3. Backtrack: try `"aaaaaaaaaa"` + `"aa"`. Fail.
4. Backtrack: try `"aaaaaaaaaa"` + `"a"` + `"a"`. Fail.
5. ... and so on.

The number of ways to partition `n` `a`s into groups of `a+` is exponential (it's related to integer compositions). For `n = 30`, the backtracking engine must explore over a billion paths before concluding there's no match. This is **catastrophic backtracking** -- the engine runs in O(2^n) time.

The Thompson NFA simulation handles this in O(n * m) time. It tracks all possible states simultaneously, so it never needs to backtrack. This is the fundamental advantage of the NFA approach.

### The Trade-off

| | NFA/DFA | Backtracking |
|---|---|---|
| Time guarantee | O(n * m) or O(n) | O(2^n) worst case |
| Backreferences | Not supported | Supported |
| Lookahead/behind | Not supported | Supported |
| Capture groups | Supported (with extra bookkeeping) | Naturally supported |
| Implementation complexity | Moderate | Simple |

Go's `regexp` package chooses NFA simulation (guaranteed linear time, no backreferences). PCRE chooses backtracking (full feature set, exponential risk). Rust's `regex` uses a hybrid: lazy DFA for the main match, with a backtracker fallback for capture group extraction.

---

## Unanchored Search: The Invisible Loop

In grep, the regex is searched for **anywhere** in each line. The regex `error` should match the line `"fatal error occurred"` even though `error` doesn't start at position 0. This is called **unanchored search**.

Implementing unanchored search naively adds an outer loop:

```
for each position i in input:
    if regex matches input starting at position i:
        report match
```

This is O(n) attempts, each of which takes O(n * m) in the worst case (NFA simulation), for O(n^2 * m) total. That's terrible.

### The Standard Trick: Prepend `.*`

The standard approach is to modify the regex from `r` to `.*r`. The `.*` prefix matches zero or more arbitrary characters, allowing the match to start at any position. The NFA simulation handles this implicitly:

```
Original NFA for "error":
  q0 --e--> q1 --r--> q2 --r--> q3 --o--> q4 --r--> q5 (accept)

Modified NFA for ".*error" (with unanchored prefix):
  q_start --any--> q_start    (self-loop on any character)
  q_start --ε--> q0           (enter the actual pattern)
  q0 --e--> q1 --r--> q2 --r--> q3 --o--> q4 --r--> q5 (accept)
```

Now the NFA simulation always keeps `q_start` in its state set (because it can always loop on any character). At every position, the simulation also includes `q0` (via the epsilon transition). So the pattern matching begins "speculatively" at every position, all within a single left-to-right pass.

### Complexity

With the `.*` prefix, the NFA has `|Q| + 1` states and the simulation runs in O(n * |Q|) -- the same as without the prefix, just with one extra state in the set. There's no outer loop, no quadratic blowup.

This is how Go's `regexp` handles unanchored search: the compiled NFA always has an implicit `.*` prefix. Every call to `Match()` or `FindAllIndex()` uses this modified NFA.

### The DFA Equivalent

For a DFA, the `.*` prefix means the start state has self-loops on all characters (it stays in the start state while consuming non-matching characters). The DFA effectively "tries" to start the match at every position, all within a single pass. The same O(n) guarantee applies.

---

## Putting It All Together: Why Grep Is Hard

Building a fast grep tool requires navigating several interconnected trade-offs:

### The Fixed-String Fast Path

For literal patterns (no metacharacters), Horspool or SIMD prefilters achieve throughputs of 10+ GB/s. The algorithm is simple, the implementation is cache-friendly, and SIMD allows 32 bytes per cycle.

agrep exploits this: the `isLiteral()` check in `factory.go` routes literal patterns to `BoyerMooreMatcher`, bypassing the regex engine entirely. This is why `agrep "define"` runs at SIMD speed.

### The Regex Slow Path

For patterns with metacharacters, the engine must build an automaton. The choice of automaton type determines performance:

- **Thompson NFA** (Go's `regexp`): O(n * m) per file. Safe, predictable, but slow. Every byte of every file goes through the simulation.

- **Lazy DFA** (Rust's `regex`): O(n) per file after warmup. Fast in steady state, bounded memory. The dominant engine in ripgrep.

- **Backtracking** (`go.elara.ws/pcre`): Unpredictable performance. Fast on simple patterns, catastrophically slow on pathological ones. Supports features NFA/DFA cannot.

### The Prefilter Bridge

The 16x gap between agrep and ripgrep on regex patterns (documented in [08: Regex Engines and Prefilters](08-regex-engines-and-prefilters.md)) comes from ripgrep using SIMD prefilters to skip non-matching regions before engaging the automaton. This bridges the fixed-string and regex worlds:

1. Extract a required literal from the regex (e.g., `"err"` from `err(or|no|code)`).
2. Search for the literal at SIMD speed (10+ GB/s).
3. Only run the automaton at candidate positions (~0.1% of the input).

The effective throughput approaches the SIMD prefilter speed, because the automaton runs on so little of the input.

### The Algorithm Hierarchy

Putting all the algorithms in context:

```
Speed        Algorithm                    Used when
-----        ---------                    ---------
~30 GB/s     SIMD memchr                  Single-byte fixed pattern
~10 GB/s     SIMD Horspool/memmem         Multi-byte fixed pattern
~10 GB/s     SIMD prefilter + lazy DFA    Regex with extractable literal (ripgrep)
~2 GB/s      Lazy DFA (no prefilter)      Regex, no extractable literal (ripgrep)
~0.7 GB/s    Aho-Corasick automaton       Multi-pattern fixed strings
~0.5 GB/s    Thompson NFA simulation      All regex (Go's regexp)
~0.1-0.5 GB/s  PCRE backtracking          Complex regex with backreferences
```

Each step down in the hierarchy represents roughly a 2-10x slowdown. The art of grep engineering is routing each pattern to the highest tier that can handle it.

---

## Cross-References

- [01: SIMD and AVX2](01-simd-and-avx2.md) -- The AVX2 implementation of the Horspool-style first+last byte prefilter discussed in "From Horspool to SIMD Prefilter." Also covers `BroadcastUint8x32`, `VPCMPEQB`, `VPMOVMSKB`, and Kernighan's trick for bitmask iteration.
- [04: String Search Algorithms](04-string-search-algorithms.md) -- Code-level walkthrough of agrep's `BoyerMooreMatcher`, `AhoCorasickMatcher`, `RegexMatcher`, and `PCREMatcher`, including the search-then-split architecture and the `Matcher` interface.
- [06: GC and Allocation Optimization](06-gc-and-allocation-optimization.md) -- How agrep's pointer-free `Match` struct and `[][2]int` positions eliminate GC pressure in the matcher hot path.
- [08: Regex Engines and Prefilters](08-regex-engines-and-prefilters.md) -- Competitive analysis of agrep vs ripgrep, the Teddy SIMD multi-pattern algorithm, literal extraction from regex ASTs, and the prefilter cascade. Builds on the automata theory in this article to explain the benchmark results.

### Foundational Papers

- **R.S. Boyer, J.S. Moore, "A Fast String Searching Algorithm"** (1977) -- The original Boyer-Moore algorithm with both the bad character and good suffix rules.
- **R.N. Horspool, "Practical Fast Searching in Strings"** (1980) -- The simplification that uses only the last-aligned character for shifting.
- **K. Thompson, "Regular Expression Search Algorithm"** (1968) -- Thompson's construction and the NFA simulation algorithm. Originally described in the context of the QED text editor.
- **R. Cox, "Regular Expression Matching Can Be Simple And Fast"** (2007) -- Russ Cox's article explaining why Thompson's algorithm is superior to backtracking for guaranteed-linear-time matching. Directly motivated Go's `regexp` design.
- **A.V. Aho, M.J. Corasick, "Efficient String Matching: An Aid to Bibliographic Search"** (1975) -- The Aho-Corasick algorithm, combining trie structure with KMP-style failure links.
- **M.O. Rabin, D. Scott, "Finite Automata and Their Decision Problems"** (1959) -- The foundational paper on NFA-to-DFA subset construction and the decidability of finite automata equivalence.

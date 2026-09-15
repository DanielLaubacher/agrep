package matcher

import "bytes"

// PipelineMatcher chains multiple matchers where each stage filters lines
// that matched in the previous stage. The final stage's match positions
// determine the output highlights.
//
// Semantics:
//   - Stage 0 searches the full buffer: lines matching become candidates.
//   - Each subsequent stage (-t) re-matches candidate lines. A line must
//     match ALL stages to survive.
//   - The final stage's match positions are used for highlighting/output.
//   - -o on the final stage means "output only matched text."
type PipelineMatcher struct {
	stages    []Matcher
	onlyMatch bool // whether the final stage has -o (only-matched output)
}

// NewPipelineMatcher creates a pipeline matcher from ordered stages.
func NewPipelineMatcher(stages []Matcher, onlyMatch bool) *PipelineMatcher {
	return &PipelineMatcher{
		stages:    stages,
		onlyMatch: onlyMatch,
	}
}

// OnlyMatch returns true if the pipeline's final stage uses -o.
func (p *PipelineMatcher) OnlyMatch() bool {
	return p.onlyMatch
}

func (p *PipelineMatcher) FindAll(data []byte) MatchSet {
	if len(p.stages) == 0 {
		return MatchSet{}
	}

	// Stage 0: search the whole buffer
	ms := p.stages[0].FindAll(data)
	if !ms.HasMatch() {
		return MatchSet{}
	}

	// Single stage: return directly
	if len(p.stages) == 1 {
		return ms
	}

	// Multi-stage: filter lines through subsequent stages
	// Extract candidate lines from stage 0, then re-match with remaining stages.
	return p.filterAndRematch(data, ms)
}

// narrowLine runs stages 1..N over one line, each stage searching only
// within the previous stage's matched fragments (stage 0's fragment is
// the whole line). Returns the final stage's fragments as line-relative
// positions, or nil if any stage found nothing — the staged-narrowing
// contract from the skill text: "-t pipes each stage's matched text into
// the next pattern."
func (p *PipelineMatcher) narrowLine(lineBytes []byte) [][2]int {
	regions := [][2]int{{0, len(lineBytes)}}
	for stageIdx := 1; stageIdx < len(p.stages); stageIdx++ {
		var next [][2]int
		for _, r := range regions {
			sub := lineBytes[r[0]:r[1]]
			sms := p.stages[stageIdx].FindAll(sub)
			for j := range sms.Matches {
				sm := &sms.Matches[j]
				if sm.IsContext || sm.LineStart < 0 {
					continue
				}
				base := r[0] + sm.LineStart
				for _, pos := range sms.MatchPositions(j) {
					next = append(next, [2]int{base + pos[0], base + pos[1]})
				}
			}
		}
		if len(next) == 0 {
			return nil
		}
		regions = next
	}
	return regions
}

// filterAndRematch takes the lines matched by stage 0 and narrows each
// through stages 1..N. The final stage's fragments define the output
// highlights (and the emitted text under -o).
func (p *PipelineMatcher) filterAndRematch(data []byte, ms0 MatchSet) MatchSet {
	var resultMatches []Match
	var resultPositions [][2]int

	for i := range ms0.Matches {
		m := &ms0.Matches[i]
		if m.IsContext || m.LineStart < 0 {
			continue
		}

		lineBytes := ms0.Data[m.LineStart : m.LineStart+m.LineLen]
		regions := p.narrowLine(lineBytes)
		if len(regions) == 0 {
			continue
		}

		posIdx := len(resultPositions)
		resultPositions = append(resultPositions, regions...)
		resultMatches = append(resultMatches, Match{
			LineNum:    m.LineNum,
			LineStart:  m.LineStart,
			LineLen:    m.LineLen,
			ByteOffset: m.ByteOffset,
			PosIdx:     posIdx,
			PosCount:   len(regions),
		})
	}

	if len(resultMatches) == 0 {
		return MatchSet{}
	}

	return MatchSet{
		Data:      data,
		Matches:   resultMatches,
		Positions: resultPositions,
	}
}

func (p *PipelineMatcher) MatchExists(data []byte) bool {
	if len(p.stages) == 0 {
		return false
	}

	if len(p.stages) == 1 {
		return p.stages[0].MatchExists(data)
	}

	// Stage 0: find all matches to get candidate lines
	ms := p.stages[0].FindAll(data)
	if !ms.HasMatch() {
		return false
	}

	for i := range ms.Matches {
		m := &ms.Matches[i]
		if m.IsContext || m.LineStart < 0 {
			continue
		}

		lineBytes := ms.Data[m.LineStart : m.LineStart+m.LineLen]
		if len(p.narrowLine(lineBytes)) > 0 {
			return true
		}
	}

	return false
}

func (p *PipelineMatcher) CountAll(data []byte) int {
	if len(p.stages) == 0 {
		return 0
	}

	if len(p.stages) == 1 {
		return p.stages[0].CountAll(data)
	}

	// For multi-stage: use FindAll and count result matches
	ms := p.FindAll(data)
	return len(ms.Matches)
}

func (p *PipelineMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	if len(p.stages) == 0 {
		return MatchSet{}, false
	}

	// Stage 0: check if line matches
	ms0, ok := p.stages[0].FindLine(line, lineNum, byteOffset)
	if !ok {
		return MatchSet{}, false
	}
	if len(p.stages) == 1 {
		return ms0, true
	}

	// Stages 1..N: narrow within the previous stage's fragments.
	regions := p.narrowLine(line)
	if len(regions) == 0 {
		return MatchSet{}, false
	}
	return MatchSet{
		Data: line,
		Matches: []Match{{
			LineNum:    lineNum,
			LineStart:  0,
			LineLen:    len(line),
			ByteOffset: byteOffset,
			PosIdx:     0,
			PosCount:   len(regions),
		}},
		Positions: regions,
	}, true
}

// MultiPipelineMatcher runs multiple pipelines (OR'd) and merges results.
type MultiPipelineMatcher struct {
	pipelines []*PipelineMatcher
}

// NewMultiPipelineMatcher creates a matcher that OR's multiple pipelines.
func NewMultiPipelineMatcher(pipelines []*PipelineMatcher) *MultiPipelineMatcher {
	return &MultiPipelineMatcher{pipelines: pipelines}
}

// OnlyMatch returns true if any pipeline's final stage uses -o.
func (m *MultiPipelineMatcher) OnlyMatch() bool {
	for _, p := range m.pipelines {
		if p.OnlyMatch() {
			return true
		}
	}
	return false
}

func (m *MultiPipelineMatcher) FindAll(data []byte) MatchSet {
	if len(m.pipelines) == 1 {
		return m.pipelines[0].FindAll(data)
	}

	// Run all pipelines and merge results
	var allMatches []Match
	var allPositions [][2]int

	for _, p := range m.pipelines {
		ms := p.FindAll(data)
		if !ms.HasMatch() {
			continue
		}

		posOffset := len(allPositions)
		allPositions = append(allPositions, ms.Positions...)
		for _, match := range ms.Matches {
			match.PosIdx += posOffset
			allMatches = append(allMatches, match)
		}
	}

	if len(allMatches) == 0 {
		return MatchSet{}
	}

	// Sort by byte offset and deduplicate by line
	sortAndDedup(&allMatches)

	return MatchSet{
		Data:      data,
		Matches:   allMatches,
		Positions: allPositions,
	}
}

func (m *MultiPipelineMatcher) MatchExists(data []byte) bool {
	for _, p := range m.pipelines {
		if p.MatchExists(data) {
			return true
		}
	}
	return false
}

func (m *MultiPipelineMatcher) CountAll(data []byte) int {
	ms := m.FindAll(data)
	return len(ms.Matches)
}

func (m *MultiPipelineMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	for _, p := range m.pipelines {
		ms, ok := p.FindLine(line, lineNum, byteOffset)
		if ok {
			return ms, true
		}
	}
	return MatchSet{}, false
}

// sortAndDedup sorts matches by ByteOffset and removes duplicate lines.
func sortAndDedup(matches *[]Match) {
	ms := *matches
	if len(ms) <= 1 {
		return
	}

	// Insertion sort (matches are nearly sorted from pipeline order)
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && ms[j].ByteOffset < ms[j-1].ByteOffset; j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}

	// Deduplicate by LineStart
	w := 0
	for r := 1; r < len(ms); r++ {
		if ms[r].LineStart != ms[w].LineStart {
			w++
			if w != r {
				ms[w] = ms[r]
			}
		}
	}
	*matches = ms[:w+1]
}

// countNewlines counts '\n' bytes in data.
func countNewlines(data []byte) int {
	return bytes.Count(data, []byte{'\n'})
}

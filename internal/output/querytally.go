package output

import "strconv"

// queryTally accumulates per-query (files, lines) totals in first-seen
// order for --batch. JSONFormatter and BudgetFormatter both need it —
// zero-hit probes must stay explicit in the summary even after budget
// wrapping — so the bookkeeping and JSON serialization live here once
// instead of being maintained in both formatters.
type queryTally struct {
	order  []string
	totals map[string]*[2]int // query -> {files, lines}
}

// entry returns (creating if needed) the totals slot for query.
func (t *queryTally) entry(query string) *[2]int {
	if t.totals == nil {
		t.totals = make(map[string]*[2]int)
	}
	qt := t.totals[query]
	if qt == nil {
		qt = &[2]int{}
		t.totals[query] = qt
		t.order = append(t.order, query)
	}
	return qt
}

// appendJSON appends `,"queries":[...]` to buf when any query was seen;
// includeLines is false in -l mode, where line counts are meaningless.
func (t *queryTally) appendJSON(buf []byte, includeLines bool) []byte {
	if len(t.order) == 0 {
		return buf
	}
	buf = append(buf, `,"queries":[`...)
	for i, q := range t.order {
		if i > 0 {
			buf = append(buf, ',')
		}
		qt := t.totals[q]
		buf = append(buf, `{"query":`...)
		buf = appendJSONString(buf, q)
		buf = append(buf, `,"files":`...)
		buf = strconv.AppendInt(buf, int64(qt[0]), 10)
		if includeLines {
			buf = append(buf, `,"lines":`...)
			buf = strconv.AppendInt(buf, int64(qt[1]), 10)
		}
		buf = append(buf, '}')
	}
	buf = append(buf, ']')
	return buf
}

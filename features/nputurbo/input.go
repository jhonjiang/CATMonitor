//go:build linux

package nputurbo

import (
	"encoding/json"
	"strings"
	"time"
)

// SlowCard is one slow-NPU entry extracted from the straggler result.
type SlowCard struct {
	Hostname string
	ID       int
	Score    float64
}

// Round is one detection round of the straggler result: the slow-card list
// plus the round's metadata. ID is the doc's top-level "id" (the detection
// round counter — it restarts at 1 when the detector process restarts) and
// StartedAt its "started_at" timestamp (which does NOT reset). Both are
// zero when the doc omits them (old format); callers treat that as "no
// round metadata" — no round dedupe, no staleness check.
type Round struct {
	ID        int
	StartedAt time.Time
	Cards     []SlowCard
}

// roundKey is the dedupe identity of one detection round. The pair — not
// the bare id — is the identity: a detector restart reuses ids, but its
// new rounds carry a fresh started_at, so the pair still changes.
type roundKey struct {
	id        int
	startedAt time.Time
}

// Key returns the round's dedupe identity. ok=false when the doc carried
// no id or no started_at — such rounds are never deduped (half-metadata
// cannot be told apart from the next round).
func (r Round) Key() (roundKey, bool) {
	if r.ID <= 0 || r.StartedAt.IsZero() {
		return roundKey{}, false
	}
	return roundKey{id: r.ID, startedAt: r.StartedAt}, true
}

type calScore struct {
	Score float64 `json:"score"`
}
type npuEntry struct {
	ID  int      `json:"id"`
	Cal calScore `json:"cal"`
}
type nodeResultEntry struct {
	Hostname string     `json:"hostname"`
	NPU      []npuEntry `json:"npu"`
}
type profilerDoc struct {
	ID        int       `json:"id"`
	StartedAt time.Time `json:"started_at"`
	Profiler  struct {
		NodeResult []nodeResultEntry `json:"node_result"`
		// comm_domain_result is ignored.
	} `json:"profiler"`
}

// ParseLatestRound extracts the newest detection round from straggler's
// output. It first tries to parse the whole buffer as one profiler doc
// (handles pretty-printed and compact single-doc). If that fails, it splits
// into lines and considers every successfully-parsed line (handles jsonl
// append). Among the candidates the newest round wins: docs carrying a
// started_at beat docs without one, the later started_at wins, and equal
// timestamps tie-break to the higher id (the id alone is NOT the ordering —
// a detector restart resets its counter, the timestamp does not). Rounds
// without any started_at fall back to the LAST parsed line (the old
// append-order behavior). An empty node_result yields an empty Cards slice
// with nil error. Returns an error only when no valid doc could be parsed
// at all.
func ParseLatestRound(data []byte) (Round, error) {
	if r, ok := tryParseRound(data); ok {
		return r, nil
	}
	var best Round
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r, ok := tryParseRound([]byte(line))
		if !ok {
			continue
		}
		if !found || laterRound(r, best) {
			best, found = r, true
		}
	}
	if !found {
		return Round{}, errUnparseable
	}
	return best, nil
}

// laterRound reports whether candidate a is the newer round compared to b.
// See ParseLatestRound for the ordering rules.
func laterRound(a, b Round) bool {
	switch {
	case !a.StartedAt.IsZero() && b.StartedAt.IsZero():
		return true // timestamped beats untimestamped
	case a.StartedAt.IsZero() && !b.StartedAt.IsZero():
		return false
	case a.StartedAt.IsZero():
		return true // neither timestamped: the later line wins (old format)
	}
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.After(b.StartedAt)
	}
	return a.ID > b.ID
}

func tryParseRound(data []byte) (Round, bool) {
	var doc profilerDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return Round{}, false
	}
	r := Round{ID: doc.ID, StartedAt: doc.StartedAt}
	for _, n := range doc.Profiler.NodeResult {
		for _, c := range n.NPU {
			r.Cards = append(r.Cards, SlowCard{Hostname: n.Hostname, ID: c.ID, Score: c.Cal.Score})
		}
	}
	return r, true
}

type jsonUnparseableError struct{}

func (jsonUnparseableError) Error() string {
	return "nputurbo: no valid profiler document found in straggler output"
}

var errUnparseable = jsonUnparseableError{}

//go:build linux

package nputurbo

import (
	"encoding/json"
	"strings"
)

// SlowCard is one slow-NPU entry extracted from the straggler result.
type SlowCard struct {
	Hostname string
	ID       int
	Score    float64
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
	Profiler struct {
		NodeResult []nodeResultEntry `json:"node_result"`
		// comm_domain_result is ignored.
	} `json:"profiler"`
}

// ParseSlowCards extracts the slow-card list from straggler's output.
// It first tries to parse the whole buffer as one profiler doc (handles
// pretty-printed and compact single-doc). If that fails, it splits into
// lines and uses the LAST successfully-parsed line (handles jsonl append).
// An empty node_result yields an empty slice with nil error.
// Returns an error only when no valid doc could be parsed at all.
func ParseSlowCards(data []byte) ([]SlowCard, error) {
	if cards, ok := tryParse(data); ok {
		return cards, nil
	}
	var last []SlowCard
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if cards, ok := tryParse([]byte(line)); ok {
			last = cards
			found = true
		}
	}
	if !found {
		return nil, errUnparseable
	}
	return last, nil
}

func tryParse(data []byte) ([]SlowCard, bool) {
	var doc profilerDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false
	}
	var out []SlowCard
	for _, n := range doc.Profiler.NodeResult {
		for _, c := range n.NPU {
			out = append(out, SlowCard{Hostname: n.Hostname, ID: c.ID, Score: c.Cal.Score})
		}
	}
	return out, true
}

type jsonUnparseableError struct{}

func (jsonUnparseableError) Error() string {
	return "nputurbo: no valid profiler document found in straggler output"
}

var errUnparseable = jsonUnparseableError{}

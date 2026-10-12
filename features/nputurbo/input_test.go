//go:build linux

package nputurbo

import (
	"reflect"
	"testing"
	"time"
)

func TestParseSingleDoc(t *testing.T) {
	data := []byte(`{"profiler":{"node_result":[{"hostname":"work4","npu":[{"id":1,"cal":{"score":1.234}},{"id":3,"cal":{"score":1.15}}]}],"comm_domain_result":{}}}`)
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	want := []SlowCard{{Hostname: "work4", ID: 1, Score: 1.234}, {Hostname: "work4", ID: 3, Score: 1.15}}
	if !reflect.DeepEqual(got.Cards, want) {
		t.Errorf("got %+v want %+v", got.Cards, want)
	}
	if got.ID != 0 || !got.StartedAt.IsZero() {
		t.Errorf("old-format doc must yield no round metadata, got id=%d started=%v", got.ID, got.StartedAt)
	}
	if _, ok := got.Key(); ok {
		t.Error("old-format round must not carry a dedupe key")
	}
}

func TestParseRoundMetadata(t *testing.T) {
	data := []byte(`{"id":2,"started_at":"2026-10-09T08:57:15Z","profiler":{"node_result":[{"hostname":"master","npu":[{"id":1,"cal":{"score":1.3225}}]}],"comm_domain_result":{}}}`)
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if got.ID != 2 {
		t.Errorf("round id = %d, want 2", got.ID)
	}
	want := time.Date(2026, 10, 9, 8, 57, 15, 0, time.UTC)
	if !got.StartedAt.Equal(want) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, want)
	}
	if len(got.Cards) != 1 || got.Cards[0].ID != 1 || got.Cards[0].Score != 1.3225 {
		t.Errorf("cards = %+v", got.Cards)
	}
	key, ok := got.Key()
	if !ok || key.id != 2 || !key.startedAt.Equal(want) {
		t.Errorf("Key() = (%+v, %v), want (id 2, %v, true)", key, ok, want)
	}
}

func TestParsePrettyPrintedDoc(t *testing.T) {
	data := []byte("{\n  \"id\": 7,\n  \"started_at\": \"2026-10-09T08:00:00Z\",\n  \"profiler\": {\n    \"node_result\": [\n      {\"hostname\":\"w1\",\"npu\":[{\"id\":7,\"cal\":{\"score\":1.5}}]}\n    ],\n    \"comm_domain_result\": {}\n  }\n}")
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if got.ID != 7 || len(got.Cards) != 1 || got.Cards[0].ID != 7 || got.Cards[0].Score != 1.5 {
		t.Errorf("got %+v", got)
	}
}

func TestParseMultiLineLastWinsOldFormat(t *testing.T) {
	// Two jsonl lines without round metadata (old format); the LAST parsed
	// line wins — the pre-round-metadata behavior.
	data := []byte("{\"profiler\":{\"node_result\":[{\"hostname\":\"a\",\"npu\":[{\"id\":1,\"cal\":{\"score\":1.1}}]}],\"comm_domain_result\":{}}}\n" +
		"{\"profiler\":{\"node_result\":[{\"hostname\":\"b\",\"npu\":[{\"id\":2,\"cal\":{\"score\":1.2}}]}],\"comm_domain_result\":{}}}\n")
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if len(got.Cards) != 1 || got.Cards[0].ID != 2 {
		t.Errorf("expected last-line card id=2, got %+v", got.Cards)
	}
}

func TestParseLatestByStartedAt(t *testing.T) {
	// Two appended rounds in order; the later started_at wins even though
	// the ids also increase.
	data := []byte("{\"id\":1,\"started_at\":\"2026-10-09T08:52:09Z\",\"profiler\":{\"node_result\":[],\"comm_domain_result\":{}}}\n" +
		"{\"id\":2,\"started_at\":\"2026-10-09T08:57:15Z\",\"profiler\":{\"node_result\":[{\"hostname\":\"master\",\"npu\":[{\"id\":1,\"cal\":{\"score\":1.32}}]}],\"comm_domain_result\":{}}}\n")
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if got.ID != 2 || len(got.Cards) != 1 || got.Cards[0].ID != 1 {
		t.Errorf("expected round id=2 with device 1, got %+v", got)
	}
}

func TestParseDetectorRestartResetsID(t *testing.T) {
	// The detector restarted and its id counter reset to 1 — but the new
	// round carries a FRESHER started_at, so it must win over the old
	// higher id.
	data := []byte("{\"id\":5,\"started_at\":\"2026-10-09T08:52:09Z\",\"profiler\":{\"node_result\":[{\"hostname\":\"h\",\"npu\":[{\"id\":3,\"cal\":{\"score\":1.4}}]}],\"comm_domain_result\":{}}}\n" +
		"{\"id\":1,\"started_at\":\"2026-10-09T09:08:00Z\",\"profiler\":{\"node_result\":[{\"hostname\":\"h\",\"npu\":[{\"id\":4,\"cal\":{\"score\":1.4}}]}],\"comm_domain_result\":{}}}\n")
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if got.ID != 1 || len(got.Cards) != 1 || got.Cards[0].ID != 4 {
		t.Errorf("expected restarted round (id=1, device 4), got %+v", got)
	}
}

func TestParseTimestampedBeatsLaterPlainLine(t *testing.T) {
	// A timestamped round must win even when an old-format (metadata-less)
	// line comes after it.
	data := []byte("{\"id\":2,\"started_at\":\"2026-10-09T08:57:15Z\",\"profiler\":{\"node_result\":[{\"hostname\":\"h\",\"npu\":[{\"id\":1,\"cal\":{\"score\":1.4}}]}],\"comm_domain_result\":{}}}\n" +
		"{\"profiler\":{\"node_result\":[{\"hostname\":\"h\",\"npu\":[{\"id\":9,\"cal\":{\"score\":1.4}}]}],\"comm_domain_result\":{}}}\n")
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if got.ID != 2 || len(got.Cards) != 1 || got.Cards[0].ID != 1 {
		t.Errorf("expected timestamped round id=2 device 1, got %+v", got)
	}
}

func TestParseTieStartedAtHigherIDWins(t *testing.T) {
	// Equal started_at (same second): the higher id wins.
	data := []byte("{\"id\":3,\"started_at\":\"2026-10-09T08:57:15Z\",\"profiler\":{\"node_result\":[{\"hostname\":\"h\",\"npu\":[{\"id\":1,\"cal\":{\"score\":1.1}}]}],\"comm_domain_result\":{}}}\n" +
		"{\"id\":7,\"started_at\":\"2026-10-09T08:57:15Z\",\"profiler\":{\"node_result\":[{\"hostname\":\"h\",\"npu\":[{\"id\":2,\"cal\":{\"score\":1.2}}]}],\"comm_domain_result\":{}}}\n")
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if got.ID != 7 || len(got.Cards) != 1 || got.Cards[0].ID != 2 {
		t.Errorf("expected round id=7 (tie-break), got %+v", got)
	}
}

func TestParseEmptyNodeResult(t *testing.T) {
	data := []byte(`{"id":1,"started_at":"2026-10-09T08:52:09Z","profiler":{"node_result":[],"comm_domain_result":{}}}`)
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if len(got.Cards) != 0 {
		t.Errorf("expected 0 cards, got %+v", got.Cards)
	}
	if got.ID != 1 {
		t.Errorf("round id = %d, want 1", got.ID)
	}
}

func TestParseMultiNodeMultiCard(t *testing.T) {
	data := []byte(`{"profiler":{"node_result":[{"hostname":"n1","npu":[{"id":1,"cal":{"score":1.2}}]},{"hostname":"n2","npu":[{"id":4,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	got, _ := ParseLatestRound(data)
	if len(got.Cards) != 2 || got.Cards[0].ID != 1 || got.Cards[1].ID != 4 {
		t.Errorf("got %+v", got.Cards)
	}
}

func TestParseBadLinesSkipped(t *testing.T) {
	// Not whole-file JSON; only line 2 valid → use it.
	data := []byte("garbage not json\n" + `{"profiler":{"node_result":[{"hostname":"x","npu":[{"id":9,"cal":{"score":1.9}}]}],"comm_domain_result":{}}}` + "\n")
	got, err := ParseLatestRound(data)
	if err != nil {
		t.Fatalf("ParseLatestRound: %v", err)
	}
	if len(got.Cards) != 1 || got.Cards[0].ID != 9 {
		t.Errorf("got %+v", got.Cards)
	}
}

func TestParseAllBadReturnsError(t *testing.T) {
	data := []byte("totally not json at all")
	_, err := ParseLatestRound(data)
	if err == nil {
		t.Fatal("expected error for unparseable input")
	}
}

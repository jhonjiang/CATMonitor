//go:build linux

package nputurbo

import (
	"reflect"
	"testing"
)

func TestParseSingleDoc(t *testing.T) {
	data := []byte(`{"profiler":{"node_result":[{"hostname":"work4","npu":[{"id":1,"cal":{"score":1.234}},{"id":3,"cal":{"score":1.15}}]}],"comm_domain_result":{}}}`)
	got, err := ParseSlowCards(data)
	if err != nil {
		t.Fatalf("ParseSlowCards: %v", err)
	}
	want := []SlowCard{{Hostname: "work4", ID: 1, Score: 1.234}, {Hostname: "work4", ID: 3, Score: 1.15}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestParsePrettyPrintedDoc(t *testing.T) {
	data := []byte("{\n  \"profiler\": {\n    \"node_result\": [\n      {\"hostname\":\"w1\",\"npu\":[{\"id\":7,\"cal\":{\"score\":1.5}}]}\n    ],\n    \"comm_domain_result\": {}\n  }\n}")
	got, err := ParseSlowCards(data)
	if err != nil {
		t.Fatalf("ParseSlowCards: %v", err)
	}
	if len(got) != 1 || got[0].ID != 7 || got[0].Score != 1.5 {
		t.Errorf("got %+v", got)
	}
}

func TestParseMultiLineLastWins(t *testing.T) {
	// Two jsonl lines (two profiler docs); parser uses the LAST.
	data := []byte("{\"profiler\":{\"node_result\":[{\"hostname\":\"a\",\"npu\":[{\"id\":1,\"cal\":{\"score\":1.1}}]}],\"comm_domain_result\":{}}}\n" +
		"{\"profiler\":{\"node_result\":[{\"hostname\":\"b\",\"npu\":[{\"id\":2,\"cal\":{\"score\":1.2}}]}],\"comm_domain_result\":{}}}\n")
	got, err := ParseSlowCards(data)
	if err != nil {
		t.Fatalf("ParseSlowCards: %v", err)
	}
	if len(got) != 1 || got[0].ID != 2 {
		t.Errorf("expected last-line card id=2, got %+v", got)
	}
}

func TestParseEmptyNodeResult(t *testing.T) {
	data := []byte(`{"profiler":{"node_result":[],"comm_domain_result":{}}}`)
	got, err := ParseSlowCards(data)
	if err != nil {
		t.Fatalf("ParseSlowCards: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 cards, got %+v", got)
	}
}

func TestParseMultiNodeMultiCard(t *testing.T) {
	data := []byte(`{"profiler":{"node_result":[{"hostname":"n1","npu":[{"id":1,"cal":{"score":1.2}}]},{"hostname":"n2","npu":[{"id":4,"cal":{"score":1.4}}]}],"comm_domain_result":{}}}`)
	got, _ := ParseSlowCards(data)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 4 {
		t.Errorf("got %+v", got)
	}
}

func TestParseBadLinesSkipped(t *testing.T) {
	// Not whole-file JSON; only line 2 valid → use it.
	data := []byte("garbage not json\n" + `{"profiler":{"node_result":[{"hostname":"x","npu":[{"id":9,"cal":{"score":1.9}}]}],"comm_domain_result":{}}}` + "\n")
	got, err := ParseSlowCards(data)
	if err != nil {
		t.Fatalf("ParseSlowCards: %v", err)
	}
	if len(got) != 1 || got[0].ID != 9 {
		t.Errorf("got %+v", got)
	}
}

func TestParseAllBadReturnsError(t *testing.T) {
	data := []byte("totally not json at all")
	_, err := ParseSlowCards(data)
	if err == nil {
		t.Fatal("expected error for unparseable input")
	}
}

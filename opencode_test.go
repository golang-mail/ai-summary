package main

import (
	"reflect"
	"testing"
)

func TestParseOpenCodeEvents(t *testing.T) {
	stream := `{"type":"step_start","part":{"type":"step-start"}}
{"type":"text","part":{"type":"text","text":"*Group A*\n• one"}}
{"type":"tool_use","part":{"type":"tool","tool":"bash"}}
{"type":"text","part":{"type":"text","text":"*Group B*\n• two"}}
{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}
`
	got, ok := parseOpenCodeEvents(stream)
	if !ok {
		t.Fatal("expected a JSON event stream to be recognised")
	}
	want := "*Group A*\n• one\n*Group B*\n• two"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseOpenCodeEventsRejectsPlainText(t *testing.T) {
	if _, ok := parseOpenCodeEvents("*Group A*\n• one\n"); ok {
		t.Error("plain text must not be treated as an event stream")
	}
	if got, ok := parseOpenCodeEvents(`{"type":"step_finish","part":{"type":"step-finish"}}`); !ok || got != "" {
		t.Errorf("stream without text parts: got (%q, %v), want (\"\", true)", got, ok)
	}
}

func TestOpenCodeArgvAddsJSONFormat(t *testing.T) {
	tests := []struct {
		in, want []string
	}{
		{[]string{"opencode", "run"}, []string{"opencode", "run", "--format", "json"}},
		{[]string{"opencode", "run", "--model", "x/y"}, []string{"opencode", "run", "--model", "x/y", "--format", "json"}},
		{[]string{"opencode", "run", "--format", "default"}, []string{"opencode", "run", "--format", "default"}},
		{[]string{"opencode", "run", "--format=json"}, []string{"opencode", "run", "--format=json"}},
	}
	for _, tt := range tests {
		if got := openCodeArgv(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("openCodeArgv(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

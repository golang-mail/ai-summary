package main

import (
	"encoding/json"
	"strings"
)

// openCodeArgv asks for the JSON event stream unless the operator already
// picked a format: the default text output was seen truncated and empty.
func openCodeArgv(argv []string) []string {
	for _, a := range argv {
		if a == "--format" || strings.HasPrefix(a, "--format=") {
			return argv
		}
	}
	out := make([]string, 0, len(argv)+2)
	out = append(out, argv...)
	return append(out, "--format", "json")
}

type openCodeEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

// parseOpenCodeEvents joins the text parts of an `opencode run --format json`
// stream. ok is false when the input is not such a stream.
func parseOpenCodeEvents(raw string) (string, bool) {
	var texts []string
	seen := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev openCodeEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type == "" {
			return "", false
		}
		seen = true
		if ev.Type == "text" && ev.Part.Text != "" {
			texts = append(texts, ev.Part.Text)
		}
	}
	if !seen {
		return "", false
	}
	return strings.TrimSpace(strings.Join(texts, "\n")), true
}

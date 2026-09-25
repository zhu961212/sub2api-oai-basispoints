package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecoverTransportEnvelopeFormats(t *testing.T) {
	const envelope = `{"name":"functions.exec","arguments":{"cmd":"echo {hello}","n":9007199254740993}}`
	quoted, _ := json.Marshal(envelope)
	for _, raw := range []string{
		envelope,
		string(quoted),
		"```json\n" + envelope + "\n```",
		"Tool request:\n```json\n" + envelope + "\n```",
		"The model returned: " + envelope,
		"functions.exec(" + envelope + ")",
		`await functions.exec({"cmd":"echo {hello}","n":9007199254740993});`,
	} {
		got := recoverTransportEnvelope(raw)
		if got == nil || string(jsonBytes(got)) != envelope {
			// JSON map order is not part of the protocol. Compare the decoded values.
			var expected any
			decoder := json.NewDecoder(strings.NewReader(envelope))
			decoder.UseNumber()
			_ = decoder.Decode(&expected)
			if !jsonValuesEqual(got, expected) {
				t.Fatalf("recovery changed %q: %#v", raw, got)
			}
		}
	}
}

func TestRecoverTransportEnvelopePreservesCustomInput(t *testing.T) {
	const input = "  *** Begin Patch\n+{\"name\":\"unrelated\"}\n*** End Patch\n  "
	encoded, _ := json.Marshal(input)
	for _, raw := range []string{
		`{"tool":"functions.apply_patch","args":` + string(encoded) + `}`,
		"functions.apply_patch(" + string(encoded) + ")",
	} {
		got := recoverTransportEnvelope(raw)
		if got == nil || got["args"] != input {
			t.Fatalf("custom input was altered: %#v", got)
		}
	}
}

func TestRecoverTransportEnvelopeRejectsAmbiguousAndIncompleteData(t *testing.T) {
	const one = `{"name":"functions.exec","arguments":{"cmd":"pwd"}}`
	for _, raw := range []string{
		"", "null", "[]", "[" + one + "]",
		one + one,
		one + ` {"name":"unknown","arguments":{}}`,
		`functions.exec({"cmd":"pwd"});` + one,
		one + `; functions.exec({"cmd":"pwd"})`,
		`functions.exec({"cmd":"pwd"}); functions.exec({"cmd":"pwd"})`,
		"```json\n" + one + "\n```\n```json\n" + one + "\n```",
		`{"name":"functions.exec","tool":"unknown","arguments":{}}`,
		`{"name":"functions.exec","args":{},"arguments":{}}`,
		`{"name":"functions.exec","input":"one","args":"two"}`,
		`{"name":42,"tool":"functions.exec","args":{}}`,
		`functions.exec({"name":"functions.exec","tool":"unknown","args":{}})`,
		`functions.exec({"cmd":"pwd"}, {"cmd":"ls"})`,
		`functions.exec({"cmd":"pwd"}`,
		`{"name":"functions.exec","arguments":{"cmd":"pwd"}`,
		`Excel.run(async () => { return ` + one + `; })`,
		strings.Repeat("x", maxRecoveredEnvelopeBytes+1),
	} {
		if got := recoverTransportEnvelope(raw); got != nil {
			t.Fatalf("accepted ambiguous or invalid transport %q: %#v", raw, got)
		}
	}
}

func TestRecoverTransportEnvelopeIgnoresQuotedBraces(t *testing.T) {
	raw := `A literal '{"name":"ignored","args":{}}' precedes {"name":"functions.exec","arguments":{"cmd":"printf '{x}'"}}`
	got := recoverTransportEnvelope(raw)
	if got == nil || got["name"] != "functions.exec" {
		t.Fatalf("quoted prose confused recovery: %#v", got)
	}
}

func TestRecoverTransportEnvelopeRepairsOnlyInvalidStringEscapes(t *testing.T) {
	got := recoverTransportEnvelope("{\"name\":\"functions.exec\",\"arguments\":{\"pattern\":\"\\d+\\s\",\"line\":\"a\nb\",\"literal\":\"\\\\n\"}}")
	if got == nil {
		t.Fatal("illegal JSON string escapes were not recovered")
	}
	args := got["arguments"].(map[string]any)
	if args["pattern"] != `\d+\s` || args["line"] != "a\nb" || args["literal"] != `\n` {
		t.Fatalf("repair altered data: %#v", args)
	}
}

package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestPromptDiscoveryKeepsEarlierConversationPrefix(t *testing.T) {
	for _, recordType := range []string{"additional_tools", "tool_search_output"} {
		t.Run(recordType, func(t *testing.T) {
			base := []any{relayCompatFunction("files.read")}
			history := []any{messageItem("user", strings.Repeat("existing conversation context ", 1024))}
			var prefix []any
			for round := 0; round < 3; round++ {
				source := map[string]any{"tools": base, "input": history}
				body, err := PrepareResponsesBody(source, DefaultConfig())
				if err != nil {
					t.Fatal(err)
				}
				items := body["input"].([]any)
				if round > 0 && (len(items) < len(prefix) || !bytes.Equal(jsonBytes(items[:len(prefix)]), jsonBytes(prefix))) {
					t.Fatalf("discovery round %d rewrote earlier conversation prefix", round)
				}
				prefix = items[:len(items)-1] // The generation-position reminder is intentionally refreshed.
				tool := relayCompatFunction("files.append")
				if round == 1 {
					tool["description"] = "Updated append contract"
				}
				history = append(history, map[string]any{"type": recordType, "status": "completed", "tools": []any{tool}}, messageItem("user", "Continue with the newly declared tool"))
			}
		})
	}
}

func TestPromptDiscoveryRetainsPositionAndContracts(t *testing.T) {
	custom := map[string]any{"type": "custom", "name": "functions.exec", "description": "Execute the complete program", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: /[a-z]+/"}}
	source := map[string]any{
		"tools": []any{relayCompatFunction("files.read")},
		"input": []any{messageItem("user", "before discovery"), map[string]any{"type": "additional_tools", "tools": []any{custom}}, messageItem("user", "after discovery"), map[string]any{"type": "compaction_trigger"}},
	}
	original := append([]byte(nil), jsonBytes(source)...)
	body, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	if len(items) != 6 || relayGuidanceText(items[1]) != "before discovery" || relayGuidanceText(items[3]) != "after discovery" || objectValue(items[5])["type"] != "compaction_trigger" {
		t.Fatalf("discovery changed conversation or compaction position: %s", jsonBytes(items))
	}
	prologue, update, reminder := relayGuidanceText(items[0]), relayGuidanceText(items[2]), relayGuidanceText(items[4])
	if strings.Contains(prologue, "functions.exec") || !strings.Contains(prologue, "files.read") {
		t.Fatal("late discovery leaked into the initial catalog")
	}
	for _, want := range []string{"functions.exec", "Execute the complete program", "start: /[a-z]+/", "raw text in code.args"} {
		if !strings.Contains(update, want) {
			t.Fatalf("discovery update lost contract %q", want)
		}
	}
	if strings.Contains(update, "files.read") || !strings.Contains(reminder, "files.read") || !strings.Contains(reminder, "functions.exec") {
		t.Fatal("delta rendering changed the active allowlist")
	}
	if _, ok := resolveClientTool(clientToolSpecs(source), "functions.exec"); !ok {
		t.Fatal("discovered tool lost request authorization")
	}
	// Preparation adds local cache fields, but caller-provided input/catalog stay intact.
	copy := map[string]any{"tools": source["tools"], "input": source["input"]}
	if !bytes.Equal(original, jsonBytes(copy)) {
		t.Fatal("prompt rendering mutated caller declarations or history")
	}
}

func TestPromptDiscoveryPreservesConflictsAndUpdates(t *testing.T) {
	base := []any{relayCompatFunction("conflict"), map[string]any{"type": "custom", "name": "conflict"}, relayCompatFunction("safe")}
	first := map[string]any{"type": "additional_tools", "tools": []any{relayCompatFunction("added")}}
	source := map[string]any{"tools": base, "input": []any{first}}
	body, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	before := body["input"].([]any)
	for _, value := range before {
		if strings.Contains(relayGuidanceText(value), "conflict") {
			t.Fatal("unrelated discovery made an ambiguous tool callable")
		}
	}
	resolved := relayCompatFunction("conflict")
	resolved["description"] = "Resolved declaration"
	next := map[string]any{"tools": base, "input": []any{first, map[string]any{"type": "additional_tools", "tools": []any{resolved, relayCompatFunction("added")}}}}
	body, err = PrepareResponsesBody(next, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	after := body["input"].([]any)
	if !bytes.Equal(jsonBytes(before[:len(before)-1]), jsonBytes(after[:len(before)-1])) {
		t.Fatal("resolving a conflict rewrote earlier prompt history")
	}
	update := relayGuidanceText(after[len(after)-2])
	if !strings.Contains(update, "Resolved declaration") || strings.Contains(update, "- added") {
		t.Fatal("update omitted the changed declaration or repeated an unchanged tool")
	}
	if _, ok := resolveClientTool(clientToolSpecs(next), "conflict"); !ok {
		t.Fatal("explicit conflict resolution lost authorization")
	}
}

func TestPromptDiscoveryIgnoresIncompleteAndMalformedRecords(t *testing.T) {
	source := map[string]any{"tools": []any{relayCompatFunction("safe")}, "input": []any{
		messageItem("user", "keep this message"),
		map[string]any{"type": "tool_search_output", "status": "in_progress", "tools": []any{relayCompatFunction("pending")}},
		map[string]any{"type": "tool_search_output", "status": "failed", "tools": []any{relayCompatFunction("failed")}},
		map[string]any{"type": "additional_tools", "tools": "bad catalog"},
		map[string]any{"type": "additional_tools", "tools": []any{nil, "bad tool"}},
	}}
	body, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	if len(items) != 3 || relayGuidanceText(items[1]) != "keep this message" || len(clientToolSpecs(source)) != 1 {
		t.Fatal("invalid discovery changed the input or callable tools")
	}
	for _, value := range items {
		text := relayGuidanceText(value)
		if strings.Contains(text, "pending") || strings.Contains(text, "failed") || strings.Contains(text, "bad tool") {
			t.Fatal("invalid discovery reached the prompt")
		}
	}
}

func TestPromptDiscoveryHonorsNoneAndExplicitEmptyBase(t *testing.T) {
	discovery := map[string]any{"type": "additional_tools", "tools": []any{relayCompatFunction("discovered")}}
	for _, choice := range []string{"auto", "none"} {
		t.Run(choice, func(t *testing.T) {
			source := map[string]any{"tools": []any{}, "tool_choice": choice, "input": []any{discovery}}
			body, err := PrepareResponsesBody(source, DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			items := body["input"].([]any)
			if choice == "none" {
				if len(items) != 1 || len(clientToolSpecs(source)) != 0 || strings.Contains(relayGuidanceText(items[0]), "discovered") {
					t.Fatal("tool_choice none advertised discovered tools")
				}
			} else if len(items) != 3 || strings.Contains(relayGuidanceText(items[0]), "discovered") || !strings.Contains(relayGuidanceText(items[1]), "initial catalog was empty") {
				t.Fatal("empty initial catalog did not retain a later explicit declaration")
			} else if update := relayGuidanceText(items[1]); !strings.Contains(update, clientRelayContract) || !strings.Contains(update, "Outer arguments include summary, extended_summary") {
				t.Fatal("first discovered tool lacks the complete relay contract")
			}
		})
	}
	// An explicit empty replacement still revokes remembered tools.
	session := t.Name()
	initial := map[string]any{"session_id": session, "tools": []any{relayCompatFunction("old")}, "input": []any{discovery}}
	if _, err := PrepareResponsesBody(initial, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	revoke := map[string]any{"session_id": session, "tools": []any{}, "input": []any{messageItem("user", "without tools")}}
	if _, err := PrepareResponsesBody(revoke, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if len(clientToolSpecs(map[string]any{"session_id": session})) != 0 {
		t.Fatal("prompt history restored revoked remembered tools")
	}
}

func TestPromptDiscoveryWithoutExplicitBaseKeepsCompatibility(t *testing.T) {
	source := map[string]any{"input": []any{map[string]any{"type": "additional_tools", "tools": []any{relayCompatFunction("discovered")}}}}
	body, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	if len(items) != 2 || !strings.Contains(relayGuidanceText(items[0]), "discovered") || !strings.Contains(relayGuidanceText(items[1]), "discovered") {
		t.Fatal("omitted-base request lost its legacy effective catalog")
	}
}

func TestPromptDiscoveryHostedLimitationsStayAtDiscoveryPosition(t *testing.T) {
	base := []any{relayCompatFunction("files.read")}
	history := []any{messageItem("user", "before hosted discovery")}
	first, err := PrepareResponsesBody(map[string]any{"tools": base, "input": history}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	before := first["input"].([]any)
	history = append(history, map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "file_search"}}})
	source := map[string]any{"tools": base, "input": history}
	second, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	after := second["input"].([]any)
	if len(after) != 4 || !bytes.Equal(jsonBytes(before[:len(before)-1]), jsonBytes(after[:len(before)-1])) {
		t.Fatal("hosted discovery rewrote the earlier prompt prefix")
	}
	if note := relayGuidanceText(after[2]); !strings.Contains(note, "Hosted tools unavailable through Basis Points: file_search") {
		t.Fatal("hosted discovery lost its capability limitation")
	}
	if len(clientToolSpecs(source)) != 1 {
		t.Fatal("hosted declaration gained client-tool authorization")
	}
	blocked := map[string]any{"tools": base, "input": []any{map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "image_generation"}}}}}
	if _, err := PrepareResponsesBody(blocked, DefaultConfig()); err == nil {
		t.Fatal("prompt transformation bypassed unsupported-capability validation")
	}
}

func TestPromptDiscoveryRepeatedDeclarationsDoNotGrowPrompt(t *testing.T) {
	base := []any{relayCompatFunction("files.read")}
	source := map[string]any{"tools": base, "input": []any{messageItem("user", "same history")}}
	before, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	next := map[string]any{"tools": base, "input": []any{messageItem("user", "same history"), map[string]any{"type": "additional_tools", "tools": base}}}
	after, err := PrepareResponsesBody(next, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(jsonBytes(before["input"]), jsonBytes(after["input"])) {
		t.Fatal("repeated identical declaration changed or expanded the prompt")
	}
}

func TestPromptDiscoveryWithdrawalDoesNotAdvertiseOldPermission(t *testing.T) {
	source := map[string]any{"tools": []any{relayCompatFunction("files.read")}, "input": []any{
		messageItem("user", "withdraw the client declaration"),
		map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "file_search", "name": "files.read"}}},
	}}
	body, err := PrepareResponsesBody(source, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	if len(clientToolSpecs(source)) != 0 || clientToolProtocolReminder(source) != "" {
		t.Fatal("withdrawn client declaration retained invocation permission")
	}
	if len(items) != 4 || !strings.Contains(relayGuidanceText(items[2]), "These tools are no longer callable: [\"files.read\"]") {
		t.Fatal("historical initial declaration was not explicitly withdrawn")
	}
}

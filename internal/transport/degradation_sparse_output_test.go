package transport

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func sparseDegradationMessage(id, role, status, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": role, "status": status,
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func sparseDegradationItemDone(index int, item map[string]any) string {
	return streamData(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
}

func sparseDegradationCompleted(kind string, includeOutput bool) string {
	response := map[string]any{"id": "resp_sparse", "object": "response", "status": "completed", "error": nil}
	if includeOutput {
		response["output"] = []any{}
	}
	return streamData(map[string]any{"type": kind, "response": response})
}

func sparseDegradationReadAnswer(data string) (string, error) {
	body, kind, err := readDegradationResponse(context.Background(), strings.NewReader(data), "text/event-stream", 1<<20)
	if err != nil {
		return "", err
	}
	return degradationAnswer(body, kind)
}

func TestDegradationReaderRecoversCompletedAssistantItemFromSparseTerminal(t *testing.T) {
	item := sparseDegradationMessage("msg_sparse", "assistant", "completed", "iPhone 17")
	for _, terminal := range []string{"response.completed", "response.done"} {
		for _, includeOutput := range []bool{false, true} {
			for _, step := range []int{0, 1, 7} {
				t.Run(fmt.Sprintf("%s/output=%t/chunk=%d", terminal, includeOutput, step), func(t *testing.T) {
					data := sparseDegradationItemDone(0, item) + sparseDegradationCompleted(terminal, includeOutput)
					reader := &degradationTailReader{data: []byte(data), step: step, err: context.DeadlineExceeded}
					body, kind, err := readDegradationResponse(context.Background(), reader, "text/event-stream", 1<<20)
					if err != nil {
						t.Fatal(err)
					}
					answer, err := degradationAnswer(body, kind)
					if err != nil || answer != "iPhone 17" {
						t.Fatalf("completed assistant item lost: answer=%q err=%v", answer, err)
					}
					if reader.tailRead {
						t.Fatal("completed sparse response waited for stream EOF")
					}
				})
			}
		}
	}
	data := sparseDegradationItemDone(0, item) + sparseDegradationCompleted("response.completed", true)
	answer, err := sparseDegradationReadAnswer(strings.TrimSpace(data))
	if err != nil || answer != "iPhone 17" {
		t.Fatalf("terminal at EOF lost completed item: answer=%q err=%v", answer, err)
	}
}

func TestDegradationSparseOutputRequiresSuccessfulResponseTerminal(t *testing.T) {
	prefix := sparseDegradationItemDone(0, sparseDegradationMessage("msg_sparse", "assistant", "completed", "iPhone 17"))
	for name, suffix := range map[string]string{
		"EOF without terminal":    "",
		"DONE without terminal":   "data: [DONE]\n\n",
		"truncated terminal":      "data: {",
		"failed":                  streamData(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed"}}),
		"incomplete":              streamData(map[string]any{"type": "response.incomplete", "response": map[string]any{"status": "incomplete"}}),
		"cancelled":               streamData(map[string]any{"type": "response.cancelled", "response": map[string]any{"status": "cancelled"}}),
		"terminal envelope error": streamData(map[string]any{"type": "response.completed", "error": map[string]any{"code": "server_error"}, "response": map[string]any{"status": "completed", "output": []any{}}}),
	} {
		t.Run(name, func(t *testing.T) {
			if answer, err := sparseDegradationReadAnswer(prefix + suffix); err == nil || answer != "" {
				t.Fatalf("item completion promoted unfinished response: answer=%q err=%v", answer, err)
			}
		})
	}
}

func TestDegradationSparseOutputIgnoresNonAssistantItems(t *testing.T) {
	for _, kind := range []string{"user", "system", "function_call", "reasoning"} {
		t.Run(kind, func(t *testing.T) {
			item := sparseDegradationMessage("msg_untrusted", "assistant", "completed", "iPhone 16")
			if kind == "user" || kind == "system" {
				item["role"] = kind
			} else {
				item["type"] = kind
			}
			prefix := sparseDegradationItemDone(0, item)
			terminal := sparseDegradationCompleted("response.completed", true)
			if answer, err := sparseDegradationReadAnswer(prefix + terminal); err == nil || answer != "" {
				t.Fatalf("non-assistant item became an answer: answer=%q err=%v", answer, err)
			}
			assistant := sparseDegradationMessage("msg_answer", "assistant", "completed", "iPhone 17")
			answer, err := sparseDegradationReadAnswer(prefix + sparseDegradationItemDone(1, assistant) + terminal)
			if err != nil || answer != "iPhone 17" {
				t.Fatalf("non-answer item contaminated assistant answer: answer=%q err=%v", answer, err)
			}
		})
	}
}

func TestDegradationSparseOutputRejectsUnfinishedOrFailedItems(t *testing.T) {
	for _, status := range []string{"in_progress", "incomplete", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			item := sparseDegradationMessage("msg_unfinished", "assistant", status, "iPhone 17")
			data := sparseDegradationItemDone(0, item) + sparseDegradationCompleted("response.completed", true)
			if answer, err := sparseDegradationReadAnswer(data); err == nil || answer != "" {
				t.Fatalf("unfinished output item became an answer: answer=%q err=%v", answer, err)
			}
		})
	}
	item := sparseDegradationMessage("msg_error", "assistant", "completed", "iPhone 17")
	item["error"] = map[string]any{"code": "server_error"}
	data := sparseDegradationItemDone(0, item) + sparseDegradationCompleted("response.completed", true)
	if answer, err := sparseDegradationReadAnswer(data); err == nil || answer != "" {
		t.Fatalf("failed output item became an answer: answer=%q err=%v", answer, err)
	}
}

func TestDegradationSparseOutputDoesNotPromoteAddedItemsOrTextDeltas(t *testing.T) {
	item := sparseDegradationMessage("msg_draft", "assistant", "in_progress", "iPhone 17")
	data := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item}) +
		streamData(map[string]any{"type": "response.output_text.delta", "item_id": "msg_draft", "output_index": 0, "content_index": 0, "delta": "iPhone 17"}) +
		sparseDegradationCompleted("response.completed", true)
	if answer, err := sparseDegradationReadAnswer(data); err == nil || answer != "" {
		t.Fatalf("unfinished draft used without completed output item: answer=%q err=%v", answer, err)
	}
}

func TestDegradationSparseOutputPreservesCompletedTerminalAnswer(t *testing.T) {
	prior := sparseDegradationMessage("msg_sparse", "assistant", "completed", "iPhone 16")
	final := sparseDegradationMessage("msg_sparse", "assistant", "completed", "iPhone 17")
	terminal := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{final}}})
	answer, err := sparseDegradationReadAnswer(sparseDegradationItemDone(0, prior) + terminal)
	if err != nil || answer != "iPhone 17" {
		t.Fatalf("terminal output was replaced or duplicated: answer=%q err=%v", answer, err)
	}
}

func TestDegradationSparseOutputDeduplicatesAndOrdersCompletedItems(t *testing.T) {
	first := sparseDegradationMessage("msg_first", "assistant", "completed", "iPhone 17")
	second := sparseDegradationMessage("msg_second", "assistant", "completed", "iPhone 17 Pro")
	data := sparseDegradationItemDone(2, second) + sparseDegradationItemDone(0, first) +
		sparseDegradationItemDone(0, first) + sparseDegradationCompleted("response.completed", true)
	answer, err := sparseDegradationReadAnswer(data)
	if err != nil || answer != "iPhone 17\niPhone 17 Pro" {
		t.Fatalf("completed items duplicated or reordered: answer=%q err=%v", answer, err)
	}
}

func TestDegradationSparseOutputRejectsConflictingCompletedItems(t *testing.T) {
	first := sparseDegradationMessage("msg_first", "assistant", "completed", "iPhone 17")
	for _, entry := range []struct {
		name  string
		index int
		item  map[string]any
	}{
		{"changed text", 0, sparseDegradationMessage("msg_first", "assistant", "completed", "iPhone 16")},
		{"changed index", 1, sparseDegradationMessage("msg_first", "assistant", "completed", "iPhone 17")},
		{"changed identity", 0, sparseDegradationMessage("msg_other", "assistant", "completed", "iPhone 17")},
		{"changed role", 0, sparseDegradationMessage("msg_first", "user", "completed", "iPhone 17")},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", entry.name, reverse), func(t *testing.T) {
				one, two := sparseDegradationItemDone(0, first), sparseDegradationItemDone(entry.index, entry.item)
				if reverse {
					one, two = two, one
				}
				if answer, err := sparseDegradationReadAnswer(one + two + sparseDegradationCompleted("response.completed", true)); err == nil || answer != "" {
					t.Fatalf("conflicting completed item accepted: answer=%q err=%v", answer, err)
				}
			})
		}
	}
	different := sparseDegradationMessage("msg_second", "assistant", "completed", "iPhone 16")
	answer, err := sparseDegradationReadAnswer(sparseDegradationItemDone(0, first) + sparseDegradationItemDone(1, different) + sparseDegradationCompleted("response.completed", true))
	if err != nil {
		t.Fatal(err)
	}
	status, err := classifyDegradationAnswer(answer)
	if err == nil || status != "error" {
		t.Fatalf("different completed answers produced a verdict: status=%s answer=%q err=%v", status, answer, err)
	}
}

func TestDegradationSparseOutputRejectsInvalidCompletedItemBesideValidAnswer(t *testing.T) {
	valid := sparseDegradationItemDone(0, sparseDegradationMessage("msg_valid", "assistant", "completed", "iPhone 17"))
	for _, state := range []string{"in_progress", "incomplete", "failed", "cancelled", "error"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", state, reverse), func(t *testing.T) {
				item := sparseDegradationMessage("msg_invalid", "assistant", state, "iPhone 17")
				if state == "error" {
					item["status"] = "completed"
					item["error"] = map[string]any{"code": "server_error"}
				}
				one, two := valid, sparseDegradationItemDone(1, item)
				if reverse {
					one, two = two, one
				}
				if answer, err := sparseDegradationReadAnswer(one + two + sparseDegradationCompleted("response.completed", true)); err == nil || answer != "" {
					t.Fatalf("invalid completed item was ignored: answer=%q err=%v", answer, err)
				}
			})
		}
	}
}

func TestDegradationSparseOutputRecoversBesideTerminalNonAnswers(t *testing.T) {
	prefix := sparseDegradationItemDone(0, sparseDegradationMessage("msg_answer", "assistant", "completed", "iPhone 17"))
	for _, kind := range []string{"user", "function_call", "reasoning", "empty assistant"} {
		t.Run(kind, func(t *testing.T) {
			item := sparseDegradationMessage("msg_terminal", "assistant", "completed", "iPhone 16")
			switch kind {
			case "user":
				item["role"] = "user"
			case "empty assistant":
				item["content"] = []any{}
			default:
				item["type"] = kind
			}
			terminal := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}})
			answer, err := sparseDegradationReadAnswer(prefix + terminal)
			if err != nil || answer != "iPhone 17" {
				t.Fatalf("terminal non-answer prevented recovery: answer=%q err=%v", answer, err)
			}
		})
	}
}

func TestDegradationSparseOutputNeverOverridesInvalidOrRefusedTerminalItems(t *testing.T) {
	prefix := sparseDegradationItemDone(0, sparseDegradationMessage("msg_answer", "assistant", "completed", "iPhone 17"))
	for _, state := range []string{"in_progress", "incomplete", "failed", "error", "refusal"} {
		t.Run(state, func(t *testing.T) {
			item := sparseDegradationMessage("msg_terminal", "assistant", state, "")
			if state == "error" {
				item["status"] = "completed"
				item["error"] = map[string]any{"code": "server_error"}
			}
			if state == "refusal" {
				item["status"] = "completed"
				item["content"] = []any{map[string]any{"type": "refusal", "refusal": "Cannot answer"}}
			}
			terminal := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}})
			if answer, err := sparseDegradationReadAnswer(prefix + terminal); err == nil || answer != "" {
				t.Fatalf("fallback replaced invalid or refused terminal: answer=%q err=%v", answer, err)
			}
		})
	}
}

func TestDegradationSparseOutputDoesNotHideMalformedTerminalMessageContent(t *testing.T) {
	prefix := sparseDegradationItemDone(0, sparseDegradationMessage("msg_answer", "assistant", "completed", "iPhone 17"))
	for name, content := range map[string]any{
		"string":         "malformed",
		"object":         map[string]any{"type": "output_text", "text": ""},
		"null":           nil,
		"nonobject part": []any{"malformed"},
		"null part":      []any{nil},
	} {
		for _, omitRole := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/omitRole=%t", name, omitRole), func(t *testing.T) {
				item := sparseDegradationMessage("msg_terminal", "assistant", "completed", "")
				item["content"] = content
				if omitRole {
					delete(item, "role")
				}
				terminal := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}})
				if answer, err := sparseDegradationReadAnswer(prefix + terminal); err == nil || answer != "" {
					t.Fatalf("fallback hid malformed terminal content: answer=%q err=%v", answer, err)
				}
			})
		}
	}
}

func TestDegradationSparseOutputPreservesTerminalRefusalWithoutRole(t *testing.T) {
	prefix := sparseDegradationItemDone(0, sparseDegradationMessage("msg_answer", "assistant", "completed", "iPhone 17"))
	item := sparseDegradationMessage("msg_terminal", "assistant", "completed", "")
	delete(item, "role")
	item["content"] = []any{map[string]any{"type": "refusal", "refusal": "Cannot answer"}}
	terminal := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}}})
	if answer, err := sparseDegradationReadAnswer(prefix + terminal); err == nil || answer != "" {
		t.Fatalf("fallback replaced terminal refusal without role: answer=%q err=%v", answer, err)
	}
}

func TestDegradationSparseOutputDoesNotIgnoreRefusedOrMalformedCompletedMessages(t *testing.T) {
	valid := sparseDegradationItemDone(0, sparseDegradationMessage("msg_answer", "assistant", "completed", "iPhone 17"))
	for name, content := range map[string]any{
		"refusal":           []any{map[string]any{"type": "refusal", "refusal": "Cannot answer"}},
		"malformed content": "malformed",
		"malformed part":    []any{nil},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", name, reverse), func(t *testing.T) {
				item := sparseDegradationMessage("msg_blocked", "assistant", "completed", "")
				item["content"] = content
				one, two := valid, sparseDegradationItemDone(1, item)
				if reverse {
					one, two = two, one
				}
				prefix := one + two
				if answer, err := sparseDegradationReadAnswer(prefix + sparseDegradationCompleted("response.completed", true)); err == nil || answer != "" {
					t.Fatalf("fallback ignored refused or malformed completed message: answer=%q err=%v", answer, err)
				}
				terminal := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{sparseDegradationMessage("msg_final", "assistant", "completed", "iPhone 16")}}})
				answer, err := sparseDegradationReadAnswer(prefix + terminal)
				if err != nil || answer != "iPhone 16" {
					t.Fatalf("completed-message fallback guard replaced authoritative terminal answer: answer=%q err=%v", answer, err)
				}
			})
		}
	}
}

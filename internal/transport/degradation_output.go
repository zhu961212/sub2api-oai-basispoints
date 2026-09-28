package transport

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Native streams can deliver complete messages before a response.completed
// envelope that omits their text. Retain only completed assistant messages;
// drafts, deltas, reasoning and tools cannot become an account verdict.
type degradationCompletedOutput struct {
	items   []degradationCompletedItem
	byID    map[string]int
	byIndex map[int]int
	blocked bool
}

type degradationCompletedItem struct {
	index int
	kind  string
	role  string
	text  string
}

func (output *degradationCompletedOutput) consume(event string, payload map[string]any) error {
	const done = "response.output_item.done"
	kind := protocol.StringValue(payload["type"])
	if event != done && kind != done {
		return nil
	}
	if (event != "" && event != "message" && event != done) || (kind != "" && kind != done) {
		return fmt.Errorf("upstream output item has conflicting event types")
	}
	item, ok := payload["item"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid upstream completed output item")
	}
	if err := degradationResponseError(item); err != nil {
		return err
	}
	itemKind, role := protocol.StringValue(item["type"]), protocol.StringValue(item["role"])
	assistant := itemKind == "message" && role == "assistant"
	if assistant && protocol.StringValue(item["status"]) != "completed" {
		return fmt.Errorf("upstream output item did not complete")
	}
	if assistant && degradationContentBlocksFallback(item) {
		output.blocked = true
	}
	id := protocol.StringValue(item["id"])
	index, indexed := relayOutputIndex(payload["output_index"])
	if id == "" || !indexed {
		if assistant {
			return fmt.Errorf("upstream output item identity is invalid")
		}
		return nil
	}
	text := ""
	if assistant {
		text = strings.TrimSpace(outputItemText(item))
	}
	if previous, exists := output.byID[id]; exists {
		saved := output.items[previous]
		if saved.index != index || saved.kind != itemKind || saved.role != role || saved.text != text {
			return fmt.Errorf("upstream output item contains conflicting completed answers")
		}
		return nil
	}
	if _, exists := output.byIndex[index]; exists {
		return fmt.Errorf("upstream output item contains conflicting identities")
	}
	if output.byID == nil {
		output.byID = make(map[string]int)
		output.byIndex = make(map[int]int)
	}
	output.byID[id], output.byIndex[index] = len(output.items), len(output.items)
	output.items = append(output.items, degradationCompletedItem{index: index, kind: itemKind, role: role, text: text})
	return nil
}

// Call only after a successful response terminal. A final answer is always
// authoritative, including a refusal; earlier message text cannot replace it.
// The normal answer validator still checks every terminal output item's status.
func (output *degradationCompletedOutput) fill(response map[string]any) {
	if len(output.items) == 0 || strings.TrimSpace(responsesOutputText(response)) != "" {
		return
	}
	if output.blocked {
		return
	}
	if value, exists := response["output"]; exists && value != nil {
		items, ok := value.([]any)
		if !ok {
			return
		}
		for _, value := range items {
			item, ok := value.(map[string]any)
			if !ok {
				return
			}
			role := protocol.StringValue(item["role"])
			if protocol.StringValue(item["type"]) != "message" || (role != "" && role != "assistant") {
				continue
			}
			if degradationContentBlocksFallback(item) {
				return
			}
		}
	}
	indices := make([]int, 0, len(output.items))
	for index := range output.byIndex {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	answers := make([]string, 0, len(indices))
	for _, index := range indices {
		if text := output.items[output.byIndex[index]].text; text != "" {
			answers = append(answers, text)
		}
	}
	if len(answers) == 0 {
		return
	}
	response["output_text"] = strings.Join(answers, "\n")
}

// Refusals and malformed explicit message content cannot be discarded just
// because another completed message supplies usable text. They block only
// sparse-terminal recovery; an explicit terminal answer remains authoritative.
func degradationContentBlocksFallback(item map[string]any) bool {
	content, supplied := item["content"]
	if !supplied {
		return false
	}
	parts, ok := content.([]any)
	if !ok {
		return true
	}
	for _, value := range parts {
		part, ok := value.(map[string]any)
		if !ok || protocol.StringValue(part["type"]) == "refusal" {
			return true
		}
	}
	return false
}

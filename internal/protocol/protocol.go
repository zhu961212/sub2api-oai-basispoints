package protocol

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	transportName       = "run_officejs"
	transportAlias      = "functions.run_officejs"
	transportRetryHint  = "The previous run_officejs relay was malformed. Retry once with exactly one outer run_officejs call; code is JSON text containing one catalog-tool object, not JavaScript."
	toolCatalogPrefix   = "This request is relayed by an external Responses API client, not by the live Excel workbook. The native run_officejs function is a transport endpoint owned by this proxy. The proxy intercepts it before execution, so it never runs Office code or changes the workbook."
	toolCatalogReminder = "Reminder: use the outer native run_officejs transport; put exactly one JSON object as JSON text in code. The inner name must be one catalog client tool and must never be run_officejs or functions.run_officejs."
)

type toolSpec struct {
	Key       string
	Name      string
	Namespace string
	Type      string
	Spec      map[string]any
	Ambiguous bool
}

// RememberResponseContext links a Responses response ID to the session scope
// that produced it. Follow-up requests often carry only previous_response_id
// and omit both the full history and the tool catalog.
func RememberResponseContext(source, response map[string]any) {
	if ClassifyResponseTerminal("", response).Failed() {
		return
	}
	if status := stringValue(response["status"]); status != "" && status != "completed" {
		return
	}
	responseID := stringValue(response["id"])
	if responseID == "" {
		return
	}
	namespace := cacheNamespaceForSource(source)
	if namespace == "" {
		namespace = "response:" + shortHash(responseID)
		source["__bps_response_scope"] = namespace
	}
	tools := sourceTools(source)
	// Prepared requests already published their catalog before going upstream.
	// A delayed response must not restore that snapshot over a newer request.
	// Still register anonymous response scopes, or restore an evicted entry.
	_, frozen := source["__bps_effective_tools"]
	storeToolCatalog(source, tools, !frozen)
	responseContextCache.put(responseID, namespace, len(namespace)+16, true)
}

func rememberedResponseContext(responseID string) string {
	value, _ := responseContextCache.get(responseID)
	namespace, _ := value.(string)
	return namespace
}

func iterToolValues(tools any, namespace string, callback func(toolSpec)) {
	list, ok := tools.([]any)
	if !ok {
		return
	}
	for _, value := range list {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.TrimSpace(stringValue(tool["name"]))
		if (toolType == "function" || toolType == "custom") && name != "" {
			scope := namespace
			if own := stringValue(tool["namespace"]); own != "" {
				if scope != "" {
					scope += "."
				}
				scope += own
			}
			key := name
			if scope != "" {
				key = scope + "." + name
			}
			callback(toolSpec{Key: key, Name: name, Namespace: scope, Type: toolType, Spec: tool})
		}
		if toolType == "namespace" && name != "" {
			if namespace != "" {
				name = namespace + "." + name
			}
			iterToolValues(namespaceChildren(tool), name, callback)
		}
	}
}

func clientToolSpecs(source map[string]any) map[string]toolSpec {
	result := map[string]toolSpec{}
	if strings.EqualFold(strings.TrimSpace(stringValue(source["tool_choice"])), "none") {
		return result
	}
	iterToolValues(sourceTools(source), "", func(spec toolSpec) {
		if prior, exists := result[spec.Key]; exists {
			spec.Ambiguous = prior.Ambiguous || prior.Type != spec.Type || !jsonValuesEqual(prior.Spec, spec.Spec)
		}
		result[spec.Key] = spec
	})
	return result
}

// Only functions is a host presentation alias. Other namespaces are never
// inferred from a leaf name, so collaboration and client namespaces stay exact.
func resolveClientTool(specs map[string]toolSpec, name string) (toolSpec, bool) {
	if spec, exists := specs[name]; exists {
		return spec, !spec.Ambiguous
	}
	alias := strings.TrimPrefix(name, "functions.")
	if alias == name {
		alias = "functions." + name
	}
	spec, exists := specs[alias]
	return spec, exists && !spec.Ambiguous
}

func sourceTools(source map[string]any) any {
	if tools, frozen := source["__bps_effective_tools"]; frozen {
		return tools
	}
	declared, explicit := source["tools"]
	additions := additionalRequestTools(source)
	if !explicit && len(additions) > 0 {
		return mergeRememberedToolCatalog(source, additions)
	}
	tools := declared
	if !explicit {
		tools = rememberedToolCatalog(source)
	}
	if len(additions) > 0 {
		tools = mergeToolCatalog(tools, additions)
	}
	if explicit || len(additions) > 0 {
		rememberToolCatalog(source, tools)
	}
	return tools
}

func rememberToolCatalog(source map[string]any, tools any) {
	storeToolCatalog(source, tools, true)
}

func storeToolCatalog(source map[string]any, tools any, replace bool) {
	key := cacheNamespaceForSource(source)
	if key == "" {
		return
	}
	lock := catalogWriteLock(key)
	lock.Lock()
	defer lock.Unlock()
	if _, exists := toolCatalogCache.get(key); exists && !replace {
		return
	}
	storeCatalogSnapshot(key, tools, replace)
}

// Serialize same-session discovery deltas so updates cannot discard each other.
// Expensive merging and snapshot creation never hold the shared cache mutex.
func mergeRememberedToolCatalog(source map[string]any, additions []any) any {
	key := cacheNamespaceForSource(source)
	if key == "" {
		return mergeToolCatalog(nil, additions)
	}
	lock := catalogWriteLock(key)
	lock.Lock()
	defer lock.Unlock()
	base, _ := toolCatalogCache.get(key)
	merged := mergeToolCatalog(base, additions)
	storeCatalogSnapshot(key, merged, true)
	return merged
}

func rememberedToolCatalog(source map[string]any) any {
	key := cacheNamespaceForSource(source)
	if key == "" {
		return nil
	}
	snapshot, _ := toolCatalogCache.get(key)
	return cloneProtocolCacheValue(snapshot)
}

func cloneJSONValue(value any) any {
	if value == nil {
		return nil
	}
	raw, _ := json.Marshal(value)
	var copy any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&copy)
	return copy
}

func messageItem(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": []any{map[string]any{"type": contentType, "text": text}},
	}
}

func clientToolProtocolInstructions(source map[string]any) string {
	specs := clientToolSpecs(source)
	if len(specs) == 0 {
		return "This request is relayed by an external Responses API client, not by the live Excel workbook. Do not call server-injected Excel, Office, connector, or workbook tools. Return the answer as assistant text."
	}
	catalog := make([]string, 0, len(specs))
	iterToolValues(sourceTools(source), "", func(spec toolSpec) {
		line := "- " + spec.Key + " (" + spec.Type + ")"
		if spec.Type == "function" {
			if parameters := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema"); parameters != nil {
				line += ". Its arguments are an object with " + describeParameterNames(parameters)
			}
			line += describeToolContract(spec) + "."
		} else {
			line += ". It receives raw text in code.args; the proxy emits it as custom_tool_call.input." + describeToolContract(spec)
		}
		catalog = append(catalog, line)
	})
	catalogText := strings.Join(catalog, "\n")
	return toolCatalogPrefix + " Other native server-injected Excel, Office, connector, workbook, list_skills, and web-search tools are unavailable. Never claim shell, filesystem, or workspace access is unavailable when the catalog contains a suitable tool. For repository inspection, invoke a suitable declared client tool through run_officejs. Transport has two layers and they must not be mixed: the outer native tool is run_officejs (some hosts display it as functions.run_officejs); the inner code value is JSON text containing exactly one compact JSON object for one catalog client tool. Outer arguments include summary, extended_summary, destructive=false, and references=[]." + clientRelayContract + " Do not put JavaScript, OfficeJS, a second run_officejs envelope, or a functions.run_officejs wrapper directly inside code. The field is named code for compatibility; it is not JavaScript. Raw source for a declared custom tool belongs only in the inner args string. Serialize the complete inner object before placing it there, including backslashes and quotes. The proxy converts this native call into the real client tool call, then replays the original run_officejs identity with the client tool result on the next request. Interpret that result as the named client tool output. Never repeat a tool request whose output is already present. Available client tools:\n" + catalogText + "\n" + toolCatalogReminder +
		" Remember: call the outer native run_officejs tool once; put exactly one catalog-tool JSON object in its code field." +
		" The available catalog is authoritative for tool names and arguments."
}

func describeParameterNames(parameters map[string]any) string {
	properties := objectValue(parameters["properties"])
	if len(properties) == 0 {
		return "the arguments required by the client"
	}
	required := map[string]bool{}
	if list, ok := parameters["required"].([]any); ok {
		for _, value := range list {
			required[stringValue(value)] = true
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		suffix := "optional"
		if required[name] {
			suffix = "required"
		}
		names = append(names, name+" ("+suffix+")")
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return strings.Join(names, ", ")
}

func clientToolProtocolReminder(source map[string]any) string {
	specs := clientToolSpecs(source)
	if len(specs) == 0 {
		return ""
	}
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	// Small deterministic ordering without importing sort in every caller.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	reminder := toolCatalogReminder + clientRelayContract + " When the task requires a tool, make the call using the active catalog. Client tools (exact code.tool allowlist): " + string(jsonBytes(names)) + ". Other native tools are unavailable."
	for _, name := range names {
		spec := specs[name]
		if spec.Type == "custom" {
			reminder += " Custom tool " + name + " takes raw text in code.args; the proxy emits it as custom_tool_call.input. Do not wrap its input in an object."
			reminder += " Exact relay JSON shape (replace the args text with this tool's raw input): " + string(jsonBytes(map[string]any{"tool": name, "args": "<raw custom-tool input>"})) + "."
		}
	}
	return reminder
}

func firstMap(object map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value := objectValue(object[key]); value != nil {
			return value
		}
	}
	return nil
}

func objectValue(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func stripClientMetadata(item map[string]any) map[string]any {
	if _, exists := item["internal_chat_message_metadata_passthrough"]; !exists {
		return item
	}
	copy := cloneObject(item)
	delete(copy, "internal_chat_message_metadata_passthrough")
	return copy
}

func cloneObject(object map[string]any) map[string]any {
	return objectValue(cloneJSONValue(object))
}

func nativeCacheKey(namespace, callID string) string {
	return namespace + "\x00" + callID
}

func rememberNativeCallInNamespace(namespace string, item map[string]any) {
	callID := stringValue(item["call_id"])
	if namespace == "" || callID == "" {
		return
	}
	key := nativeCacheKey(namespace, callID)
	owned, weight, ok := protocolCacheSnapshot(item)
	if !ok {
		nativeCallCache.forget(key)
		return
	}
	nativeCallCache.put(key, owned, weight, true)
}

func rememberedNativeCallInNamespace(namespace, callID string) map[string]any {
	if namespace == "" || callID == "" {
		return nil
	}
	snapshot, _ := nativeCallCache.get(nativeCacheKey(namespace, callID))
	return objectValue(cloneProtocolCacheValue(snapshot))
}

func nativeCallMatchesClientItem(native, client map[string]any, specs map[string]toolSpec) bool {
	if native == nil || client == nil || stringValue(native["call_id"]) != stringValue(client["call_id"]) {
		return false
	}
	if clientID := stringValue(client["id"]); clientID != "" && clientID != stringValue(native["id"]) {
		return false
	}
	inner := transportEnvelope(native)
	if inner == nil {
		return false
	}
	toolName := recoveryEnvelopeName(inner)
	clientName := clientToolKey(client)
	if len(specs) > 0 {
		nativeSpec, nativeOK := resolveClientTool(specs, toolName)
		clientSpec, clientOK := resolveClientTool(specs, clientName)
		if !nativeOK || !clientOK || nativeSpec.Key != clientSpec.Key {
			return false
		}
	} else if toolName != clientName {
		return false
	}
	payload, ok := envelopePayload(inner)
	if !ok {
		return false
	}
	if stringValue(client["type"]) == "custom_tool_call" {
		want, wantOK := client["input"].(string)
		got, gotOK := payload.(string)
		return wantOK && gotOK && want == got
	}
	want := parseArguments(client["arguments"])
	return want != nil && jsonValuesEqual(want, parseArguments(payload))
}

func functionItemID(callID string) string {
	if callID == "" {
		return ""
	}
	id := callID
	if !strings.HasPrefix(id, "fc_") {
		id = "fc_" + id
	}
	if len(id) > 64 {
		id = "fc_" + shortHash(callID)[:48]
	}
	return id
}

func fallbackTransportCall(item map[string]any) map[string]any {
	name := clientToolKey(item)
	callID := stringValue(item["call_id"])
	if callID == "" {
		callID = "call_bp_" + shortHash(fmt.Sprintf("%v", time.Now().UnixNano()))
	}
	inner := map[string]any{"tool": name}
	if stringValue(item["type"]) == "custom_tool_call" {
		input := item["input"]
		if _, ok := input.(string); !ok && input != nil {
			input = string(jsonBytes(input))
		}
		inner["args"] = input
	} else {
		arguments := parseArguments(item["arguments"])
		if arguments == nil {
			arguments = map[string]any{}
		}
		inner["args"] = arguments
	}
	outerArguments := map[string]any{
		"summary":          "Run client tool " + name,
		"extended_summary": "Relay " + name + " through the external client",
		"code":             string(jsonBytes(inner)),
		"destructive":      false,
		"references":       []any{},
	}
	return map[string]any{
		"type":      "function_call",
		"id":        functionItemID(callID),
		"call_id":   callID,
		"name":      transportName,
		"arguments": string(jsonBytes(outerArguments)),
		"status":    "completed",
	}
}

func translateInputItems(rawInput any, allowed map[string]toolSpec) []any {
	return translateInputItemsInNamespace(rawInput, allowed, "")
}

func translateInputItemsInNamespace(rawInput any, allowed map[string]toolSpec, namespace string) []any {
	if text, ok := rawInput.(string); ok {
		return []any{messageItem("user", text)}
	}
	items, ok := rawInput.([]any)
	if !ok {
		return []any{}
	}
	result := make([]any, 0, len(items))
	origins := map[string]string{}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		item = stripClientMetadata(item)
		itemType := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
		if itemType == "additional_tools" || itemType == "tool_search_output" {
			// Client-side catalog records are consumed by sourceTools and are not
			// valid upstream conversation items.
			continue
		}
		if itemType == "function_call" || itemType == "custom_tool_call" {
			callID := stringValue(item["call_id"])
			native := rememberedNativeCallInNamespace(namespace, callID)
			// A repeated opening message or a shared prompt_cache_key can make
			// two independent sessions use the same namespace. Match the cached
			// native call by tool name and arguments before replaying it.
			if native != nil && !nativeCallMatchesClientItem(native, item, allowed) {
				native = nil
			}
			if native != nil {
				if callID != "" {
					origins[callID] = stringValue(native["name"])
				}
				result = append(result, native)
				continue
			}
			name := clientToolKey(item)
			if name == transportName || name == transportAlias {
				rememberNativeCallInNamespace(namespace, item)
				if callID != "" {
					origins[callID] = transportName
				}
				result = append(result, item)
				continue
			}
			// Historical calls are data, not new tool requests. Rebuild their
			// native envelope even if the active catalog has since changed.
			if name != "" {
				if callID != "" {
					origins[callID] = transportName
				}
				result = append(result, fallbackTransportCall(item))
				continue
			}
			result = append(result, item)
			continue
		}
		if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			callID := stringValue(item["call_id"])
			native := rememberedNativeCallInNamespace(namespace, callID)
			if origins[callID] == transportName || native != nil {
				if origins[callID] == "" && native != nil {
					result = append(result, native)
					origins[callID] = transportName
				}
				copy := cloneObject(item)
				copy["type"] = "function_call_output"
				copy["id"] = functionItemID(callID)
				if toolOutputIsEmpty(copy["output"]) {
					copy["output"] = "(tool call succeeded with no output)"
				}
				result = append(result, copy)
			} else {
				result = append(result, item)
			}
			continue
		}
		if itemType == "reasoning" {
			if encrypted := stringValue(item["encrypted_content"]); encrypted != "" {
				result = append(result, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted})
			}
			continue
		}
		if itemType == "item_reference" {
			continue
		}
		result = append(result, item)
	}
	return result
}

func clientToolKey(item map[string]any) string {
	name := stringValue(item["name"])
	if namespace := stringValue(item["namespace"]); namespace != "" && !strings.HasPrefix(name, namespace+".") {
		return namespace + "." + name
	}
	return name
}

func toolOutputIsEmpty(value any) bool {
	if value == nil {
		return true
	}
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) == ""
}

func itemText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if list, ok := value.([]any); ok {
		var builder strings.Builder
		for _, part := range list {
			if text := stringValue(part); text != "" {
				builder.WriteString(text)
				continue
			}
			if object := objectValue(part); object != nil {
				builder.WriteString(stringValue(object["text"]))
			}
		}
		return builder.String()
	}
	return ""
}

func conversationKey(source map[string]any, translated []any) string {
	if explicit := explicitConversationKey(source); explicit != "" {
		return explicit
	}
	return conversationFingerprint(translated)
}

func explicitConversationKey(source map[string]any) string {
	for _, key := range []string{"prompt_cache_key", "promptCacheKey"} {
		if value := stringValue(source[key]); value != "" {
			return value
		}
	}
	return ""
}

// nativeCallNamespace keeps the replay cache scoped to one conversation. Call IDs
// such as "call_1" are commonly reused by different sessions; a global call-ID-only
// cache can otherwise replay another user's native transport item.
func nativeCallNamespace(source map[string]any) string {
	return cacheNamespaceForSource(source)
}

// prompt_cache_key and repeated input are not conversation identities. Never
// share replay/catalog state merely because two users sent identical prompts.
func conversationIdentity(source map[string]any) string {
	if scope := stringValue(source["__bps_session_scope"]); scope != "" {
		return "host:" + scope
	}
	if value := stringValue(source["conversation"]); value != "" {
		return "conversation:" + value
	}
	if conversation := objectValue(source["conversation"]); conversation != nil {
		if value := stringValue(conversation["id"]); value != "" {
			return "conversation:" + value
		}
	}
	for _, field := range []string{"", "metadata", "client_metadata"} {
		values := source
		if field != "" {
			values = objectValue(source[field])
		}
		for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId", "thread_id", "threadId", "chat_id", "chatId", "client_session_id", "clientSessionId"} {
			if value := stringValue(values[key]); value != "" {
				return "client:" + value
			}
		}
	}
	return ""
}

func cacheNamespaceForSource(source map[string]any) string {
	if identity := conversationIdentity(source); identity != "" {
		return "session:" + shortHash(identity)
	}
	for _, field := range []string{"previous_response_id", "previousResponseId"} {
		if previous := stringValue(source[field]); previous != "" {
			// The cached value is already a namespace: do not prepend id: again.
			if namespace := rememberedResponseContext(previous); namespace != "" {
				return namespace
			}
		}
	}
	return stringValue(source["__bps_response_scope"])
}

func firstConversationObject(raw any) map[string]any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var first map[string]any
	for _, value := range items {
		object := objectValue(value)
		if object == nil {
			continue
		}
		if first == nil {
			first = object
		}
		if strings.EqualFold(stringValue(object["role"]), "user") {
			return object
		}
	}
	return first
}

func conversationFingerprint(items []any) string {
	for _, value := range items {
		if object := objectValue(value); object != nil {
			return shortHash(string(jsonBytes(object)))
		}
	}
	return "anonymous"
}

func turnState(rawInput any) (string, string) {
	items, ok := rawInput.([]any)
	if !ok {
		return shortHash(string(jsonBytes(rawInput))), "1"
	}
	lastUser := -1
	for index, value := range items {
		if object := objectValue(value); object != nil && strings.EqualFold(stringValue(object["role"]), "user") {
			lastUser = index
		}
	}
	if lastUser < 0 {
		// 没有任何 user 回合（含空 input）时不能对切片取前缀：items[:1] 会直接 panic。
		if len(items) == 0 {
			return shortHash("empty-input"), "1"
		}
		lastUser = 0
	}
	prefix := items[:lastUser+1]
	fingerprint := shortHash(string(jsonBytes(prefix)))
	iteration := 1
	for _, value := range items[lastUser+1:] {
		if object := objectValue(value); object != nil {
			typeName := stringValue(object["type"])
			if typeName == "function_call_output" || typeName == "custom_tool_call_output" {
				iteration++
			}
		}
	}
	return fingerprint, fmt.Sprintf("%d", iteration)
}

func shortHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

var urlNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

func uuidV5(name string) string {
	hash := sha1.New()
	_, _ = hash.Write(urlNamespace[:])
	_, _ = hash.Write([]byte(name))
	digest := hash.Sum(nil)
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func appendBeforeCompaction(items []any, injected []any) []any {
	if len(items) > 0 {
		if last := objectValue(items[len(items)-1]); last != nil && stringValue(last["type"]) == "compaction_trigger" {
			result := append([]any{}, items[:len(items)-1]...)
			result = append(result, injected...)
			return append(result, items[len(items)-1])
		}
	}
	return append(items, injected...)
}

func prependBeforeCompaction(items []any, prefix []any) []any {
	result := append([]any{}, prefix...)
	return append(result, items...)
}

func prepareResponsesBody(source map[string]any, cfg Config) (map[string]any, error) {
	return prepareResponsesBodyWithImages(source, cfg, false)
}

func prepareResponsesBodyWithImages(source map[string]any, cfg Config, allowInlineImages bool) (map[string]any, error) {
	if err := validateRequestCapabilities(source, allowInlineImages); err != nil {
		return nil, err
	}
	structuredInstructions, err := prepareStructuredOutput(source)
	if err != nil {
		return nil, err
	}
	if _, frozen := source["__bps_effective_tools"]; !frozen {
		source["__bps_effective_tools"] = cloneJSONValue(sourceTools(source))
	}
	cacheNamespace := nativeCallNamespace(source)
	inputItems := translateInputItemsInNamespace(source["input"], clientToolSpecs(source), cacheNamespace)
	historyRoot := conversationFingerprint(inputItems)
	prologue := []any{}
	if instructions := stringValue(source["instructions"]); instructions != "" {
		prologue = append(prologue, messageItem("developer", instructions))
	}
	// 工具目录紧跟 instructions 前置，让整段请求前缀保持字节稳定，
	// 上游的 prompt cache 才能逐轮复用（目录放到末尾会把可复用前缀压缩到首字节）。
	prologue = append(prologue, messageItem("developer", clientToolProtocolInstructions(source)))
	if note := RequestCapabilityInstructions(source); note != "" {
		prologue = append(prologue, messageItem("developer", note))
	}
	if structuredInstructions != "" {
		prologue = append(prologue, messageItem("developer", structuredInstructions))
	}
	inputItems = prependBeforeCompaction(inputItems, prologue)
	if reminder := clientToolProtocolReminder(source); reminder != "" {
		// 只在生成位置附近保留一条短提醒：模型需要这个 recency 才会遵守两段式传输协议。
		inputItems = appendBeforeCompaction(inputItems, []any{messageItem("developer", reminder)})
	}

	output := map[string]any{
		"model":              cfg.UpstreamModelFor(stringValue(source["model"])),
		"model_selection":    "explicit",
		"stream":             true,
		"store":              false,
		"input":              inputItems,
		"reasoning_effort":   reasoningEffortFromSource(source),
		"context_management": contextManagement(source),
	}
	if cacheKey := explicitConversationKey(source); cacheKey != "" {
		output["prompt_cache_key"] = cacheKey
	}
	metadata := map[string]any{}
	if rawMetadata := objectValue(source["metadata"]); rawMetadata != nil {
		for key, value := range rawMetadata {
			if key == "turn_id" || key == "task_id" || key == "agent_iteration" {
				continue
			}
			switch typed := value.(type) {
			case string:
				metadata[key[:minInt(len(key), 64)]] = typed[:minInt(len(typed), 512)]
			case json.Number, bool, float64:
				text := fmt.Sprint(typed)
				metadata[key[:minInt(len(key), 64)]] = text[:minInt(len(text), 512)]
			}
		}
	}
	turnFingerprint, iteration := turnState(source["input"])
	conversation := conversationIdentity(source)
	if conversation == "" {
		conversation = historyRoot
	}
	metadata["task_id"] = uuidV5("cpa-oai-basispoints/" + conversation)
	metadata["turn_id"] = uuidV5("cpa-oai-basispoints/" + conversation + "/turn/" + turnFingerprint)
	metadata["agent_iteration"] = iteration
	if cfg.ToolsVersionID != "" {
		metadata["bps_tools_version_id"] = cfg.ToolsVersionID
	}
	output["metadata"] = metadata
	return output, nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func reasoningEffortFromSource(source map[string]any) string {
	if reasoning := objectValue(source["reasoning"]); reasoning != nil {
		return normalizeEffort(reasoning["effort"])
	}
	return normalizeEffort(source["reasoning_effort"])
}

func contextManagement(source map[string]any) []any {
	if value, ok := source["context_management"].([]any); ok {
		return value
	}
	return []any{map[string]any{"type": "compaction", "compact_threshold": 200000}}
}

func decodeTransportCode(value any) map[string]any {
	if object := objectValue(value); object != nil {
		return unambiguousEnvelope(object)
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return recoverTransportEnvelope(text)
}

func isTransportName(name string) bool {
	return name == transportName || name == transportAlias
}

func parseArguments(value any) map[string]any {
	if object := objectValue(value); object != nil {
		return object
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil
	}
	// Function payload strings have the same ambiguity rules as the outer
	// relay. Do not silently choose the last duplicate argument key.
	value, _, valid := relayJSONValue(text, true)
	if !valid {
		return nil
	}
	return objectValue(value)
}

func transportEnvelope(native map[string]any) map[string]any {
	if stringValue(native["type"]) != "function_call" || !isTransportName(stringValue(native["name"])) {
		return nil
	}
	arguments := parseTransportArguments(native["arguments"])
	if arguments == nil {
		return nil
	}
	envelope := decodeTransportCode(arguments["code"])
	for depth := 0; depth < 2 && envelope != nil && isTransportName(recoveryEnvelopeName(envelope)); depth++ {
		payload, ok := envelopePayload(envelope)
		if !ok {
			return nil
		}
		nestedArguments := parseTransportArguments(payload)
		if nestedArguments == nil {
			return nil
		}
		envelope = decodeTransportCode(nestedArguments["code"])
	}
	if envelope != nil && isTransportName(recoveryEnvelopeName(envelope)) {
		return nil
	}
	return envelope
}

func schemaMatches(value any, schema map[string]any) bool {
	if len(schema) == 0 {
		return true
	}
	if value == nil && schema["nullable"] == true {
		return true
	}
	if constant, ok := schema["const"]; ok && !jsonValuesEqual(value, constant) {
		return false
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		matched := false
		for _, option := range enum {
			if jsonValuesEqual(option, value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if alternatives, ok := schema["allOf"].([]any); ok {
		for _, alternative := range alternatives {
			if !schemaMatches(value, objectValue(alternative)) {
				return false
			}
		}
	}
	if alternatives, ok := schema["anyOf"].([]any); ok && len(alternatives) > 0 {
		matched := false
		for _, alternative := range alternatives {
			if schemaMatches(value, objectValue(alternative)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if alternatives, ok := schema["oneOf"].([]any); ok && len(alternatives) > 0 {
		matches := 0
		for _, alternative := range alternatives {
			if schemaMatches(value, objectValue(alternative)) {
				matches++
			}
		}
		if matches != 1 {
			return false
		}
	}
	if denied := objectValue(schema["not"]); denied != nil && schemaMatches(value, denied) {
		return false
	}
	if alternatives, ok := schema["type"].([]any); ok {
		for _, alternative := range alternatives {
			copy := cloneObject(schema)
			delete(copy, "anyOf")
			delete(copy, "oneOf")
			delete(copy, "allOf")
			delete(copy, "not")
			copy["type"] = alternative
			if schemaMatches(value, copy) {
				return true
			}
		}
		return false
	}
	switch stringValue(schema["type"]) {
	case "object":
		object := objectValue(value)
		if object == nil {
			return false
		}
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				if _, exists := object[stringValue(name)]; !exists {
					return false
				}
			}
		} else if required, ok := schema["required"].([]string); ok {
			for _, name := range required {
				if _, exists := object[name]; !exists {
					return false
				}
			}
		}
		properties := objectValue(schema["properties"])
		for key, nested := range object {
			var nestedSchema map[string]any
			if properties != nil {
				nestedSchema = objectValue(properties[key])
			}
			if nestedSchema == nil {
				if schema["additionalProperties"] == false {
					return false
				}
				if additional := objectValue(schema["additionalProperties"]); additional != nil && !schemaMatches(nested, additional) {
					return false
				}
				continue
			}
			if !schemaMatches(nested, nestedSchema) {
				return false
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		if itemSchema := objectValue(schema["items"]); itemSchema != nil {
			for _, item := range items {
				if !schemaMatches(item, itemSchema) {
					return false
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return false
		}
	case "integer", "number":
		number, ok := parseSchemaNumber(value)
		if !ok || stringValue(schema["type"]) == "integer" && number.exponent.Sign() < 0 {
			return false
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	case "null":
		if value != nil {
			return false
		}
	}
	return numericSchemaMatches(value, schema)
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if bytes.Equal(leftJSON, rightJSON) {
		return true
	}
	leftValue, _, leftOK := recoveryJSONValue(string(leftJSON), true)
	rightValue, _, rightOK := recoveryJSONValue(string(rightJSON), true)
	return leftOK && rightOK && decodedJSONValuesEqual(leftValue, rightValue)
}

func decodedJSONValuesEqual(left, right any) bool {
	switch value := left.(type) {
	case nil:
		return right == nil
	case bool:
		other, ok := right.(bool)
		return ok && value == other
	case string:
		other, ok := right.(string)
		return ok && value == other
	case json.Number:
		other, ok := right.(json.Number)
		if !ok {
			return false
		}
		coefficient, exponent := normalizedJSONNumber(value)
		otherCoefficient, otherExponent := normalizedJSONNumber(other)
		return coefficient == otherCoefficient && exponent.Cmp(otherExponent) == 0
	case []any:
		other, ok := right.([]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for index, item := range value {
			if !decodedJSONValuesEqual(item, other[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		other, ok := right.(map[string]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for key, item := range value {
			otherItem, exists := other[key]
			if !exists || !decodedJSONValuesEqual(item, otherItem) {
				return false
			}
		}
		return true
	}
	return false
}

// JSON Schema compares numbers by mathematical value. Keep the decimal scale
// separate instead of expanding powers: even huge exponents remain bounded by
// the input length, and adjacent large integers never pass through float64.
// The caller supplies a number already validated by the JSON decoder.
func normalizedJSONNumber(number json.Number) (string, *big.Int) {
	text := string(number)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	exponent := new(big.Int)
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent.SetString(text[index+1:], 10)
		text = text[:index]
	}
	fractionalDigits := 0
	if index := strings.IndexByte(text, '.'); index >= 0 {
		fractionalDigits = len(text) - index - 1
		text = text[:index] + text[index+1:]
	}
	text = strings.TrimLeft(text, "0")
	if text == "" {
		return "0", new(big.Int)
	}
	coefficient := strings.TrimRight(text, "0")
	exponent.Add(exponent, big.NewInt(int64(len(text)-len(coefficient)-fractionalDigits)))
	return sign + coefficient, exponent
}

func extractNativeClientToolCall(response map[string]any, source map[string]any) (map[string]any, bool) {
	output, ok := response["output"].([]any)
	if !ok {
		return nil, false
	}
	var native map[string]any
	transportCount := 0
	for _, value := range output {
		item := objectValue(value)
		if item == nil {
			continue
		}
		typeName := stringValue(item["type"])
		if typeName == "function_call" || typeName == "custom_tool_call" {
			if isTransportName(stringValue(item["name"])) {
				native = item
				transportCount++
			}
		}
	}
	if native == nil || transportCount != 1 {
		return nil, false
	}
	return extractNativeClientToolCallFromItem(native, source, true)
}

func extractNativeClientToolCallFromItem(native, source map[string]any, remember bool) (map[string]any, bool) {
	call, reason := decodeNativeClientToolCallFromItem(native, source, remember)
	return call, reason == ""
}

// Rejection reasons are static: never expose an upstream argument, custom
// input, or untrusted tool name in client-visible transport diagnostics.
func decodeNativeClientToolCallFromItem(native, source map[string]any, remember bool) (map[string]any, string) {
	specs := clientToolSpecs(source)
	allowedName := clientToolKey(native)
	inner := transportEnvelope(native)
	if isTransportName(allowedName) && inner == nil {
		return nil, malformedClientToolMessage
	}
	if inner != nil {
		allowedName = recoveryEnvelopeName(inner)
	}
	if allowedName == "" || isTransportName(allowedName) {
		return nil, malformedClientToolMessage
	}
	spec, exists := resolveClientTool(specs, allowedName)
	if !exists {
		if spec.Ambiguous {
			return nil, "Basis Points returned a client tool call with an ambiguous catalog declaration"
		}
		return nil, unknownClientToolMessage
	}
	callID := stringValue(native["call_id"])
	if callID == "" {
		callID = "call_bp_" + shortHash(string(jsonBytes(native)))[:24]
	}
	result := map[string]any{
		"type":    "function_call",
		"id":      stringValue(native["id"]),
		"call_id": callID,
		"name":    spec.Name,
	}
	if spec.Namespace != "" {
		result["namespace"] = spec.Namespace
	}
	if result["id"] == "" {
		result["id"] = functionItemID(callID)
	}
	if spec.Type == "custom" {
		input := any(nil)
		if inner != nil {
			input, _ = envelopePayload(inner)
		} else {
			input = native["input"]
		}
		if _, ok := input.(string); !ok {
			return nil, "Basis Points returned invalid client tool arguments: custom input must be raw text"
		}
		result["type"] = "custom_tool_call"
		result["input"] = input
	} else {
		var arguments any
		if inner != nil {
			arguments, _ = envelopePayload(inner)
		} else {
			arguments = native["arguments"]
		}
		parameters := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")
		parsed := parseArguments(arguments)
		if parsed == nil || !schemaMatches(parsed, parameters) {
			// 计划工具允许一次有界归一化：模型常把描述写成 title/description、
			// 把状态写成 done/todo。归一化后仍要通过声明的 JSON Schema 校验，
			// 否则这次调用按原样拒绝。
			normalized, ok := normalizeNativePlan(parsed, spec, specs)
			if !ok || !schemaMatches(normalized, parameters) {
				return nil, "Basis Points returned invalid client tool arguments that do not match the declared schema"
			}
			parsed = normalized
		}
		result["arguments"] = string(jsonBytes(parsed))
		// 标记函数参数为明文：Codex 协作工具（collaboration.spawn_agent /
		// send_message / followup_task）按目录里 encrypted:true 的参数声明
		// 处理调用；它们区分"显式空列表"与"缺失字段"，缺失时会把明文 message
		// 当密文发给子代理（客户端报 Encrypted function output content could
		// not be decrypted or decoded 并断流）。relay 信封的内容一定是明文；
		// 直接调用保留上游已有的非 null 加密元数据。
		if inner != nil {
			result["encrypted_function_args"] = []string{}
		} else if encrypted := native["encrypted_function_args"]; encrypted != nil {
			result["encrypted_function_args"] = encrypted
		}
	}
	if remember {
		namespace := nativeCallNamespace(source)
		cacheItem := native
		if !isTransportName(stringValue(native["name"])) {
			cacheItem = fallbackTransportCall(result)
		}
		if stringValue(native["call_id"]) == "" {
			cacheItem = objectValue(cloneJSONValue(native))
			cacheItem["call_id"] = callID
		}
		rememberNativeCallInNamespace(namespace, cacheItem)
	}
	return result, ""
}

func transformResponseBody(body []byte, source map[string]any) ([]byte, map[string]any, bool, error) {
	response, err := RawObject(body)
	if err != nil {
		return nil, nil, false, fail(502, "invalid_upstream_response", "Basis Points returned invalid JSON")
	}
	output, _ := response["output"].([]any)
	terminal := ClassifyResponseTerminal("", response)
	if status := stringValue(response["status"]); terminal.Failed() || status != "" && status != "completed" {
		if terminal.Failed() {
			NormalizeResponseFailure(map[string]any{"response": response}, terminal)
		}
		filtered := make([]any, 0, len(output))
		for _, value := range output {
			if !clientCallableItem(objectValue(value)) {
				filtered = append(filtered, value)
			}
		}
		response["output"] = filtered
		return jsonBytes(response), response, len(filtered) != len(output), nil
	}
	if err := validateStructuredResponse(response, source); err != nil {
		return nil, nil, false, err
	}
	replaced := make([]any, 0, len(output))
	changed := false
	for _, value := range output {
		item := objectValue(value)
		if !clientCallableItem(item) {
			replaced = append(replaced, value)
			continue
		}
		translated, reason := decodeNativeClientToolCallFromItem(item, source, false)
		if reason != "" {
			return nil, nil, false, fail(502, "invalid_tool_call", reason)
		}
		translated["status"] = "completed"
		replaced = append(replaced, translated)
		changed = true
	}
	RememberResponseContext(source, response)
	if changed {
		for _, value := range output {
			item := objectValue(value)
			if clientCallableItem(item) {
				_, _ = extractNativeClientToolCallFromItem(item, source, true)
			}
		}
		response["output"] = replaced
	}
	if text := objectValue(source["text"]); text != nil && HasStructuredOutput(source) {
		response["text"] = cloneObject(text)
	}
	return jsonBytes(response), response, changed, nil
}

func clientCallableItem(item map[string]any) bool {
	kind := stringValue(item["type"])
	return kind == "function_call" || kind == "custom_tool_call" || isTransportName(stringValue(item["name"]))
}

func syntheticStream(response map[string]any) []byte {
	if response == nil {
		return nil
	}
	response = cloneObject(response)
	terminal := ClassifyResponseTerminal("", response)
	if terminal == TerminalNone {
		terminal = TerminalCompleted
		if status := stringValue(response["status"]); status != "" && status != "completed" {
			terminal = TerminalInvalid
		}
	}
	event := "response.completed"
	if terminal.Failed() {
		event = NormalizeResponseFailure(map[string]any{"response": response}, terminal)
		output, _ := response["output"].([]any)
		filtered := make([]any, 0, len(output))
		for _, value := range output {
			if !clientCallableItem(objectValue(value)) {
				filtered = append(filtered, value)
			}
		}
		response["output"] = filtered
	} else {
		response["status"] = "completed"
	}
	created := cloneObject(response)
	created["status"] = "in_progress"
	created["output"] = []any{}
	for _, key := range []string{"error", "incomplete_details", "status_code", "http_status", "ok", "success"} {
		delete(created, key)
	}
	var builder strings.Builder
	writeSSE(&builder, "response.created", map[string]any{"type": "response.created", "response": created})
	writeSSE(&builder, "response.in_progress", map[string]any{"type": "response.in_progress", "response": created})
	if output, ok := response["output"].([]any); ok {
		for index, value := range output {
			item := objectValue(value)
			if item == nil {
				continue
			}
			writeSSE(&builder, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
			if stringValue(item["type"]) == "function_call" {
				arguments := stringValue(item["arguments"])
				if arguments != "" {
					writeSSE(&builder, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": stringValue(item["id"]), "arguments": arguments})
				}
			} else if stringValue(item["type"]) == "custom_tool_call" {
				input := stringValue(item["input"])
				if input != "" {
					writeSSE(&builder, "response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": index, "item_id": stringValue(item["id"]), "input": input})
				}
			}
			writeSSE(&builder, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
		}
	}
	writeSSE(&builder, event, map[string]any{"type": event, "response": response})
	builder.WriteString("data: [DONE]\n\n")
	return []byte(builder.String())
}

func writeSSE(builder *strings.Builder, event string, value any) {
	builder.WriteString("event: ")
	builder.WriteString(event)
	builder.WriteString("\ndata: ")
	builder.Write(jsonBytes(value))
	builder.WriteString("\n\n")
}

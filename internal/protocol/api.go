package protocol

// The implementation was intentionally kept in a pure package so it can be
// tested without starting a go-plugin process.
func PrepareResponsesBody(source map[string]any, cfg Config) (map[string]any, error) {
	return prepareResponsesBody(source, cfg)
}

// PrepareResponsesBodyForImageUpload freezes turn/task metadata from original
// image content before upload assigns short-lived file IDs. The returned body
// is an intermediate form: upload its images and validate it before forwarding.
func PrepareResponsesBodyForImageUpload(source map[string]any, cfg Config) (map[string]any, error) {
	body, err := prepareResponsesBodyWithImages(source, cfg, true)
	if err != nil {
		return nil, err
	}
	// Copy containers, retaining immutable strings instead of marshaling and
	// duplicating large base64 payloads. Source stays intact for tool replay.
	body["input"] = cloneAttachmentInput(body["input"])
	return body, nil
}

func cloneAttachmentInput(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, value := range typed {
			cloned[key] = cloneAttachmentInput(value)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, value := range typed {
			cloned[index] = cloneAttachmentInput(value)
		}
		return cloned
	default:
		return value
	}
}

func TransformResponseBody(body []byte, source map[string]any) ([]byte, map[string]any, bool, error) {
	return transformResponseBody(body, source)
}

func ParseFinalStreamResponse(raw []byte) (map[string]any, error) {
	trimmed := string(raw)
	if len(trimmed) > 0 {
		if object, err := RawObject(raw); err == nil {
			return object, nil
		}
	}
	decoder := newSSEDecoder()
	var completed map[string]any
	consume := func(_ string, data string) error {
		if data == "[DONE]" {
			return nil
		}
		object, err := RawObject([]byte(data))
		if err != nil {
			return nil
		}
		if response, ok := object["response"].(map[string]any); ok {
			if object["type"] == "response.completed" || response["status"] == "completed" {
				completed = response
			}
		}
		return nil
	}
	if err := decoder.feed(raw, consume); err != nil {
		return nil, err
	}
	// A server is allowed to close an SSE response immediately after the last
	// data line. In that case there is no blank line for sseDecoder.feed to
	// dispatch the pending event; append a synthetic record separator solely to
	// flush the decoder at EOF.
	if err := decoder.feed([]byte("\n\n"), consume); err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, fail(502, "invalid_upstream_response", "Basis Points stream ended without response.completed")
	}
	return completed, nil
}

func SyntheticStream(response map[string]any) []byte { return syntheticStream(response) }

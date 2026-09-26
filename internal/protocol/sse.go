package protocol

import (
	"bytes"
	"strings"
)

type sseDecoder struct {
	buffer strings.Builder
	data   []string
	event  string
}

func newSSEDecoder() *sseDecoder { return &sseDecoder{} }

func (d *sseDecoder) feed(chunk []byte, emit func(event, data string) error) error {
	for len(chunk) > 0 {
		// Search only newly received bytes. Rebuilding the buffered suffix on
		// each chunk makes one large data line quadratic in its byte length.
		index := bytes.IndexByte(chunk, '\n')
		if index < 0 {
			d.buffer.Write(chunk)
			return nil
		}
		var line string
		if d.buffer.Len() > 0 {
			d.buffer.Write(chunk[:index])
			line = d.buffer.String()
			d.buffer.Reset()
		} else {
			line = string(chunk[:index])
		}
		line = strings.TrimSuffix(line, "\r")
		chunk = chunk[index+1:]
		if line == "" {
			if len(d.data) > 0 || terminalKind(d.event).Failed() {
				if err := emit(d.event, strings.Join(d.data, "\n")); err != nil {
					return err
				}
			}
			d.data = nil
			d.event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			d.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			d.data = append(d.data, strings.TrimPrefix(value, " "))
		}
	}
	return nil
}

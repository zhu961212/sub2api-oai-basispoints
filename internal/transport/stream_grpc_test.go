package transport

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type largeEventRelayServer struct {
	pluginv1.UnimplementedTransportPluginServer
	body string
}

func (s *largeEventRelayServer) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	response := &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: make(http.Header), Body: io.NopCloser(strings.NewReader(s.body)), ContentLength: -1,
	}
	return sendTransformedHTTPResponseStream(stream, response, 16<<20, map[string]any{"stream": true})
}

// This deliberately uses an ordinary gRPC client with its default 4 MiB
// receive limit. Production go-plugin v1.8.0 raises that limit to MaxInt32;
// this test protects chunked interoperability, not a claimed production limit.
func TestStreamLargeSSEEventFitsDefaultGRPCClient(t *testing.T) {
	text := strings.Repeat("文", (5<<20)/len("文")+1)
	upstream := streamData(map[string]any{"type": "response.output_text.delta", "delta": text}) +
		streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_large_grpc", "status": "completed", "output": []any{}}})
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pluginv1.RegisterTransportPluginServer(server, &largeEventRelayServer{body: upstream})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	connection, err := grpc.NewClient("passthrough:///relay-large-event",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := pluginv1.NewTransportPluginClient(connection).Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	var endBytes int64
	starts, ends, chunks := 0, 0, 0
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("default gRPC client could not receive large SSE event: %v", err)
		}
		if frame.GetStart() != nil {
			starts++
			if chunks != 0 || ends != 0 {
				t.Fatal("Start arrived after body or End")
			}
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			if starts != 1 || ends != 0 {
				t.Fatal("body arrived outside Start/End frame boundaries")
			}
			if len(chunk) > 32<<10 {
				t.Fatalf("BodyChunk size = %d, want at most 32 KiB", len(chunk))
			}
			chunks++
			_, _ = body.Write(chunk)
		}
		if failure := frame.GetError(); failure != nil {
			t.Fatalf("unexpected plugin failure: %v", failure)
		}
		if end := frame.GetEnd(); end != nil {
			ends++
			endBytes = end.GetBytesReceived()
		}
	}
	if starts != 1 || ends != 1 || chunks < 2 || endBytes != int64(body.Len()) {
		t.Fatalf("invalid chunked lifecycle: starts=%d ends=%d chunks=%d bytes=%d/%d", starts, ends, chunks, endBytes, body.Len())
	}
	events := parsedStreamEvents(t, forwardResult{body: body.Bytes(), ended: ends == 1, received: endBytes})
	if len(events) != 2 || events[0]["type"] != "response.output_text.delta" || events[1]["type"] != "response.completed" {
		t.Fatalf("large SSE stream did not retain its delta and terminal: event count=%d", len(events))
	}
	if got, _ := events[0]["delta"].(string); got != text {
		t.Fatalf("reassembled text differs: got %d bytes, want %d", len(got), len(text))
	}
	if strings.Count(body.String(), "data: [DONE]") != 1 {
		t.Fatal("large SSE stream must end with exactly one DONE marker")
	}
}

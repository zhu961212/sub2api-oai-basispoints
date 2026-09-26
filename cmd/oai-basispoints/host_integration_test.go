package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hclog "github.com/hashicorp/go-hclog"
	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

type integrationHostServices struct {
	pluginv1.UnimplementedHostServiceServer
	listCalls atomic.Int32
}

func (h *integrationHostServices) ListAccounts(_ context.Context, req *pluginv1.ListAccountsRequest) (*pluginv1.ListAccountsResponse, error) {
	if req.Platform == "openai" && req.AccountType == "oauth" {
		h.listCalls.Add(1)
	}
	return &pluginv1.ListAccountsResponse{AccountIds: []int64{42}, Accounts: []*pluginv1.AccountInfo{
		{Id: 42, Platform: "openai", AccountType: "oauth", Name: "host-account", Status: "active", Schedulable: true},
	}}, nil
}

func (h *integrationHostServices) KVList(context.Context, *pluginv1.KVListRequest) (*pluginv1.KVListResponse, error) {
	return &pluginv1.KVListResponse{}, nil
}

// This follows Sub2API 0.2.8 startPluginRuntime and roundTrip with an actual
// compiled executable, including secure checksum startup and the reverse broker.
// All HTTP traffic terminates at the local fixture; no real accounts are used.
func TestOfficialHostProcessLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "oai-basispoints")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v: %s", err, output)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(raw)
	client := hcplugin.NewClient(&hcplugin.ClientConfig{
		HandshakeConfig: pluginv1.HandshakeConfig, Plugins: pluginv1.ClientPluginMap(),
		Cmd: exec.CommandContext(ctx, binary), AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC},
		StartTimeout: 15 * time.Second, SkipHostEnv: true,
		SecureConfig: &hcplugin.SecureConfig{Checksum: checksum[:], Hash: sha256.New()},
		Logger:       hclog.NewNullLogger(), SyncStdout: io.Discard, SyncStderr: io.Discard,
	})
	t.Cleanup(client.Kill)
	rpc, err := client.Client()
	if err != nil {
		t.Fatalf("official host handshake: %v", err)
	}
	dispensed, err := rpc.Dispense(pluginv1.TransportPluginName)
	if err != nil {
		t.Fatal(err)
	}
	api, ok := dispensed.(*pluginv1.TransportClient)
	if !ok || api.Broker == nil {
		t.Fatalf("invalid host client: %T", dispensed)
	}
	info, err := api.GetInfo(ctx, &pluginv1.GetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join("..", "..", "manifest.source.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ID      string
		Version string
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if info.PluginId != manifest.ID || info.PluginVersion != manifest.Version || info.ProtocolVersion != 1 || info.TransportApiVersion != 1 {
		t.Fatalf("manifest/runtime contract mismatch: %v", info)
	}
	health, err := api.Health(ctx, &pluginv1.HealthRequest{})
	if err != nil || !health.GetHealthy() {
		t.Fatalf("initial health: %v, %v", health, err)
	}
	host := &integrationHostServices{}
	brokerID := api.Broker.NextId()
	go api.Broker.AcceptAndServe(brokerID, func(options []grpc.ServerOption) *grpc.Server {
		server := grpc.NewServer(options...)
		pluginv1.RegisterHostServiceServer(server, host)
		return server
	})
	initialized, err := api.InitHostServices(ctx, &pluginv1.InitHostServicesRequest{
		HostServiceId: brokerID, HostServiceApiVersion: pluginv1.HostServiceAPIVersion,
	})
	if err != nil || !initialized.GetReady() {
		t.Fatalf("reverse host broker: %v, %v", initialized, err)
	}
	health, err = api.Health(ctx, &pluginv1.HealthRequest{})
	if err != nil || !health.GetHealthy() || !json.Valid([]byte(health.GetStatusJson())) {
		t.Fatalf("host-connected health: %v, %v", health, err)
	}
	var status struct {
		Accounts []struct {
			ID   int64
			Name string
		}
	}
	if err := json.Unmarshal([]byte(health.GetStatusJson()), &status); err != nil {
		t.Fatal(err)
	}
	if host.listCalls.Load() != 1 || len(status.Accounts) != 1 || status.Accounts[0].ID != 42 || status.Accounts[0].Name != "host-account" {
		t.Fatalf("host account directory was not delivered: %+v", status)
	}
	validated, err := api.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: []byte("{}")})
	if err != nil || !validated.GetValid() || !json.Valid(validated.GetNormalizedConfigJson()) {
		t.Fatalf("normalize config: %v, %v", validated, err)
	}
	applied, err := api.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: validated.GetNormalizedConfigJson()})
	if err != nil || !applied.GetApplied() {
		t.Fatalf("apply normalized config: %v, %v", applied, err)
	}

	body := bytes.Repeat([]byte("upstream-body-"), 8000)
	requestBody := []byte("{\"model\":\"official-host-passthrough-model\",\"input\":\"hello\"}")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, readErr := io.ReadAll(r.Body)
		if readErr != nil || !bytes.Equal(received, requestBody) || r.ContentLength != int64(len(requestBody)) {
			t.Errorf("request body/framing changed: %v, %q", readErr, received)
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-host-token" || r.Header.Get("Chatgpt-Account-Id") != "synthetic-host-account" {
			t.Error("host outbound identity was not preserved")
		}
		w.Header().Set("X-Official-Host-Test", "passed")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	stream, err := api.Forward(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frames := []*pluginv1.ForwardRequest{
		{Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{
			RequestId: "official-host-roundtrip", Method: http.MethodPost, Url: upstream.URL,
			AccountId: 42, AccountConcurrency: 1, Platform: "openai", AccountType: "oauth",
			ContentLength: int64(len(requestBody)), HasBody: true,
			Headers: map[string]*pluginv1.HeaderValues{
				"Authorization":      {Values: []string{"Bearer synthetic-host-token"}},
				"Chatgpt-Account-Id": {Values: []string{"synthetic-host-account"}},
			},
		}}},
		{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: requestBody}},
		{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}},
	}
	for _, frame := range frames {
		if err := stream.Send(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	var received bytes.Buffer
	starts, ends := 0, 0
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if failure := frame.GetError(); failure != nil {
			t.Fatalf("forward failure: %v", failure)
		}
		if start := frame.GetStart(); start != nil {
			starts++
			values := start.GetHeaders()["X-Official-Host-Test"].GetValues()
			if starts != 1 || received.Len() != 0 || ends != 0 || start.StatusCode != http.StatusCreated || len(values) != 1 || values[0] != "passed" {
				t.Fatalf("invalid response start: %v", start)
			}
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			if starts != 1 || ends != 0 || len(chunk) > 32*1024 {
				t.Fatal("invalid response frame ordering/size")
			}
			received.Write(chunk)
		}
		if end := frame.GetEnd(); end != nil {
			ends++
			if end.BytesReceived != int64(len(body)) {
				t.Fatalf("incorrect bytes_received: %v", end)
			}
		}
	}
	if starts != 1 || ends != 1 || !bytes.Equal(received.Bytes(), body) {
		t.Fatal("incomplete response lifecycle")
	}
	if info.PluginId != protocol.PluginID {
		t.Fatal("protocol plugin ID drift")
	}
}

package iot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coderouter/coderouter/internal/provider"
)

type fakeStore struct {
	saved   []TelemetryEvent
	touched []string
}

func (f *fakeStore) SaveTelemetry(_ context.Context, e TelemetryEvent) error {
	f.saved = append(f.saved, e)
	return nil
}

func (f *fakeStore) TouchDevice(_ context.Context, id string) error {
	f.touched = append(f.touched, id)
	return nil
}

func (f *fakeStore) RecentTelemetry(_ context.Context, _ string, _ int) ([]TelemetryEvent, error) {
	return f.saved, nil
}

type fakeRouter struct {
	called bool
	err    error
}

func (f *fakeRouter) Complete(_ context.Context, req *provider.ChatRequest, _ string) (*provider.ChatResponse, error) {
	f.called = true
	if f.err != nil {
		return nil, f.err
	}
	return &provider.ChatResponse{
		Model: "cloud-model",
		Choices: []provider.Choice{{
			Message: &provider.Message{Role: "assistant", Content: "cloud answer"},
		}},
	}, nil
}

func TestParseTopic(t *testing.T) {
	prefix := "coderouter/v1"
	cases := []struct {
		topic, device, kind string
	}{
		{"coderouter/v1/devices/sensor-1/telemetry", "sensor-1", "telemetry"},
		{"coderouter/v1/devices/sensor-1/inference/request", "sensor-1", "inference"},
		{"coderouter/v1/devices/sensor-1/inference/response", "sensor-1", ""},
		{"coderouter/v1/other/thing/here", "", ""},
	}
	for _, c := range cases {
		device, kind := parseTopic(prefix, c.topic)
		if device != c.device || kind != c.kind {
			t.Errorf("parseTopic(%q) = (%q,%q), want (%q,%q)", c.topic, device, kind, c.device, c.kind)
		}
	}
}

func TestTelemetryOverMQTTIsStored(t *testing.T) {
	store := &fakeStore{}
	b := NewBridge(Config{}, nil, store)

	payload := `{"device_id":"impostor","type":"reading","data":{"temp_c":21.5}}`
	err := b.HandleMQTTMessage(MQTTMessage{
		Topic:   "coderouter/v1/devices/sensor-1/telemetry",
		Payload: json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("HandleMQTTMessage: %v", err)
	}

	if len(store.saved) != 1 {
		t.Fatalf("saved %d events, want 1", len(store.saved))
	}
	// A device must not be able to file telemetry under another device's id.
	if got := store.saved[0].DeviceID; got != "sensor-1" {
		t.Errorf("device_id = %q, want sensor-1 from the topic", got)
	}
	if store.saved[0].Data["temp_c"] != 21.5 {
		t.Errorf("data lost in transit: %+v", store.saved[0].Data)
	}
}

func TestUnknownTopicFallsToRegisteredHandler(t *testing.T) {
	b := NewBridge(Config{}, nil, &fakeStore{})

	called := false
	b.RegisterHandler("custom/topic", func(MQTTMessage) error {
		called = true
		return nil
	})

	if err := b.HandleMQTTMessage(MQTTMessage{Topic: "custom/topic", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("HandleMQTTMessage: %v", err)
	}
	if !called {
		t.Error("registered handler was not invoked")
	}

	if err := b.HandleMQTTMessage(MQTTMessage{Topic: "nobody/listens", Payload: json.RawMessage(`{}`)}); err == nil {
		t.Error("expected an error for a topic with no handler")
	}
}

func TestInferenceUsesCloudWhenNoEdgeConfigured(t *testing.T) {
	router := &fakeRouter{}
	b := NewBridge(Config{}, router, &fakeStore{})

	resp, err := b.Infer(context.Background(), InferenceRequest{DeviceID: "sensor-1", Prompt: "how hot?"})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	if resp.Source != "cloud" {
		t.Errorf("source = %q, want cloud", resp.Source)
	}
	if resp.Text != "cloud answer" {
		t.Errorf("text = %q", resp.Text)
	}
	if !router.called {
		t.Error("the gateway router was never consulted")
	}
}

func TestInferencePrefersTheEdge(t *testing.T) {
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("edge called at %q, want /chat/completions", r.URL.Path)
		}
		fmt.Fprint(w, `{"model":"local-llama","choices":[{"index":0,"message":{"role":"assistant","content":"edge answer"}}]}`)
	}))
	defer edge.Close()

	router := &fakeRouter{}
	b := NewBridge(Config{EdgeEndpoint: edge.URL}, router, &fakeStore{})

	resp, err := b.Infer(context.Background(), InferenceRequest{DeviceID: "sensor-1", Prompt: "how hot?"})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	if resp.Source != "edge" || resp.Text != "edge answer" {
		t.Errorf("got %+v, want an edge-served answer", resp)
	}
	if router.called {
		t.Error("cloud was used even though the edge succeeded")
	}
}

func TestInferenceFallsBackWhenEdgeIsDown(t *testing.T) {
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "edge box is offline", http.StatusServiceUnavailable)
	}))
	defer edge.Close()

	router := &fakeRouter{}
	b := NewBridge(Config{EdgeEndpoint: edge.URL}, router, &fakeStore{})

	resp, err := b.Infer(context.Background(), InferenceRequest{DeviceID: "sensor-1", Prompt: "how hot?"})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	if resp.Source != "cloud" {
		t.Errorf("source = %q; an unreachable edge must fall back to cloud", resp.Source)
	}
	if !router.called {
		t.Error("cloud fallback never fired")
	}
}

func TestForwardToHTTPReturnsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	b := NewBridge(Config{}, nil, &fakeStore{})

	// The original implementation declared a nil slice and returned it,
	// so every caller downstream saw an empty body.
	got, err := b.ForwardToHTTP(context.Background(), srv.URL, map[string]string{"hello": "world"})
	if err != nil {
		t.Fatalf("ForwardToHTTP: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("body = %q, want the response payload", got)
	}
}

func TestEmptyPromptIsRejected(t *testing.T) {
	b := NewBridge(Config{}, &fakeRouter{}, &fakeStore{})
	if _, err := b.Infer(context.Background(), InferenceRequest{DeviceID: "d", Prompt: "  "}); err == nil {
		t.Error("expected an error for an empty prompt")
	}
}

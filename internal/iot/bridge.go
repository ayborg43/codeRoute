package iot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/coderouter/coderouter/internal/provider"
)

const defaultTopicPrefix = "coderouter/v1"

type MQTTMessage struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
	QoS     int             `json:"qos"`
}

type TelemetryEvent struct {
	DeviceID  string                 `json:"device_id"`
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`
}

type InferenceRequest struct {
	DeviceID  string `json:"device_id"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

type InferenceResponse struct {
	DeviceID string `json:"device_id"`
	Model    string `json:"model,omitempty"`
	// Source is "edge" when a local model served the request, "cloud" when it
	// went out through the gateway's providers.
	Source string `json:"source,omitempty"`
	Text   string `json:"text,omitempty"`
	Error  string `json:"error,omitempty"`
}

type MessageHandler func(msg MQTTMessage) error

// Router is the slice of the gateway the bridge needs, kept as an interface so
// device handling stays testable without live providers.
type Router interface {
	Complete(ctx context.Context, req *provider.ChatRequest, keyID string) (*provider.ChatResponse, error)
}

// TelemetryStore is the persistence the bridge needs, as an interface so
// device handling can be tested without a database.
type TelemetryStore interface {
	SaveTelemetry(ctx context.Context, event TelemetryEvent) error
	TouchDevice(ctx context.Context, deviceID string) error
	RecentTelemetry(ctx context.Context, deviceID string, limit int) ([]TelemetryEvent, error)
}

type Config struct {
	Broker       string
	ClientID     string
	Username     string
	Password     string
	TopicPrefix  string
	EdgeEndpoint string
	// APIKeyID attributes MQTT-originated usage, which has no HTTP caller.
	APIKeyID string
}

type Bridge struct {
	cfg    Config
	router Router
	store  TelemetryStore
	client mqtt.Client
	http   *http.Client

	mu       sync.RWMutex
	handlers map[string]MessageHandler
}

func NewBridge(cfg Config, router Router, store TelemetryStore) *Bridge {
	if cfg.TopicPrefix == "" {
		cfg.TopicPrefix = defaultTopicPrefix
	}
	cfg.TopicPrefix = strings.Trim(cfg.TopicPrefix, "/")
	if cfg.ClientID == "" {
		cfg.ClientID = "coderouter"
	}

	return &Bridge{
		cfg:      cfg,
		router:   router,
		store:    store,
		http:     &http.Client{Timeout: 30 * time.Second},
		handlers: make(map[string]MessageHandler),
	}
}

// Enabled reports whether an MQTT broker is configured. Without one the HTTP
// IoT endpoints still work; only the MQTT half is inert.
func (b *Bridge) Enabled() bool { return b.cfg.Broker != "" }

func (b *Bridge) telemetryTopic() string {
	return b.cfg.TopicPrefix + "/devices/+/telemetry"
}

func (b *Bridge) inferenceTopic() string {
	return b.cfg.TopicPrefix + "/devices/+/inference/request"
}

func (b *Bridge) responseTopic(deviceID string) string {
	return fmt.Sprintf("%s/devices/%s/inference/response", b.cfg.TopicPrefix, deviceID)
}

// Connect dials the broker and subscribes. It is a no-op when no broker is set.
func (b *Bridge) Connect() error {
	if !b.Enabled() {
		return nil
	}

	opts := mqtt.NewClientOptions().AddBroker(b.cfg.Broker).SetClientID(b.cfg.ClientID)
	if b.cfg.Username != "" {
		opts.SetUsername(b.cfg.Username)
		opts.SetPassword(b.cfg.Password)
	}
	opts.SetAutoReconnect(true)
	// Subscriptions are re-established on every connect, including reconnects.
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		if err := b.subscribe(c); err != nil {
			log.Printf("iot: subscribe failed: %v", err)
		}
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		log.Printf("iot: mqtt connection lost: %v", err)
	})

	b.client = mqtt.NewClient(opts)
	token := b.client.Connect()
	if !token.WaitTimeout(15 * time.Second) {
		return fmt.Errorf("iot: timed out connecting to %s", b.cfg.Broker)
	}
	return token.Error()
}

func (b *Bridge) subscribe(c mqtt.Client) error {
	for _, topic := range []string{b.telemetryTopic(), b.inferenceTopic()} {
		token := c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
			msg := MQTTMessage{Topic: m.Topic(), Payload: json.RawMessage(m.Payload()), QoS: int(m.Qos())}
			if err := b.HandleMQTTMessage(msg); err != nil {
				log.Printf("iot: %s: %v", m.Topic(), err)
			}
		})
		if !token.WaitTimeout(10 * time.Second) {
			return fmt.Errorf("timed out subscribing to %s", topic)
		}
		if err := token.Error(); err != nil {
			return err
		}
		log.Printf("iot: subscribed to %s", topic)
	}
	return nil
}

func (b *Bridge) Close() {
	if b.client != nil && b.client.IsConnected() {
		b.client.Disconnect(250)
	}
}

func (b *Bridge) RegisterHandler(topic string, handler MessageHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = handler
}

// HandleMQTTMessage routes one inbound message. Built-in telemetry and
// inference topics are handled first; anything else falls to a registered
// handler.
func (b *Bridge) HandleMQTTMessage(msg MQTTMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	device, kind := parseTopic(b.cfg.TopicPrefix, msg.Topic)

	switch kind {
	case "telemetry":
		var event TelemetryEvent
		if err := json.Unmarshal(msg.Payload, &event); err != nil {
			return fmt.Errorf("invalid telemetry payload: %w", err)
		}
		// The topic is authoritative: a device cannot claim to be another.
		if device != "" {
			event.DeviceID = device
		}
		return b.IngestTelemetry(ctx, event)

	case "inference":
		var req InferenceRequest
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			return fmt.Errorf("invalid inference payload: %w", err)
		}
		if device != "" {
			req.DeviceID = device
		}

		resp, err := b.Infer(ctx, req)
		if err != nil {
			// Devices get told the request failed rather than waiting forever.
			resp = &InferenceResponse{DeviceID: req.DeviceID, Error: err.Error()}
		}
		return b.publishResponse(resp)
	}

	b.mu.RLock()
	handler, ok := b.handlers[msg.Topic]
	b.mu.RUnlock()

	if !ok {
		return fmt.Errorf("no handler for topic: %s", msg.Topic)
	}
	return handler(msg)
}

// parseTopic pulls the device id and kind out of <prefix>/devices/<id>/<kind>.
func parseTopic(prefix, topic string) (device, kind string) {
	trimmed := strings.Trim(topic, "/")
	trimmed = strings.TrimPrefix(trimmed, prefix+"/")

	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 || parts[0] != "devices" {
		return "", ""
	}
	device = parts[1]

	switch {
	case parts[2] == "telemetry":
		return device, "telemetry"
	case parts[2] == "inference" && len(parts) > 3 && parts[3] == "request":
		return device, "inference"
	}
	return device, ""
}

func (b *Bridge) publishResponse(resp *InferenceResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if b.client == nil || !b.client.IsConnected() {
		return fmt.Errorf("cannot publish inference response: mqtt not connected")
	}

	topic := b.responseTopic(resp.DeviceID)
	token := b.client.Publish(topic, 1, false, payload)
	if !token.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("timed out publishing to %s", topic)
	}
	return token.Error()
}

// IngestTelemetry persists one device reading.
func (b *Bridge) IngestTelemetry(ctx context.Context, event TelemetryEvent) error {
	if b.store == nil {
		return fmt.Errorf("telemetry store is not configured")
	}
	return b.store.SaveTelemetry(ctx, event)
}

// Connected reports whether the MQTT half is currently live.
func (b *Bridge) Connected() bool {
	return b.client != nil && b.client.IsConnected()
}

// RecentTelemetry returns a device's latest readings.
func (b *Bridge) RecentTelemetry(ctx context.Context, deviceID string, limit int) ([]TelemetryEvent, error) {
	if b.store == nil {
		return nil, fmt.Errorf("telemetry store is not configured")
	}
	return b.store.RecentTelemetry(ctx, deviceID, limit)
}

// Infer serves an MQTT-originated request, attributed to the configured IoT key.
func (b *Bridge) Infer(ctx context.Context, req InferenceRequest) (*InferenceResponse, error) {
	return b.InferAs(ctx, req, b.cfg.APIKeyID)
}

// InferAs serves a device request from the edge when one is configured, falling
// back to the cloud providers through the gateway. Usage is attributed to
// keyID, so HTTP callers are billed to their own key.
func (b *Bridge) InferAs(ctx context.Context, req InferenceRequest, keyID string) (*InferenceResponse, error) {
	if keyID == "" {
		keyID = b.cfg.APIKeyID
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("prompt must not be empty")
	}
	if b.store != nil {
		_ = b.store.TouchDevice(ctx, req.DeviceID)
	}

	chat := &provider.ChatRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Messages:  []provider.Message{{Role: "user", Content: provider.Content(req.Prompt)}},
	}

	if b.cfg.EdgeEndpoint != "" {
		resp, err := b.EdgeInference(ctx, chat)
		if err == nil {
			return &InferenceResponse{
				DeviceID: req.DeviceID,
				Model:    resp.Model,
				Source:   "edge",
				Text:     firstText(resp),
			}, nil
		}
		// Edge is best-effort; an unreachable box must not fail the request.
		log.Printf("iot: edge inference failed, falling back to cloud: %v", err)
	}

	if b.router == nil {
		return nil, fmt.Errorf("no router configured for cloud inference")
	}

	resp, err := b.router.Complete(ctx, chat, keyID)
	if err != nil {
		return nil, err
	}

	return &InferenceResponse{
		DeviceID: req.DeviceID,
		Model:    resp.Model,
		Source:   "cloud",
		Text:     firstText(resp),
	}, nil
}

// EdgeInference asks a local OpenAI-compatible model server (llama.cpp, Ollama,
// vLLM) so a device can be served without leaving the network.
func (b *Bridge) EdgeInference(ctx context.Context, chat *provider.ChatRequest) (*provider.ChatResponse, error) {
	if b.cfg.EdgeEndpoint == "" {
		return nil, fmt.Errorf("no edge endpoint configured")
	}

	endpoint := strings.TrimRight(b.cfg.EdgeEndpoint, "/") + "/chat/completions"
	body, err := b.ForwardToHTTP(ctx, endpoint, chat)
	if err != nil {
		return nil, err
	}

	var resp provider.ChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("edge returned malformed response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("edge returned no choices")
	}
	return &resp, nil
}

// ForwardToHTTP posts a payload and returns the response body.
func (b *Bridge) ForwardToHTTP(ctx context.Context, endpoint string, payload interface{}) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", endpoint, resp.StatusCode, truncate(data))
	}

	return data, nil
}

func firstText(resp *provider.ChatResponse) string {
	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		return ""
	}
	return string(resp.Choices[0].Message.Content)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

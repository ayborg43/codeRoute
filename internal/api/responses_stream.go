package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/provider"
)

// A streamed Responses call is not a reformatted chat stream: chat sends one
// flat sequence of deltas, while Responses frames every piece of output as an
// item that is opened, filled and closed. This file does that framing, keeping
// a running transcript so the terminal response.completed event can carry the
// whole output — which is where clients read the final text and tool calls
// from, rather than reassembling the deltas themselves.

// responsesStream accumulates a chat stream and emits Responses SSE events.
type responsesStream struct {
	w       http.ResponseWriter
	flusher http.Flusher

	seq     int
	started bool
	body    *responsesBody

	textID   string
	textIdx  int
	textOpen bool
	text     strings.Builder

	tools     map[int]*toolStream
	toolOrder []int

	nextIdx int
}

// toolStream is one in-flight function call. Arguments arrive as fragments
// that must be concatenated in order.
type toolStream struct {
	itemID string
	callID string
	name   string
	args   strings.Builder
	idx    int
}

func (h *Handler) streamResponses(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, key *db.ClientKey) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported by server", "internal_error")
		return
	}

	s := &responsesStream{
		w:       w,
		flusher: flusher,
		body:    newResponsesBody(req.Model),
		tools:   map[int]*toolStream{},
	}

	err := h.gw.Stream(r.Context(), req, key, s.consume)
	if err != nil {
		// Headers are only committed once the first event goes out, so an
		// upstream failure before then can still be a real HTTP error.
		if !s.started {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
			return
		}
		s.fail(err)
		return
	}

	s.finish()
}

// consume folds one chat chunk into the response being assembled.
func (s *responsesStream) consume(chunk *provider.ChatResponse) error {
	s.begin(chunk.Model)

	if chunk.Usage != nil {
		s.body.Usage = &responsesUsage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
	}

	for _, choice := range chunk.Choices {
		if choice.Delta == nil {
			continue
		}
		if text := choice.Delta.Content; text != "" {
			if err := s.appendText(text); err != nil {
				return err
			}
		}
		for i, tc := range choice.Delta.ToolCalls {
			if err := s.appendToolCall(i, tc); err != nil {
				return err
			}
		}
	}

	return nil
}

// begin emits the opening pair of events, once, on the first chunk.
func (s *responsesStream) begin(model string) {
	if s.started {
		return
	}
	s.started = true

	if model != "" {
		s.body.Model = model
	}

	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	// Stops reverse proxies from buffering the stream into one response.
	s.w.Header().Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)

	s.send("response.created", map[string]any{"response": s.body})
	s.send("response.in_progress", map[string]any{"response": s.body})
}

func (s *responsesStream) appendText(text string) error {
	if !s.textOpen {
		s.textOpen = true
		s.textID = "msg_" + uuid.New().String()
		s.textIdx = s.nextIdx
		s.nextIdx++

		s.send("response.output_item.added", map[string]any{
			"output_index": s.textIdx,
			"item": map[string]any{
				"type": "message", "id": s.textID, "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		})
		s.send("response.content_part.added", map[string]any{
			"item_id": s.textID, "output_index": s.textIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}

	s.text.WriteString(text)
	s.send("response.output_text.delta", map[string]any{
		"item_id": s.textID, "output_index": s.textIdx, "content_index": 0,
		"delta": text,
	})
	return nil
}

// appendToolCall folds one tool-call fragment in. position is the fragment's
// place in the delta, used only when the provider omits the index field.
func (s *responsesStream) appendToolCall(position int, tc provider.ToolCall) error {
	idx := position
	if tc.Index != nil {
		idx = *tc.Index
	}

	call, ok := s.tools[idx]
	if !ok {
		call = &toolStream{
			itemID: "fc_" + uuid.New().String(),
			callID: tc.ID,
			name:   tc.Function.Name,
			idx:    s.nextIdx,
		}
		if call.callID == "" {
			call.callID = "call_" + uuid.New().String()
		}
		s.nextIdx++
		s.tools[idx] = call
		s.toolOrder = append(s.toolOrder, idx)

		s.send("response.output_item.added", map[string]any{
			"output_index": call.idx,
			"item": map[string]any{
				"type": "function_call", "id": call.itemID, "status": "in_progress",
				"call_id": call.callID, "name": call.name, "arguments": "",
			},
		})
	}

	// Later fragments may be the first to carry the id or name.
	if tc.ID != "" {
		call.callID = tc.ID
	}
	if tc.Function.Name != "" {
		call.name = tc.Function.Name
	}

	if args := tc.Function.Arguments; args != "" {
		call.args.WriteString(args)
		s.send("response.function_call_arguments.delta", map[string]any{
			"item_id": call.itemID, "output_index": call.idx, "delta": args,
		})
	}

	return nil
}

// finish closes every open item and emits the terminal response.completed.
func (s *responsesStream) finish() {
	// A stream that produced nothing still owes the client a well-formed pair
	// of opening and closing events.
	s.begin(s.body.Model)

	if s.textOpen {
		text := s.text.String()
		s.send("response.output_text.done", map[string]any{
			"item_id": s.textID, "output_index": s.textIdx, "content_index": 0, "text": text,
		})
		s.send("response.content_part.done", map[string]any{
			"item_id": s.textID, "output_index": s.textIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})

		item := responsesOutput{
			Type: "message", ID: s.textID, Status: "completed", Role: "assistant",
			Content: []responsesPart{{Type: "output_text", Text: text, Annotations: []any{}}},
		}
		s.body.Output = append(s.body.Output, item)
		s.send("response.output_item.done", map[string]any{
			"output_index": s.textIdx, "item": item,
		})
	}

	for _, key := range s.toolOrder {
		call := s.tools[key]
		args := call.args.String()

		s.send("response.function_call_arguments.done", map[string]any{
			"item_id": call.itemID, "output_index": call.idx, "arguments": args,
		})

		item := responsesOutput{
			Type: "function_call", ID: call.itemID, Status: "completed",
			CallID: call.callID, Name: call.name, Arguments: args,
		}
		s.body.Output = append(s.body.Output, item)
		s.send("response.output_item.done", map[string]any{
			"output_index": call.idx, "item": item,
		})
	}

	s.body.Status = "completed"
	s.send("response.completed", map[string]any{"response": s.body})
}

// fail reports an error that struck after the status line was already sent.
func (s *responsesStream) fail(err error) {
	s.body.Status = "failed"
	s.body.Error = map[string]string{"message": err.Error(), "type": "upstream_error"}
	s.send("response.failed", map[string]any{"response": s.body})
}

// send writes one SSE event. Both the event line and the type field are set:
// clients key off one or the other, and OpenAI's own stream carries both.
func (s *responsesStream) send(kind string, payload map[string]any) {
	payload["type"] = kind
	payload["sequence_number"] = s.seq
	s.seq++

	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", kind, encoded)
	s.flusher.Flush()
}

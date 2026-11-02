// Package mcp is a minimal Model Context Protocol server (JSON-RPC 2.0 over
// stdio) that gives AI agents Omni-Notify's voice call tools. The agent never
// touches APNs or the phone: it asks Omni-Notify, which applies the owner's
// policy and owns delivery.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"

	"github.com/pod32g/omni-notify/internal/agentclient"
)

// ProtocolVersion is the newest MCP revision this server speaks.
const ProtocolVersion = "2025-06-18"

// Server serves the tools.
type Server struct {
	client  *agentclient.Client
	version string
}

// NewServer builds a server around an agent client.
func NewServer(c *agentclient.Client, version string) *Server {
	return &Server{client: c, version: version}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads newline-delimited JSON-RPC messages from r and answers on w
// until r ends or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	write := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(v)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification (e.g. notifications/initialized): no reply
		}
		result, rerr := s.handle(ctx, req)
		resp := response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
		write(resp)
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := ProtocolVersion
		if p.ProtocolVersion != "" && p.ProtocolVersion < ProtocolVersion {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "omni-notify", "version": s.version},
			"instructions": "Omni-Notify voice calls ring the owner's real iPhone. The call is spoken by Omni's " +
				"on-device voice (Gobbie's), introduced as a message from you; you are not on the line and must not " +
				"say you will be. Use it when a call is worth an interruption, respect the answer (rate limits and " +
				"quiet hours are the owner's policy), and check the outcome with omni_notify_get_call_status.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		return s.callTool(ctx, p.Name, p.Arguments), nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func textResult(isError bool, text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

func (s *Server) callTool(ctx context.Context, name string, args map[string]any) map[string]any {
	var (
		out map[string]any
		err error
	)
	switch name {
	case "omni_notify_call", "omni_notify_schedule_call":
		body := map[string]any{"recipient": str(args, "recipient", "me"), "message": str(args, "message", "")}
		for _, k := range []string{"title", "details", "type", "severity", "idempotency_key", "expires_in"} {
			if v := str(args, k, ""); v != "" {
				body[k] = v
			}
		}
		for _, k := range []string{"interactive", "user_requested", "test"} {
			if v, ok := args[k].(bool); ok {
				body[k] = v
			}
		}
		if name == "omni_notify_schedule_call" {
			at, in := str(args, "at", ""), str(args, "in", "")
			if (at == "") == (in == "") {
				return textResult(true, "give exactly one of at (RFC 3339 with offset) or in (like 10m)")
			}
			sched := map[string]any{}
			if at != "" {
				sched["at"] = at
			} else {
				sched["in"] = in
			}
			body["schedule"] = sched
		}
		out, err = s.client.Do(ctx, "POST", "/api/v1/calls", body)
	case "omni_notify_cancel_call":
		id := str(args, "call_id", "")
		if id == "" {
			return textResult(true, "call_id is required")
		}
		out, err = s.client.Do(ctx, "POST", "/api/v1/calls/"+url.PathEscape(id)+"/cancel", nil)
	case "omni_notify_get_call_status":
		id := str(args, "call_id", "")
		if id == "" {
			return textResult(true, "call_id is required")
		}
		out, err = s.client.Do(ctx, "GET", "/api/v1/calls/"+url.PathEscape(id), nil)
	case "omni_notify_list_scheduled_calls":
		out, err = s.client.Do(ctx, "GET", "/api/v1/calls?state=scheduled", nil)
	default:
		return textResult(true, "unknown tool "+name)
	}
	if err != nil {
		b, _ := json.Marshal(out)
		return textResult(true, fmt.Sprintf("%v\n%s", err, b))
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return textResult(false, summarize(name, out)+"\n\n"+string(b))
}

// summarize leads each result with a sentence the agent can relay as is.
func summarize(tool string, out map[string]any) string {
	call, _ := out["call"].(map[string]any)
	switch tool {
	case "omni_notify_call", "omni_notify_schedule_call":
		if call == nil {
			return "No call returned."
		}
		s := fmt.Sprintf("Call %v is %v (scheduled for %v).", call["id"], call["state"], call["scheduled_at"])
		if out["deduplicated"] == true {
			s = "An identical call already exists: " + s
		}
		if d, _ := out["deferred"].(string); d != "" {
			s += " Deferred by the owner's policy: " + d + "."
		}
		return s
	case "omni_notify_get_call_status":
		if call == nil {
			return "No call returned."
		}
		s := fmt.Sprintf("Call %v: state %v", call["id"], call["state"])
		if o, _ := call["outcome"].(string); o != "" {
			s += ", outcome " + o
		}
		return s + "."
	case "omni_notify_cancel_call":
		return "Cancelled."
	case "omni_notify_list_scheduled_calls":
		return fmt.Sprintf("%v scheduled call(s).", out["count"])
	}
	return ""
}

func str(args map[string]any, k, def string) string {
	if v, ok := args[k].(string); ok && v != "" {
		return v
	}
	return def
}

var callProps = map[string]any{
	"message": map[string]any{"type": "string", "description": "What to say, in your own words, as one or two short spoken sentences (under 1000 characters). It is read aloud after \"Hi, this is Gobbie with a reminder from <you>\"."},
	"title":   map[string]any{"type": "string", "description": "Short title for the call log and the fallback notification."},
	"details": map[string]any{"type": "string", "description": "More context, read only if the owner asks \"tell me more\"."},
	"type":    map[string]any{"type": "string", "description": "Purpose: reminder, report, alert, notification (default notification)."},
	"severity": map[string]any{"type": "string", "enum": []string{"info", "warning", "error", "critical"},
		"description": "Importance. Independent of the channel; critical does not bypass the owner's quiet hours unless your agent was granted that."},
	"recipient":       map[string]any{"type": "string", "description": "Who to call (default \"me\", the owner)."},
	"user_requested":  map[string]any{"type": "boolean", "description": "True only when the owner explicitly asked you for this call (e.g. \"remind me at 5\")."},
	"interactive":     map[string]any{"type": "boolean", "description": "Let the owner answer back: acknowledge, snooze, reschedule (default true)."},
	"idempotency_key": map[string]any{"type": "string", "description": "Retrying with the same key never creates a second call."},
	"test":            map[string]any{"type": "boolean", "description": "Mark the call as a test (said on screen and aloud)."},
	"expires_in":      map[string]any{"type": "string", "description": "How late the call may still ring after its time, like 2h (default 2h)."},
}

func withProps(extra map[string]any) map[string]any {
	p := map[string]any{}
	for k, v := range callProps {
		p[k] = v
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

var tools = []map[string]any{
	{
		"name": "omni_notify_call",
		"description": "Ring the owner's iPhone now with a real incoming call. Gobbie's voice (Omni Notify's on-phone voice) reads your message; the owner " +
			"can acknowledge, dismiss, snooze (\"give me ten minutes\") or reschedule, and Omni-Notify records it. " +
			"Use sparingly: it interrupts. Returns the call id; check the result with omni_notify_get_call_status.",
		"inputSchema": map[string]any{"type": "object", "properties": callProps, "required": []string{"message"}},
	},
	{
		"name":        "omni_notify_schedule_call",
		"description": "Schedule a call for later (a reminder). Give at (RFC 3339 with offset, e.g. 2026-10-26T17:00:00-06:00) or in (e.g. 10m, 2h). Survives restarts; can be cancelled.",
		"inputSchema": map[string]any{"type": "object", "properties": withProps(map[string]any{
			"at": map[string]any{"type": "string", "description": "When to ring, RFC 3339 with offset."},
			"in": map[string]any{"type": "string", "description": "Delay from now, like 10m or 1h30m."},
		}), "required": []string{"message"}},
	},
	{
		"name":        "omni_notify_cancel_call",
		"description": "Cancel one of your scheduled (or still ringing) calls.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"call_id": map[string]any{"type": "string"}}, "required": []string{"call_id"}},
	},
	{
		"name":        "omni_notify_get_call_status",
		"description": "State (scheduled, ringing, answered, active, completed, declined, missed, expired, failed, cancelled), outcome (acknowledged, dismissed, snoozed, rescheduled) and log of one of your calls.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"call_id": map[string]any{"type": "string"}}, "required": []string{"call_id"}},
	},
	{
		"name":        "omni_notify_list_scheduled_calls",
		"description": "Your calls that are scheduled and have not rung yet, soonest first (including snoozes the owner asked for).",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

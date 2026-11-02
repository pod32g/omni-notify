package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-notify/internal/agentclient"
)

func TestMCPToolsOverStdio(t *testing.T) {
	var got []map[string]any
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			w.WriteHeader(401)
			return
		}
		paths = append(paths, r.Method+" "+r.URL.String())
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"only scheduled or ringing calls can be cancelled"}`))
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"call":{"id":"c1","state":"scheduled","scheduled_at":"2026-10-26T23:00:00Z"},"created":true}`))
	}))
	defer api.Close()

	srv := NewServer(&agentclient.Client{BaseURL: api.URL, HTTP: api.Client(), Tokens: agentclient.StaticToken("agent-token")}, "test")
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"omni_notify_schedule_call","arguments":{"message":"Lend me your phone.","in":"10m","user_requested":true}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"omni_notify_schedule_call","arguments":{"message":"x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"omni_notify_cancel_call","arguments":{"call_id":"c1"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
	}, "\n")
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad line %q", line)
		}
		resps = append(resps, m)
	}
	if len(resps) != 6 {
		t.Fatalf("%d responses (notification must not be answered): %s", len(resps), out.String())
	}
	if v := resps[0]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Errorf("negotiated %v", v)
	}
	if n := len(resps[1]["result"].(map[string]any)["tools"].([]any)); n != 5 {
		t.Errorf("%d tools", n)
	}
	r3 := resps[2]["result"].(map[string]any)
	if r3["isError"] != false || !strings.Contains(r3["content"].([]any)[0].(map[string]any)["text"].(string), "Call c1 is scheduled") {
		t.Errorf("schedule result = %v", r3)
	}
	if got[0]["schedule"].(map[string]any)["in"] != "10m" || got[0]["recipient"] != "me" || got[0]["user_requested"] != true {
		t.Errorf("request body = %v", got[0])
	}
	if resps[3]["result"].(map[string]any)["isError"] != true {
		t.Error("schedule without a time was not an error")
	}
	if r5 := resps[4]["result"].(map[string]any); r5["isError"] != true ||
		!strings.Contains(r5["content"].([]any)[0].(map[string]any)["text"].(string), "409") {
		t.Errorf("cancel conflict = %v", r5)
	}
	if resps[5]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("unknown method = %v", resps[5])
	}
	if len(paths) != 2 || paths[1] != "POST /api/v1/calls/c1/cancel" {
		t.Errorf("paths = %v", paths)
	}
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/pod32g/omni-notify/internal/agentclient"
	"github.com/pod32g/omni-notify/internal/mcp"
)

// runMCP serves the voice call tools over stdio for an AI agent (Claude Code,
// or any MCP client). Configuration is by environment; see agentclient.FromEnv.
func runMCP() error {
	c, err := agentclient.FromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcp.NewServer(c, version).Serve(ctx, os.Stdin, os.Stdout)
}

// runCall places one call: the test-mode command.
//
//	omni-notify call -message "Remember to lend me your phone." [-in 10m | -at RFC3339] [-test]
//	                 [-agent claude] [-recipient me] [-type reminder] [-severity info] [-user-requested]
//
// With an agent credential the agent is whoever the credential says. With the
// admin token, -agent names who the call is for and the call is simulated.
func runCall(args []string) int {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	message := fs.String("message", "", "what to say (required)")
	title := fs.String("title", "", "title")
	details := fs.String("details", "", "read if asked for more")
	typ := fs.String("type", "reminder", "purpose")
	severity := fs.String("severity", "info", "info|warning|error|critical")
	recipient := fs.String("recipient", "me", "who to call")
	agent := fs.String("agent", "", "agent the call is for (admin token only; marks it simulated)")
	at := fs.String("at", "", "ring at (RFC 3339 with offset)")
	in := fs.String("in", "", "ring after (e.g. 10m)")
	test := fs.Bool("test", false, "mark as a test call")
	userRequested := fs.Bool("user-requested", false, "the owner asked for this reminder")
	key := fs.String("key", "", "idempotency key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *message == "" {
		fmt.Fprintln(os.Stderr, "call: -message is required")
		return 2
	}
	c, err := agentclient.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		return 1
	}
	body := map[string]any{"recipient": *recipient, "message": *message, "type": *typ, "severity": *severity,
		"test": *test, "user_requested": *userRequested}
	for k, v := range map[string]string{"title": *title, "details": *details, "agent": *agent, "idempotency_key": *key} {
		if v != "" {
			body[k] = v
		}
	}
	switch {
	case *at != "" && *in != "":
		fmt.Fprintln(os.Stderr, "call: use -at or -in, not both")
		return 2
	case *at != "":
		body["schedule"] = map[string]any{"at": *at}
	case *in != "":
		body["schedule"] = map[string]any{"in": *in}
	}
	out, err := c.Do(context.Background(), "POST", "/api/v1/calls", body)
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	if err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		return 1
	}
	return 0
}

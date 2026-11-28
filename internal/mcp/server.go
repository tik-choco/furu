package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/tik-choco/furu/internal/config"
	"github.com/tik-choco/furu/internal/worker"
)

type Worker interface {
	Run(context.Context, worker.Request) worker.Result
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
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

type server struct {
	worker Worker
	ctx    context.Context
	cancel context.CancelFunc

	output   *json.Encoder
	outputMu sync.Mutex
	writeErr error

	pending   map[string]context.CancelFunc
	pendingMu sync.Mutex
	calls     sync.WaitGroup
}

func Serve(in io.Reader, out io.Writer, runner Worker) error {
	ctx, cancel := context.WithCancel(context.Background())
	s := server{
		worker: runner, ctx: ctx, cancel: cancel,
		output: json.NewEncoder(out), pending: make(map[string]context.CancelFunc),
	}
	reader := bufio.NewReader(in)
	var readErr error
	for s.ctx.Err() == nil {
		line, err := reader.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			s.handle(line)
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	// Closing stdin ends the MCP session and any delegated work.
	cancel()
	s.calls.Wait()
	if s.writeErr != nil {
		return s.writeErr
	}
	return readErr
}

func (s *server) handle(line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		if _, ok := err.(*json.SyntaxError); ok {
			s.fail(nil, -32700, "Parse error")
		} else {
			s.fail(nil, -32600, "Invalid request")
		}
		return
	}
	key, validID := requestKey(req.ID)
	if req.JSONRPC != "2.0" || req.Method == "" || !validID {
		s.fail(nil, -32600, "Invalid request")
		return
	}
	if len(req.ID) == 0 {
		if req.Method == "notifications/cancelled" {
			s.cancelCall(req.Params)
		}
		return
	}

	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 && (req.Params[0] != '{' || json.Unmarshal(req.Params, &params) != nil) {
			s.fail(req.ID, -32602, "Invalid initialize params")
			return
		}
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2025-06-18"
		}
		s.send(response{ID: req.ID, Result: map[string]any{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]any{"tools": struct{}{}},
			"serverInfo":      map[string]string{"name": "furu", "version": "0.1.0"},
			"instructions":    "Delegate substantive implementation, repository investigation and testing to furu workers. Use backend=auto to select an available worker according to usage limits. Keep orchestration, integration and review in the parent session.",
		}})
	case "ping", "tools/list":
		if len(req.Params) > 0 && req.Params[0] != '{' {
			s.fail(req.ID, -32602, "Expected params object")
			return
		}
		var result any = struct{}{}
		if req.Method == "tools/list" {
			result = map[string]any{"tools": []any{delegateTool()}}
		}
		s.send(response{ID: req.ID, Result: result})
	case "tools/call":
		args, err := parseCall(req.Params)
		if err != nil {
			s.fail(req.ID, -32602, err.Error())
			return
		}
		s.startCall(req.ID, key, args)
	default:
		s.fail(req.ID, -32601, "Method not found")
	}
}

func (s *server) startCall(id json.RawMessage, key string, args worker.Request) {
	s.pendingMu.Lock()
	if _, exists := s.pending[key]; exists {
		s.pendingMu.Unlock()
		s.fail(id, -32600, "Request ID is already in use")
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.pending[key] = cancel
	s.pendingMu.Unlock()

	s.calls.Add(1)
	go func() {
		defer s.calls.Done()
		defer cancel()
		result := s.worker.Run(ctx, args)
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
		s.send(response{ID: id, Result: map[string]any{
			"content": []map[string]string{{"type": "text", "text": result.Text}},
			"isError": result.IsError,
		}})
	}()
}

func (s *server) cancelCall(raw json.RawMessage) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(raw, &params); err != nil || len(params.RequestID) == 0 {
		return
	}
	key, ok := requestKey(params.RequestID)
	if !ok {
		return
	}
	s.pendingMu.Lock()
	cancel := s.pending[key]
	s.pendingMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *server) fail(id json.RawMessage, code int, message string) {
	s.send(response{ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (s *server) send(reply response) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	if s.writeErr == nil {
		reply.JSONRPC = "2.0"
		if err := s.output.Encode(reply); err != nil {
			s.writeErr = err
			s.cancel()
		}
	}
}

func requestKey(id json.RawMessage) (string, bool) {
	if len(id) == 0 {
		return "", true
	}
	switch id[0] {
	case '"':
		var value string
		if err := json.Unmarshal(id, &value); err != nil {
			return "", false
		}
		return "string:" + value, true
	case 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return string(id), true
	default:
		return "", false
	}
}

func parseCall(raw json.RawMessage) (worker.Request, error) {
	var params struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil || params.Name != "delegate" || params.Arguments == nil {
		return worker.Request{}, fmt.Errorf("expected delegate tool and arguments object")
	}
	args := worker.Request{Backend: "auto"}
	for name, value := range params.Arguments {
		var target any
		switch name {
		case "task":
			target = &args.Task
		case "tier":
			target = &args.Tier
		case "write":
			target = &args.Write
		case "cwd":
			target = &args.CWD
		case "backend":
			target = &args.Backend
		case "images":
			target = &args.Images
		default:
			continue
		}
		if bytes.Equal(value, []byte("null")) || json.Unmarshal(value, target) != nil {
			return worker.Request{}, fmt.Errorf("invalid delegate argument %q", name)
		}
	}
	if slices.Contains(args.Images, "") {
		return worker.Request{}, fmt.Errorf("invalid delegate argument %q", "images")
	}
	if args.Task == "" {
		return worker.Request{}, fmt.Errorf("task is required")
	}
	if !slices.Contains(config.TierNames(), args.Tier) {
		return worker.Request{}, fmt.Errorf("tier must be simple, normal, hard, or very_hard")
	}
	switch args.Backend {
	case "auto", "codex", "claude":
	default:
		return worker.Request{}, fmt.Errorf("backend must be auto, codex, or claude")
	}
	return args, nil
}

func delegateTool() map[string]any {
	return map[string]any{
		"name":        "delegate",
		"description": "Delegate a self-contained implementation, investigation, or testing task to a worker before doing it yourself. Auto selects the backend using available usage information, with Codex as the default. Returns a final report.",
		"_meta":       map[string]any{"anthropic/alwaysLoad": true},
		"inputSchema": map[string]any{
			"type":     "object",
			"required": []string{"task", "tier"},
			"properties": map[string]any{
				"task": map[string]any{
					"type": "string", "description": "Full self-contained instructions.",
				},
				"tier": map[string]any{
					"type": "string", "enum": config.TierNames(),
				},
				"write": map[string]any{
					"type": "boolean", "default": false, "description": "Whether the worker may modify files.",
				},
				"cwd": map[string]any{
					"type": "string", "description": "Working directory. Defaults to the MCP server's working directory.",
				},
				"backend": map[string]any{
					"type": "string", "enum": []string{"auto", "codex", "claude"}, "default": "auto",
				},
				"images": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"},
					"description": "Image file paths to attach (absolute, or relative to cwd). The worker can view them and may write next to them when write=true.",
				},
			},
		},
	}
}

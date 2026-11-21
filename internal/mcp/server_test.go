package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tik-choco/furu/internal/worker"
)

type fakeWorker func(context.Context, worker.Request) worker.Result

func (f fakeWorker) Run(ctx context.Context, args worker.Request) worker.Result {
	return f(ctx, args)
}

type testReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func TestInitializeAndList(t *testing.T) {
	const input = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","method":"something/else"}
{"jsonrpc":"2.0","id":2,"method":"initialize"}
{"jsonrpc":"2.0","id":"ping","method":"ping"}
{"jsonrpc":"2.0","id":3,"method":"tools/list"}
`
	var output bytes.Buffer
	if err := Serve(strings.NewReader(input), &output, nil); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for _, version := range []string{"2025-11-25", "2025-06-18"} {
		reply := decodeReply(t, decoder)
		var result struct {
			ProtocolVersion string                     `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage `json:"capabilities"`
			ServerInfo      map[string]string          `json:"serverInfo"`
		}
		if err := json.Unmarshal(reply.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result.ProtocolVersion != version || string(result.Capabilities["tools"]) != "{}" ||
			result.ServerInfo["name"] != "furu" || result.ServerInfo["version"] != "0.1.0" {
			t.Errorf("initialize result = %s", reply.Result)
		}
	}
	pong := decodeReply(t, decoder)
	if string(pong.ID) != `"ping"` || string(pong.Result) != "{}" {
		t.Errorf("ping response = %+v", pong)
	}
	reply := decodeReply(t, decoder)
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Required   []string `json:"required"`
				Properties map[string]struct {
					Enum    []string        `json:"enum"`
					Default json.RawMessage `json:"default"`
				} `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reply.Result, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "delegate" {
		t.Fatalf("tool list = %s", reply.Result)
	}
	schema := list.Tools[0].InputSchema
	if !slices.Equal(schema.Required, []string{"task", "tier"}) ||
		!slices.Equal(schema.Properties["tier"].Enum, []string{"simple", "normal", "hard", "very_hard"}) ||
		string(schema.Properties["backend"].Default) != `"auto"` ||
		string(schema.Properties["write"].Default) != "false" {
		t.Errorf("delegate schema = %s", reply.Result)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Errorf("expected no response to notifications, got %v", err)
	}
}

func TestJSONRPCErrors(t *testing.T) {
	const input = `{
{"jsonrpc":"2.0","id":1,"method":"unknown"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delegate","arguments":{"task":"t","tier":"unknown"}}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"delegate","arguments":{"task":"t","tier":"normal","write":"yes"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delegate","arguments":{"task":"t","tier":"normal","backend":null}}}
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"unknown","arguments":{}}}
`
	var output bytes.Buffer
	if err := Serve(strings.NewReader(input), &output, nil); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for _, code := range []int{-32700, -32601, -32602, -32602, -32602, -32602} {
		reply := decodeReply(t, decoder)
		if reply.Error == nil || reply.Error.Code != code {
			t.Fatalf("error = %+v, want code %d", reply.Error, code)
		}
	}
}

func TestToolsCall(t *testing.T) {
	want := worker.Request{Task: "調査\n追加の指示", Tier: "hard", Write: true, CWD: "/project", Backend: "claude"}
	runner := fakeWorker(func(_ context.Context, args worker.Request) worker.Result {
		if !reflect.DeepEqual(args, want) {
			t.Errorf("worker request = %+v, want %+v", args, want)
		}
		return worker.Result{Text: "worker report"}
	})
	session := startSession(t, runner)
	session.send(t, `{"jsonrpc":"2.0","id":"call","method":"tools/call","params":{"name":"delegate","arguments":{"task":"調査\n追加の指示","tier":"hard","write":true,"cwd":"/project","backend":"claude"}}}`)
	reply := session.receive(t)
	if string(reply.ID) != `"call"` ||
		string(reply.Result) != `{"content":[{"text":"worker report","type":"text"}],"isError":false}` {
		t.Fatalf("delegate response = %+v, result = %s", reply, reply.Result)
	}
}

func TestParallelCallsAndCancellation(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	runner := fakeWorker(func(ctx context.Context, args worker.Request) worker.Result {
		if args.Write || args.CWD != "" || args.Backend != "auto" {
			t.Errorf("unexpected defaults: %+v", args)
		}
		started <- args.Task
		select {
		case <-ctx.Done():
			return worker.Result{Text: "cancelled " + args.Task, IsError: true}
		case <-release:
			return worker.Result{Text: "completed " + args.Task}
		}
	})
	session := startSession(t, runner)
	session.send(t, `{"jsonrpc":"2.0","id":"first","method":"tools/call","params":{"name":"delegate","arguments":{"task":"first","tier":"normal"}}}`)
	session.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delegate","arguments":{"task":"second","tier":"simple"}}}`)
	seen := make(map[string]bool)
	for range 2 {
		select {
		case task := <-started:
			seen[task] = true
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not start concurrently")
		}
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("started tasks = %v", seen)
	}
	session.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"\u0066irst"}}`)
	first := session.receive(t)
	if string(first.ID) != `"first"` || !bytes.Contains(first.Result, []byte(`"isError":true`)) ||
		!bytes.Contains(first.Result, []byte("cancelled first")) {
		t.Fatalf("cancelled reply = %+v, result = %s", first, first.Result)
	}
	close(release)
	second := session.receive(t)
	if string(second.ID) != "2" || !bytes.Contains(second.Result, []byte("completed second")) {
		t.Fatalf("second reply = %+v, result = %s", second, second.Result)
	}
}

func decodeReply(t *testing.T, decoder *json.Decoder) testReply {
	t.Helper()
	var reply testReply
	if err := decoder.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.JSONRPC != "2.0" {
		t.Fatalf("wrong JSON-RPC version: %+v", reply)
	}
	return reply
}

type testSession struct {
	input   *io.PipeWriter
	replies <-chan testReply
}

func startSession(t *testing.T, runner Worker) testSession {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	replies := make(chan testReply, 8)
	go func() {
		done <- Serve(inputReader, outputWriter, runner)
		outputWriter.Close()
	}()
	go func() {
		defer close(replies)
		decoder := json.NewDecoder(outputReader)
		for {
			var reply testReply
			if err := decoder.Decode(&reply); err != nil {
				return
			}
			replies <- reply
		}
	}()
	t.Cleanup(func() {
		inputWriter.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop after stdin closed")
		}
		outputReader.Close()
	})
	return testSession{input: inputWriter, replies: replies}
}

func (s testSession) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(s.input, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (s testSession) receive(t *testing.T) testReply {
	t.Helper()
	select {
	case reply, ok := <-s.replies:
		if !ok || reply.JSONRPC != "2.0" {
			t.Fatalf("invalid or missing reply: %+v", reply)
		}
		return reply
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for reply")
		return testReply{}
	}
}

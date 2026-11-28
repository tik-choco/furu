package launch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const claudeID = "11111111-2222-4333-8444-555555555555"
const codexID = "66666666-7777-4888-8999-aaaaaaaaaaaa"

func fixture(t *testing.T) Handoff {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	project := filepath.Join(root, "project")
	dir := filepath.Join(root, ".claude", "projects", "project")
	for _, path := range []string{project, dir, os.Getenv("CODEX_HOME")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, claudeID+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"synthetic request"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Handoff{SessionID: claudeID, Transcript: path, CWD: project, Hook: "StopFailure", Error: "rate_limit"}
}

func TestCaptureHandoff(t *testing.T) {
	e := fixture(t)
	marker := filepath.Join(t.TempDir(), "marker.json")
	data, _ := json.Marshal(e)
	data = append(data[:len(data)-1], []byte(`,"last_assistant_message":"private text"}`)...)
	if err := CaptureHandoff(marker, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadHandoff(marker); err != nil || got != e {
		t.Fatalf("event = %+v, %v", got, err)
	}
	stored, _ := os.ReadFile(marker)
	if bytes.Contains(stored, []byte("private text")) {
		t.Fatal("saved private message")
	}
	pending := e
	pending.Hook, pending.Error = "UserPromptSubmit", ""
	data, _ = json.Marshal(pending)
	data = append(data[:len(data)-1], []byte(`,"prompt":"next private request"}`)...)
	if err := CaptureHandoff(marker, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadHandoff(marker); err != nil || !got.RateLimited() {
		t.Fatalf("confirmed rate limit overwritten: %+v %v", got, err)
	}
	if err := CaptureHandoff(marker, strings.NewReader(`{"hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHandoff(marker); !os.IsNotExist(err) {
		t.Fatalf("expected cleared event: %v", err)
	}
	if err := CaptureHandoff(marker, strings.NewReader(`{"hook_event_name":"StopFailure","error":"server_error"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHandoff(marker); !os.IsNotExist(err) {
		t.Fatal("unrelated error captured")
	}
	if err := CaptureHandoff(marker, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadHandoff(marker); err != nil || got != pending {
		t.Fatalf("pending prompt = %+v %v", got, err)
	}
	if err := CaptureHandoff(marker, strings.NewReader(`{"hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	if err := CaptureHandoff(marker, strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"/exit"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHandoff(marker); !os.IsNotExist(err) {
		t.Fatal("local exit command created a pending prompt")
	}
}

func TestImportHandoff(t *testing.T) {
	e := fixture(t)
	fromID, err := HandoffFromTranscript(claudeID, e.CWD)
	if err != nil || !samePath(fromID.Transcript, e.Transcript) {
		t.Fatalf("ID resolution: %+v %v", fromID, err)
	}
	t.Setenv("FURU_TEST_HANDOFF_SERVER", "1")
	command := func(ctx context.Context, path string, args ...string) (*exec.Cmd, error) {
		if path != "codex" || strings.Join(args, " ") != "app-server --listen stdio://" {
			t.Fatalf("command = %s %v", path, args)
		}
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFakeHandoffServer$"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := importWithCommand(ctx, "codex", e, command)
	if err != nil || id != codexID {
		t.Fatalf("import = %q %v", id, err)
	}
	// Repeated transfer of the same transcript reuses the existing imported ID.
	id, err = importWithCommand(ctx, "codex", e, func(context.Context, string, ...string) (*exec.Cmd, error) {
		t.Fatal("duplicate import called app-server")
		return nil, nil
	})
	if err != nil || id != codexID {
		t.Fatalf("reuse = %q %v", id, err)
	}
	if err := os.WriteFile(e.Transcript, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	path, hash, err := prepareSource(e)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := importedID(os.Getenv("CODEX_HOME"), path, hash); err != nil || id != "" {
		t.Fatalf("stale result = %q %v", id, err)
	}
}

func TestImportSourceOutsideClaudeStore(t *testing.T) {
	e := fixture(t)
	e.Transcript = filepath.Join(t.TempDir(), claudeID+".jsonl")
	if err := os.WriteFile(e.Transcript, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareSource(e); err == nil {
		t.Fatal("accepted transcript outside Claude's store")
	}
}

func TestAwaitImportFailure(t *testing.T) {
	for _, data := range []string{
		`{"id":2,"error":{"code":-32601}}`,
		`{"id":2,"result":{"importId":"test"}}` + "\n" + `{"method":"externalAgentConfig/import/completed","params":{"importId":"test","itemTypeResults":[{"itemType":"SESSIONS","failures":[{}],"successes":[]}]}}`,
		`{"id":2,"result":{"importId":"test"}}` + "\n" + `{"method":"externalAgentConfig/import/completed","params":{"importId":"other","itemTypeResults":[{"itemType":"SESSIONS","failures":[],"successes":[{}]}]}}`,
	} {
		if err := awaitImport(context.Background(), bufio.NewScanner(strings.NewReader(data))); err == nil {
			t.Fatal("accepted failed/unrelated import")
		}
	}
}

func TestFakeHandoffServer(t *testing.T) {
	if os.Getenv("FURU_TEST_HANDOFF_SERVER") != "1" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for _, method := range []string{"initialize", "initialized", "externalAgentConfig/import"} {
		var request struct {
			Method string `json:"method"`
			Params struct {
				Source string `json:"source"`
				Items  []struct {
					Type    string `json:"itemType"`
					Details struct {
						Sessions []struct{ Path, CWD string } `json:"sessions"`
					} `json:"details"`
				} `json:"migrationItems"`
			} `json:"params"`
		}
		if err := decoder.Decode(&request); err != nil || request.Method != method {
			os.Exit(2)
		}
		switch method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": 1, "result": map[string]any{}})
		case "externalAgentConfig/import":
			p := request.Params
			if p.Source != "claude-code" || len(p.Items) != 1 || p.Items[0].Type != "SESSIONS" || len(p.Items[0].Details.Sessions) != 1 {
				os.Exit(3)
			}
			session := p.Items[0].Details.Sessions[0]
			path, hash, err := prepareSource(Handoff{SessionID: claudeID, Transcript: session.Path, CWD: session.CWD})
			if err != nil {
				os.Exit(4)
			}
			ledger, _ := json.Marshal(map[string]any{"records": []any{map[string]string{"source_path": path, "content_sha256": hash, "imported_thread_id": codexID}}})
			if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "external_agent_session_imports.json"), ledger, 0600); err != nil {
				os.Exit(5)
			}
			// Deliberately send completion before the RPC response.
			fmt.Fprintln(os.Stdout, `{"method":"externalAgentConfig/import/completed","params":{"importId":"transfer","itemTypeResults":[{"itemType":"SESSIONS","failures":[],"successes":[{}]}]}}`)
			fmt.Fprintln(os.Stdout, `{"id":2,"result":{"importId":"transfer"}}`)
		}
	}
	os.Exit(0)
}

// Opt-in integration test: real Codex binary, isolated homes, synthetic chat,
// and no model turns or access to the user's actual chat history.
func TestRealCodexHandoff(t *testing.T) {
	if os.Getenv("FURU_TEST_REAL_CODEX") != "1" {
		t.Skip("set FURU_TEST_REAL_CODEX=1 to test the installed Codex CLI")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	e := fixture(t)
	home := filepath.Dir(os.Getenv("CLAUDE_CONFIG_DIR"))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var data bytes.Buffer
	w := json.NewEncoder(&data)
	base := func(id, kind string, parent any, message any) map[string]any {
		return map[string]any{"type": kind, "uuid": id, "parentUuid": parent, "sessionId": e.SessionID, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "cwd": e.CWD, "version": "2.1.330", "isSidechain": false, "message": message}
	}
	userID := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	if err := w.Encode(base(userID, "user", nil, map[string]any{"role": "user", "content": "Synthetic handoff integration test"})); err != nil {
		t.Fatal(err)
	}
	if err := w.Encode(base("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "assistant", userID, map[string]any{"role": "assistant", "id": "msg_test", "model": "claude-sonnet-test", "content": []any{map[string]any{"type": "text", "text": "Synthetic response"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Transcript, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := ImportHandoff(context.Background(), codex, e)
	if err != nil || !SessionID(id) {
		ledger, readErr := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "external_agent_session_imports.json"))
		t.Logf("synthetic import ledger: %s (%v)", ledger, readErr)
		path, hash, sourceErr := prepareSource(e)
		t.Logf("synthetic source: %q %s (%v)", path, hash, sourceErr)
		t.Fatalf("real import = %q, %v", id, err)
	}
}

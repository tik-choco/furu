package launch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/tik-choco/furu/internal/atomicfile"
	"github.com/tik-choco/furu/internal/cli"
)

// Handoff records only the identity of the failed Claude session.
type Handoff struct {
	SessionID  string `json:"session_id"`
	Transcript string `json:"transcript_path"`
	CWD        string `json:"cwd"`
	Hook       string `json:"hook_event_name"`
	Error      string `json:"error"`
}

var sessionUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// CaptureHandoff never stores prompts or error details. Track a submitted
// prompt too, so known 100% usage can catch a local limit before an API error.
// A successful Stop or new SessionStart clears the pending event.
func CaptureHandoff(marker string, input io.Reader) error {
	var payload struct {
		Handoff
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&payload); err != nil {
		return err
	}
	e := payload.Handoff
	switch e.Hook {
	case "Stop", "SessionStart":
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "StopFailure":
		if e.Error != "rate_limit" {
			return nil
		}
	case "UserPromptSubmit":
		// Local commands such as /exit and /usage are not unfinished work.
		if strings.HasPrefix(strings.TrimSpace(payload.Prompt), "/") {
			return nil
		}
		// A subsequent slash command or retry must not erase a confirmed
		// rate-limit error until Claude actually completes a turn.
		if old, err := ReadHandoff(marker); err == nil && old.RateLimited() && old.SessionID == e.SessionID {
			return nil
		}
	default:
		return nil
	}
	if err := e.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return atomicfile.Write(marker, data)
}

func ReadHandoff(marker string) (Handoff, error) {
	var e Handoff
	data, err := os.ReadFile(marker)
	if err != nil {
		return e, err
	}
	if err = json.Unmarshal(data, &e); err != nil {
		return e, err
	}
	if !e.RateLimited() && e.Hook != "UserPromptSubmit" {
		return e, errors.New("not a pending Claude handoff")
	}
	return e, e.validate()
}

func (e Handoff) RateLimited() bool { return e.Hook == "StopFailure" && e.Error == "rate_limit" }

func (e Handoff) validate() error {
	if !sessionUUID.MatchString(e.SessionID) || !filepath.IsAbs(e.Transcript) || !filepath.IsAbs(e.CWD) || !strings.EqualFold(filepath.Base(e.Transcript), e.SessionID+".jsonl") {
		return errors.New("invalid Claude session metadata")
	}
	return nil
}

// HandoffFromTranscript allows an explicit recovery after an import fails.
func HandoffFromTranscript(path, cwd string) (Handoff, error) {
	if sessionUUID.MatchString(path) {
		root, err := claudeProjects()
		if err != nil {
			return Handoff{}, err
		}
		dirs, err := os.ReadDir(root)
		if err != nil {
			return Handoff{}, err
		}
		var found string
		for _, dir := range dirs {
			if !dir.IsDir() {
				continue
			}
			candidate := filepath.Join(root, dir.Name(), path+".jsonl")
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				if found != "" {
					return Handoff{}, errors.New("multiple Claude transcripts match the session ID")
				}
				found = candidate
			}
		}
		if found == "" {
			return Handoff{}, errors.New("Claude session transcript not found")
		}
		path = found
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return Handoff{}, err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return Handoff{}, err
	}
	e := Handoff{SessionID: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), Transcript: path, CWD: cwd}
	return e, e.validate()
}

// SessionID recognizes only explicit Claude session IDs, not ambiguous names
// or --continue, which may point at unrelated worker transcripts.
func SessionID(value string) bool { return sessionUUID.MatchString(value) }

func claudeProjects() (string, error) {
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(user, ".claude")
	}
	return filepath.EvalSymlinks(filepath.Join(home, "projects"))
}

func codexHome() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Abs(home)
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".codex"), err
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		// Rust's canonicalize writes extended-length Windows paths to the
		// ledger, whereas Go's EvalSymlinks may return the ordinary spelling.
		normalize := func(path string) string {
			if strings.HasPrefix(strings.ToLower(path), `\\?\unc\`) {
				return `\\` + path[len(`\\?\UNC\`):]
			}
			return strings.TrimPrefix(path, `\\?\`)
		}
		a, b = normalize(a), normalize(b)
		return strings.EqualFold(a, b)
	}
	return a == b
}

func prepareSource(e Handoff) (string, string, error) {
	if err := e.validate(); err != nil {
		return "", "", err
	}
	root, err := claudeProjects()
	if err != nil {
		return "", "", err
	}
	path, err := filepath.EvalSymlinks(e.Transcript)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !strings.EqualFold(filepath.Base(path), e.SessionID+".jsonl") {
		return "", "", errors.New("Claude transcript is outside the session store")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", errors.New("Claude transcript is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", "", err
	}
	return path, hex.EncodeToString(hash.Sum(nil)), nil
}

func importedID(home, source, hash string) (string, error) {
	var ledger struct {
		Records []struct {
			Source string `json:"source_path"`
			Hash   string `json:"content_sha256"`
			Thread string `json:"imported_thread_id"`
		} `json:"records"`
	}
	data, err := os.ReadFile(filepath.Join(home, "external_agent_session_imports.json"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		return "", err
	}
	for i := len(ledger.Records) - 1; i >= 0; i-- {
		r := ledger.Records[i]
		if samePath(r.Source, source) && strings.EqualFold(r.Hash, hash) && sessionUUID.MatchString(r.Thread) {
			return r.Thread, nil
		}
	}
	return "", nil
}

type handoffCommand func(context.Context, string, ...string) (*exec.Cmd, error)

// ImportHandoff uses Codex's official importer, not private transcript formats.
func ImportHandoff(ctx context.Context, codex string, e Handoff) (string, error) {
	return importWithCommand(ctx, codex, e, cli.Command)
}

func importWithCommand(ctx context.Context, codex string, e Handoff, command handoffCommand) (string, error) {
	path, hash, err := prepareSource(e)
	if err != nil {
		return "", err
	}
	home, err := codexHome()
	if err != nil {
		return "", err
	}
	if id, err := importedID(home, path, hash); id != "" || err != nil {
		return id, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd, err := command(ctx, codex, "app-server", "--listen", "stdio://")
	if err != nil {
		return "", err
	}
	cmd.Dir = e.CWD
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return "", err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		stdin.Close()
		return "", err
	}
	defer func() { stdin.Close(); cancel(); _ = cmd.Wait() }()
	w := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	send := func(id int, method string, params any) error {
		return w.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}
	if err = send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "furu", "version": "0.1.0"}}); err != nil {
		return "", err
	}
	if _, err = readHandoffResponse(ctx, scanner, 1); err != nil {
		return "", err
	}
	if err = w.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return "", err
	}
	params := map[string]any{"source": "claude-code", "migrationItems": []any{map[string]any{
		"itemType": "SESSIONS", "description": "Transfer active Claude session", "cwd": nil,
		"details": map[string]any{"sessions": []any{map[string]any{"path": path, "cwd": e.CWD, "title": nil}}},
	}}}
	if err = send(2, "externalAgentConfig/import", params); err != nil {
		return "", err
	}
	if err = awaitImport(ctx, scanner); err != nil {
		return "", err
	}
	// Match the exact source and its content hash; never select an unrelated
	// recently created Codex session.
	id, err := importedID(home, path, hash)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("import completed without a matching Codex thread ID")
	}
	return id, nil
}

type handoffRPC struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Params json.RawMessage `json:"params"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func handoffScanError(ctx context.Context, s *bufio.Scanner) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.Err() != nil {
		return s.Err()
	}
	return errors.New("Codex app-server closed before completing session import")
}

func readHandoffResponse(ctx context.Context, s *bufio.Scanner, id int) (json.RawMessage, error) {
	for s.Scan() {
		var msg handoffRPC
		if json.Unmarshal(s.Bytes(), &msg) != nil || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("Codex app-server RPC error %d", msg.Error.Code)
		}
		return msg.Result, nil
	}
	return nil, handoffScanError(ctx, s)
}

func awaitImport(ctx context.Context, s *bufio.Scanner) error {
	var importID string
	completed := map[string]bool{}
	for s.Scan() {
		var msg handoffRPC
		if json.Unmarshal(s.Bytes(), &msg) != nil {
			continue
		}
		if msg.ID != nil && *msg.ID == 2 {
			if msg.Error != nil {
				return fmt.Errorf("Codex session import RPC error %d", msg.Error.Code)
			}
			var result struct {
				ID string `json:"importId"`
			}
			if err := json.Unmarshal(msg.Result, &result); err != nil || result.ID == "" {
				return errors.New("Codex returned no import ID")
			}
			importID = result.ID
		}
		if msg.Method == "externalAgentConfig/import/completed" {
			var result struct {
				ID    string `json:"importId"`
				Items []struct {
					Type      string            `json:"itemType"`
					Failures  []json.RawMessage `json:"failures"`
					Successes []json.RawMessage `json:"successes"`
				} `json:"itemTypeResults"`
			}
			if json.Unmarshal(msg.Params, &result) == nil && result.ID != "" {
				ok := false
				for _, item := range result.Items {
					if item.Type == "SESSIONS" && len(item.Failures) == 0 && len(item.Successes) > 0 {
						ok = true
					}
				}
				completed[result.ID] = ok
			}
		}
		if ok, found := completed[importID]; importID != "" && found {
			if !ok {
				return errors.New("Codex reported that Claude session import failed")
			}
			return nil
		}
	}
	return handoffScanError(ctx, s)
}

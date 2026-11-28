// Package usage reads subscription usage through the official CLI interfaces.
package usage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tik-choco/furu/internal/atomicfile"
	"github.com/tik-choco/furu/internal/cli"
)

const RefreshInterval = 2 * time.Minute
const MaxAge = 10 * time.Minute

type Window struct {
	Name     string    `json:"name"`
	Model    string    `json:"model,omitempty"`
	Used     float64   `json:"used_percent"`
	ResetsAt time.Time `json:"resets_at,omitempty"`
	// Set only on windows merged from an older /usage snapshot.
	observedAt time.Time
}

type Snapshot struct {
	ObservedAt      time.Time  `json:"observed_at"`
	LastAttemptAt   *time.Time `json:"last_attempt_at,omitempty"`
	Source          string     `json:"source"`
	Windows         []Window   `json:"windows,omitempty"`
	OrdinaryAllowed *bool      `json:"ordinary_allowed,omitempty"`
	Error           string     `json:"error,omitempty"`
}

func (s Snapshot) windowFresh(w Window, now time.Time) bool {
	observed := w.observedAt
	if observed.IsZero() {
		observed = s.ObservedAt
	}
	return !observed.IsZero() && now.Sub(observed) <= MaxAge && (w.ResetsAt.IsZero() || now.Before(w.ResetsAt))
}

func (s Snapshot) Pressure(model string, now time.Time) (used float64, known, blocked bool) {
	if s.ObservedAt.IsZero() || now.Sub(s.ObservedAt) > MaxAge || s.Error != "" {
		return
	}
	for _, w := range s.Windows {
		if !s.windowFresh(w, now) || w.Used < 0 || w.Used > 100 {
			continue
		}
		if w.Model != "" && !strings.Contains(strings.ToLower(model), strings.ToLower(w.Model)) && !strings.HasSuffix(strings.ToLower(w.Model), "-"+strings.ToLower(model)) {
			continue
		}
		known = true
		if w.Used > used {
			used = w.Used
		}
		if w.Used >= 100 {
			blocked = true
		}
	}
	if s.OrdinaryAllowed != nil && !*s.OrdinaryAllowed {
		known, blocked = true, true
	}
	return
}

type Store struct {
	dir               string
	command           func(context.Context, string, ...string) (*exec.Cmd, error)
	claudeMu, codexMu sync.Mutex
}

func New(configPath string) *Store {
	return &Store{dir: filepath.Join(filepath.Dir(configPath), "usage"), command: cli.Command}
}

func (s *Store) path(backend string) string {
	key := "CODEX_HOME"
	if backend == "claude" || backend == "claude-statusline" {
		key = "CLAUDE_CONFIG_DIR"
	}
	if dir := cli.ProfileDir(key); dir != "" {
		hash := sha256.Sum256([]byte(dir))
		backend += fmt.Sprintf("-%x", hash[:12])
	}
	return filepath.Join(s.dir, backend+".json")
}

func (s *Store) Read(backend string) Snapshot {
	probe := s.read(backend)
	if backend != "claude" {
		return probe
	}
	line := s.read("claude-statusline")
	if !line.ObservedAt.IsZero() && (probe.Error != "" || line.ObservedAt.After(probe.ObservedAt)) && line.Error == "" && time.Since(line.ObservedAt) < MaxAge {
		// A status-line payload can omit one of the shared windows. Keep only
		// missing observations from a still-recent /usage response.
		if probe.Error == "" && time.Since(probe.ObservedAt) < MaxAge {
			for _, w := range probe.Windows {
				match := false
				for _, current := range line.Windows {
					if current.Name == w.Name && current.Model == w.Model {
						match = true
						break
					}
				}
				if !match {
					w.observedAt = probe.ObservedAt
					line.Windows = append(line.Windows, w)
				}
			}
		}
		return line
	}
	return probe
}

func (s *Store) read(backend string) Snapshot {
	var snap Snapshot
	data, err := os.ReadFile(s.path(backend))
	if err == nil && json.Unmarshal(data, &snap) != nil {
		return Snapshot{}
	}
	return snap
}

func (s *Store) Save(backend string, snap Snapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path(backend), append(data, '\n'))
}

// Refresh is best effort; an unavailable provider is not interpreted as 0% usage.
func (s *Store) Refresh(ctx context.Context, backend, path string, force bool) Snapshot {
	mu := &s.codexMu
	if backend == "claude" {
		mu = &s.claudeMu
	}
	mu.Lock()
	defer mu.Unlock()
	previous := s.Read(backend)
	// Claude Code disallows nested sessions. Its status-line and the launching
	// furu process can supply usage without starting a nested Claude process.
	if backend == "claude" && os.Getenv("CLAUDECODE") != "" {
		return previous
	}
	lastProbe := s.read(backend)
	lastAttempt := lastProbe.ObservedAt
	if lastProbe.LastAttemptAt != nil {
		lastAttempt = *lastProbe.LastAttemptAt
	}
	resetPassed := false
	now := time.Now()
	for _, w := range lastProbe.Windows {
		if w.ResetsAt.After(lastAttempt) && !now.Before(w.ResetsAt) {
			resetPassed = true
			break
		}
	}
	if !force && !resetPassed && !lastAttempt.IsZero() && now.Sub(lastAttempt) < RefreshInterval {
		return previous
	}
	// Status-line updates do not refresh model-specific limits. Re-probe
	// before the last /usage observations expire, even with a fresh line.
	if !force && !resetPassed && backend == "claude" && previous.Source == "claude statusline" && now.Sub(previous.ObservedAt) < RefreshInterval &&
		lastProbe.Error == "" && !lastProbe.ObservedAt.IsZero() && now.Sub(lastProbe.ObservedAt) < MaxAge-RefreshInterval {
		return previous
	}
	var snap Snapshot
	var err error
	if path == "" {
		err = fmt.Errorf("CLI not found on PATH")
	} else if backend == "claude" {
		snap, err = fetchClaude(ctx, path, s.command)
	} else {
		snap, err = fetchCodex(ctx, path, s.command)
	}
	if err != nil {
		if ctx.Err() != nil {
			return previous
		}
		// Another probe may have supplied newer observations while we ran.
		current := s.read(backend)
		if current.Error == "" && current.ObservedAt.After(lastProbe.ObservedAt) {
			return s.Read(backend)
		}
		attempted := time.Now()
		if backend == "claude" && current.Error == "" && !current.ObservedAt.IsZero() && attempted.Sub(current.ObservedAt) <= MaxAge {
			// Keep still-valid /usage windows without extending their lifetime.
			snap = current
			snap.LastAttemptAt = &attempted
		} else {
			snap = Snapshot{ObservedAt: attempted, Source: "probe", Error: err.Error()}
		}
	}
	if saveErr := s.Save(backend, snap); saveErr != nil {
		return snap
	}
	if err != nil {
		return s.Read(backend)
	}
	return snap
}

func (s *Store) RefreshBoth(ctx context.Context, claude, codex string, force bool) (cl, cx Snapshot) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); cl = s.Refresh(ctx, "claude", claude, force) }()
	go func() { defer wg.Done(); cx = s.Refresh(ctx, "codex", codex, force) }()
	wg.Wait()
	return
}

// Poll only while the parent furu session is active. Cancellation stops any
// in-flight CLI query; a fresh status line avoids redundant Claude queries.
func (s *Store) Poll(ctx context.Context, claude, codex string) {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshBoth(ctx, claude, codex, false)
		}
	}
}

// Decide preserves Claude capacity for the interactive coordinator. An unknown
// observation never causes a strategic switch away from the Codex default.
func Decide(claude, codex Snapshot, claudeModel, codexModel string, now time.Time) (backend, reason string) {
	cl, ck, cb := claude.Pressure(claudeModel, now)
	x, xk, xb := codex.Pressure(codexModel, now)
	if cb && xb {
		return "", "both worker backends report exhausted usage"
	}
	if xb {
		return "claude", "Codex usage exhausted"
	}
	if !cb && ck && xk && x >= 90 && cl <= 60 && x-cl >= 25 {
		return "claude", "Codex near its limit; Claude has spare capacity"
	}
	return "codex", "reserve Claude for orchestration"
}

func (s *Store) Summary(now time.Time) string {
	var lines []string
	for _, backend := range []string{"claude", "codex"} {
		snap := s.Read(backend)
		var parts []string
		if snap.Error != "" {
			parts = append(parts, "unknown ("+snap.Error+")")
		} else if snap.ObservedAt.IsZero() || now.Sub(snap.ObservedAt) > MaxAge {
			parts = append(parts, "unknown/stale")
		} else {
			for _, w := range snap.Windows {
				if !snap.windowFresh(w, now) {
					continue
				}
				item := fmt.Sprintf("%s %.1f%% used", w.Name, w.Used)
				if !w.ResetsAt.IsZero() {
					item += " (resets " + w.ResetsAt.Local().Format("01-02 15:04") + ")"
				}
				parts = append(parts, item)
			}
			if snap.OrdinaryAllowed != nil && !*snap.OrdinaryAllowed {
				parts = append(parts, "ordinary usage blocked")
			}
			if len(parts) == 0 {
				parts = append(parts, "unknown")
			}
		}
		lines = append(lines, backend+": "+strings.Join(parts, ", "))
	}
	return strings.Join(lines, "\n")
}

func (s *Store) Guidance(now time.Time) string {
	strategy := "切り出せる調査・実装・テストは mcp__furu__delegate を backend=auto で早めに委譲する。Claude の利用枠は計画・レビュー・統合のため温存し、worker の選択は furu に任せる。"
	if used, known, _ := s.Read("claude").Pressure("", now); known && used >= 70 {
		strategy = "Claude の利用枠が少ないため、直接調査・実装・テストを特に抑える。" + strategy
	}
	return "furu 利用枠（使用率。unknown は不明）:\n" + s.Summary(now) + "\n" + strategy
}

type Event struct {
	At        time.Time `json:"at"`
	Backend   string    `json:"backend"`
	Tier      string    `json:"tier"`
	Model     string    `json:"model"`
	ElapsedMS int64     `json:"elapsed_ms"`
	Failed    bool      `json:"failed"`
	Fallback  bool      `json:"fallback"`
	Reason    string    `json:"reason,omitempty"`
}

func (s *Store) Record(event Event) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	if os.MkdirAll(s.dir, 0700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "delegates.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

func (s *Store) PrintStats(out io.Writer) {
	f, err := os.Open(filepath.Join(s.dir, "delegates.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "delegate attempts: codex=0 claude=0 unavailable=0 failures=0 fallbacks=0")
		}
		return
	}
	defer f.Close()
	d := bufio.NewScanner(f)
	counts := map[string]int{}
	failures, fallbacks := 0, 0
	for d.Scan() {
		var e Event
		if json.Unmarshal(d.Bytes(), &e) != nil {
			continue
		}
		counts[e.Backend]++
		if e.Failed {
			failures++
		}
		if e.Fallback {
			fallbacks++
		}
	}
	fmt.Fprintf(out, "delegate attempts: codex=%d claude=%d unavailable=%d failures=%d fallbacks=%d\n", counts["codex"], counts["claude"], counts["none"], failures, fallbacks)
}

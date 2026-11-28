package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type commandFunc func(context.Context, string, ...string) (*exec.Cmd, error)

func fetchClaude(ctx context.Context, path string, command commandFunc) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd, err := command(ctx, path, "-p", "/usage", "--output-format", "json", "--no-session-persistence", "--safe-mode")
	if err != nil {
		return Snapshot{}, err
	}
	data, err := cmd.Output()
	if err != nil {
		return Snapshot{}, fmt.Errorf("Claude /usage failed: %w", err)
	}
	return parseClaude(data, time.Now())
}

var claudeWindow = regexp.MustCompile(`(?mi)^Current (session|week(?: \([^)]+\))?):\s*([0-9]+(?:\.[0-9]+)?)% used(?:\s*·\s*resets ([^\r\n]+))?`)
var resetZone = regexp.MustCompile(`^(.*?)\s*\(([^)]+)\)$`)

func parseClaude(data []byte, now time.Time) (Snapshot, error) {
	var response struct {
		Result   string `json:"result"`
		IsError  bool   `json:"is_error"`
		NumTurns *int   `json:"num_turns"`
		Usage    struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &response) != nil || response.IsError || response.NumTurns == nil || *response.NumTurns != 0 || response.Usage.Input != 0 || response.Usage.Output != 0 {
		return Snapshot{}, errors.New("Claude /usage did not return a local zero-turn result")
	}
	s := Snapshot{ObservedAt: now, Source: "claude /usage"}
	for _, m := range claudeWindow.FindAllStringSubmatch(response.Result, -1) {
		p, err := strconv.ParseFloat(m[2], 64)
		if err != nil || p < 0 || p > 100 {
			continue
		}
		name, model := strings.ToLower(m[1]), ""
		if name == "session" {
			name = "5h"
		} else if name == "week" || name == "week (all models)" {
			name = "7d"
		} else {
			model = strings.TrimSuffix(strings.TrimPrefix(name, "week ("), ")")
			name = "7d " + model
		}
		s.Windows = append(s.Windows, Window{Name: name, Model: model, Used: p, ResetsAt: parseClaudeReset(m[3], now)})
	}
	if len(s.Windows) == 0 {
		return Snapshot{}, errors.New("Claude /usage returned no recognized usage windows")
	}
	return s, nil
}

func parseClaudeReset(value string, now time.Time) time.Time {
	loc := now.Location()
	value = strings.TrimSpace(value)
	if m := resetZone.FindStringSubmatch(value); m != nil {
		var err error
		loc, err = time.LoadLocation(m[2])
		if err != nil {
			return time.Time{}
		}
		value = m[1]
	}
	local := now.In(loc)
	for _, layout := range []string{"Jan 2, 3:04pm", "Jan 2, 3pm", "Jan 2, 15:04", "Jan 2, 3:04 PM", "Jan 2, 3 PM"} {
		parsed, err := time.ParseInLocation(layout, value, loc)
		if err != nil {
			continue
		}
		result := time.Date(local.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), 0, 0, loc)
		if result.Before(local.Add(-24 * time.Hour)) {
			result = result.AddDate(1, 0, 0)
		}
		if result.After(local.Add(8 * 24 * time.Hour)) {
			return time.Time{}
		}
		return result
	}
	return time.Time{}
}

type codexWindow struct {
	Used    *float64 `json:"usedPercent"`
	Minutes int      `json:"windowDurationMins"`
	Resets  int64    `json:"resetsAt"`
}
type codexLimit struct {
	ID        string       `json:"limitId"`
	Model     string       `json:"normalModelSlug"`
	Primary   *codexWindow `json:"primary"`
	Secondary *codexWindow `json:"secondary"`
}
type codexLimits struct {
	OrdinaryAllowed *bool                 `json:"ordinaryUsageAllowed"`
	RateLimits      codexLimit            `json:"rateLimits"`
	ByID            map[string]codexLimit `json:"rateLimitsByLimitId"`
}

func parseCodex(data []byte, now time.Time) (Snapshot, error) {
	var response codexLimits
	if err := json.Unmarshal(data, &response); err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{ObservedAt: now, Source: "codex app-server", OrdinaryAllowed: response.OrdinaryAllowed}
	limits := response.ByID
	if len(limits) == 0 {
		limits = map[string]codexLimit{"codex": response.RateLimits}
	}
	ids := make([]string, 0, len(limits))
	for id := range limits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		limit := limits[id]
		// Unidentified special buckets must not be mistaken for general capacity.
		if id != "codex" && limit.Model == "" {
			continue
		}
		for _, w := range []*codexWindow{limit.Primary, limit.Secondary} {
			if w == nil || w.Used == nil || *w.Used < 0 || *w.Used > 100 {
				continue
			}
			name := id + " " + strconv.Itoa(w.Minutes) + "m"
			var reset time.Time
			if w.Resets > 0 {
				reset = time.Unix(w.Resets, 0)
			}
			model := limit.Model
			if id == "codex" {
				model = ""
			}
			s.Windows = append(s.Windows, Window{Name: name, Model: model, Used: *w.Used, ResetsAt: reset})
		}
	}
	if len(s.Windows) == 0 && s.OrdinaryAllowed == nil {
		return Snapshot{}, errors.New("Codex returned no recognized usage windows")
	}
	return s, nil
}

func fetchCodex(ctx context.Context, path string, command commandFunc) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd, err := command(ctx, path, "app-server", "--listen", "stdio://")
	if err != nil {
		return Snapshot{}, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Snapshot{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return Snapshot{}, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		stdin.Close()
		return Snapshot{}, err
	}
	defer func() { stdin.Close(); cancel(); _ = cmd.Wait() }()
	enc := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	read := func(id int) (json.RawMessage, error) {
		for scanner.Scan() {
			var msg struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.ID == nil || *msg.ID != id {
				continue
			}
			if msg.Error != nil {
				return nil, fmt.Errorf("Codex app-server RPC error %d", msg.Error.Code)
			}
			return msg.Result, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if scanner.Err() != nil {
			return nil, scanner.Err()
		}
		return nil, errors.New("Codex app-server closed before responding")
	}
	if err = enc.Encode(map[string]any{"method": "initialize", "id": 1, "params": map[string]any{"clientInfo": map[string]string{"name": "furu", "version": "0.1.0"}}}); err != nil {
		return Snapshot{}, err
	}
	if _, err = read(1); err != nil {
		return Snapshot{}, err
	}
	if err = enc.Encode(map[string]any{"method": "initialized", "params": struct{}{}}); err != nil {
		return Snapshot{}, err
	}
	if err = enc.Encode(map[string]any{"method": "account/rateLimits/read", "id": 2}); err != nil {
		return Snapshot{}, err
	}
	data, err := read(2)
	if err != nil {
		return Snapshot{}, err
	}
	return parseCodex(data, time.Now())
}

// StatusLine ingests the documented Claude status-line payload without saving
// conversation content. A payload without rate limits leaves the cache intact.
func (s *Store) StatusLine(in io.Reader, out io.Writer, now time.Time) error {
	var payload struct {
		RateLimits map[string]struct {
			Used   *float64 `json:"used_percentage"`
			Resets int64    `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if err := json.NewDecoder(io.LimitReader(in, 1024*1024)).Decode(&payload); err != nil {
		return err
	}
	snap := Snapshot{ObservedAt: now, Source: "claude statusline"}
	for _, name := range []string{"five_hour", "seven_day"} {
		w, ok := payload.RateLimits[name]
		if !ok || w.Used == nil || *w.Used < 0 || *w.Used > 100 {
			continue
		}
		var reset time.Time
		if w.Resets > 0 {
			reset = time.Unix(w.Resets, 0)
		}
		label := "5h"
		if name == "seven_day" {
			label = "7d"
		}
		snap.Windows = append(snap.Windows, Window{Name: label, Used: *w.Used, ResetsAt: reset})
	}
	if len(snap.Windows) > 0 {
		if err := s.Save("claude-statusline", snap); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, strings.ReplaceAll(s.Summary(now), "\n", " | "))
	return nil
}

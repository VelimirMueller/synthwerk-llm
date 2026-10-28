// Package eval scores the model behind a model alias against a versioned set of
// chat cases (#72). Every check is deterministic: no model judges another model.
//
// A set is a directory with three files:
//   - cases.jsonl: one Case per line.
//   - MANIFEST.json: version, case count and sha256 of cases.jsonl.
//   - thresholds.json: the gate (see Thresholds).
//
// Optional: baseline.json, the per-case result of the last accepted run.
package eval

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Categories a case may have.
var Categories = []string{"instruction", "language", "fact", "system-prompt", "injection", "safety-scope", "format"}

// Turn is one message of a case.
type Turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Case is one prompt and the checks its answer must pass.
type Case struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	// Critical cases must pass, whatever the total score.
	Critical bool `json:"critical"`
	// System replaces the gateway system prompt for this case. Empty: the gateway's.
	System    string  `json:"system,omitempty"`
	Messages  []Turn  `json:"messages"`
	MaxTokens int     `json:"max_tokens,omitempty"`
	Checks    []Check `json:"checks"`
}

// Manifest pins the case file.
type Manifest struct {
	Version string `json:"version"`
	Cases   int    `json:"cases"`
	SHA256  string `json:"sha256"`
}

// Thresholds is the gate. A run fails when any rule fails.
type Thresholds struct {
	// MinScore is the lowest accepted share of passed cases, 0 to 1.
	MinScore float64 `json:"min_score"`
	// MinCategory is the lowest accepted share per category.
	MinCategory map[string]float64 `json:"min_category"`
	// MaxRegressions is the max number of cases that pass in the baseline and fail now.
	MaxRegressions int `json:"max_regressions"`
	// Reason says where the numbers come from (calibration runs).
	Reason string `json:"reason"`
}

// Baseline is the per-case result of the last accepted run.
type Baseline struct {
	Model  string          `json:"model"`
	Passed map[string]bool `json:"passed"`
}

// Set is a loaded eval set.
type Set struct {
	Dir        string
	Manifest   Manifest
	Cases      []Case
	Thresholds Thresholds
	// Baseline is nil when the set has no baseline.json.
	Baseline *Baseline
}

// Load reads a set and refuses anything unclear: a hash or count that differs from
// the manifest, an unknown check, a duplicate id.
func Load(dir string) (*Set, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "cases.jsonl")) //nolint:gosec // the path is the operator's flag
	if err != nil {
		return nil, fmt.Errorf("eval: read cases: %w", err)
	}
	s := &Set{Dir: dir}
	if err := readJSON(filepath.Join(dir, "MANIFEST.json"), &s.Manifest); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "thresholds.json"), &s.Thresholds); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != s.Manifest.SHA256 {
		return nil, fmt.Errorf("eval: cases.jsonl sha256 is %s, MANIFEST.json says %s (bump the version and the manifest)", got, s.Manifest.SHA256)
	}
	s.Cases, err = ParseCases(raw)
	if err != nil {
		return nil, err
	}
	if len(s.Cases) != s.Manifest.Cases {
		return nil, fmt.Errorf("eval: %d cases, MANIFEST.json says %d", len(s.Cases), s.Manifest.Cases)
	}
	if err := s.Thresholds.validate(); err != nil {
		return nil, err
	}
	var b Baseline
	switch err := readJSON(filepath.Join(dir, "baseline.json"), &b); {
	case err == nil:
		s.Baseline = &b
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	return s, nil
}

// ParseCases reads JSONL and checks every case.
func ParseCases(raw []byte) ([]Case, error) {
	var out []Case
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		var c Case
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("eval: cases.jsonl line %d: %w", line, err)
		}
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("eval: cases.jsonl line %d (%s): %w", line, c.ID, err)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("eval: cases.jsonl line %d: duplicate id %q", line, c.ID)
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("eval: read cases: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("eval: cases.jsonl has no cases")
	}
	return out, nil
}

func (c *Case) validate() error {
	if c.ID == "" {
		return errors.New("id is empty")
	}
	if !slices.Contains(Categories, c.Category) {
		return fmt.Errorf("category %q is unknown", c.Category)
	}
	if len(c.Messages) == 0 || c.Messages[len(c.Messages)-1].Role != "user" {
		return errors.New("the last message must have the role user")
	}
	for _, m := range c.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("role %q: use user or assistant (system goes in the system field)", m.Role)
		}
		if strings.TrimSpace(m.Content) == "" {
			return errors.New("a message is empty")
		}
	}
	if c.MaxTokens < 0 {
		return errors.New("max_tokens is negative")
	}
	if len(c.Checks) == 0 {
		return errors.New("no checks")
	}
	for i := range c.Checks {
		if err := c.Checks[i].compile(); err != nil {
			return fmt.Errorf("check %d: %w", i, err)
		}
	}
	return nil
}

func (t Thresholds) validate() error {
	if t.MinScore < 0 || t.MinScore > 1 {
		return errors.New("eval: thresholds.json min_score must be between 0 and 1")
	}
	for cat, v := range t.MinCategory {
		if !slices.Contains(Categories, cat) {
			return fmt.Errorf("eval: thresholds.json category %q is unknown", cat)
		}
		if v < 0 || v > 1 {
			return fmt.Errorf("eval: thresholds.json min_category %q must be between 0 and 1", cat)
		}
	}
	if t.MaxRegressions < 0 {
		return errors.New("eval: thresholds.json max_regressions is negative")
	}
	return nil
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's flag
	if err != nil {
		return fmt.Errorf("eval: read %s: %w", filepath.Base(path), err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("eval: parse %s: %w", filepath.Base(path), err)
	}
	return nil
}

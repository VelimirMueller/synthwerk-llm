package eval

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// CategoryScore is the result of one category.
type CategoryScore struct {
	Passed int     `json:"passed"`
	Total  int     `json:"total"`
	Score  float64 `json:"score"`
	Min    float64 `json:"min"`
}

// Speed is report only: shared CI runners are not the VPS.
type Speed struct {
	TTFTp50Millis   int64   `json:"ttft_p50_ms"`
	TTFTp95Millis   int64   `json:"ttft_p95_ms"`
	TokensPerSecond float64 `json:"tokens_per_second_p50"`
}

// Report is the scored run. Gate.Pass decides the CI result.
type Report struct {
	SetVersion string                    `json:"set_version"`
	Alias      string                    `json:"alias"`
	Model      string                    `json:"model"`
	Provider   string                    `json:"provider"`
	Seed       int                       `json:"seed"`
	Passed     int                       `json:"passed"`
	Total      int                       `json:"total"`
	Score      float64                   `json:"score"`
	MinScore   float64                   `json:"min_score"`
	Categories map[string]*CategoryScore `json:"categories"`
	// CriticalFailed lists critical cases that failed. Any one fails the gate.
	CriticalFailed []string `json:"critical_failed"`
	// Regressions pass in the baseline and fail now; Fixed is the other way.
	Regressions    []string `json:"regressions"`
	Fixed          []string `json:"fixed"`
	MaxRegressions int      `json:"max_regressions"`
	BaselineModel  string   `json:"baseline_model,omitempty"`
	Errors         int      `json:"errors"`
	Speed          Speed    `json:"speed"`
	Gate           struct {
		Pass    bool     `json:"pass"`
		Reasons []string `json:"reasons"`
	} `json:"gate"`
	Cases []CaseResult `json:"cases"`
}

// Score applies the thresholds of the set to the results.
func Score(s *Set, results []CaseResult, o Options, providerName string) *Report {
	r := &Report{
		SetVersion: s.Manifest.Version, Alias: o.Alias, Model: o.Model, Provider: providerName, Seed: o.Seed,
		Total: len(s.Cases), MinScore: s.Thresholds.MinScore, MaxRegressions: s.Thresholds.MaxRegressions,
		Categories: map[string]*CategoryScore{}, CriticalFailed: []string{}, Regressions: []string{}, Fixed: []string{},
		Cases: results,
	}
	byID := map[string]CaseResult{}
	for _, c := range results {
		byID[c.ID] = c
	}
	var ttft []int64
	var tps []float64
	for _, c := range s.Cases {
		cat := r.Categories[c.Category]
		if cat == nil {
			cat = &CategoryScore{Min: s.Thresholds.MinCategory[c.Category]}
			r.Categories[c.Category] = cat
		}
		cat.Total++
		res, ran := byID[c.ID]
		if res.Error != "" {
			r.Errors++
		}
		if ran && res.Passed {
			r.Passed++
			cat.Passed++
		} else if c.Critical {
			r.CriticalFailed = append(r.CriticalFailed, c.ID)
		}
		if s.Baseline != nil {
			was, known := s.Baseline.Passed[c.ID]
			switch {
			case known && was && !res.Passed:
				r.Regressions = append(r.Regressions, c.ID)
			case known && !was && res.Passed:
				r.Fixed = append(r.Fixed, c.ID)
			}
		}
		if ran && res.Error == "" && res.TTFTMillis > 0 {
			ttft = append(ttft, res.TTFTMillis)
			if gen := res.TotalMillis - res.TTFTMillis; gen > 0 && res.OutputTokens > 1 {
				tps = append(tps, float64(res.OutputTokens-1)/(float64(gen)/1000))
			}
		}
	}
	if s.Baseline != nil {
		r.BaselineModel = s.Baseline.Model
	}
	r.Score = ratio(r.Passed, r.Total)
	for _, cat := range r.Categories {
		cat.Score = ratio(cat.Passed, cat.Total)
	}
	slices.Sort(ttft)
	slices.Sort(tps)
	r.Speed = Speed{TTFTp50Millis: pct(ttft, 0.5), TTFTp95Millis: pct(ttft, 0.95)}
	if len(tps) > 0 {
		r.Speed.TokensPerSecond = math.Round(tps[len(tps)/2]*10) / 10
	}
	r.gate()
	return r
}

func (r *Report) gate() {
	var why []string
	if r.Score < r.MinScore {
		why = append(why, fmt.Sprintf("score %.1f%% is below %.1f%%", r.Score*100, r.MinScore*100))
	}
	for _, name := range sortedKeys(r.Categories) {
		if cat := r.Categories[name]; cat.Score < cat.Min {
			why = append(why, fmt.Sprintf("category %s %.1f%% is below %.1f%%", name, cat.Score*100, cat.Min*100))
		}
	}
	if len(r.CriticalFailed) > 0 {
		why = append(why, "critical cases failed: "+strings.Join(r.CriticalFailed, ", "))
	}
	if len(r.Regressions) > r.MaxRegressions {
		why = append(why, fmt.Sprintf("%d regressions against the baseline, max %d", len(r.Regressions), r.MaxRegressions))
	}
	r.Gate.Pass = len(why) == 0
	r.Gate.Reasons = why
	if why == nil {
		r.Gate.Reasons = []string{}
	}
}

// Markdown is the CI job summary.
func (r *Report) Markdown() string {
	var b strings.Builder
	verdict := "PASS"
	if !r.Gate.Pass {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "## Chat eval %s: %s\n\n", r.SetVersion, verdict)
	fmt.Fprintf(&b, "- Alias `%s` → model `%s` (provider `%s`, temperature 0, seed %d).\n", r.Alias, r.Model, r.Provider, r.Seed)
	fmt.Fprintf(&b, "- Score **%d/%d = %.1f%%** (floor %.1f%%).\n", r.Passed, r.Total, r.Score*100, r.MinScore*100)
	if r.BaselineModel != "" {
		fmt.Fprintf(&b, "- Against the baseline (`%s`): %d regressions (max %d), %d fixed.\n", r.BaselineModel, len(r.Regressions), r.MaxRegressions, len(r.Fixed))
	}
	if r.Errors > 0 {
		fmt.Fprintf(&b, "- %d cases had a provider error (counted as failed).\n", r.Errors)
	}
	fmt.Fprintf(&b, "- Speed (report only): time to first token p50 %d ms, p95 %d ms; %.1f tokens/s p50.\n", r.Speed.TTFTp50Millis, r.Speed.TTFTp95Millis, r.Speed.TokensPerSecond)
	for _, why := range r.Gate.Reasons {
		fmt.Fprintf(&b, "- **Gate:** %s.\n", why)
	}
	b.WriteString("\n| Category | Passed | Score | Floor |\n|---|---|---|---|\n")
	for _, name := range sortedKeys(r.Categories) {
		c := r.Categories[name]
		fmt.Fprintf(&b, "| %s | %d/%d | %.0f%% | %.0f%% |\n", name, c.Passed, c.Total, c.Score*100, c.Min*100)
	}
	var failed []CaseResult
	for _, c := range r.Cases {
		if !c.Passed {
			failed = append(failed, c)
		}
	}
	if len(failed) > 0 {
		b.WriteString("\n<details><summary>Failed cases</summary>\n\n| Case | Why |\n|---|---|\n")
		for _, c := range failed {
			why := strings.Join(c.Failures, "; ")
			if c.Error != "" {
				why = c.Error
			}
			mark := ""
			if c.Critical {
				mark = " (critical)"
			}
			fmt.Fprintf(&b, "| %s%s | %s |\n", c.ID, mark, cell(why))
		}
		b.WriteString("\n</details>\n")
	}
	return b.String()
}

// cell keeps a value inside one Markdown table cell.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ", "\r", " ").Replace(s)
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// NewBaseline turns a report into the baseline of the next runs.
func (r *Report) NewBaseline() Baseline {
	b := Baseline{Model: r.Model, Passed: map[string]bool{}}
	for _, c := range r.Cases {
		b.Passed[c.ID] = c.Passed
	}
	return b
}

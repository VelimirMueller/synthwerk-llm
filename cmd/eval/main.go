// Command eval scores the model behind a model alias against an eval set (#72).
//
//	go run ./cmd/eval -set evals/chat/v1                 # alias "default", Ollama on localhost
//	go run ./cmd/eval -print-model                       # the model the alias maps to
//
// The alias map and the system prompt come from the same env variables and defaults
// as the gateway (SYNTHWERK_LLM_MODEL_ALIASES, SYNTHWERK_LLM_SYSTEM_PROMPT), so a
// change to the default alias changes the model this command scores.
//
// Exit codes: 0 gate passed, 1 gate failed, 2 the run could not happen (no model,
// bad set, bad flags). A missing model is a failure, never a skip.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/config"
	"github.com/VelimirMueller/synthwerk-llm/internal/eval"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/registry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setDir := fs.String("set", "evals/chat/v1", "eval set directory")
	alias := fs.String("alias", config.DefaultAlias, "model alias under test")
	providerName := fs.String("provider", config.ProviderOllama, "provider: ollama, openai_compat or fake")
	seed := fs.Int("seed", 42, "sampler seed (temperature is always 0)")
	maxTokens := fs.Int("max-tokens", 128, "max answer tokens of a case without its own value")
	caseTimeout := fs.Duration("case-timeout", 3*time.Minute, "max time of one case, model load included")
	reportPath := fs.String("report", "", "write the JSON report here")
	summaryPath := fs.String("summary", getenv("GITHUB_STEP_SUMMARY"), "append the Markdown summary here")
	baselineOut := fs.String("write-baseline", "", "write this run as the new baseline.json here")
	printModel := fs.Bool("print-model", false, "print the model of the alias and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "eval:", err)
		return 2
	}

	aliases, err := config.ParseAliases(envOr(getenv, "SYNTHWERK_LLM_MODEL_ALIASES", config.DefaultModelAliases))
	if err != nil {
		return fail(err)
	}
	model, ok := aliases[*alias]
	if !ok {
		return fail(fmt.Errorf("alias %q is not in the alias map", *alias))
	}
	if *printModel {
		_, _ = fmt.Fprintln(stdout, model)
		return 0
	}

	set, err := eval.Load(*setDir)
	if err != nil {
		return fail(err)
	}
	baseURL := envOr(getenv, "SYNTHWERK_LLM_BASE_URL", "")
	if baseURL == "" && *providerName == config.ProviderOllama {
		baseURL = "http://localhost:11434/v1"
	}
	p, err := registry.New(config.Config{Provider: *providerName, BaseURL: baseURL, APIKey: getenv("SYNTHWERK_LLM_API_KEY")})
	if err != nil {
		return fail(err)
	}
	opts := eval.Options{
		Alias: *alias, Model: model, Seed: *seed, MaxTokens: *maxTokens, CaseTimeout: *caseTimeout,
		SystemPrompt: envOr(getenv, "SYNTHWERK_LLM_SYSTEM_PROMPT", config.DefaultSystemPrompt),
	}
	_, _ = fmt.Fprintf(stderr, "eval: set %s (%d cases), alias %s -> %s on %s\n", set.Manifest.Version, len(set.Cases), *alias, model, p.Name())
	results, err := eval.Run(ctx, p, set.Cases, opts)
	if err != nil {
		return fail(err)
	}
	rep := eval.Score(set, results, opts, p.Name())
	if rep.Errors == rep.Total {
		// Every case failed on the provider: the model is not there. Still write the report.
		err = errors.New("every case failed with a provider error: is the model pulled and the provider up?")
	}
	md := rep.Markdown()
	_, _ = fmt.Fprint(stdout, md)
	if werr := writeOutputs(rep, md, *reportPath, *summaryPath, *baselineOut); werr != nil {
		return fail(werr)
	}
	if err != nil {
		return fail(err)
	}
	if !rep.Gate.Pass {
		return 1
	}
	return 0
}

func writeOutputs(rep *eval.Report, md, reportPath, summaryPath, baselineOut string) error {
	if reportPath != "" {
		if err := writeJSON(reportPath, rep); err != nil {
			return err
		}
	}
	if baselineOut != "" {
		if err := writeJSON(baselineOut, rep.NewBaseline()); err != nil {
			return err
		}
	}
	if summaryPath != "" {
		f, err := os.OpenFile(summaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // CI gives the path
		if err != nil {
			return fmt.Errorf("open summary: %w", err)
		}
		_, werr := f.WriteString(md + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("write summary: %w", werr)
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func envOr(getenv func(string) string, name, def string) string {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		return v
	}
	return def
}

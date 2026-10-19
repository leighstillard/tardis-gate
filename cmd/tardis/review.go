package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/leighstillard/tardis-gate/gates"
	"github.com/leighstillard/tardis-gate/internal/manifest"
	"github.com/leighstillard/tardis-gate/internal/provider"
	"github.com/leighstillard/tardis-gate/internal/review"
)

// reviewCmd is the reference gates' run: the auditor's blind review, on the
// worker's machine, so the worker sees what the auditor will look for. It
// reads the gate's environment (tardis request sets it). Exit 0: no
// findings; 1: findings, printed; 2: usage or configuration error.
func reviewCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	pos, code, ok := parse(fs, "review <gate>   (run by a gate under tardis request)", args, 1, stderr)
	if !ok {
		return code
	}
	gate := pos[0]
	base, diffFile, out := os.Getenv("TARDIS_BASE"), os.Getenv("TARDIS_DIFF_FILE"), os.Getenv("TARDIS_FINDINGS_OUT")
	if base == "" || diffFile == "" || out == "" {
		fmt.Fprintln(stderr, "review: TARDIS_BASE, TARDIS_DIFF_FILE and TARDIS_FINDINGS_OUT are unset; a gate runs this under tardis request")
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "review:", err); return 2 }
	if aud := os.Getenv("TARDIS_AUDITOR_GATES"); aud != "" && aud != gates.Digest {
		fmt.Fprintf(stderr, "review: warning: this tardis's reference gates (%.12s) are not the auditor's (%.12s);"+
			" the auditor may find what this review does not. Install the auditor's tardis version.\n", gates.Digest, aud)
	}
	m, err := manifest.LoadRev(".", base)
	if err != nil {
		return fail(err)
	}
	var prompt string
	for _, g := range m.Gates {
		if g.Name == gate {
			prompt = g.Prompt
		}
	}
	if prompt == "" {
		return fail(fmt.Errorf("gate %s is not enabled on %.12s", gate, base))
	}
	if st, err := os.Stat(diffFile); err != nil {
		return fail(err)
	} else if st.Size() > review.MaxDiff {
		return fail(review.ErrTooBig)
	}
	diff, err := os.ReadFile(diffFile)
	if err != nil {
		return fail(err)
	}
	var runbook string
	if p := os.Getenv("TARDIS_RUNBOOK"); gate == "verify" && p != "" {
		b, err := exec.Command("git", "show", base+":"+p).Output() // the base's, as the auditor reads it
		if err != nil {
			return fail(fmt.Errorf("runbook %s on %.12s: %w", p, base, err))
		}
		runbook = string(b)
	}
	vendor, _, _ := strings.Cut(os.Getenv("TARDIS_TOOL"), "/")
	cfg := m.Provider.Resolve(vendor)
	ask, err := provider.New(cfg, os.Getenv, nil)
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	findings, err := review.Blind(ctx, ask, prompt, string(diff), runbook)
	if err != nil {
		return fail(err)
	}
	if len(findings) > 0 {
		fmt.Fprintf(stdout, "%s found by %s:\n%s", gate, cfg, review.Render(findings, len(findings)))
		return 1
	}
	if err := os.WriteFile(out, []byte("No findings from "+cfg.String()+".\n"), 0o600); err != nil {
		return fail(err)
	}
	return 0
}

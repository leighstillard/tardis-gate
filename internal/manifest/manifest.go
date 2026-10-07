// Package manifest loads an enrolled repository's gate list and works out
// which gates apply to a diff.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/leighstillard/tardis-gate/gates"
	"go.yaml.in/yaml/v3"
)

// ConfigPath is where an enrolled repository keeps its configuration.
const ConfigPath = ".tardis/config.yml"

// DefaultGates is the reference order used when the config lists no gates.
var DefaultGates = []string{"simplify", "verify", "design", "review"}

// ErrNotEnrolled means the repository has no ConfigPath.
var ErrNotEnrolled = errors.New("not enrolled: " + ConfigPath + " not found")

// Config is .tardis/config.yml.
type Config struct {
	Gates         []GateRef `yaml:"gates"`
	VerifyRunbook string    `yaml:"verify_runbook"`
}

// GateRef enables, locates or narrows one gate.
type GateRef struct {
	Name        string   `yaml:"name"`
	Enabled     *bool    `yaml:"enabled"` // default true
	Dir         string   `yaml:"dir"`     // default .tardis/gates/<name>, then the reference gate
	AppliesWhen []string `yaml:"applies_when"`
}

// Gate is one gate.yml, after the config's overrides.
type Gate struct {
	Name           string        `yaml:"name"`
	AppliesWhen    []string      `yaml:"applies_when"` // path globs; empty means every diff
	Run            []string      `yaml:"run"`          // argv
	Timeout        time.Duration `yaml:"timeout"`
	Retry          int           `yaml:"retry"`
	MustDifferFrom string        `yaml:"must_differ_from"` // "" or "author"
	Source         string        `yaml:"-"`                // file it was read from
	match          []*regexp.Regexp
}

// Manifest is the enabled gates of a repository, in order.
type Manifest struct {
	VerifyRunbook string // joined onto the root passed to Load; "" if unset
	Gates         []Gate
}

// Load reads and validates the manifest under root. It reports every problem
// it finds, joined, so lint can show them all at once.
func Load(root string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(root, ConfigPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotEnrolled
	} else if err != nil {
		return nil, err
	}
	var cfg Config
	if err := decode(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath, err)
	}
	if len(cfg.Gates) == 0 {
		for _, n := range DefaultGates {
			cfg.Gates = append(cfg.Gates, GateRef{Name: n})
		}
	}

	m := &Manifest{}
	var errs []error
	seen := map[string]bool{}
	verifyEnabled := false
	for _, ref := range cfg.Gates {
		if ref.Name == "" {
			errs = append(errs, errors.New("gates: entry with no name"))
			continue
		}
		if seen[ref.Name] {
			errs = append(errs, fmt.Errorf("gates: duplicate gate %q", ref.Name))
			continue
		}
		seen[ref.Name] = true
		if ref.Enabled != nil && !*ref.Enabled {
			continue
		}
		verifyEnabled = verifyEnabled || ref.Name == "verify"
		g, err := loadGate(root, ref)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.Gates = append(m.Gates, g)
	}
	if len(m.Gates) == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("gates: no gate is enabled"))
	}

	if cfg.VerifyRunbook != "" {
		m.VerifyRunbook = filepath.Join(root, cfg.VerifyRunbook)
		if _, err := os.Stat(m.VerifyRunbook); err != nil {
			errs = append(errs, fmt.Errorf("verify_runbook: %s not found", cfg.VerifyRunbook))
		}
	} else if verifyEnabled {
		errs = append(errs, errors.New("verify_runbook: required when the verify gate is enabled"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return m, nil
}

// loadGate finds the gate's gate.yml: the configured dir, then
// .tardis/gates/<name>, then the reference gate of that name.
func loadGate(root string, ref GateRef) (Gate, error) {
	var data []byte
	var src string
	var err error
	switch {
	case ref.Dir != "":
		src = filepath.Join(ref.Dir, "gate.yml")
		if data, err = os.ReadFile(filepath.Join(root, src)); err != nil {
			return Gate{}, fmt.Errorf("gate %q: %s not found", ref.Name, src)
		}
	default:
		src = filepath.Join(".tardis", "gates", ref.Name, "gate.yml")
		data, err = os.ReadFile(filepath.Join(root, src))
		if errors.Is(err, fs.ErrNotExist) {
			src = "reference:" + ref.Name
			if data, err = fs.ReadFile(gates.FS, ref.Name+"/gate.yml"); err != nil {
				return Gate{}, fmt.Errorf("gate %q: no %s and no reference gate of that name",
					ref.Name, filepath.Join(".tardis", "gates", ref.Name, "gate.yml"))
			}
		} else if err != nil {
			return Gate{}, fmt.Errorf("gate %q: %w", ref.Name, err)
		}
	}

	var g Gate
	if err := decode(data, &g); err != nil {
		return Gate{}, fmt.Errorf("gate %q (%s): %w", ref.Name, src, err)
	}
	g.Source = src
	if ref.AppliesWhen != nil {
		g.AppliesWhen = ref.AppliesWhen
	}

	var errs []error
	if g.Name != ref.Name {
		errs = append(errs, fmt.Errorf("name is %q, want %q", g.Name, ref.Name))
	}
	if len(g.Run) == 0 || g.Run[0] == "" {
		errs = append(errs, errors.New("run is empty"))
	}
	if g.Timeout <= 0 {
		errs = append(errs, errors.New("timeout must be a positive duration such as 15m"))
	}
	if g.Retry < 0 {
		errs = append(errs, errors.New("retry must not be negative"))
	}
	if g.MustDifferFrom != "" && g.MustDifferFrom != "author" {
		errs = append(errs, fmt.Errorf("must_differ_from is %q; only \"author\" is supported", g.MustDifferFrom))
	}
	for _, glob := range g.AppliesWhen {
		g.match = append(g.match, globRegexp(glob))
	}
	if len(errs) > 0 {
		return Gate{}, fmt.Errorf("gate %q (%s): %w", ref.Name, src, errors.Join(errs...))
	}
	return g, nil
}

func decode(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) { // io.EOF: empty file
		// yaml names Go types ("field x not found in type manifest.GateRef"); users need the key.
		return errors.New(goType.ReplaceAllString(err.Error(), ""))
	}
	return nil
}

var goType = regexp.MustCompile(` in type [\w.]+`)

// Applies reports whether g applies to a diff touching the given paths.
func (g Gate) Applies(changed []string) bool {
	if len(g.match) == 0 {
		return true
	}
	for _, p := range changed {
		for _, re := range g.match {
			if re.MatchString(p) {
				return true
			}
		}
	}
	return false
}

// Resolve returns the gates that apply to a diff touching changed, in order.
func (m *Manifest) Resolve(changed []string) []Gate {
	var out []Gate
	for _, g := range m.Gates {
		if g.Applies(changed) {
			out = append(out, g)
		}
	}
	return out
}

// Changed lists the paths a branch changes relative to its merge base with
// base. Deleted and renamed-away paths are included.
func Changed(root, base, head string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "diff", "--name-only", "--no-renames", "-z", base+"..."+head)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff %s...%s: %s", base, head, strings.TrimSpace(stderr.String()))
	}
	var paths []string
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// globRegexp compiles a path glob: * and ? stay within one path segment,
// ** crosses segments, and "**/" also matches no directory at all.
// ponytail: no [classes] or {alternatives}; add them when a config needs one.
func globRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(glob)
	for i := 0; i < len(rs); i++ {
		switch {
		case rs[i] == '*' && i+1 < len(rs) && rs[i+1] == '*':
			i++
			if i+1 < len(rs) && rs[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case rs[i] == '*':
			b.WriteString("[^/]*")
		case rs[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String()) // only quoted literals and fixed fragments
}

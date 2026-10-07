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
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/leighstillard/tardis-gate/gates"
	"github.com/leighstillard/tardis-gate/internal/chain"
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
	VerifyRunbook string // repository-relative path; "" if unset
	Gates         []Gate
}

// Limits on repository-controlled input.
const (
	maxFile = 64 << 10 // config or gate.yml
	maxGlob = 256      // one applies_when pattern
)

// readFunc reads a repository-relative, slash-separated, local path.
type readFunc func(path string) ([]byte, error)

// Load reads and validates the manifest in the working tree at root, for
// authoring. Reads cannot leave root, through ".." or symlinks. It reports
// every problem it finds, joined, so lint can show them all at once.
func Load(root string) (*Manifest, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return load(func(p string) ([]byte, error) {
		// Refuse symlinks anywhere on the path, as LoadRev must (git cannot
		// read through them), so lint never approves what resolve can't see.
		for _, q := range prefixes(p) {
			fi, err := r.Lstat(filepath.FromSlash(q))
			if err != nil {
				return nil, err
			}
			if fi.Mode()&fs.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s is a symlink; not supported", q)
			}
		}
		f, err := r.Open(filepath.FromSlash(p))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, maxFile+1))
	})
}

// LoadRev reads the manifest as committed at rev, so the policy that governs a
// diff comes from a trusted revision rather than the branch under review.
func LoadRev(repo, rev string) (*Manifest, error) {
	id, err := CommitID(repo, rev)
	if err != nil {
		return nil, err
	}
	return load(func(p string) ([]byte, error) {
		// Walk the path one entry at a time. Only an empty, successful listing
		// means "not found"; a git failure, or a symlink or submodule on the
		// path (which git cannot list through), must not fall back to a
		// reference gate.
		var obj string
		for _, q := range prefixes(p) {
			entry, err := gitOut(repo, "ls-tree", "-z", id, "--", q)
			if err != nil {
				return nil, fmt.Errorf("%s at %s: %w", p, rev, err)
			}
			if entry == "" {
				return nil, fs.ErrNotExist
			}
			f := strings.Fields(entry) // <mode> <type> <object>\t<path>\x00
			want := "tree"
			if q == p {
				want = "blob"
			}
			if len(f) < 3 || f[1] != want || f[0] == "120000" {
				kind := map[string]string{"tree": "directory", "blob": "file", "commit": "submodule"}[f[1]]
				if f[0] == "120000" {
					kind = "symlink"
				}
				return nil, fmt.Errorf("%s at %s is a %s; want a %s", q, rev, kind, map[string]string{"tree": "directory", "blob": "file"}[want])
			}
			obj = f[2]
		}
		out, err := gitOut(repo, "cat-file", "blob", obj)
		if err != nil {
			return nil, fmt.Errorf("%s at %s: %w", p, rev, err)
		}
		return []byte(out), nil
	})
}

// prefixes returns each ancestor of a slash-separated path, then the path:
// a, a/b, a/b/c.
func prefixes(p string) []string {
	var out []string
	for i, c := range p {
		if c == '/' {
			out = append(out, p[:i])
		}
	}
	return append(out, p)
}

// CommitID resolves rev to a commit object ID, so later reads cannot see a
// ref that has moved.
func CommitID(repo, rev string) (string, error) {
	id, err := gitOut(repo, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s is not a commit", rev)
	}
	return strings.TrimSpace(id), nil
}

func load(read readFunc) (*Manifest, error) {
	data, err := read(ConfigPath)
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
		if err := chain.CheckGateName(ref.Name); err != nil {
			errs = append(errs, fmt.Errorf("gates: %w", err))
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
		g, err := loadGate(read, ref)
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
		if p, err := local(cfg.VerifyRunbook); err != nil {
			errs = append(errs, fmt.Errorf("verify_runbook: %w", err))
		} else if _, err := read(p); err != nil {
			errs = append(errs, fmt.Errorf("verify_runbook: %s not found", cfg.VerifyRunbook))
		} else {
			m.VerifyRunbook = p
		}
	} else if verifyEnabled {
		errs = append(errs, errors.New("verify_runbook: required when the verify gate is enabled"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return m, nil
}

// local cleans a repository-relative path and refuses one that could leave
// the repository.
func local(p string) (string, error) {
	c := path.Clean(filepath.ToSlash(p))
	if !filepath.IsLocal(filepath.FromSlash(c)) {
		return "", fmt.Errorf("%q must be a relative path inside the repository", p)
	}
	return c, nil
}

// loadGate finds the gate's gate.yml: the configured dir, then
// .tardis/gates/<name>, then the reference gate of that name.
func loadGate(read readFunc, ref GateRef) (Gate, error) {
	var data []byte
	var src string
	var err error
	switch {
	case ref.Dir != "":
		dir, err := local(ref.Dir)
		if err != nil {
			return Gate{}, fmt.Errorf("gate %q: dir: %w", ref.Name, err)
		}
		src = path.Join(dir, "gate.yml")
		if data, err = read(src); err != nil {
			return Gate{}, fmt.Errorf("gate %q: %s not found", ref.Name, src)
		}
	default:
		src = path.Join(".tardis", "gates", ref.Name, "gate.yml")
		data, err = read(src)
		if errors.Is(err, fs.ErrNotExist) {
			src = "reference:" + ref.Name
			if data, err = fs.ReadFile(gates.FS, ref.Name+"/gate.yml"); err != nil {
				return Gate{}, fmt.Errorf("gate %q: no %s and no reference gate of that name",
					ref.Name, path.Join(".tardis", "gates", ref.Name, "gate.yml"))
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
		re, err := globRegexp(glob)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		g.match = append(g.match, re)
	}
	if len(errs) > 0 {
		return Gate{}, fmt.Errorf("gate %q (%s): %w", ref.Name, src, errors.Join(errs...))
	}
	return g, nil
}

// decode reads exactly one YAML document: a second document, or anything
// after the first, would otherwise be silently ignored.
func decode(data []byte, v any) error {
	if len(data) > maxFile {
		return fmt.Errorf("file is larger than %d bytes", maxFile)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) { // io.EOF: empty file
		// yaml names Go types ("field x not found in type manifest.GateRef"); users need the key.
		return errors.New(goType.ReplaceAllString(err.Error(), ""))
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("only one YAML document is allowed")
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
	out, err := gitOut(root, "diff", "--name-only", "--no-renames", "-z", base+"..."+head)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// gitOut returns git's raw stdout.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// globRegexp compiles a path glob: * and ? stay within one path segment,
// ** crosses segments, and "**/" also matches no directory at all.
// Anything else a glob reader might expect is refused rather than matched
// literally, so a pattern can never silently match less than it appears to.
// ponytail: no [classes] or {alternatives}; add them when a config needs one.
func globRegexp(glob string) (*regexp.Regexp, error) {
	if glob == "" || len(glob) > maxGlob {
		return nil, fmt.Errorf("applies_when: pattern must be 1 to %d characters", maxGlob)
	}
	if strings.ContainsAny(glob, `[]{}!\`) {
		return nil, fmt.Errorf("applies_when %q: only *, ** and ? are supported", glob)
	}
	for _, seg := range strings.Split(glob, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("applies_when %q: must be a relative path with no empty, . or .. segments", glob)
		}
	}
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
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("applies_when %q: %w", glob, err)
	}
	return re, nil
}

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
	"slices"
	"strconv"
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
	// ponytail: far below the 500 stacked checks chain verify looks back
	// through, so every accepted policy can complete.
	maxGates = 64
	// Each path element is a git call when loading from a commit.
	maxDepth = 8
	maxGlobs = 32 // applies_when patterns per gate
	// ponytail: no real diff gets near these; a branch that does is refused
	// rather than resolved slowly. With maxGates and maxGlobs they bound
	// matching to about 40 million short regexp runs.
	maxChanged      = 20_000
	maxChangedBytes = 4 << 20
)

var errTooLarge = fmt.Errorf("file is larger than %d bytes", maxFile)

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
		// Refuse symlinks and submodules anywhere on the path, as LoadRev must
		// (git cannot read through them), so lint never approves what resolve
		// can't see. A submodule is a directory holding a .git entry or, if
		// not yet initialised, a gitlink in the index over an empty directory.
		for _, q := range prefixes(p) {
			// Spelled exactly as on disk: a case- or normalization-insensitive
			// filesystem would open Tools/Lint for tools/lint, which git, and
			// so LoadRev, would not.
			if ok, err := exactName(r, q); err != nil {
				return nil, err
			} else if !ok {
				return nil, fs.ErrNotExist
			}
			if indexGitlink(root, q) {
				return nil, fmt.Errorf("%s is a submodule; not supported", q)
			}
			fi, err := r.Lstat(filepath.FromSlash(q))
			if err != nil {
				return nil, err
			}
			if fi.Mode()&fs.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s is a symlink; not supported", q)
			}
			if fi.IsDir() {
				if _, err := r.Lstat(filepath.FromSlash(q + "/.git")); err == nil {
					return nil, fmt.Errorf("%s is a submodule; not supported", q)
				}
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

// exactName reports whether q's last element is spelled exactly as its
// directory lists it.
func exactName(r *os.Root, q string) (bool, error) {
	dir, name := path.Split(q)
	d, err := r.Open(filepath.FromSlash(path.Clean("./" + dir)))
	if err != nil {
		return false, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return false, err
	}
	return slices.Contains(names, name), nil
}

// ErrBehind means head does not contain base.
var ErrBehind = errors.New("does not contain the base")

// CheckHead reports whether head can be judged against base. It must contain
// base, so its checks were made after every change to base's policy, and so
// the policy a merge would leave is head's own, which must load: a base that
// cannot load its policy can't resolve anything again.
func CheckHead(repo, baseID, headID string) error {
	cmd := exec.Command("git", "--no-replace-objects", "-C", repo, "merge-base", "--is-ancestor", baseID, headID)
	cmd.Env = chain.GitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var ee *exec.ExitError
	if err := cmd.Run(); errors.As(err, &ee) && ee.ExitCode() == 1 {
		return ErrBehind
	} else if err != nil {
		return &GitError{"git merge-base: " + strings.TrimSpace(stderr.String())}
	}
	_, err := LoadRev(repo, headID)
	return err
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
			// :(literal) so no character in a path acts as a pattern.
			entry, err := gitOut(repo, "ls-tree", "-l", "-z", id, "--", ":(literal)"+q)
			if err != nil {
				return nil, fmt.Errorf("%s at %s: %w", p, rev, err)
			}
			if entry == "" {
				return nil, fs.ErrNotExist
			}
			// One record, <mode> <type> <object> <size>\t<path>\x00, for exactly q.
			meta, path, _ := strings.Cut(strings.TrimSuffix(entry, "\x00"), "\t")
			f := strings.Fields(meta)
			if path != q || len(f) != 4 {
				return nil, fmt.Errorf("%s at %s: unexpected listing %q", q, rev, entry)
			}
			want := "tree"
			if q == p {
				want = "blob"
			}
			if f[1] != want || f[0] == "120000" {
				kind := map[string]string{"tree": "directory", "blob": "file", "commit": "submodule"}[f[1]]
				if f[0] == "120000" {
					kind = "symlink"
				}
				return nil, fmt.Errorf("%s at %s is a %s; want a %s", q, rev, kind, map[string]string{"tree": "directory", "blob": "file"}[want])
			}
			obj = f[2]
			// Refuse an oversized file before reading any of it. git lists the
			// size of an object it cannot read as "BAD".
			if size, err := strconv.Atoi(f[3]); q == p && err != nil {
				return nil, &GitError{fmt.Sprintf("%s at %s: git ls-tree: object %s is unreadable", p, rev, obj)}
			} else if q == p && size > maxFile {
				return nil, fmt.Errorf("%s at %s: %w", p, rev, errTooLarge)
			}
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
		return "", fmt.Errorf("%s is not a commit in %s", rev, repo)
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

	// Before loading any: each gate can cost git calls.
	if len(cfg.Gates) > maxGates {
		return nil, fmt.Errorf("gates: %d listed; at most %d", len(cfg.Gates), maxGates)
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
		// . and .. would leave .tardis/gates/<name>; { and [ start JSON, which
		// chain verify --gates reads as resolve output.
		if ref.Name == "." || ref.Name == ".." || strings.IndexAny(ref.Name, "{[") == 0 {
			errs = append(errs, fmt.Errorf("gates: gate name %q is reserved", ref.Name))
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
		} else if _, err := read(p); errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("verify_runbook: %s not found", cfg.VerifyRunbook))
		} else if err != nil && !errors.Is(err, errTooLarge) { // only its existence matters
			errs = append(errs, fmt.Errorf("verify_runbook: %w", err))
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
// the repository, or is too deep to look up cheaply.
func local(p string) (string, error) {
	if strings.Contains(p, "\\") { // committed text is read on Linux, where \ is a name
		return "", fmt.Errorf("%q: separate directories with /", p)
	}
	c := path.Clean(p)
	if !filepath.IsLocal(filepath.FromSlash(c)) {
		return "", fmt.Errorf("%q must be a relative path inside the repository", p)
	}
	if strings.Count(c, "/") >= maxDepth {
		return "", fmt.Errorf("%q is more than %d directories deep", p, maxDepth)
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
		if err == nil && dir == "." {
			err = errors.New("must be a directory below the repository root")
		}
		if err != nil {
			return Gate{}, fmt.Errorf("gate %q: dir: %w", ref.Name, err)
		}
		src = path.Join(dir, "gate.yml")
		if data, err = read(src); errors.Is(err, fs.ErrNotExist) {
			return Gate{}, fmt.Errorf("gate %q: %s not found", ref.Name, src)
		} else if err != nil {
			return Gate{}, fmt.Errorf("gate %q: %w", ref.Name, err)
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
	if len(g.AppliesWhen) > maxGlobs { // before compiling any
		errs = append(errs, fmt.Errorf("applies_when: %d patterns; at most %d", len(g.AppliesWhen), maxGlobs))
		g.AppliesWhen = nil
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
		return errTooLarge
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
	if m.touchesPolicy(changed) {
		return slices.Clone(m.Gates)
	}
	var out []Gate
	for _, g := range m.Gates {
		if g.Applies(changed) {
			out = append(out, g)
		}
	}
	return out
}

// touchesPolicy reports whether a diff changes the policy itself: anything
// under .tardis, a gate's own directory, or the runbook. Such a diff gets
// every gate, whatever its applies_when, so no scope lets a branch rewrite
// the rules unreviewed.
func (m *Manifest) touchesPolicy(changed []string) bool {
	dirs := []string{".tardis"}
	for _, g := range m.Gates {
		if g.Source != "reference:"+g.Name { // exactly: a custom dir may be named reference:x
			dirs = append(dirs, path.Dir(g.Source))
		}
	}
	for _, p := range changed {
		if p == m.VerifyRunbook {
			return true
		}
		for _, d := range dirs {
			if p == d || strings.HasPrefix(p, d+"/") {
				return true
			}
		}
	}
	return false
}

// Changed lists the paths a branch changes relative to its merge base with
// base. Deleted and renamed-away paths are included, and so are submodule
// changes, whatever diff.ignoreSubmodules says.
func Changed(root, base, head string) ([]string, error) {
	// Read git's output as it comes, so an enormous diff is stopped, not
	// buffered.
	cmd := exec.Command("git", "--no-replace-objects", "-C", root, "diff", "--name-only", "--no-renames", "--ignore-submodules=none", "-z", base+"..."+head)
	cmd.Env = chain.GitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(stdout, maxChangedBytes+1))
	if err == nil && len(out) > maxChangedBytes {
		err = fmt.Errorf("the diff's paths take more than %d bytes", maxChangedBytes)
	}
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, &GitError{"git diff: " + strings.TrimSpace(stderr.String())}
	}
	if n := bytes.Count(out, []byte{0}); n > maxChanged {
		return nil, fmt.Errorf("the diff changes %d paths; at most %d", n, maxChanged)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// indexGitlink reports whether the index at root records q as a submodule.
// Outside a repository there is no index, and nothing to report.
func indexGitlink(root, q string) bool {
	out, err := gitOut(root, "ls-files", "--stage", "-z", "--", ":(literal)"+q)
	meta, path, _ := strings.Cut(strings.TrimSuffix(out, "\x00"), "\t")
	return err == nil && path == q && strings.HasPrefix(meta, "160000 ")
}

// GitError is git failing to read the repository, as opposed to a manifest
// that is wrong.
type GitError struct{ msg string }

func (e *GitError) Error() string { return e.msg }

// gitOut returns git's raw stdout. Replacement objects are off: a local
// refs/replace entry must not change what a commit ID means.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--no-replace-objects", "-C", dir}, args...)...)
	cmd.Env = chain.GitEnv() // a hook's GIT_DIR must not override -C
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", &GitError{fmt.Sprintf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))}
	}
	return stdout.String(), nil
}

// globRegexp compiles a path glob: * and ? stay within one path segment,
// ** crosses segments, "**/" also matches no directory at all, and a trailing
// "/**" also matches the directory itself, which is how git names a submodule
// that changed.
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
	suffix := "$"
	if g, ok := strings.CutSuffix(glob, "/**"); ok {
		glob, suffix = g, "(?:/.*)?$"
	}
	var b strings.Builder
	b.WriteString("(?s)^") // . matches a newline too: git allows one in a path
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
	b.WriteString(suffix)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("applies_when %q: %w", glob, err)
	}
	return re, nil
}

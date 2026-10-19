// Package gates holds the reference reviews shipped with tardis-gate. An
// enrolled repository uses them by name unless it supplies its own directory.
package gates

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
)

// FS contains <name>/gate.yml and <name>/prompt.md for each reference gate.
//
//go:embed */gate.yml */prompt.md
var FS embed.FS

// Digest identifies this binary's reference gates: two tardis versions with
// the same digest hold the same reference policy.
var Digest = func() string {
	h := sha256.New()
	fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error { // lexical order
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(FS, p)
			h.Write([]byte(p + "\x00"))
			h.Write(b)
			h.Write([]byte{0})
		}
		return err
	})
	return hex.EncodeToString(h.Sum(nil))
}()

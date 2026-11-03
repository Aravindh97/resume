package site

import (
	"io/fs"
	"os"
	"path/filepath"
)

// WriteStatic writes the site for static hosting (GitHub Pages) into dir:
// the React page as index.html plus every embedded asset. In the browser
// the API is answered by the WebAssembly module instead of a server. It
// returns the number of files written.
func WriteStatic(dir string) (int, error) {
	page, err := assets.ReadFile("static/ui/index.html")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), page, 0o644); err != nil {
		return 0, err
	}
	n := 1
	// static/ (the WebAssembly module, resume.pdf) keeps its path; the React
	// build's assets/ and shots/ go to the root, where index.html expects them.
	copyTree := func(root, to string) error {
		return fs.WalkDir(assets, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			dst := filepath.Join(dir, to, rel)
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			b, err := assets.ReadFile(p)
			if err != nil {
				return err
			}
			n++
			return os.WriteFile(dst, b, 0o644)
		})
	}
	for _, t := range [][2]string{{"static", "static"}, {"static/ui/assets", "assets"}, {"static/ui/shots", "shots"}} {
		if err := copyTree(t[0], t[1]); err != nil {
			return n, err
		}
	}
	// Tell GitHub Pages not to run Jekyll, which would hide _-prefixed paths.
	n++
	return n, os.WriteFile(filepath.Join(dir, ".nojekyll"), nil, 0o644)
}

package site

import (
	"html/template"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Aravindh97/resume/internal/profile"
)

// WriteStatic renders the page for static hosting (GitHub Pages) into dir
// and copies the static assets next to it. It returns the number of files.
func WriteStatic(dir string) (int, error) {
	t, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(filepath.Join(dir, "index.html"))
	if err != nil {
		return 0, err
	}
	err = t.ExecuteTemplate(f, "index.html", map[string]any{"Node": "", "Region": "",
		"Evals": profile.Evals, "Profile": profile.Profile, "Static": true})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	n := 1
	err = fs.WalkDir(assets, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := assets.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		return os.WriteFile(filepath.Join(dir, p), b, 0o644)
	})
	if err != nil {
		return n, err
	}
	// Tell GitHub Pages not to run Jekyll, which would hide _-prefixed paths.
	n++
	return n, os.WriteFile(filepath.Join(dir, ".nojekyll"), nil, 0o644)
}

#!/usr/bin/env bash
# Copies the source of the three project repos into internal/ask/corpus so
# the site can answer questions with file and line citations. Run from the
# repo root with the projects checked out next to it (../granth, ...).
set -euo pipefail
out=internal/ask/_corpus  # leading underscore: the go tool ignores it, embed uses all:
rm -rf "$out" && mkdir -p "$out"
for r in granth smriti sutra; do
  src="../$r"
  [ -d "$src" ] || { echo "missing $src" >&2; exit 1; }
  (cd "$src" && git ls-files -- '*.go' '*.md' 'docs/*.txt' 'config/*.yaml' 'docker-compose.yml') | while read -r f; do
    mkdir -p "$out/$r/$(dirname "$f")"
    cp "$src/$f" "$out/$r/$f"
  done
  (cd "$src" && git rev-parse --short HEAD) > "$out/$r/.commit"
done
echo "corpus: $(find "$out" -type f | wc -l) files"

.PHONY: wasm ui corpus test mesh build
wasm:            ## build the browser module (race simulator + in-browser API)
	GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o internal/site/static/app.wasm ./cmd/app-wasm
	install -m 644 "$$(go env GOROOT)/lib/wasm/wasm_exec.js" internal/site/static/wasm_exec.js
ui:              ## build the React frontend into internal/site/static/ui
	cd web && ([ -d node_modules ] || npm ci --no-audit --no-fund) && npx tsc --noEmit && npx vite build --logLevel warn
corpus:          ## snapshot ../granth ../smriti ../sutra for "Ask my work"
	./scripts/snapshot-corpus.sh
test:
	go vet ./... && go test -race ./... && go test -count=50 -run Unsafe ./internal/racesim
build: wasm ui
	go build -o bin/site ./cmd/site && go build -o bin/career ./cmd/career
mesh: wasm ui       ## 4 nodes on 127.0.0.1:8081-8084
	./scripts/mesh-local.sh
static: wasm ui     ## GitHub Pages build into dist/
	go run ./cmd/build-static -out dist

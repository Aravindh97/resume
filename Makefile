.PHONY: wasm corpus test mesh build
wasm:            ## build the race simulator for the browser
	GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o internal/site/static/racesim.wasm ./cmd/racesim-wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" internal/site/static/
corpus:          ## snapshot ../granth ../smriti ../sutra for "Ask my work"
	./scripts/snapshot-corpus.sh
test:
	go vet ./... && go test -race ./... && go test -count=50 -run Unsafe ./internal/racesim
build: wasm
	go build -o bin/site ./cmd/site && go build -o bin/career ./cmd/career
mesh: wasm       ## 4 nodes on 127.0.0.1:8081-8084
	./scripts/mesh-local.sh

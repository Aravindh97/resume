FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o internal/site/static/app.wasm ./cmd/app-wasm \
 && install -m 644 "$(go env GOROOT)/lib/wasm/wasm_exec.js" internal/site/static/wasm_exec.js \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/site ./cmd/site

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/site /site
ENTRYPOINT ["/site"]

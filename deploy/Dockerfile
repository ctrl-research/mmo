# Two stages: build the client and the WebAssembly simulation, then the server
# binary with both embedded. The result is a single static artefact with no
# runtime dependency beyond Postgres and Redis, which is the point of the
# self-hosting goal.

FROM golang:1.26-alpine AS wasm
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p /out \
 && GOOS=js GOARCH=wasm go build -o /out/sim.wasm ./cmd/simwasm \
 && cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" /out/wasm_exec.js


FROM node:24-alpine AS client
WORKDIR /src

# Dependencies first, so a source-only change does not reinstall them.
COPY client/package.json client/package-lock.json* ./client/
RUN cd client && npm ci

COPY client ./client
COPY --from=wasm /out/sim.wasm /out/wasm_exec.js ./client/public/
RUN cd client && npm run build


FROM golang:1.26-alpine AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off keeps the binary static so it runs on a distroless base.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mmo ./cmd/mmo


FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /out/mmo /usr/local/bin/mmo
COPY --from=client /src/client/dist /srv/client
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/mmo"]

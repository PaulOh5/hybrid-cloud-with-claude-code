#!/usr/bin/env bash
# Regenerate Go protobuf stubs from proto/*.proto.
# Requires: protoc, protoc-gen-go, protoc-gen-go-grpc on PATH (or $GOPATH/bin).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="$ROOT/shared/proto"

mkdir -p "$OUT_DIR"

PROTOS=("$ROOT"/proto/*.proto)
if [[ ! -e "${PROTOS[0]}" ]]; then
  echo "no proto files found in $ROOT/proto — skipping"
  exit 0
fi

export PATH="$(go env GOPATH)/bin:$PATH"

protoc \
  --proto_path="$ROOT/proto" \
  --go_out="$OUT_DIR" --go_opt=paths=source_relative \
  --go-grpc_out="$OUT_DIR" --go-grpc_opt=paths=source_relative \
  "${PROTOS[@]}"

echo "generated $(ls "$OUT_DIR"/*.go | wc -l) Go files in $OUT_DIR"

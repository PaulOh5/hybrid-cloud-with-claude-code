#!/usr/bin/env bash
# Regenerate Go protobuf stubs from proto/*.proto.
# Output goes to shared/proto/<package>/<version> (per go_package option).
# Requires protoc + protoc-gen-go + protoc-gen-go-grpc on PATH.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export PATH="$(go env GOPATH)/bin:$PATH"

PROTOS=("$ROOT"/proto/*.proto)
if [[ ! -e "${PROTOS[0]}" ]]; then
  echo "no proto files found in $ROOT/proto — skipping"
  exit 0
fi

mkdir -p "$ROOT/shared"

protoc \
  --proto_path="$ROOT/proto" \
  --go_out="$ROOT/shared" \
  --go_opt=module=hybridcloud/shared \
  --go-grpc_out="$ROOT/shared" \
  --go-grpc_opt=module=hybridcloud/shared \
  "${PROTOS[@]}"

echo "generated Go stubs in $ROOT/shared/proto/"

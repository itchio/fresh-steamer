#!/bin/sh
# Regenerates ../pb from the .proto files in this directory.
#
# The .proto files are verbatim mirrors of SteamDatabase/Protobufs and must not
# be edited. protoc compiles them all into a descriptor set, then proto/prune
# keeps only the types named in allowlist.txt (plus their dependencies) and
# runs protoc-gen-go on the result. See README.md for details.
set -e
cd "$(dirname "$0")"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

protoc --include_imports --descriptor_set_out="$tmp/all.pb" *.proto
rm -f ../pb/*.pb.go
go run ./prune \
  -desc "$tmp/all.pb" \
  -allowlist allowlist.txt \
  -out ../pb \
  -gopkg github.com/itchio/fresh-steamer/pb
gofmt -l ../pb

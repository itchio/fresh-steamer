#!/bin/sh
# Regenerates ../pb from the .proto files in this directory.
set -e
cd "$(dirname "$0")"
opts=""
for f in *.proto; do
  opts="$opts --go_opt=M$f=github.com/itchio/fresh-steamer/pb"
done
protoc --go_out=../pb --go_opt=paths=source_relative $opts *.proto

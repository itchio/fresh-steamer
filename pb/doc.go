// Package pb holds Go types generated from Valve's protobuf definitions,
// mirrored from https://github.com/SteamDatabase/Protobufs. Only the types
// listed in ../proto/allowlist.txt (and what they reference) are generated;
// see the "Protobuf generation" section of the README. Regenerate with
// `go generate ./pb`.
package pb

//go:generate ../proto/generate.sh

# fresh-steamer

A pure Go client for authenticating and downloading depots from Steam. It logs
in with a Steam account, reads an app's product info, and fetches manifests and
chunks from the content servers.

Built for [`butler steam-sync`](https://itch.io/docs/butler/steam-sync.html),
but a low level CLI is provided here for testing

```
go run ./cmd/fresh-steamer login
go run ./cmd/fresh-steamer partner-key
go run ./cmd/fresh-steamer apps
go run ./cmd/fresh-steamer partner-apps
go run ./cmd/fresh-steamer builds 440
go run ./cmd/fresh-steamer info 440
go run ./cmd/fresh-steamer download -app 440 -depot 441 -dir ./out
```

Two credentials are involved:

* A regular Steam account is used to access the content content download
  servers, since a consumer license is required to initiate a download. Log in
  can be done via provided username and password directly, or using QR code
  login to avoid entering password
* A publisher Web API key is used to list the apps developed by an account. The
  key can be obtained from the Steamworks Partner Portal

Together both can be used to gate downloads only to accounts that prove they
are developers of the app.

Packages:

- `auth`: credential login through IAuthenticationService, Steam Guard
- `cm`: websocket connection manager client, logon, jobs, unified calls
- `partner`: publisher Web API, app list and build history
- `appinfo`: PICS product info parsed into depots and branches
- `session`: depot keys, manifest request codes, branch passwords, CDN setup
- `cdn`: manifest and chunk fetch, decrypt and decompress
- `depot`: write a manifest to disk with parallel chunk fetch and skip of unchanged files

[SteamKit](https://github.com/steamre/steamkit) and
[DepotDownloader](https://github.com/steamre/depotdownloader) were used as
references for undocumented protocol implementation.

## Protobuf generation

The `pb` package is generated from Valve's protobuf definitions. The `.proto`
files in `proto/` are verbatim copies of the `steam/` directory in
[SteamDatabase/Protobufs](https://github.com/SteamDatabase/Protobufs) and
should never be edited by hand. To refresh them, copy the upstream files over
the existing ones and regenerate.

Those files define several hundred messages, and protobuf's generated code
registers every message at init time, so anything compiled into `pb` ends up
in every binary that links this module. To keep that small, generation is
driven by an allowlist:

1. `protoc` compiles every `.proto` into a single descriptor set.
2. `proto/prune` reads `proto/allowlist.txt`, keeps the listed messages and
   enums plus everything they reference (field types, nested types, enclosing
   messages), drops services, extensions and Steam's custom options, and
   rewrites each file's import list to match.
3. The pruned descriptors are fed to `protoc-gen-go`, which writes `pb/*.pb.go`.

To use a message or enum that isn't generated yet, add its fully qualified
name to `proto/allowlist.txt` and run:

```
go generate ./pb
```

This needs `protoc` and `protoc-gen-go` on `PATH`. Names in the allowlist are
proto names, not Go names: Steam's protos declare no package, so a top-level
message is just `CMsgClientLogon`, and a nested one is written with a dot, as
in `CMsgClientPICSProductInfoRequest.AppInfo`. The generator fails if a name
doesn't exist in the descriptor set. Nested types are pulled in when their
enclosing message needs them, so they rarely need listing explicitly.

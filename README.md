# fresh-steamer

A pure Go client for downloading depots from Steam. It logs in with a Steam
account, reads an app's product info, and fetches manifests and chunks from
the content servers. No steamcmd, no DepotDownloader.

Built for `butler steam-sync`, usable on its own.

Two credentials are involved. A Steam account is needed because the content
servers only hand out depots to accounts holding a license. A publisher Web
API key from the partner site is needed to prove the account actually
develops the app; every command that touches an app checks the app against
the key's app list first. This is a tool for moving your own builds, not
for copying a library.

```
go run ./cmd/fresh-steamer login
go run ./cmd/fresh-steamer partner-key
go run ./cmd/fresh-steamer apps
go run ./cmd/fresh-steamer builds 440
go run ./cmd/fresh-steamer info 440
go run ./cmd/fresh-steamer download -app 440 -depot 441 -dir ./out
```

Packages:

- `auth`: credential login through IAuthenticationService, Steam Guard included
- `cm`: websocket connection manager client, logon, jobs, unified calls
- `partner`: publisher Web API, app list and build history
- `appinfo`: PICS product info parsed into depots and branches
- `session`: depot keys, manifest request codes, branch passwords, CDN setup
- `cdn`: manifest and chunk fetch, decrypt and decompress
- `depot`: write a manifest to disk with parallel chunk fetch and skip of unchanged files
- `vdf`, `steamcrypto`, `webapi`, `pb`: supporting pieces

The protocol is undocumented. SteamKit2 and DepotDownloader are the
references for anything here.

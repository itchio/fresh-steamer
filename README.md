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

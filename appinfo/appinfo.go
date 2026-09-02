// Package appinfo fetches an app's product info through PICS and exposes
// the parts that matter for downloading: depots and branches.
package appinfo

import (
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/itchio/fresh-steamer/cm"
	"github.com/itchio/fresh-steamer/pb"
	"github.com/itchio/fresh-steamer/vdf"
	"google.golang.org/protobuf/proto"
)

const (
	emsgPICSProductInfoRequest = 8903
	emsgPICSAccessTokenRequest = 8905
	emsgCheckAppBetaPassword   = 5450
)

type App struct {
	ID           uint32
	Name         string
	Type         string // "Game", "Demo", "Beta", "Tool", "DLC"
	ChangeNumber uint32
	// OSList and OSArch come from the app's common section. Depots that set
	// no oslist of their own inherit these.
	OSList   []string
	OSArch   string
	Depots   []*Depot
	Branches []*Branch
	// Raw is the full KeyValues tree for anything not modelled here.
	Raw *vdf.Node
}

type Depot struct {
	ID       uint32
	Name     string
	OSList   []string // "windows", "macos", "linux"; empty means all
	OSArch   string   // "32", "64" or ""
	Language string   // "" for language-neutral
	// DLCAppID is set for depots that belong to a DLC.
	DLCAppID uint32
	// SharedFromApp is set when the content lives in another app's depot.
	SharedFromApp uint32
	// Manifests maps branch name to manifest gid for unencrypted branches.
	Manifests map[string]*Manifest
	// EncryptedManifests holds branches that need a password; values are
	// hex-encoded ciphertext of the gid.
	EncryptedManifests map[string]string
}

type Manifest struct {
	GID      uint64
	Size     uint64
	Download uint64
}

type Branch struct {
	Name             string
	BuildID          uint32
	Description      string
	PasswordRequired bool
	TimeUpdated      uint64
}

func (a *App) Branch(name string) *Branch {
	for _, b := range a.Branches {
		if strings.EqualFold(b.Name, name) {
			return b
		}
	}
	return nil
}

func (a *App) Depot(id uint32) *Depot {
	for _, d := range a.Depots {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// EffectiveOSList is the depot's oslist, or the app's when the depot sets
// none. An empty result means the depot is not platform specific at all.
func (a *App) EffectiveOSList(d *Depot) []string {
	if len(d.OSList) > 0 {
		return d.OSList
	}
	return a.OSList
}

// MatchesOS reports whether the depot applies to os ("windows", "macos",
// "linux"). Depots without an oslist apply everywhere.
func (d *Depot) MatchesOS(os string) bool {
	if len(d.OSList) == 0 {
		return true
	}
	for _, o := range d.OSList {
		if strings.EqualFold(o, os) {
			return true
		}
	}
	return false
}

// Fetch retrieves product info for one app the logged-in account can see.
func Fetch(ctx context.Context, c *cm.Client, appID uint32) (*App, error) {
	tokPkt, err := c.Request(ctx, emsgPICSAccessTokenRequest, &pb.CMsgClientPICSAccessTokenRequest{Appids: []uint32{appID}})
	if err != nil {
		return nil, fmt.Errorf("requesting PICS token: %w", err)
	}
	var tokRes pb.CMsgClientPICSAccessTokenResponse
	if err := tokPkt.Unmarshal(&tokRes); err != nil {
		return nil, err
	}
	var token uint64
	for _, t := range tokRes.GetAppAccessTokens() {
		if t.GetAppid() == appID {
			token = t.GetAccessToken()
		}
	}
	for _, denied := range tokRes.GetAppDeniedTokens() {
		if denied == appID {
			return nil, fmt.Errorf("app %d: access token denied, does this account own the app?", appID)
		}
	}

	var info *pb.CMsgClientPICSProductInfoResponse_AppInfo
	var httpHost string
	var httpMinSize uint32
	err = c.RequestMulti(ctx, emsgPICSProductInfoRequest, &pb.CMsgClientPICSProductInfoRequest{
		Apps: []*pb.CMsgClientPICSProductInfoRequest_AppInfo{{
			Appid:       proto.Uint32(appID),
			AccessToken: proto.Uint64(token),
		}},
	}, func(p *cm.Packet) (bool, error) {
		var res pb.CMsgClientPICSProductInfoResponse
		if err := p.Unmarshal(&res); err != nil {
			return true, err
		}
		for _, a := range res.GetApps() {
			if a.GetAppid() == appID {
				info = a
				httpHost = res.GetHttpHost()
				httpMinSize = res.GetHttpMinSize()
			}
		}
		for _, u := range res.GetUnknownAppids() {
			if u == appID {
				return true, fmt.Errorf("app %d: unknown to steam", appID)
			}
		}
		return !res.GetResponsePending(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("requesting product info: %w", err)
	}
	if info == nil {
		return nil, fmt.Errorf("app %d: no product info returned", appID)
	}
	if info.GetMissingToken() {
		return nil, fmt.Errorf("app %d: product info needs a token this account does not have", appID)
	}

	buf := info.GetBuffer()
	if len(buf) == 0 && httpHost != "" && info.GetSize() >= httpMinSize {
		buf, err = fetchOverHTTP(ctx, httpHost, appID, info.GetSha())
		if err != nil {
			return nil, err
		}
	}
	if len(buf) == 0 {
		return nil, fmt.Errorf("app %d: empty product info", appID)
	}

	root, err := vdf.Parse(buf)
	if err != nil {
		return nil, fmt.Errorf("parsing product info for app %d: %w", appID, err)
	}
	app := parse(root.Get("appinfo"))
	if app == nil {
		return nil, fmt.Errorf("app %d: product info has no appinfo section", appID)
	}
	app.ChangeNumber = info.GetChangeNumber()
	return app, nil
}

// Large app infos are served over HTTP instead of inline. The URL layout
// follows SteamKit's handling of http_host.
func fetchOverHTTP(ctx context.Context, host string, appID uint32, sha []byte) ([]byte, error) {
	url := fmt.Sprintf("https://%s/appinfo/%d/sha/%s.txt.gz", host, appID, hex.EncodeToString(sha))
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching product info over http: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("fetching product info over http: HTTP %d", res.StatusCode)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	if got := sha1.Sum(data); !hexEqual(got[:], sha) {
		return nil, errors.New("product info over http: sha mismatch")
	}
	return data, nil
}

func hexEqual(a, b []byte) bool { return hex.EncodeToString(a) == hex.EncodeToString(b) }

func parse(n *vdf.Node) *App {
	if n == nil {
		return nil
	}
	app := &App{
		ID:     n.Get("appid").Uint32(),
		Name:   n.Path("common", "name").String(),
		Type:   n.Path("common", "type").String(),
		OSList: splitList(n.Path("common", "oslist").String()),
		OSArch: n.Path("common", "osarch").String(),
		Raw:    n,
	}
	depots := n.Get("depots")
	if depots == nil {
		return app
	}
	for _, d := range depots.Children {
		if strings.EqualFold(d.Key, "branches") {
			for _, b := range d.Children {
				app.Branches = append(app.Branches, &Branch{
					Name:             b.Key,
					BuildID:          b.Get("buildid").Uint32(),
					Description:      b.Get("description").String(),
					PasswordRequired: b.Get("pwdrequired").Bool(),
					TimeUpdated:      b.Get("timeupdated").Uint64(),
				})
			}
			continue
		}
		if d.IsLeaf() {
			// Keys like "baselanguages" or "overridescddb" sit next to depots.
			continue
		}
		id := parseUint32(d.Key)
		if id == 0 {
			continue
		}
		dep := &Depot{
			ID:                 id,
			Name:               d.Get("name").String(),
			Language:           d.Path("config", "language").String(),
			OSArch:             d.Path("config", "osarch").String(),
			DLCAppID:           d.Get("dlcappid").Uint32(),
			SharedFromApp:      d.Get("depotfromapp").Uint32(),
			Manifests:          map[string]*Manifest{},
			EncryptedManifests: map[string]string{},
		}
		dep.OSList = splitList(d.Path("config", "oslist").String())
		if m := d.Get("manifests"); m != nil {
			for _, br := range m.Children {
				if br.IsLeaf() {
					// Very old app infos stored the gid directly.
					dep.Manifests[br.Key] = &Manifest{GID: br.Uint64()}
					continue
				}
				dep.Manifests[br.Key] = &Manifest{
					GID:      br.Get("gid").Uint64(),
					Size:     br.Get("size").Uint64(),
					Download: br.Get("download").Uint64(),
				}
			}
		}
		if m := d.Get("encryptedmanifests"); m != nil {
			for _, br := range m.Children {
				if v := br.Get("gid").String(); v != "" {
					dep.EncryptedManifests[br.Key] = v
				} else if v := br.Get("encrypted_gid_2").String(); v != "" {
					dep.EncryptedManifests[br.Key] = v
				}
			}
		}
		app.Depots = append(app.Depots, dep)
	}
	sort.Slice(app.Depots, func(i, j int) bool { return app.Depots[i].ID < app.Depots[j].ID })
	return app
}

func splitList(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func parseUint32(s string) uint32 {
	var v uint32
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + uint32(c-'0')
	}
	return v
}

// BranchPasswords asks Steam which private branches the given password
// unlocks. Steam only returns the branches the password matches, so an
// empty result means a wrong password.
func BranchPasswords(ctx context.Context, c *cm.Client, appID uint32, password string) (map[string]string, error) {
	pkt, err := c.Request(ctx, emsgCheckAppBetaPassword, &pb.CMsgClientCheckAppBetaPassword{
		AppId:        proto.Uint32(appID),
		Betapassword: proto.String(password),
	})
	if err != nil {
		return nil, err
	}
	var res pb.CMsgClientCheckAppBetaPasswordResponse
	if err := pkt.Unmarshal(&res); err != nil {
		return nil, err
	}
	if res.GetEresult() != 1 {
		return nil, &cm.EResultError{EResult: res.GetEresult(), Context: "checking branch password"}
	}
	out := map[string]string{}
	for _, b := range res.GetBetapasswords() {
		out[b.GetBetaname()] = b.GetBetapassword()
	}
	return out, nil
}

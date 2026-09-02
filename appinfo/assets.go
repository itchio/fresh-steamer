package appinfo

import (
	"fmt"

	"github.com/itchio/fresh-steamer/vdf"
)

// Assets are store artwork URLs taken from the app's common section. They
// exist for unreleased and private apps too, unlike the store API.
type Assets struct {
	Icon           string `json:"icon,omitempty"`
	ClientIcon     string `json:"client_icon,omitempty"`
	Header         string `json:"header,omitempty"`
	SmallCapsule   string `json:"small_capsule,omitempty"`
	LibraryCapsule string `json:"library_capsule,omitempty"`
	LibraryHero    string `json:"library_hero,omitempty"`
	LibraryLogo    string `json:"library_logo,omitempty"`
	LibraryHeader  string `json:"library_header,omitempty"`
}

const (
	storeAssetBase     = "https://shared.akamai.steamstatic.com/store_item_assets/steam/apps"
	communityAssetBase = "https://shared.akamai.steamstatic.com/community_assets/images/apps"
)

// Assets resolves the app's artwork to URLs, preferring the english
// variant of localized assets and falling back to whichever exists.
func (a *App) Assets() Assets {
	common := a.Raw.Get("common")
	store := func(n *vdf.Node) string {
		p := localized(n)
		if p == "" {
			return ""
		}
		return fmt.Sprintf("%s/%d/%s", storeAssetBase, a.ID, p)
	}
	icon := func(hash string) string {
		if hash == "" {
			return ""
		}
		return fmt.Sprintf("%s/%d/%s.jpg", communityAssetBase, a.ID, hash)
	}
	full := common.Get("library_assets_full")
	return Assets{
		Icon:           icon(common.Get("icon").String()),
		ClientIcon:     icon(common.Get("clienticon").String()),
		Header:         store(common.Get("header_image")),
		SmallCapsule:   store(common.Get("small_capsule")),
		LibraryCapsule: store(full.Path("library_capsule", "image")),
		LibraryHero:    store(full.Path("library_hero", "image")),
		LibraryLogo:    store(full.Path("library_logo", "image")),
		LibraryHeader:  store(full.Path("library_header", "image")),
	}
}

func localized(n *vdf.Node) string {
	if n == nil {
		return ""
	}
	if v := n.Get("english").String(); v != "" {
		return v
	}
	for _, c := range n.Children {
		if c.IsLeaf() && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// Parent is the app a playtest or demo belongs to, or zero.
func (a *App) Parent() uint32 {
	return a.Raw.Path("common", "parent").Uint32()
}

// Association is a developer, publisher or franchise credit.
type Association struct {
	Type string `json:"type"` // "developer", "publisher", "franchise"
	Name string `json:"name"`
}

func (a *App) Associations() []Association {
	var out []Association
	assoc := a.Raw.Path("common", "associations")
	if assoc == nil {
		return nil
	}
	for _, c := range assoc.Children {
		if t, n := c.Get("type").String(), c.Get("name").String(); t != "" && n != "" {
			out = append(out, Association{Type: t, Name: n})
		}
	}
	return out
}

// ReleaseDate is the unix time Steam considers the app released, or zero.
func (a *App) ReleaseDate() int64 {
	return int64(a.Raw.Path("common", "steam_release_date").Uint64())
}

func (a *App) ReleaseState() string {
	return a.Raw.Path("common", "releasestate").String()
}

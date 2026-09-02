// Package store reads public store page data for an app: descriptions,
// screenshots, trailers and the rest of what a store listing shows. It
// only works for apps with a public store page.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const DefaultBaseURL = "https://store.steampowered.com"

// ErrNoStorePage means the app has no public listing, which is normal for
// playtests, unreleased apps and tools.
var ErrNoStorePage = errors.New("app has no public store page")

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

func NewClient() *Client {
	return &Client{HTTP: http.DefaultClient, BaseURL: DefaultBaseURL}
}

type Page struct {
	AppID               uint32       `json:"app_id"`
	Type                string       `json:"type"`
	Name                string       `json:"name"`
	ShortDescription    string       `json:"short_description"`
	DetailedDescription string       `json:"detailed_description"` // HTML
	AboutTheGame        string       `json:"about_the_game"`       // HTML
	HeaderImage         string       `json:"header_image"`
	CapsuleImage        string       `json:"capsule_image"`
	Background          string       `json:"background"`
	Website             string       `json:"website"`
	Developers          []string     `json:"developers"`
	Publishers          []string     `json:"publishers"`
	ReleaseDate         string       `json:"release_date"`
	ComingSoon          bool         `json:"coming_soon"`
	IsFree              bool         `json:"is_free"`
	Price               *Price       `json:"price,omitempty"`
	Platforms           Platforms    `json:"platforms"`
	Genres              []string     `json:"genres"`
	Categories          []string     `json:"categories"`
	SupportedLanguages  string       `json:"supported_languages"` // HTML
	Screenshots         []Screenshot `json:"screenshots"`
	Movies              []Movie      `json:"movies"`
	Requirements        Requirements `json:"requirements"`
	LegalNotice         string       `json:"legal_notice,omitempty"`
	RequiredAge         int          `json:"required_age"`
	ControllerSupport   string       `json:"controller_support,omitempty"`
	DLC                 []uint32     `json:"dlc,omitempty"`
	FullGameAppID       uint32       `json:"full_game_app_id,omitempty"`
}

type Price struct {
	Currency string `json:"currency"`
	Initial  int    `json:"initial"` // cents
	Final    int    `json:"final"`
	Discount int    `json:"discount_percent"`
}

type Platforms struct {
	Windows bool `json:"windows"`
	Mac     bool `json:"mac"`
	Linux   bool `json:"linux"`
}

type Screenshot struct {
	Thumbnail string `json:"thumbnail"`
	Full      string `json:"full"`
}

// Movie is a trailer. Newer uploads only offer DASH and HLS manifests;
// older ones still have direct webm and mp4 files.
type Movie struct {
	Name      string `json:"name"`
	Thumbnail string `json:"thumbnail"`
	WebM      string `json:"webm,omitempty"`
	MP4       string `json:"mp4,omitempty"`
	DashAV1   string `json:"dash_av1,omitempty"`
	DashH264  string `json:"dash_h264,omitempty"`
	HLS       string `json:"hls,omitempty"`
	Highlight bool   `json:"highlight"`
}

type Requirements struct {
	Windows RequirementSet `json:"windows"`
	Mac     RequirementSet `json:"mac"`
	Linux   RequirementSet `json:"linux"`
}

type RequirementSet struct {
	Minimum     string `json:"minimum,omitempty"`     // HTML
	Recommended string `json:"recommended,omitempty"` // HTML
}

// Fetch reads the store page for an app. language is a Steam API language
// name like "english"; empty means english.
func (c *Client) Fetch(ctx context.Context, appID uint32, language string) (*Page, error) {
	if language == "" {
		language = "english"
	}
	q := url.Values{"appids": {strconv.FormatUint(uint64(appID), 10)}, "l": {language}}
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/appdetails?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fresh-steamer/0.1")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching store page: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == 429 {
		return nil, errors.New("fetching store page: rate limited, try again in a few minutes")
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("fetching store page: HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var envelope map[string]struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding store page: %w", err)
	}
	entry, ok := envelope[strconv.FormatUint(uint64(appID), 10)]
	if !ok || !entry.Success {
		return nil, ErrNoStorePage
	}
	var raw rawDetails
	if err := json.Unmarshal(entry.Data, &raw); err != nil {
		return nil, fmt.Errorf("decoding store page: %w", err)
	}
	return raw.page(appID), nil
}

type named struct {
	Description string `json:"description"`
}

// The store returns requirements as an object normally but as an empty
// array when a platform has none, so they need a tolerant decoder.
type requirements RequirementSet

func (r *requirements) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		return nil
	}
	var v struct {
		Minimum     string `json:"minimum"`
		Recommended string `json:"recommended"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	r.Minimum, r.Recommended = dropPlaceholder(v.Minimum), dropPlaceholder(v.Recommended)
	return nil
}

var tagPattern = regexp.MustCompile(`<[^>]*>`)

// Unsupported platforms still get a heading and an empty list, which is
// noise to anyone building a page from this.
func dropPlaceholder(html string) string {
	text := strings.TrimSpace(tagPattern.ReplaceAllString(html, ""))
	if text == "" || text == "Minimum:" || text == "Recommended:" {
		return ""
	}
	return html
}

type rawDetails struct {
	Type                string          `json:"type"`
	Name                string          `json:"name"`
	RequiredAge         json.RawMessage `json:"required_age"`
	IsFree              bool            `json:"is_free"`
	ControllerSupport   string          `json:"controller_support"`
	DLC                 []uint32        `json:"dlc"`
	DetailedDescription string          `json:"detailed_description"`
	AboutTheGame        string          `json:"about_the_game"`
	ShortDescription    string          `json:"short_description"`
	Fullgame            *struct {
		AppID json.RawMessage `json:"appid"`
	} `json:"fullgame"`
	SupportedLanguages string       `json:"supported_languages"`
	HeaderImage        string       `json:"header_image"`
	CapsuleImage       string       `json:"capsule_image"`
	Website            string       `json:"website"`
	PCRequirements     requirements `json:"pc_requirements"`
	MacRequirements    requirements `json:"mac_requirements"`
	LinuxRequirements  requirements `json:"linux_requirements"`
	LegalNotice        string       `json:"legal_notice"`
	Developers         []string     `json:"developers"`
	Publishers         []string     `json:"publishers"`
	PriceOverview      *struct {
		Currency        string `json:"currency"`
		Initial         int    `json:"initial"`
		Final           int    `json:"final"`
		DiscountPercent int    `json:"discount_percent"`
	} `json:"price_overview"`
	Platforms   Platforms `json:"platforms"`
	Categories  []named   `json:"categories"`
	Genres      []named   `json:"genres"`
	Screenshots []struct {
		PathThumbnail string `json:"path_thumbnail"`
		PathFull      string `json:"path_full"`
	} `json:"screenshots"`
	Movies []struct {
		Name      string            `json:"name"`
		Thumbnail string            `json:"thumbnail"`
		WebM      map[string]string `json:"webm"`
		MP4       map[string]string `json:"mp4"`
		DashAV1   string            `json:"dash_av1"`
		DashH264  string            `json:"dash_h264"`
		HLS       string            `json:"hls_h264"`
		Highlight bool              `json:"highlight"`
	} `json:"movies"`
	ReleaseDate struct {
		ComingSoon bool   `json:"coming_soon"`
		Date       string `json:"date"`
	} `json:"release_date"`
	Background string `json:"background"`
}

func (r *rawDetails) page(appID uint32) *Page {
	p := &Page{
		AppID:               appID,
		Type:                r.Type,
		Name:                r.Name,
		ShortDescription:    r.ShortDescription,
		DetailedDescription: r.DetailedDescription,
		AboutTheGame:        r.AboutTheGame,
		HeaderImage:         r.HeaderImage,
		CapsuleImage:        r.CapsuleImage,
		Background:          r.Background,
		Website:             r.Website,
		Developers:          r.Developers,
		Publishers:          r.Publishers,
		ReleaseDate:         r.ReleaseDate.Date,
		ComingSoon:          r.ReleaseDate.ComingSoon,
		IsFree:              r.IsFree,
		Platforms:           r.Platforms,
		SupportedLanguages:  r.SupportedLanguages,
		LegalNotice:         r.LegalNotice,
		ControllerSupport:   r.ControllerSupport,
		DLC:                 r.DLC,
		RequiredAge:         looseInt(r.RequiredAge),
		Requirements: Requirements{
			Windows: RequirementSet(r.PCRequirements),
			Mac:     RequirementSet(r.MacRequirements),
			Linux:   RequirementSet(r.LinuxRequirements),
		},
	}
	if r.Fullgame != nil {
		p.FullGameAppID = uint32(looseInt(r.Fullgame.AppID))
	}
	if r.PriceOverview != nil {
		p.Price = &Price{Currency: r.PriceOverview.Currency, Initial: r.PriceOverview.Initial, Final: r.PriceOverview.Final, Discount: r.PriceOverview.DiscountPercent}
	}
	for _, g := range r.Genres {
		p.Genres = append(p.Genres, g.Description)
	}
	for _, c := range r.Categories {
		p.Categories = append(p.Categories, c.Description)
	}
	for _, s := range r.Screenshots {
		p.Screenshots = append(p.Screenshots, Screenshot{Thumbnail: s.PathThumbnail, Full: s.PathFull})
	}
	for _, m := range r.Movies {
		p.Movies = append(p.Movies, Movie{Name: m.Name, Thumbnail: m.Thumbnail, WebM: best(m.WebM), MP4: best(m.MP4), DashAV1: m.DashAV1, DashH264: m.DashH264, HLS: m.HLS, Highlight: m.Highlight})
	}
	return p
}

// Movie sources are keyed by quality ("480", "max"); take the best.
func best(m map[string]string) string {
	if v, ok := m["max"]; ok {
		return v
	}
	for _, v := range m {
		return v
	}
	return ""
}

// Some fields come back as a number or a numeric string depending on the
// app, so accept both.
func looseInt(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, _ = strconv.Atoi(s)
	}
	return n
}

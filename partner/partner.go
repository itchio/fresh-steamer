// Package partner calls the Steamworks publisher Web API with a publisher
// key. It answers the question the depot protocol cannot: which apps does
// this developer control?
package partner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

const DefaultBaseURL = "https://partner.steam-api.com"

type Client struct {
	Key     string
	HTTP    *http.Client
	BaseURL string
}

func NewClient(key string) *Client {
	return &Client{Key: key, HTTP: http.DefaultClient, BaseURL: DefaultBaseURL}
}

// StatusError is a non-200 answer from the partner API. Unauthorized
// reports whether it means the key itself was rejected.
type StatusError struct {
	Method string
	Status int
}

func (e *StatusError) Error() string {
	if e.Unauthorized() {
		return fmt.Sprintf("partner api %s: HTTP %d, is the publisher key valid?", e.Method, e.Status)
	}
	return fmt.Sprintf("partner api %s: HTTP %d", e.Method, e.Status)
}

func (e *StatusError) Unauthorized() bool { return e.Status == 401 || e.Status == 403 }

type App struct {
	ID   uint32
	Name string
	Type string // game, application, tool, demo, dlc, music
}

// Apps lists every app the publisher key has access to.
func (c *Client) Apps(ctx context.Context) ([]App, error) {
	var res struct {
		AppList struct {
			Apps struct {
				App []struct {
					AppID   uint32 `json:"appid"`
					AppName string `json:"app_name"`
					AppType string `json:"app_type"`
				} `json:"app"`
			} `json:"apps"`
		} `json:"applist"`
	}
	if err := c.get(ctx, "ISteamApps", "GetPartnerAppListForWebAPIKey", 2, nil, &res); err != nil {
		return nil, err
	}
	var out []App
	for _, a := range res.AppList.Apps.App {
		out = append(out, App{ID: a.AppID, Name: a.AppName, Type: a.AppType})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// HasApp reports whether the key controls the given app.
func (c *Client) HasApp(ctx context.Context, appID uint32) (bool, error) {
	apps, err := c.Apps(ctx)
	if err != nil {
		return false, err
	}
	for _, a := range apps {
		if a.ID == appID {
			return true, nil
		}
	}
	return false, nil
}

type Build struct {
	ID          uint32
	Description string
	CreatedAt   int64
	CreatorID   uint32
	// Depots maps depot id to manifest gid.
	Depots map[uint32]uint64
}

// Builds returns an app's build history, newest first.
func (c *Client) Builds(ctx context.Context, appID uint32, count int) ([]Build, error) {
	if count <= 0 {
		count = 20
	}
	var res struct {
		Response struct {
			Builds map[string]struct {
				BuildID     uint32 `json:"BuildID"`
				Description string `json:"Description"`
				Created     int64  `json:"CreationTime"`
				Creator     uint32 `json:"AccountIDCreator"`
				Depots      map[string]struct {
					GID string `json:"DepotVersionGID"`
				} `json:"depots"`
			} `json:"builds"`
		} `json:"response"`
	}
	q := url.Values{"appid": {strconv.FormatUint(uint64(appID), 10)}, "count": {strconv.Itoa(count)}}
	if err := c.get(ctx, "ISteamApps", "GetAppBuilds", 1, q, &res); err != nil {
		return nil, err
	}
	var out []Build
	for _, b := range res.Response.Builds {
		build := Build{ID: b.BuildID, Description: b.Description, CreatedAt: b.Created, CreatorID: b.Creator, Depots: map[uint32]uint64{}}
		for id, d := range b.Depots {
			depot, _ := strconv.ParseUint(id, 10, 32)
			gid, _ := strconv.ParseUint(d.GID, 10, 64)
			build.Depots[uint32(depot)] = gid
		}
		out = append(out, build)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (c *Client) get(ctx context.Context, iface, method string, version int, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("key", c.Key)
	q.Set("format", "json")
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/%s/%s/v%d/?%s", c.BaseURL, iface, method, version, q.Encode()), nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("partner api %s: %w", method, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return &StatusError{Method: method, Status: res.StatusCode}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("partner api %s: decoding response: %w", method, err)
	}
	return nil
}

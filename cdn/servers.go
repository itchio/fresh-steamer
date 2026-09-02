// Package cdn fetches depot manifests and chunks from Steam's content
// servers and decodes them into plain bytes.
package cdn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/itchio/fresh-steamer/pb"
	"github.com/itchio/fresh-steamer/webapi"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	Host  string
	VHost string
	HTTPS bool
	Type  string
	Load  int32
}

func (s Server) BaseURL() string {
	if s.HTTPS {
		return "https://" + s.Host
	}
	return "http://" + s.Host
}

// Servers lists content servers for the given cell, best first.
func Servers(ctx context.Context, api *webapi.Client, cellID uint32, max uint32) ([]Server, error) {
	if api == nil {
		api = webapi.NewClient()
	}
	if max == 0 {
		max = 20
	}
	var res pb.CContentServerDirectory_GetServersForSteamPipe_Response
	err := api.Call(ctx, false, "IContentServerDirectoryService", "GetServersForSteamPipe", 1, &pb.CContentServerDirectory_GetServersForSteamPipe_Request{
		CellId:     proto.Uint32(cellID),
		MaxServers: proto.Uint32(max),
	}, &res)
	if err != nil {
		return nil, fmt.Errorf("listing content servers: %w", err)
	}
	var out []Server
	for _, s := range res.GetServers() {
		switch s.GetType() {
		case "CDN", "SteamCache":
		default:
			continue
		}
		if s.GetUseAsProxy() || s.GetSteamChinaOnly() {
			continue
		}
		out = append(out, Server{
			Host:  s.GetHost(),
			VHost: s.GetVhost(),
			HTTPS: s.GetHttpsSupport() == "mandatory" || s.GetHttpsSupport() == "optional",
			Type:  s.GetType(),
			Load:  s.GetLoad(),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable content servers returned for cell %d", cellID)
	}
	return out, nil
}

// Client fetches from a list of servers. Each request starts on a
// different server so load spreads out, moves to the next one on failure,
// and the whole sweep is retried with backoff.
type Client struct {
	HTTP    *http.Client
	Servers []Server
	Logf    func(format string, args ...any)
	// Retries is how many extra sweeps over the server list to make before
	// giving up on a request. Zero means the default of 3.
	Retries int
	// Backoff is the wait before the first retry; it doubles each time up to
	// ten times this value. Zero means the default of 500ms.
	Backoff time.Duration

	next atomic.Uint32
}

func NewClient(servers []Server) *Client {
	return &Client{HTTP: http.DefaultClient, Servers: servers, Logf: func(string, ...any) {}}
}

func (c *Client) retries() int {
	if c.Retries <= 0 {
		return 3
	}
	return c.Retries
}

func (c *Client) backoff(attempt int) time.Duration {
	base := c.Backoff
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	d := base << attempt
	if d > base*10 {
		d = base * 10
	}
	return d
}

// permanentError marks failures that every server will repeat, so retrying
// only burns time.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// retry runs fn until it succeeds, returns a permanent error, or the retry
// budget is spent.
func (c *Client) retry(ctx context.Context, what string, fn func() error) error {
	var err error
	for attempt := 0; attempt <= c.retries(); attempt++ {
		if attempt > 0 {
			wait := c.backoff(attempt - 1)
			c.Logf("cdn: %s failed (%v), retry %d/%d in %s", what, err, attempt, c.retries(), wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err = fn()
		if err == nil {
			return nil
		}
		var perm permanentError
		if errors.As(err, &perm) {
			return perm.err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return err
}

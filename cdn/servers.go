// Package cdn fetches depot manifests and chunks from Steam's content
// servers and decodes them into plain bytes.
package cdn

import (
	"context"
	"fmt"
	"net/http"

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

// Client fetches from a fixed list of servers, rotating on failure.
type Client struct {
	HTTP    *http.Client
	Servers []Server
	Logf    func(format string, args ...any)
}

func NewClient(servers []Server) *Client {
	return &Client{HTTP: http.DefaultClient, Servers: servers, Logf: func(string, ...any) {}}
}

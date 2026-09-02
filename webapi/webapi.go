// Package webapi calls Steam's public Web API with protobuf-encoded
// requests and responses, the way the Steam client itself does.
package webapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"google.golang.org/protobuf/proto"
)

const DefaultBaseURL = "https://api.steampowered.com"

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

func NewClient() *Client {
	return &Client{HTTP: http.DefaultClient, BaseURL: DefaultBaseURL}
}

// Error is a non-OK EResult reported by the Web API in its response headers.
type Error struct {
	EResult int
	Message string
	Method  string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("steam web api %s: eresult %d: %s", e.Method, e.EResult, e.Message)
	}
	return fmt.Sprintf("steam web api %s: eresult %d", e.Method, e.EResult)
}

// Call invokes iface/method/vN. GET is used when post is false. The request
// message is sent as input_protobuf_encoded and the response body is parsed
// into out. A nil out discards the body.
func (c *Client) Call(ctx context.Context, post bool, iface, method string, version int, in proto.Message, out proto.Message) error {
	raw, err := proto.Marshal(in)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("format", "protobuf_raw")
	q.Set("input_protobuf_encoded", base64.StdEncoding.EncodeToString(raw))

	endpoint := fmt.Sprintf("%s/%s/%s/v%d/", c.BaseURL, iface, method, version)
	var req *http.Request
	if post {
		req, err = http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBufferString(q.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req, err = http.NewRequestWithContext(ctx, "GET", endpoint+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
	}
	req.Header.Set("User-Agent", "fresh-steamer/0.1")

	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if er := res.Header.Get("x-eresult"); er != "" && er != "1" {
		n, _ := strconv.Atoi(er)
		return &Error{EResult: n, Message: res.Header.Get("x-error_message"), Method: iface + "/" + method}
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("steam web api %s/%s: HTTP %d", iface, method, res.StatusCode)
	}
	if out == nil {
		return nil
	}
	return proto.Unmarshal(body, out)
}

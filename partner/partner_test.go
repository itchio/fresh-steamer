package partner

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestTransportErrorOmitsKey(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	c := NewClient("secret-publisher-key")
	c.BaseURL = "http://" + addr
	err = c.get(context.Background(), "ISteamApps", "GetAppList", 2, nil, &struct{}{})
	if err == nil {
		t.Fatal("expected connection error")
	}
	if strings.Contains(err.Error(), "secret-publisher-key") {
		t.Fatalf("error leaks key: %v", err)
	}
	if !strings.Contains(err.Error(), "GetAppList") {
		t.Fatalf("error lost method name: %v", err)
	}
}

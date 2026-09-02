package cdn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *int32) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	c := NewClient([]Server{{Host: host}, {Host: host}})
	c.Backoff = time.Millisecond
	c.Retries = 2
	return c, &hits
}

func TestGetRetriesTransientErrors(t *testing.T) {
	var n int32
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 4 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("ok"))
	})
	var got []byte
	err := c.retry(context.Background(), "test", func() error {
		var err error
		got, err = c.get(context.Background(), "/x")
		return err
	})
	if err != nil || string(got) != "ok" {
		t.Fatalf("err=%v got=%q", err, got)
	}
	// Two servers per sweep, so the fourth request lands in the second sweep.
	if *hits != 4 {
		t.Fatalf("hits=%d", *hits)
	}
}

func TestGetStopsOnPermanentError(t *testing.T) {
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	err := c.retry(context.Background(), "test", func() error {
		_, err := c.get(context.Background(), "/x")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err=%v", err)
	}
	if *hits != 1 {
		t.Fatalf("hits=%d, expected no retries on 404", *hits)
	}
}

func TestGetGivesUpAfterBudget(t *testing.T) {
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	err := c.retry(context.Background(), "test", func() error {
		_, err := c.get(context.Background(), "/x")
		return err
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	// 1 initial sweep + 2 retries, 2 servers each.
	if *hits != 6 {
		t.Fatalf("hits=%d", *hits)
	}
}

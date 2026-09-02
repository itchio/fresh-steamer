package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestFetchParsesRealResponse(t *testing.T) {
	body, err := os.ReadFile("testdata/appdetails_3372060.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("appids") != "3372060" {
			w.WriteHeader(404)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	p, err := c.Fetch(context.Background(), 3372060, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Hell Maiden" || p.Type != "game" || !p.Platforms.Windows || p.Platforms.Linux {
		t.Fatalf("basics: %+v", p)
	}
	if len(p.Screenshots) != 9 || p.Screenshots[0].Full == "" {
		t.Fatalf("screenshots: %d", len(p.Screenshots))
	}
	if len(p.Movies) != 3 || p.Movies[0].DashH264 == "" || p.Movies[0].HLS == "" || p.Movies[0].Thumbnail == "" {
		t.Fatalf("movies: %+v", p.Movies)
	}
	if p.Price == nil || p.Price.Currency != "USD" || p.Price.Final != 999 {
		t.Fatalf("price: %+v", p.Price)
	}
	if len(p.Genres) != 5 || p.Genres[0] != "Action" || len(p.Developers) != 1 {
		t.Fatalf("genres %v devs %v", p.Genres, p.Developers)
	}
	if p.Requirements.Windows.Minimum == "" || p.Requirements.Linux.Minimum != "" {
		t.Fatalf("requirements: %+v", p.Requirements)
	}
	if p.ReleaseDate != "Jul 16, 2026" || p.ComingSoon || len(p.DetailedDescription) < 1000 {
		t.Fatalf("release %q coming %v desc %d", p.ReleaseDate, p.ComingSoon, len(p.DetailedDescription))
	}
}

func TestFetchNoStorePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"999":{"success":false}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	if _, err := c.Fetch(context.Background(), 999, ""); err != ErrNoStorePage {
		t.Fatalf("err=%v", err)
	}
}

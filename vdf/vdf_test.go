package vdf

import "testing"

func TestParse(t *testing.T) {
	src := `"appinfo"
{
	"appid"		"440"
	"depots"
	{
		"441"
		{
			"config" { "oslist" "windows" }
			"manifests" { "public" { "gid" "123" "size" "9" } }
		}
		"branches" { "public" { "buildid" "77" } }
	}
	"escaped"	"a \"quoted\" \\ path"
}
`
	root, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	app := root.Get("AppInfo")
	if app.Get("appid").Uint32() != 440 {
		t.Fatalf("appid: %q", app.Get("appid").String())
	}
	if got := app.Path("depots", "441", "manifests", "public", "gid").Uint64(); got != 123 {
		t.Fatalf("gid: %d", got)
	}
	if got := app.Path("depots", "branches", "public", "buildid").Uint32(); got != 77 {
		t.Fatalf("buildid: %d", got)
	}
	if got := app.Get("escaped").String(); got != `a "quoted" \ path` {
		t.Fatalf("escaped: %q", got)
	}
	if app.Path("depots", "nope") != nil {
		t.Fatal("expected nil for missing path")
	}
}

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

func TestParseBinary(t *testing.T) {
	// "pkg" { "packageid" int32 7, "appids" { "0" int32 440 }, "name" "x" }
	src := []byte{
		0, 'p', 'k', 'g', 0,
		2, 'p', 'a', 'c', 'k', 'a', 'g', 'e', 'i', 'd', 0, 7, 0, 0, 0,
		0, 'a', 'p', 'p', 'i', 'd', 's', 0,
		2, '0', 0, 0xB8, 0x01, 0, 0,
		8,
		1, 'n', 'a', 'm', 'e', 0, 'x', 0,
		8,
		8,
	}
	root, err := ParseBinary(src)
	if err != nil {
		t.Fatal(err)
	}
	pkg := root.Get("pkg")
	if pkg.Get("packageid").Uint32() != 7 {
		t.Fatalf("packageid: %q", pkg.Get("packageid").String())
	}
	if pkg.Path("appids", "0").Uint32() != 440 {
		t.Fatalf("appid: %q", pkg.Path("appids", "0").String())
	}
	if pkg.Get("name").String() != "x" {
		t.Fatalf("name: %q", pkg.Get("name").String())
	}
}

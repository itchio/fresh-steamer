package appinfo

import (
	"testing"

	"github.com/itchio/fresh-steamer/vdf"
)

func TestMergePrivateDepots(t *testing.T) {
	public, err := vdf.Parse([]byte(`
"appinfo"
{
	"appid" "10"
	"depots"
	{
		"branches" { "public" { "buildid" "1" } }
		"11" { "name" "win" "config" { "oslist" "windows" } "manifests" { "public" { "gid" "100" } } }
	}
}`))
	if err != nil {
		t.Fatal(err)
	}
	app := parse(public.Get("appinfo"))

	private, err := vdf.Parse([]byte(`
"privatedepots"
{
	"branches" { "alphatest" { "buildid" "2" "pwdrequired" "1" } }
	"11" { "manifests" { "alphatest" { "gid" "200" "size" "5" } } }
}`))
	if err != nil {
		t.Fatal(err)
	}
	app.mergeDepots(private.Get("privatedepots"))

	if b := app.Branch("alphatest"); b == nil || b.BuildID != 2 || !b.PasswordRequired {
		t.Fatalf("branch not merged: %+v", b)
	}
	if len(app.Depots) != 1 {
		t.Fatalf("depot duplicated: %d depots", len(app.Depots))
	}
	d := app.Depot(11)
	if d.Manifests["public"].GID != 100 || d.Manifests["alphatest"].GID != 200 {
		t.Fatalf("manifests: %+v", d.Manifests)
	}
	if len(d.OSList) != 1 || d.OSList[0] != "windows" {
		t.Fatalf("depot config lost on merge: %+v", d)
	}
	app.mergeDepots(private.Get("privatedepots"))
	if len(app.Branches) != 2 {
		t.Fatalf("branch duplicated on second merge: %d", len(app.Branches))
	}
}

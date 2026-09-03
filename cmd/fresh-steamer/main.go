// Command fresh-steamer is a development CLI for the library: log in, list
// an app's depots and branches, and download a depot.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/itchio/fresh-steamer/appinfo"
	"github.com/itchio/fresh-steamer/auth"
	"github.com/itchio/fresh-steamer/depot"
	"github.com/itchio/fresh-steamer/partner"
	"github.com/itchio/fresh-steamer/session"
	"github.com/itchio/fresh-steamer/store"
	"github.com/mdp/qrterminal/v3"
	"golang.org/x/term"
)

type creds struct {
	AccountName  string `json:"account_name"`
	SteamID      uint64 `json:"steam_id"`
	RefreshToken string `json:"refresh_token"`
	PublisherKey string `json:"publisher_key,omitempty"`
}

func credsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "fresh-steamer", "creds.json")
}

func loadCreds() (*creds, error) {
	data, err := os.ReadFile(credsPath())
	if err != nil {
		return nil, fmt.Errorf("not logged in, run `fresh-steamer login` first (%w)", err)
	}
	var c creds
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.RefreshToken == "" {
		return nil, fmt.Errorf("not logged in, run `fresh-steamer login` first")
	}
	return &c, nil
}

func loadCredsOptional() (*creds, error) {
	data, err := os.ReadFile(credsPath())
	if os.IsNotExist(err) {
		return &creds{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c creds
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func saveCreds(c *creds) error {
	p := credsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "login":
		err = cmdLogin(ctx, os.Args[2:])
	case "logout":
		err = os.Remove(credsPath())
	case "partner-key":
		err = cmdPartnerKey(ctx)
	case "apps":
		err = cmdApps(ctx, os.Args[2:])
	case "partner-apps":
		err = cmdPartnerApps(ctx)
	case "builds":
		err = cmdBuilds(ctx, os.Args[2:])
	case "info":
		err = cmdInfo(ctx, os.Args[2:])
	case "page":
		err = cmdPage(ctx, os.Args[2:])
	case "download":
		err = cmdDownload(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  fresh-steamer login [-password] [-user NAME]
  fresh-steamer logout
  fresh-steamer partner-key
  fresh-steamer apps [-all]        apps the Steam account holds a license for
  fresh-steamer partner-apps       apps the publisher key controls
  fresh-steamer builds APPID
  fresh-steamer info APPID
  fresh-steamer page APPID [-out DIR]   store listing and artwork as JSON, assets downloaded into DIR
  fresh-steamer download -app APPID -depot DEPOTID [-branch public] [-password PW] -dir DIR`)
	os.Exit(2)
}

func prompt(label string, secret bool) (string, error) {
	fmt.Fprint(os.Stderr, label)
	if secret && term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}

func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	user := fs.String("user", "", "account name, password flow only")
	usePassword := fs.Bool("password", false, "log in with account name and password instead of scanning a QR code")
	fs.Parse(args)

	var c *auth.Credentials
	var err error
	if *usePassword {
		c, err = loginPassword(ctx, *user)
	} else {
		c, err = loginQR(ctx)
	}
	if err != nil {
		return err
	}
	existing, err := loadCredsOptional()
	if err != nil {
		return err
	}
	existing.AccountName = c.AccountName
	existing.SteamID = c.SteamID
	existing.RefreshToken = c.RefreshToken
	if err := saveCreds(existing); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Logged in as %s, credentials saved to %s\n", c.AccountName, credsPath())
	return nil
}

func loginQR(ctx context.Context) (*auth.Credentials, error) {
	return auth.LoginQR(ctx, auth.QROptions{
		OnChallenge: func(url string) {
			fmt.Fprintln(os.Stderr, "\nScan with the Steam mobile app, then approve the login there.")
			fmt.Fprintln(os.Stderr, "Or open this link on your phone:", url)
			fmt.Fprintln(os.Stderr)
			qrterminal.GenerateWithConfig(url, qrterminal.Config{
				Level:          qrterminal.L,
				Writer:         os.Stderr,
				HalfBlocks:     true,
				BlackChar:      qrterminal.BLACK_BLACK,
				WhiteChar:      qrterminal.WHITE_WHITE,
				BlackWhiteChar: qrterminal.BLACK_WHITE,
				WhiteBlackChar: qrterminal.WHITE_BLACK,
				QuietZone:      2,
			})
			fmt.Fprintln(os.Stderr, "\nWaiting for approval... (ctrl-c to cancel, or use `login -password`)")
		},
	})
}

func loginPassword(ctx context.Context, name string) (*auth.Credentials, error) {
	var err error
	if name == "" {
		if name, err = prompt("Steam account name: ", false); err != nil {
			return nil, err
		}
	}
	pass, err := prompt("Password: ", true)
	if err != nil {
		return nil, err
	}

	return auth.Login(ctx, auth.Options{
		AccountName: name,
		Password:    pass,
		Guard: auth.GuardFunc(func(ctx context.Context, kind auth.GuardType, msg string) (string, error) {
			if kind == auth.GuardDeviceConfirmation {
				fmt.Fprintln(os.Stderr, "Approve the login in the Steam mobile app...")
				return "", nil
			}
			label := fmt.Sprintf("Steam Guard %s", kind)
			if msg != "" {
				label += " (" + msg + ")"
			}
			return prompt(label+": ", false)
		}),
	})
}

func openSession(ctx context.Context) (*session.Session, *creds, error) {
	c, err := loadCreds()
	if err != nil {
		return nil, nil, err
	}
	s, err := session.Open(ctx, session.Options{
		AccountName:  c.AccountName,
		RefreshToken: c.RefreshToken,
		KeyFile:      filepath.Join(filepath.Dir(credsPath()), "depot_keys.json"),
		Logf: func(format string, args ...any) {
			if os.Getenv("FRESH_STEAMER_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, format+"\n", args...)
			}
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return s, c, nil
}

func partnerClient() (*partner.Client, error) {
	c, err := loadCredsOptional()
	if err != nil {
		return nil, err
	}
	if c.PublisherKey == "" {
		return nil, fmt.Errorf("no publisher key stored, run `fresh-steamer partner-key` first")
	}
	return partner.NewClient(c.PublisherKey), nil
}

func cmdPartnerKey(ctx context.Context) error {
	fmt.Fprintln(os.Stderr, "Create a publisher Web API key at https://partner.steamgames.com/pub/groups/ under your publisher group.")
	key, err := prompt("Publisher Web API key: ", true)
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("empty key")
	}
	pc := partner.NewClient(key)
	apps, err := pc.Apps(ctx)
	if err != nil {
		return err
	}
	c, err := loadCredsOptional()
	if err != nil {
		return err
	}
	c.PublisherKey = key
	if err := saveCreds(c); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Key verified, %d apps accessible. Saved to %s\n", len(apps), credsPath())
	return nil
}

func cmdApps(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apps", flag.ExitOnError)
	all := fs.Bool("all", false, "include package 0, the free tools every account has")
	fs.Parse(args)

	s, _, err := openSession(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	licenses, err := appinfo.Licenses(ctx, s.CM)
	if err != nil {
		return err
	}
	packages, err := appinfo.Packages(ctx, s.CM, licenses)
	if err != nil {
		return err
	}
	byApp := map[uint32][]*appinfo.Package{}
	var ids []uint32
	for _, p := range packages {
		if p.ID == 0 && !*all {
			continue
		}
		for _, a := range p.AppIDs {
			if _, seen := byApp[a]; !seen {
				ids = append(ids, a)
			}
			byApp[a] = append(byApp[a], p)
		}
	}
	names, err := appinfo.Names(ctx, s.CM, ids)
	if err != nil {
		return err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	fmt.Printf("%d licenses, %d apps\n\n", len(licenses), len(ids))
	for _, id := range ids {
		var via []string
		for _, p := range byApp[id] {
			label := fmt.Sprintf("%d (%s)", p.ID, appinfo.BillingTypeName(p.BillingType))
			if p.DeveloperOnly {
				label += " [developer]"
			}
			via = append(via, label)
		}
		fmt.Printf("%-10d %-40s via package %s\n", id, names[id], strings.Join(via, "; "))
	}
	return nil
}

func cmdPartnerApps(ctx context.Context) error {
	pc, err := partnerClient()
	if err != nil {
		return err
	}
	apps, err := pc.Apps(ctx)
	if err != nil {
		return err
	}
	for _, a := range apps {
		fmt.Printf("%-10d %-12s %s\n", a.ID, a.Type, a.Name)
	}
	return nil
}

func cmdBuilds(ctx context.Context, args []string) error {
	if len(args) < 1 {
		usage()
	}
	appID, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return err
	}
	pc, err := partnerClient()
	if err != nil {
		return err
	}
	builds, err := pc.Builds(ctx, uint32(appID), 20)
	if err != nil {
		return err
	}
	for _, b := range builds {
		fmt.Printf("build %d  %s  %s\n", b.ID, time.Unix(b.CreatedAt, 0).Format("2006-01-02 15:04"), b.Description)
		ids := make([]uint32, 0, len(b.Depots))
		for id := range b.Depots {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			fmt.Printf("    depot %d manifest %d\n", id, b.Depots[id])
		}
	}
	return nil
}

func cmdInfo(ctx context.Context, args []string) error {
	if len(args) < 1 {
		usage()
	}
	appID, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return err
	}
	s, _, err := openSession(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	app, err := s.AppInfo(ctx, uint32(appID))
	if err != nil {
		return err
	}
	appOS := strings.Join(app.OSList, ",")
	if app.OSArch != "" {
		appOS += "/" + app.OSArch
	}
	fmt.Printf("app %d: %s (%s, %s, change %d)\n\nbranches:\n", app.ID, app.Name, app.Type, appOS, app.ChangeNumber)
	for _, b := range app.Branches {
		pw := ""
		if b.PasswordRequired {
			pw = " [password]"
		}
		fmt.Printf("  %-20s build %d%s %s\n", b.Name, b.BuildID, pw, b.Description)
	}
	fmt.Println("\ndepots:")
	for _, d := range app.Depots {
		extra := ""
		if d.DLCAppID != 0 {
			extra += fmt.Sprintf(" dlc=%d", d.DLCAppID)
		}
		if d.SharedFromApp != 0 {
			extra += fmt.Sprintf(" from-app=%d", d.SharedFromApp)
		}
		if d.Language != "" {
			extra += " lang=" + d.Language
		}
		os := strings.Join(d.OSList, ",")
		if os == "" {
			if len(app.OSList) > 0 {
				os = strings.Join(app.OSList, ",") + " (from app)"
			} else {
				os = "all"
			}
		}
		if d.OSArch != "" {
			os += "/" + d.OSArch
		}
		fmt.Printf("  %-10d %-30s %-16s%s\n", d.ID, d.Name, os, extra)
		for br, m := range d.Manifests {
			fmt.Printf("             %-20s gid %d size %d\n", br, m.GID, m.Size)
		}
		for br := range d.EncryptedManifests {
			fmt.Printf("             %-20s [encrypted]\n", br)
		}
	}
	return nil
}

// pageDump is what the page command prints: store data when a public
// listing exists, app info artwork always, and the parent app for
// playtests and demos so a caller can look there for the listing.
type pageDump struct {
	AppID        uint32                `json:"app_id"`
	Name         string                `json:"name"`
	Type         string                `json:"type"`
	Parent       uint32                `json:"parent,omitempty"`
	ReleaseState string                `json:"release_state"`
	ReleaseDate  int64                 `json:"release_date_unix,omitempty"`
	OSList       []string              `json:"os_list"`
	Associations []appinfo.Association `json:"associations"`
	Assets       appinfo.Assets        `json:"assets"`
	Store        *store.Page           `json:"store,omitempty"`
	StoreError   string                `json:"store_error,omitempty"`
	Files        map[string]string     `json:"files,omitempty"`
}

func cmdPage(ctx context.Context, args []string) error {
	if len(args) < 1 {
		usage()
	}
	appID, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("page", flag.ExitOnError)
	out := fs.String("out", "", "directory to download artwork and screenshots into")
	lang := fs.String("lang", "english", "store language")
	fs.Parse(args[1:])

	s, _, err := openSession(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	app, err := s.AppInfo(ctx, uint32(appID))
	if err != nil {
		return err
	}
	dump := pageDump{
		AppID:        app.ID,
		Name:         app.Name,
		Type:         app.Type,
		Parent:       app.Parent(),
		ReleaseState: app.ReleaseState(),
		ReleaseDate:  app.ReleaseDate(),
		OSList:       app.OSList,
		Associations: app.Associations(),
		Assets:       app.Assets(),
	}

	page, err := store.NewClient().Fetch(ctx, app.ID, *lang)
	if err != nil {
		dump.StoreError = err.Error()
	} else {
		dump.Store = page
	}

	if *out != "" {
		dump.Files = map[string]string{}
		get := func(label, url string) {
			if url == "" {
				return
			}
			p, err := store.Download(ctx, nil, url, *out, label)
			if err != nil {
				fmt.Fprintln(os.Stderr, "warning:", err)
				return
			}
			dump.Files[label] = p
		}
		a := dump.Assets
		get("icon", a.Icon)
		get("header", a.Header)
		get("small_capsule", a.SmallCapsule)
		get("library_capsule", a.LibraryCapsule)
		get("library_hero", a.LibraryHero)
		get("library_logo", a.LibraryLogo)
		get("library_header", a.LibraryHeader)
		if page != nil {
			get("store_header", page.HeaderImage)
			get("store_capsule", page.CapsuleImage)
			get("background", page.Background)
			for i, sc := range page.Screenshots {
				get(fmt.Sprintf("screenshot_%02d", i+1), sc.Full)
			}
			for i, m := range page.Movies {
				get(fmt.Sprintf("movie_%02d_thumb", i+1), m.Thumbnail)
			}
		}
		data, _ := json.MarshalIndent(dump, "", "  ")
		if err := os.WriteFile(filepath.Join(*out, "page.json"), data, 0o644); err != nil {
			return err
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(dump)
}

func cmdDownload(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	appID := fs.Uint("app", 0, "app id")
	depotID := fs.Uint("depot", 0, "depot id")
	branch := fs.String("branch", "public", "branch name")
	password := fs.String("password", "", "branch password")
	dir := fs.String("dir", "", "output directory")
	fs.Parse(args)
	if *appID == 0 || *depotID == 0 || *dir == "" {
		usage()
	}

	s, _, err := openSession(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	app, err := s.AppInfo(ctx, uint32(*appID))
	if err != nil {
		return err
	}
	d := app.Depot(uint32(*depotID))
	if d == nil {
		return fmt.Errorf("app %d has no depot %d", *appID, *depotID)
	}
	gid, err := s.ResolveManifest(ctx, app, d, *branch, *password)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "depot %d branch %s manifest %d\n", d.ID, *branch, gid)

	key, err := s.DepotKey(ctx, app.ID, d.ID)
	if err != nil {
		return err
	}

	code, err := s.ManifestRequestCode(ctx, app.ID, d.ID, gid, *branch, "")
	if err != nil {
		return err
	}
	cdnClient, err := s.CDN(ctx)
	if err != nil {
		return err
	}
	manifest, err := cdnClient.FetchManifest(ctx, d.ID, gid, code, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "manifest: %d files, %d bytes\n", len(manifest.Files), manifest.TotalSize)

	var last depot.Progress
	err = depot.Download(ctx, cdnClient, depot.Options{
		Dir:      *dir,
		DepotID:  d.ID,
		DepotKey: key,
		Manifest: manifest,
		Store:    &depot.Store{Dir: filepath.Join(*dir, ".fresh-steamer")},
		OnProgress: func(p depot.Progress) {
			last = p
			if p.BytesTotal > 0 {
				fmt.Fprintf(os.Stderr, "\r%d/%d files, %.1f%%", p.FilesDone, p.FilesTotal, float64(p.BytesDone)*100/float64(p.BytesTotal))
			}
		},
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "done, %d bytes skipped as unchanged, %d bytes reused from old files\n", last.BytesSkipped, last.BytesReused)
	return nil
}

// Command fresh-steamer is a development CLI for the library: log in, list
// an app's depots and branches, and download a depot.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/itchio/fresh-steamer/auth"
	"github.com/itchio/fresh-steamer/cdn"
	"github.com/itchio/fresh-steamer/depot"
	"github.com/itchio/fresh-steamer/session"
	"golang.org/x/term"
)

type creds struct {
	AccountName  string            `json:"account_name"`
	SteamID      uint64            `json:"steam_id"`
	RefreshToken string            `json:"refresh_token"`
	DepotKeys    map[string]string `json:"depot_keys,omitempty"`
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
	case "info":
		err = cmdInfo(ctx, os.Args[2:])
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
  fresh-steamer login [-user NAME]
  fresh-steamer logout
  fresh-steamer info APPID
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
	user := fs.String("user", "", "account name")
	fs.Parse(args)

	name := *user
	var err error
	if name == "" {
		if name, err = prompt("Steam account name: ", false); err != nil {
			return err
		}
	}
	pass, err := prompt("Password: ", true)
	if err != nil {
		return err
	}

	c, err := auth.Login(ctx, auth.Options{
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
	if err != nil {
		return err
	}
	if err := saveCreds(&creds{AccountName: c.AccountName, SteamID: c.SteamID, RefreshToken: c.RefreshToken}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Logged in as %s, credentials saved to %s\n", c.AccountName, credsPath())
	return nil
}

func openSession(ctx context.Context) (*session.Session, *creds, error) {
	c, err := loadCreds()
	if err != nil {
		return nil, nil, err
	}
	keys := map[uint32][]byte{}
	for id, k := range c.DepotKeys {
		n, _ := strconv.ParseUint(id, 10, 32)
		if b, err := hex.DecodeString(k); err == nil {
			keys[uint32(n)] = b
		}
	}
	s, err := session.Open(ctx, session.Options{
		AccountName:  c.AccountName,
		RefreshToken: c.RefreshToken,
		DepotKeys:    keys,
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

func saveKeys(s *session.Session, c *creds) {
	c.DepotKeys = map[string]string{}
	for id, k := range s.DepotKeys() {
		c.DepotKeys[strconv.FormatUint(uint64(id), 10)] = hex.EncodeToString(k)
	}
	_ = saveCreds(c)
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
	fmt.Printf("app %d: %s (change %d)\n\nbranches:\n", app.ID, app.Name, app.ChangeNumber)
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
			os = "all"
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

	s, c, err := openSession(ctx)
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
	saveKeys(s, c)

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

	stateDir := filepath.Join(*dir, ".fresh-steamer")
	prev := loadPrevious(stateDir, d.ID)

	var last depot.Progress
	err = depot.Download(ctx, cdnClient, depot.Options{
		Dir:      *dir,
		DepotID:  d.ID,
		DepotKey: key,
		Manifest: manifest,
		Previous: prev,
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
	fmt.Fprintf(os.Stderr, "done, %d bytes skipped as unchanged\n", last.BytesSkipped)
	return savePrevious(stateDir, d.ID, manifest)
}

// Previous manifests are kept as JSON next to the download so a later run
// can skip unchanged files.
func loadPrevious(stateDir string, depotID uint32) *cdn.Manifest {
	data, err := os.ReadFile(filepath.Join(stateDir, fmt.Sprintf("manifest-%d.json", depotID)))
	if err != nil {
		return nil
	}
	var m cdn.Manifest
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return &m
}

func savePrevious(stateDir string, depotID uint32, m *cdn.Manifest) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("manifest-%d.json", depotID)), data, 0o644)
}

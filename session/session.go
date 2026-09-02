// Package session ties the pieces together: a logged-in CM connection, app
// info lookups, depot keys, manifest request codes and a CDN client.
package session

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/itchio/fresh-steamer/appinfo"
	"github.com/itchio/fresh-steamer/cdn"
	"github.com/itchio/fresh-steamer/cm"
	"github.com/itchio/fresh-steamer/pb"
	"github.com/itchio/fresh-steamer/steamcrypto"
	"github.com/itchio/fresh-steamer/webapi"
	"google.golang.org/protobuf/proto"
)

const emsgGetDepotDecryptionKey = 5438

type Session struct {
	CM   *cm.Client
	API  *webapi.Client
	Logf func(format string, args ...any)

	mu        sync.Mutex
	depotKeys map[uint32][]byte
	cdnClient *cdn.Client
}

type Options struct {
	AccountName  string
	RefreshToken string
	Logf         func(format string, args ...any)
	// DepotKeys seeds the key cache, typically from a previous run.
	DepotKeys map[uint32][]byte
}

func Open(ctx context.Context, opts Options) (*Session, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	client, err := cm.Connect(ctx, cm.Options{Logf: logf})
	if err != nil {
		return nil, err
	}
	if err := client.Logon(ctx, opts.AccountName, opts.RefreshToken); err != nil {
		client.Close()
		return nil, err
	}
	s := &Session{
		CM:        client,
		API:       webapi.NewClient(),
		Logf:      logf,
		depotKeys: map[uint32][]byte{},
	}
	for id, k := range opts.DepotKeys {
		s.depotKeys[id] = k
	}
	return s, nil
}

func (s *Session) Close() error { return s.CM.Close() }

// DepotKeys returns every key learned so far, for callers that cache them.
func (s *Session) DepotKeys() map[uint32][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint32][]byte, len(s.depotKeys))
	for id, k := range s.depotKeys {
		out[id] = k
	}
	return out
}

func (s *Session) AppInfo(ctx context.Context, appID uint32) (*appinfo.App, error) {
	return appinfo.Fetch(ctx, s.CM, appID)
}

func (s *Session) DepotKey(ctx context.Context, appID, depotID uint32) ([]byte, error) {
	s.mu.Lock()
	if k, ok := s.depotKeys[depotID]; ok {
		s.mu.Unlock()
		return k, nil
	}
	s.mu.Unlock()

	pkt, err := s.CM.Request(ctx, emsgGetDepotDecryptionKey, &pb.CMsgClientGetDepotDecryptionKey{
		DepotId: proto.Uint32(depotID),
		AppId:   proto.Uint32(appID),
	})
	if err != nil {
		return nil, fmt.Errorf("requesting key for depot %d: %w", depotID, err)
	}
	var res pb.CMsgClientGetDepotDecryptionKeyResponse
	if err := pkt.Unmarshal(&res); err != nil {
		return nil, err
	}
	if res.GetEresult() != 1 {
		return nil, &cm.EResultError{EResult: res.GetEresult(), Context: fmt.Sprintf("depot %d key", depotID)}
	}
	key := res.GetDepotEncryptionKey()
	s.mu.Lock()
	s.depotKeys[depotID] = key
	s.mu.Unlock()
	return key, nil
}

// ManifestRequestCode fetches the short-lived code the CDN demands for a
// manifest. branchPassword is only needed for password branches.
func (s *Session) ManifestRequestCode(ctx context.Context, appID, depotID uint32, gid uint64, branch, branchPassword string) (uint64, error) {
	req := &pb.CContentServerDirectory_GetManifestRequestCode_Request{
		AppId:      proto.Uint32(appID),
		DepotId:    proto.Uint32(depotID),
		ManifestId: proto.Uint64(gid),
		AppBranch:  proto.String(strings.ToLower(branch)),
	}
	if branchPassword != "" {
		req.BranchPasswordHash = proto.String(branchPassword)
	}
	var res pb.CContentServerDirectory_GetManifestRequestCode_Response
	err := s.CM.Unified(ctx, "ContentServerDirectory.GetManifestRequestCode#1", req, &res)
	if err != nil {
		return 0, err
	}
	if res.GetManifestRequestCode() == 0 {
		return 0, fmt.Errorf("no manifest request code for depot %d manifest %d on branch %q; is the account licensed?", depotID, gid, branch)
	}
	return res.GetManifestRequestCode(), nil
}

// CDN returns a content client for the session's cell, built on first use.
func (s *Session) CDN(ctx context.Context) (*cdn.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cdnClient != nil {
		return s.cdnClient, nil
	}
	servers, err := cdn.Servers(ctx, s.API, s.CM.CellID, 20)
	if err != nil {
		return nil, err
	}
	s.cdnClient = cdn.NewClient(servers)
	s.cdnClient.Logf = s.Logf
	return s.cdnClient, nil
}

// ResolveManifest returns the manifest gid for a depot on a branch,
// decrypting it with the branch password when the branch is private.
func (s *Session) ResolveManifest(ctx context.Context, app *appinfo.App, depot *appinfo.Depot, branch, password string) (uint64, error) {
	if m, ok := lookupFold(depot.Manifests, branch); ok {
		return m.GID, nil
	}
	enc, ok := lookupFold(depot.EncryptedManifests, branch)
	if !ok {
		return 0, fmt.Errorf("depot %d has no manifest on branch %q", depot.ID, branch)
	}
	if password == "" {
		return 0, fmt.Errorf("branch %q needs a password", branch)
	}
	keys, err := appinfo.BranchPasswords(ctx, s.CM, app.ID, password)
	if err != nil {
		return 0, err
	}
	keyHex, ok := lookupFold(keys, branch)
	if !ok {
		return 0, fmt.Errorf("password does not unlock branch %q", branch)
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return 0, fmt.Errorf("branch key: %w", err)
	}
	cipher, err := hex.DecodeString(enc)
	if err != nil {
		return 0, fmt.Errorf("encrypted manifest id: %w", err)
	}
	plain, err := steamcrypto.DecryptECB(cipher, key)
	if err != nil {
		return 0, fmt.Errorf("decrypting manifest id for branch %q: %w", branch, err)
	}
	if len(plain) < 8 {
		return 0, fmt.Errorf("decrypted manifest id for branch %q is too short", branch)
	}
	return binary.LittleEndian.Uint64(plain), nil
}

func lookupFold[V any](m map[string]V, key string) (V, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	var zero V
	return zero, false
}

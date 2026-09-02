// Package auth logs a Steam account in through IAuthenticationService and
// yields the refresh token later used for a connection manager logon.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"time"

	"github.com/itchio/fresh-steamer/pb"
	"github.com/itchio/fresh-steamer/webapi"
	"google.golang.org/protobuf/proto"
)

const authService = "IAuthenticationService"

// GuardType mirrors EAuthSessionGuardType for the confirmation kinds a
// caller can act on.
type GuardType int

const (
	GuardNone               GuardType = 1
	GuardEmailCode          GuardType = 2
	GuardDeviceCode         GuardType = 3
	GuardDeviceConfirmation GuardType = 4
	GuardEmailConfirmation  GuardType = 5
)

func (g GuardType) String() string {
	switch g {
	case GuardNone:
		return "none"
	case GuardEmailCode:
		return "email code"
	case GuardDeviceCode:
		return "authenticator code"
	case GuardDeviceConfirmation:
		return "mobile app confirmation"
	case GuardEmailConfirmation:
		return "email confirmation"
	}
	return fmt.Sprintf("guard type %d", int(g))
}

// Guard supplies Steam Guard input. When the allowed confirmations include a
// code type, PromptCode is called with it. Returning an empty code means
// "wait for out-of-band confirmation" if one is allowed.
type Guard interface {
	PromptCode(ctx context.Context, kind GuardType, message string) (string, error)
}

// GuardFunc adapts a function to Guard.
type GuardFunc func(ctx context.Context, kind GuardType, message string) (string, error)

func (f GuardFunc) PromptCode(ctx context.Context, kind GuardType, message string) (string, error) {
	return f(ctx, kind, message)
}

type Credentials struct {
	AccountName  string
	SteamID      uint64
	RefreshToken string
	AccessToken  string
}

type Options struct {
	AccountName string
	Password    string
	Guard       Guard
	// DeviceName shows up in the account's authorized devices list.
	DeviceName string
	WebAPI     *webapi.Client
}

// Login runs the full credential flow, including Steam Guard.
func Login(ctx context.Context, opts Options) (*Credentials, error) {
	if opts.WebAPI == nil {
		opts.WebAPI = webapi.NewClient()
	}
	if opts.DeviceName == "" {
		opts.DeviceName = "fresh-steamer"
	}
	api := opts.WebAPI

	var keyRes pb.CAuthentication_GetPasswordRSAPublicKey_Response
	err := api.Call(ctx, false, authService, "GetPasswordRSAPublicKey", 1, &pb.CAuthentication_GetPasswordRSAPublicKey_Request{
		AccountName: proto.String(opts.AccountName),
	}, &keyRes)
	if err != nil {
		return nil, fmt.Errorf("fetching password key: %w", err)
	}

	encPassword, err := encryptPassword(opts.Password, keyRes.GetPublickeyMod(), keyRes.GetPublickeyExp())
	if err != nil {
		return nil, err
	}

	var begin pb.CAuthentication_BeginAuthSessionViaCredentials_Response
	err = api.Call(ctx, true, authService, "BeginAuthSessionViaCredentials", 1, &pb.CAuthentication_BeginAuthSessionViaCredentials_Request{
		AccountName:         proto.String(opts.AccountName),
		EncryptedPassword:   proto.String(encPassword),
		EncryptionTimestamp: proto.Uint64(keyRes.GetTimestamp()),
		RememberLogin:       proto.Bool(true),
		Persistence:         pb.ESessionPersistence_k_ESessionPersistence_Persistent.Enum(),
		WebsiteId:           proto.String("Client"),
		DeviceFriendlyName:  proto.String(opts.DeviceName),
		PlatformType:        pb.EAuthTokenPlatformType_k_EAuthTokenPlatformType_SteamClient.Enum(),
		DeviceDetails: &pb.CAuthentication_DeviceDetails{
			DeviceFriendlyName: proto.String(opts.DeviceName),
			PlatformType:       pb.EAuthTokenPlatformType_k_EAuthTokenPlatformType_SteamClient.Enum(),
			OsType:             proto.Int32(osType()),
		},
	}, &begin)
	if err != nil {
		return nil, fmt.Errorf("beginning auth session: %w", err)
	}

	if err := handleGuard(ctx, api, &begin, opts.Guard); err != nil {
		return nil, err
	}

	interval := time.Duration(begin.GetInterval() * float32(time.Second))
	if interval <= 0 {
		interval = 5 * time.Second
	}

	for {
		var poll pb.CAuthentication_PollAuthSessionStatus_Response
		err = api.Call(ctx, true, authService, "PollAuthSessionStatus", 1, &pb.CAuthentication_PollAuthSessionStatus_Request{
			ClientId:  proto.Uint64(begin.GetClientId()),
			RequestId: begin.GetRequestId(),
		}, &poll)
		if err != nil {
			return nil, fmt.Errorf("polling auth session: %w", err)
		}
		if poll.GetRefreshToken() != "" {
			return &Credentials{
				AccountName:  poll.GetAccountName(),
				SteamID:      begin.GetSteamid(),
				RefreshToken: poll.GetRefreshToken(),
				AccessToken:  poll.GetAccessToken(),
			}, nil
		}
		if poll.GetNewClientId() != 0 {
			begin.ClientId = proto.Uint64(poll.GetNewClientId())
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func handleGuard(ctx context.Context, api *webapi.Client, begin *pb.CAuthentication_BeginAuthSessionViaCredentials_Response, guard Guard) error {
	var codeKind GuardType
	var codeMsg string
	canWait := false
	for _, c := range begin.GetAllowedConfirmations() {
		switch GuardType(c.GetConfirmationType()) {
		case GuardNone:
			return nil
		case GuardEmailCode, GuardDeviceCode:
			// Prefer the authenticator over email when both are offered.
			if codeKind == 0 || GuardType(c.GetConfirmationType()) == GuardDeviceCode {
				codeKind = GuardType(c.GetConfirmationType())
				codeMsg = c.GetAssociatedMessage()
			}
		case GuardDeviceConfirmation, GuardEmailConfirmation:
			canWait = true
		}
	}
	if codeKind == 0 && !canWait {
		return errors.New("steam guard: no supported confirmation method offered")
	}
	if codeKind == 0 {
		// The poll loop picks up the confirmation once the user approves.
		if guard != nil {
			_, _ = guard.PromptCode(ctx, GuardDeviceConfirmation, "")
		}
		return nil
	}
	if guard == nil {
		return fmt.Errorf("steam guard: %s required but no Guard provided", codeKind)
	}
	code, err := guard.PromptCode(ctx, codeKind, codeMsg)
	if err != nil {
		return err
	}
	if code == "" {
		if canWait {
			return nil
		}
		return errors.New("steam guard: empty code")
	}
	err = api.Call(ctx, true, authService, "UpdateAuthSessionWithSteamGuardCode", 1, &pb.CAuthentication_UpdateAuthSessionWithSteamGuardCode_Request{
		ClientId: proto.Uint64(begin.GetClientId()),
		Steamid:  proto.Uint64(begin.GetSteamid()),
		Code:     proto.String(code),
		CodeType: pb.EAuthSessionGuardType(codeKind).Enum(),
	}, nil)
	if err != nil {
		return fmt.Errorf("submitting steam guard code: %w", err)
	}
	return nil
}

func encryptPassword(password, modHex, expHex string) (string, error) {
	mod, ok := new(big.Int).SetString(modHex, 16)
	if !ok {
		return "", errors.New("invalid RSA modulus from steam")
	}
	exp, ok := new(big.Int).SetString(expHex, 16)
	if !ok {
		return "", errors.New("invalid RSA exponent from steam")
	}
	pub := &rsa.PublicKey{N: mod, E: int(exp.Int64())}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(password))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// osType returns an EOSType value. The exact version is cosmetic; it only
// shows up in the account's device list.
func osType() int32 {
	switch runtime.GOOS {
	case "windows":
		return 16 // Windows 10
	case "darwin":
		return -102 // macOS unknown
	default:
		return -203 // Linux unknown
	}
}

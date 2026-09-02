package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/itchio/fresh-steamer/pb"
	"github.com/itchio/fresh-steamer/webapi"
	"google.golang.org/protobuf/proto"
)

type QROptions struct {
	// OnChallenge receives the URL to show as a QR code. Steam rotates the
	// challenge every so often, so it may be called more than once.
	OnChallenge func(url string)
	DeviceName  string
	WebAPI      *webapi.Client
}

// LoginQR logs in without a password. The user scans the challenge with the
// Steam mobile app and approves there; the password never reaches this
// process.
func LoginQR(ctx context.Context, opts QROptions) (*Credentials, error) {
	if opts.WebAPI == nil {
		opts.WebAPI = webapi.NewClient()
	}
	if opts.DeviceName == "" {
		opts.DeviceName = "fresh-steamer"
	}
	if opts.OnChallenge == nil {
		return nil, fmt.Errorf("qr login: OnChallenge is required")
	}
	api := opts.WebAPI

	var begin pb.CAuthentication_BeginAuthSessionViaQR_Response
	err := api.Call(ctx, true, authService, "BeginAuthSessionViaQR", 1, &pb.CAuthentication_BeginAuthSessionViaQR_Request{
		DeviceFriendlyName: proto.String(opts.DeviceName),
		PlatformType:       pb.EAuthTokenPlatformType_k_EAuthTokenPlatformType_SteamClient.Enum(),
		WebsiteId:          proto.String("Client"),
		DeviceDetails: &pb.CAuthentication_DeviceDetails{
			DeviceFriendlyName: proto.String(opts.DeviceName),
			PlatformType:       pb.EAuthTokenPlatformType_k_EAuthTokenPlatformType_SteamClient.Enum(),
			OsType:             proto.Int32(osType()),
		},
	}, &begin)
	if err != nil {
		return nil, fmt.Errorf("beginning qr auth session: %w", err)
	}

	clientID := begin.GetClientId()
	opts.OnChallenge(begin.GetChallengeUrl())

	interval := time.Duration(begin.GetInterval() * float32(time.Second))
	if interval <= 0 {
		interval = 5 * time.Second
	}

	for {
		var poll pb.CAuthentication_PollAuthSessionStatus_Response
		err = api.Call(ctx, true, authService, "PollAuthSessionStatus", 1, &pb.CAuthentication_PollAuthSessionStatus_Request{
			ClientId:  proto.Uint64(clientID),
			RequestId: begin.GetRequestId(),
		}, &poll)
		if err != nil {
			return nil, fmt.Errorf("polling qr auth session: %w", err)
		}
		if poll.GetRefreshToken() != "" {
			return &Credentials{
				AccountName:  poll.GetAccountName(),
				RefreshToken: poll.GetRefreshToken(),
				AccessToken:  poll.GetAccessToken(),
			}, nil
		}
		if poll.GetNewClientId() != 0 {
			clientID = poll.GetNewClientId()
		}
		if u := poll.GetNewChallengeUrl(); u != "" {
			opts.OnChallenge(u)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

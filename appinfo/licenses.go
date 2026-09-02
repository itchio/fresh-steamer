package appinfo

import (
	"context"
	"fmt"
	"sort"

	"github.com/itchio/fresh-steamer/cm"
	"github.com/itchio/fresh-steamer/pb"
	"github.com/itchio/fresh-steamer/vdf"
	"google.golang.org/protobuf/proto"
)

type License struct {
	PackageID   uint32
	AccessToken uint64
	OwnerID     uint32
	LicenseType uint32
	Flags       uint32
	TimeCreated uint32
	Country     string
}

// Licenses returns the packages the account owns. Steam pushes this right
// after logon, so the call usually returns immediately.
func Licenses(ctx context.Context, c *cm.Client) ([]License, error) {
	pkt, err := c.WaitEMsg(ctx, cm.EMsgClientLicenseList)
	if err != nil {
		return nil, fmt.Errorf("waiting for license list: %w", err)
	}
	var res pb.CMsgClientLicenseList
	if err := pkt.Unmarshal(&res); err != nil {
		return nil, err
	}
	var out []License
	for _, l := range res.GetLicenses() {
		out = append(out, License{
			PackageID:   l.GetPackageId(),
			AccessToken: l.GetAccessToken(),
			OwnerID:     l.GetOwnerId(),
			LicenseType: l.GetLicenseType(),
			Flags:       l.GetFlags(),
			TimeCreated: uint32(l.GetTimeCreated()),
			Country:     l.GetPurchaseCountryCode(),
		})
	}
	return out, nil
}

type Package struct {
	ID          uint32
	BillingType uint32
	// DeveloperOnly is set for packages the partner site grants to the
	// publisher's own accounts.
	DeveloperOnly bool
	AppIDs        []uint32
	DepotIDs      []uint32
	Raw           *vdf.Node
}

// BillingTypeName names an EBillingType value.
func BillingTypeName(t uint32) string {
	names := []string{"no cost", "bill once", "monthly", "prepurchase", "guest pass", "hardware promo", "gift", "auto grant", "oem ticket", "recurring option", "bill once or cd key", "repurchaseable", "free on demand", "rental", "commercial license", "free commercial license"}
	if int(t) < len(names) {
		return names[t]
	}
	return fmt.Sprintf("billing type %d", t)
}

// Packages fetches product info for the given licenses' packages.
func Packages(ctx context.Context, c *cm.Client, licenses []License) ([]*Package, error) {
	if len(licenses) == 0 {
		return nil, nil
	}
	req := &pb.CMsgClientPICSProductInfoRequest{}
	for _, l := range licenses {
		req.Packages = append(req.Packages, &pb.CMsgClientPICSProductInfoRequest_PackageInfo{
			Packageid:   proto.Uint32(l.PackageID),
			AccessToken: proto.Uint64(l.AccessToken),
		})
	}
	var out []*Package
	err := c.RequestMulti(ctx, emsgPICSProductInfoRequest, req, func(p *cm.Packet) (bool, error) {
		var res pb.CMsgClientPICSProductInfoResponse
		if err := p.Unmarshal(&res); err != nil {
			return true, err
		}
		for _, info := range res.GetPackages() {
			pkg, err := parsePackage(info)
			if err != nil {
				return true, err
			}
			out = append(out, pkg)
		}
		return !res.GetResponsePending(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("requesting package info: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Package buffers start with the package id, then binary KeyValues.
func parsePackage(info *pb.CMsgClientPICSProductInfoResponse_PackageInfo) (*Package, error) {
	buf := info.GetBuffer()
	if len(buf) < 4 {
		return nil, fmt.Errorf("package %d: empty product info", info.GetPackageid())
	}
	root, err := vdf.ParseBinary(buf[4:])
	if err != nil {
		return nil, fmt.Errorf("package %d: %w", info.GetPackageid(), err)
	}
	var kv *vdf.Node
	if len(root.Children) > 0 {
		kv = root.Children[0]
	}
	ext := kv.Get("extended")
	pkg := &Package{
		ID:            info.GetPackageid(),
		BillingType:   kv.Get("billingtype").Uint32(),
		DeveloperOnly: ext.Get("developeronly").Bool() || ext.Get("devcomp").Bool(),
		Raw:           kv,
	}
	if apps := kv.Get("appids"); apps != nil {
		for _, a := range apps.Children {
			pkg.AppIDs = append(pkg.AppIDs, a.Uint32())
		}
	}
	if depots := kv.Get("depotids"); depots != nil {
		for _, d := range depots.Children {
			pkg.DepotIDs = append(pkg.DepotIDs, d.Uint32())
		}
	}
	return pkg, nil
}

// Names fetches display names for many apps at once. Apps the account cannot
// see are left out of the result.
func Names(ctx context.Context, c *cm.Client, appIDs []uint32) (map[uint32]string, error) {
	names := map[uint32]string{}
	if len(appIDs) == 0 {
		return names, nil
	}
	tokPkt, err := c.Request(ctx, emsgPICSAccessTokenRequest, &pb.CMsgClientPICSAccessTokenRequest{Appids: appIDs})
	if err != nil {
		return nil, fmt.Errorf("requesting PICS tokens: %w", err)
	}
	var tokRes pb.CMsgClientPICSAccessTokenResponse
	if err := tokPkt.Unmarshal(&tokRes); err != nil {
		return nil, err
	}
	tokens := map[uint32]uint64{}
	for _, t := range tokRes.GetAppAccessTokens() {
		tokens[t.GetAppid()] = t.GetAccessToken()
	}

	req := &pb.CMsgClientPICSProductInfoRequest{}
	for _, id := range appIDs {
		req.Apps = append(req.Apps, &pb.CMsgClientPICSProductInfoRequest_AppInfo{
			Appid:       proto.Uint32(id),
			AccessToken: proto.Uint64(tokens[id]),
		})
	}
	err = c.RequestMulti(ctx, emsgPICSProductInfoRequest, req, func(p *cm.Packet) (bool, error) {
		var res pb.CMsgClientPICSProductInfoResponse
		if err := p.Unmarshal(&res); err != nil {
			return true, err
		}
		for _, a := range res.GetApps() {
			if len(a.GetBuffer()) == 0 {
				continue
			}
			root, err := vdf.Parse(a.GetBuffer())
			if err != nil {
				continue
			}
			names[a.GetAppid()] = root.Path("appinfo", "common", "name").String()
		}
		return !res.GetResponsePending(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("requesting app names: %w", err)
	}
	return names, nil
}

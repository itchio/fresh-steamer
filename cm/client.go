// Package cm speaks to a Steam connection manager over a websocket. It
// covers logon with a refresh token, job-correlated request/response
// messages, and unified service calls.
package cm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/itchio/fresh-steamer/pb"
	"google.golang.org/protobuf/proto"
)

const (
	protoMask       = 0x80000000
	protocolVersion = 65580
	eResultOK       = 1

	// SteamID with universe public, type individual, desktop instance and
	// no account id yet, which is what a fresh logon carries in its header.
	logonSteamID = uint64(1)<<56 | uint64(1)<<52 | uint64(1)<<32
)

// EMsg values used here, from enums_clientserver.proto.
const (
	EMsgMulti                       = 1
	EMsgServiceMethodResponse       = 147
	EMsgServiceMethodCallFromClient = 151
	EMsgClientHeartBeat             = 703
	EMsgClientLogOff                = 706
	EMsgClientLogOnResponse         = 751
	EMsgClientLoggedOff             = 757
	EMsgClientLogon                 = 5514
)

// EResultError is a non-OK EResult in a response header or body.
type EResultError struct {
	EResult int32
	Message string
	Context string
}

func (e *EResultError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: eresult %d: %s", e.Context, e.EResult, e.Message)
	}
	return fmt.Sprintf("%s: eresult %d", e.Context, e.EResult)
}

// Packet is one protobuf-framed message from the CM.
type Packet struct {
	EMsg   uint32
	Header *pb.CMsgProtoBufHeader
	Body   []byte
}

func (p *Packet) Unmarshal(out proto.Message) error {
	return proto.Unmarshal(p.Body, out)
}

type Client struct {
	conn      *websocket.Conn
	steamID   atomic.Uint64
	sessionID atomic.Int32
	jobID     atomic.Uint64

	mu      sync.Mutex
	jobs    map[uint64]chan *Packet
	byEMsg  map[uint32]chan *Packet
	closed  chan struct{}
	closeMu sync.Once
	err     error

	CellID uint32
	Logf   func(format string, args ...any)
}

type Options struct {
	// Endpoint is host:port of a websocket CM. Empty picks one from the
	// directory service.
	Endpoint string
	HTTP     *http.Client
	Logf     func(format string, args ...any)
}

func Connect(ctx context.Context, opts Options) (*Client, error) {
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		list, err := fetchCMList(ctx, opts.HTTP)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, errors.New("steam directory returned no websocket CMs")
		}
		endpoint = list[0]
	}

	conn, _, err := websocket.Dial(ctx, "wss://"+endpoint+"/cmsocket/", &websocket.DialOptions{HTTPClient: opts.HTTP})
	if err != nil {
		return nil, fmt.Errorf("dialing CM %s: %w", endpoint, err)
	}
	conn.SetReadLimit(64 << 20)

	c := &Client{
		conn:   conn,
		jobs:   map[uint64]chan *Packet{},
		byEMsg: map[uint32]chan *Packet{},
		closed: make(chan struct{}),
		Logf:   opts.Logf,
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	c.jobID.Store(uint64(time.Now().Unix()) << 20)
	go c.readLoop()
	return c, nil
}

func fetchCMList(ctx context.Context, hc *http.Client) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.steampowered.com/ISteamDirectory/GetCMListForConnect/v1/?cellid=0&cmtype=websockets&format=json", nil)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching CM list: %w", err)
	}
	defer res.Body.Close()
	var out struct {
		Response struct {
			ServerList []struct {
				Endpoint string `json:"endpoint"`
				Type     string `json:"type"`
				Realm    string `json:"realm"`
			} `json:"serverlist"`
		} `json:"response"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding CM list: %w", err)
	}
	var list []string
	for _, s := range out.Response.ServerList {
		if s.Type == "websockets" && (s.Realm == "" || s.Realm == "steamglobal") {
			list = append(list, s.Endpoint)
		}
	}
	return list, nil
}

func (c *Client) Close() error {
	c.fail(errors.New("client closed"))
	return nil
}

func (c *Client) fail(err error) {
	c.closeMu.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.closed)
		c.conn.Close(websocket.StatusNormalClosure, "")
	})
}

// Done is closed once the connection is gone; Err says why.
func (c *Client) Done() <-chan struct{} { return c.closed }

func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) SteamID() uint64 { return c.steamID.Load() }

// Logon authenticates with a refresh token obtained from package auth.
func (c *Client) Logon(ctx context.Context, accountName, refreshToken string) error {
	ch := c.waitEMsg(EMsgClientLogOnResponse)
	defer c.unwaitEMsg(EMsgClientLogOnResponse)

	c.steamID.Store(logonSteamID)
	body := &pb.CMsgClientLogon{
		ProtocolVersion:           proto.Uint32(protocolVersion),
		ClientOsType:              proto.Uint32(uint32(osType())),
		ClientLanguage:            proto.String("english"),
		CellId:                    proto.Uint32(0),
		ShouldRememberPassword:    proto.Bool(false),
		SupportsRateLimitResponse: proto.Bool(true),
		ChatMode:                  proto.Uint32(2),
		AccountName:               proto.String(accountName),
		AccessToken:               proto.String(refreshToken),
	}
	if err := c.send(EMsgClientLogon, nil, body); err != nil {
		return err
	}

	pkt, err := c.recv(ctx, ch)
	if err != nil {
		return fmt.Errorf("waiting for logon response: %w", err)
	}
	var res pb.CMsgClientLogonResponse
	if err := pkt.Unmarshal(&res); err != nil {
		return err
	}
	if res.GetEresult() != eResultOK {
		err := &EResultError{EResult: res.GetEresult(), Context: "steam logon"}
		c.fail(err)
		return err
	}
	c.steamID.Store(pkt.Header.GetSteamid())
	c.sessionID.Store(pkt.Header.GetClientSessionid())
	c.CellID = res.GetCellId()

	hb := time.Duration(res.GetHeartbeatSeconds()) * time.Second
	if hb <= 0 {
		hb = 9 * time.Second
	}
	go c.heartbeat(hb)
	return nil
}

func (c *Client) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			if err := c.send(EMsgClientHeartBeat, nil, &pb.CMsgClientHeartBeat{}); err != nil {
				c.fail(fmt.Errorf("heartbeat: %w", err))
				return
			}
		}
	}
}

// Request sends body with a fresh job id and returns the first packet
// addressed back to that job.
func (c *Client) Request(ctx context.Context, emsg uint32, body proto.Message) (*Packet, error) {
	var got *Packet
	err := c.RequestMulti(ctx, emsg, body, func(p *Packet) (bool, error) {
		got = p
		return true, nil
	})
	return got, err
}

// RequestMulti is Request for calls whose reply spans several packets. The
// handler returns true once it has seen the last one.
func (c *Client) RequestMulti(ctx context.Context, emsg uint32, body proto.Message, handler func(*Packet) (bool, error)) error {
	id := c.jobID.Add(1)
	ch := make(chan *Packet, 8)
	c.mu.Lock()
	c.jobs[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.jobs, id)
		c.mu.Unlock()
	}()

	hdr := &pb.CMsgProtoBufHeader{JobidSource: proto.Uint64(id)}
	if err := c.send(emsg, hdr, body); err != nil {
		return err
	}
	for {
		pkt, err := c.recv(ctx, ch)
		if err != nil {
			return err
		}
		done, err := handler(pkt)
		if err != nil || done {
			return err
		}
	}
}

// Unified calls a Steam service method such as
// "ContentServerDirectory.GetManifestRequestCode#1".
func (c *Client) Unified(ctx context.Context, method string, in proto.Message, out proto.Message) error {
	id := c.jobID.Add(1)
	ch := make(chan *Packet, 1)
	c.mu.Lock()
	c.jobs[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.jobs, id)
		c.mu.Unlock()
	}()

	hdr := &pb.CMsgProtoBufHeader{
		JobidSource:   proto.Uint64(id),
		TargetJobName: proto.String(method),
	}
	if err := c.send(EMsgServiceMethodCallFromClient, hdr, in); err != nil {
		return err
	}
	pkt, err := c.recv(ctx, ch)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if pkt.Header.GetEresult() != eResultOK {
		return &EResultError{EResult: pkt.Header.GetEresult(), Message: pkt.Header.GetErrorMessage(), Context: method}
	}
	if out == nil {
		return nil
	}
	return pkt.Unmarshal(out)
}

func (c *Client) recv(ctx context.Context, ch <-chan *Packet) (*Packet, error) {
	select {
	case pkt := <-ch:
		return pkt, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, c.Err()
	}
}

func (c *Client) waitEMsg(emsg uint32) chan *Packet {
	ch := make(chan *Packet, 1)
	c.mu.Lock()
	c.byEMsg[emsg] = ch
	c.mu.Unlock()
	return ch
}

func (c *Client) unwaitEMsg(emsg uint32) {
	c.mu.Lock()
	delete(c.byEMsg, emsg)
	c.mu.Unlock()
}

func (c *Client) send(emsg uint32, hdr *pb.CMsgProtoBufHeader, body proto.Message) error {
	if hdr == nil {
		hdr = &pb.CMsgProtoBufHeader{}
	}
	hdr.Steamid = proto.Uint64(c.steamID.Load())
	hdr.ClientSessionid = proto.Int32(c.sessionID.Load())

	hdrRaw, err := proto.Marshal(hdr)
	if err != nil {
		return err
	}
	bodyRaw, err := proto.Marshal(body)
	if err != nil {
		return err
	}
	buf := make([]byte, 8, 8+len(hdrRaw)+len(bodyRaw))
	binary.LittleEndian.PutUint32(buf[0:], emsg|protoMask)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(hdrRaw)))
	buf = append(buf, hdrRaw...)
	buf = append(buf, bodyRaw...)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageBinary, buf); err != nil {
		c.fail(err)
		return err
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.fail(fmt.Errorf("CM connection lost: %w", err))
			return
		}
		c.handleRaw(data)
	}
}

func (c *Client) handleRaw(data []byte) {
	if len(data) < 4 {
		return
	}
	raw := binary.LittleEndian.Uint32(data[0:])
	if raw&protoMask == 0 {
		// Legacy non-protobuf messages; nothing we care about uses them.
		c.Logf("cm: ignoring non-protobuf emsg %d", raw)
		return
	}
	emsg := raw &^ protoMask
	if len(data) < 8 {
		return
	}
	hlen := binary.LittleEndian.Uint32(data[4:])
	if uint32(len(data)-8) < hlen {
		return
	}
	hdr := &pb.CMsgProtoBufHeader{}
	if err := proto.Unmarshal(data[8:8+hlen], hdr); err != nil {
		c.Logf("cm: bad header for emsg %d: %v", emsg, err)
		return
	}
	pkt := &Packet{EMsg: emsg, Header: hdr, Body: data[8+hlen:]}
	c.dispatch(pkt)
}

func (c *Client) dispatch(pkt *Packet) {
	switch pkt.EMsg {
	case EMsgMulti:
		c.handleMulti(pkt)
		return
	case EMsgClientLoggedOff:
		var off pb.CMsgClientLoggedOff
		_ = pkt.Unmarshal(&off)
		c.fail(&EResultError{EResult: off.GetEresult(), Context: "logged off by steam"})
		return
	}

	c.mu.Lock()
	if ch, ok := c.jobs[pkt.Header.GetJobidTarget()]; ok && pkt.Header.JobidTarget != nil {
		c.mu.Unlock()
		select {
		case ch <- pkt:
		default:
			c.Logf("cm: dropping packet for job %d, channel full", pkt.Header.GetJobidTarget())
		}
		return
	}
	ch, ok := c.byEMsg[pkt.EMsg]
	c.mu.Unlock()
	if ok {
		select {
		case ch <- pkt:
		default:
		}
		return
	}
	c.Logf("cm: unhandled emsg %d", pkt.EMsg)
}

func (c *Client) handleMulti(pkt *Packet) {
	var multi pb.CMsgMulti
	if err := pkt.Unmarshal(&multi); err != nil {
		return
	}
	body := multi.GetMessageBody()
	if multi.GetSizeUnzipped() > 0 {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			c.Logf("cm: bad gzip in multi: %v", err)
			return
		}
		body, err = io.ReadAll(zr)
		if err != nil {
			c.Logf("cm: bad gzip in multi: %v", err)
			return
		}
	}
	for len(body) >= 4 {
		n := binary.LittleEndian.Uint32(body)
		body = body[4:]
		if uint32(len(body)) < n {
			return
		}
		c.handleRaw(body[:n])
		body = body[n:]
	}
}

func osType() int32 {
	switch runtime.GOOS {
	case "windows":
		return 16
	case "darwin":
		return -102
	default:
		return -203
	}
}

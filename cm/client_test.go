package cm

import (
	"context"
	"testing"
	"time"

	"github.com/itchio/fresh-steamer/pb"
	"google.golang.org/protobuf/proto"
)

func testClient() *Client {
	return &Client{
		jobs:    map[uint64]*job{},
		byEMsg:  map[uint32]chan *Packet{},
		stash:   map[uint32]*Packet{},
		stashed: make(chan struct{}),
		closed:  make(chan struct{}),
		Logf:    func(string, ...any) {},
	}
}

func TestJobKeepsEveryPacketInOrder(t *testing.T) {
	c := testClient()
	id, j := c.newJob()
	defer c.endJob(id)

	const n = 100
	for i := 0; i < n; i++ {
		c.dispatch(&Packet{
			EMsg:   8903,
			Header: &pb.CMsgProtoBufHeader{JobidTarget: proto.Uint64(id)},
			Body:   []byte{byte(i)},
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < n; i++ {
		pkt, err := c.recv(ctx, j)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if pkt.Body[0] != byte(i) {
			t.Fatalf("packet %d out of order: got %d", i, pkt.Body[0])
		}
	}
}

func TestJobOverflowFailsInsteadOfHanging(t *testing.T) {
	c := testClient()
	id, j := c.newJob()
	defer c.endJob(id)
	for i := 0; i <= maxJobQueue; i++ {
		j.push(&Packet{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < maxJobQueue; i++ {
		if _, err := c.recv(ctx, j); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	if _, err := c.recv(ctx, j); err == nil {
		t.Fatal("expected overflow error")
	}
}

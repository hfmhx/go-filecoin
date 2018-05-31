package filnet

import (
	"context"
	"testing"
	"time"

	pstore "gx/ipfs/QmXauCuJzmzapetmC6W4TuDJLL1yFFrVzSHoWv8YdbmnxH/go-libp2p-peerstore"
	peer "gx/ipfs/QmZoWKhxUmZ2seW4BzX6fJkNR8hh9PsGModr7q171yq2SS/go-libp2p-peer"

	"github.com/stretchr/testify/assert"
)

func nopConnect(context.Context, pstore.PeerInfo) error   { return nil }
func panicConnect(context.Context, pstore.PeerInfo) error { panic("shouldn't be called") }
func nopPeers() []peer.ID                                 { return []peer.ID{} }
func panicPeers() []peer.ID                               { panic("shouldn't be called") }

func TestBootstrapperStartAndStop(t *testing.T) {
	assert := assert.New(t)
	fakeHost := &fakeHost{ConnectImpl: nopConnect}
	fakeDialer := &fakeDialer{PeersImpl: nopPeers}

	// Check that Start() causes Bootstrap() to be periodically called and
	// that canceling the context causes it to stop being called. Do this
	// by stubbing out Bootstrap to keep a count of the number of times it
	// is called and to cancel its context after several calls.
	b := NewBootstrapper([]pstore.PeerInfo{}, fakeHost, fakeDialer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callCount := 0
	b.Bootstrap = func([]peer.ID) {
		callCount++
		if callCount == 3 {
			cancel()
		}
	}

	b.Period = 10 * time.Millisecond
	b.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	assert.Equal(3, callCount)
}

func TestBootstrapperBootstrap(t *testing.T) {
	t.Run("Doesn't connect if already have enough peers", func(t *testing.T) {
		assert := assert.New(t)
		fakeHost := &fakeHost{ConnectImpl: panicConnect}
		fakeDialer := &fakeDialer{PeersImpl: panicPeers}

		b := NewBootstrapper([]pstore.PeerInfo{}, fakeHost, fakeDialer)
		b.MinPeerThreshold = 1                          // Need 1
		currentPeers := []peer.ID{requireRandPeerID(t)} // Have 1
		assert.NotPanics(func() { b.bootstrap(currentPeers) })
	})

	var connectCount int
	countingConnect := func(context.Context, pstore.PeerInfo) error {
		connectCount++
		return nil
	}

	t.Run("Connects if don't have enough peers", func(t *testing.T) {
		assert := assert.New(t)
		fakeHost := &fakeHost{ConnectImpl: countingConnect}
		connectCount = 0
		fakeDialer := &fakeDialer{PeersImpl: panicPeers}

		bootstrapPeers := []pstore.PeerInfo{
			{ID: requireRandPeerID(t)},
			{ID: requireRandPeerID(t)},
		}
		b := NewBootstrapper(bootstrapPeers, fakeHost, fakeDialer)
		b.ctx = context.Background()
		b.MinPeerThreshold = 3                          // Need 3
		currentPeers := []peer.ID{requireRandPeerID(t)} // Have 1
		b.bootstrap(currentPeers)
		time.Sleep(20 * time.Millisecond)
		assert.Equal(2, connectCount)
	})

	t.Run("Doesn't try to connect to an already connected peer", func(t *testing.T) {
		assert := assert.New(t)
		fakeHost := &fakeHost{ConnectImpl: countingConnect}
		connectCount = 0
		fakeDialer := &fakeDialer{PeersImpl: panicPeers}

		connectedPeerID := requireRandPeerID(t)
		bootstrapPeers := []pstore.PeerInfo{
			{ID: connectedPeerID},
		}

		b := NewBootstrapper(bootstrapPeers, fakeHost, fakeDialer)
		b.ctx = context.Background()
		b.MinPeerThreshold = 2                     // Need 2
		currentPeers := []peer.ID{connectedPeerID} // Have 1, which is the bootstrap peer.
		b.bootstrap(currentPeers)
		time.Sleep(20 * time.Millisecond)
		assert.Equal(0, connectCount)
	})
}
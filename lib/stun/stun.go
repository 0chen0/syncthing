// Copyright (C) 2019 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package stun

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/AudriusButkevicius/pfilter"
	"github.com/ccding/go-stun/stun"
	"github.com/syncthing/syncthing/lib/config"
)

const stunRetryInterval = 5 * time.Minute

type Host = stun.Host
type NATType = stun.NATType

// NAT types.

const (
	NATError                = stun.NATError
	NATUnknown              = stun.NATUnknown
	NATNone                 = stun.NATNone
	NATBlocked              = stun.NATBlocked
	NATFull                 = stun.NATFull
	NATSymmetric            = stun.NATSymmetric
	NATRestricted           = stun.NATRestricted
	NATPortRestricted       = stun.NATPortRestricted
	NATSymmetricUDPFirewall = stun.NATSymmetricUDPFirewall
)

type writeTrackingPacketConn struct {
	lastWrite int64
	net.PacketConn
}

func (c *writeTrackingPacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	atomic.StoreInt64(&c.lastWrite, time.Now().Unix())
	return c.PacketConn.WriteTo(p, addr)
}

func (c *writeTrackingPacketConn) getLastWrite() time.Time {
	unix := atomic.LoadInt64(&c.lastWrite)
	return time.Unix(unix, 0)
}

type Subscriber interface {
	OnNATTypeChanged(natType NATType)
	OnExternalAddressChanged(address *Host, via string)
}

type Service struct {
	name       string
	cfg        config.Wrapper
	subscriber Subscriber
	stunConn   net.PacketConn
	client     *stun.Client

	writeTrackingPacketConn *writeTrackingPacketConn

	natType NATType
	addr    *Host

	stop chan struct{}
}

func New(cfg config.Wrapper, subscriber Subscriber, conn net.PacketConn) (*Service, net.PacketConn) {
	// Wrap the original connection to track writes on it
	writeTrackingPacketConn := &writeTrackingPacketConn{lastWrite: 0, PacketConn: conn}

	// Wrap it in a filter and split it up, so that stun packets arrive on stun conn, others arrive on the data conn
	filterConn := pfilter.NewPacketFilter(writeTrackingPacketConn)
	otherDataConn := filterConn.NewConn(otherDataPriority, nil)
	stunConn := filterConn.NewConn(stunFilterPriority, &stunFilter{
		ids: make(map[string]time.Time),
	})

	filterConn.Start()

	// Construct the client to use the stun conn
	client := stun.NewClientWithConnection(stunConn)
	client.SetSoftwareName("") // Explicitly unset this, seems to freak some servers out.

	// Return the service and the other conn to the client
	return &Service{
		name: "Stun@" + conn.LocalAddr().Network() + "://" + conn.LocalAddr().String(),

		cfg:        cfg,
		subscriber: subscriber,
		stunConn:   stunConn,
		client:     client,

		writeTrackingPacketConn: writeTrackingPacketConn,

		natType: NATUnknown,
		addr:    nil,
		stop:    make(chan struct{}),
	}, otherDataConn
}

func (s *Service) Stop() {
	close(s.stop)
	_ = s.stunConn.Close()
}

func (s *Service) Serve() {
	for {
	disabled:
		s.subscriber.OnNATTypeChanged(NATUnknown)
		s.subscriber.OnExternalAddressChanged(nil, "")

		if s.cfg.Options().IsStunDisabled() {
			select {
			case <-s.stop:
				return
			case <-time.After(time.Second):
				continue
			}
		}

		l.Debugf("Starting stun for %s", s)

		for _, addr := range s.cfg.StunServers() {
			// This blocks until we hit an exit condition or there are issues with the STUN server.
			// This returns a boolean signifying if a different STUN server should be tried (oppose to the whole thing
			// shutting down and this winding itself down.
			if !s.runStunForServer(addr) {
				// Check exit conditions.

				// Have we been asked to stop?
				select {
				case <-s.stop:
					return
				default:
				}

				// Are we disabled?
				if s.cfg.Options().IsStunDisabled() {
					l.Infoln("STUN disabled")
					goto disabled
				}

				// Unpunchable NAT? Chillout for some time.
				if !s.isCurrentNATTypePunchable() {
					break
				}
			}
		}

		// Failed all servers, sad.
		s.subscriber.OnNATTypeChanged(NATUnknown)
		s.subscriber.OnExternalAddressChanged(nil, "")

		// We failed to contact all provided stun servers or the nat is not punchable.
		// Chillout for a while.
		time.Sleep(stunRetryInterval)
	}
}

func (s *Service) runStunForServer(addr string) (tryNext bool) {
	l.Debugf("Running stun for %s via %s", s, addr)

	// Resolve the address, so that in case the server advertises two
	// IPs, we always hit the same one, as otherwise, the mapping might
	// expire as we hit the other address, and cause us to flip flop
	// between servers/external addresses, as a result flooding discovery
	// servers.
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		l.Debugf("%s stun addr resolution on %s: %s", s, addr, err)
		return true
	}
	s.client.SetServerAddr(udpAddr.String())

	natType, extAddr, err := s.client.Discover()
	if err != nil || extAddr == nil {
		l.Debugf("%s stun discovery on %s: %s", s, addr, err)
		return true
	}

	// The stun server is most likely borked, try another one.
	if natType == NATError || natType == NATUnknown || natType == NATBlocked {
		l.Debugf("%s stun discovery on %s resolved to %s", s, addr, natType)
		return true
	}

	s.setNATType(natType)
	l.Debugf("%s detected NAT type: %s via %s", s, natType, addr)

	// We can't punch through this one, so no point doing keepalives
	// and such, just let the caller check the nat type and work it out themselves.
	if !s.isCurrentNATTypePunchable() {
		l.Debugf("%s cannot punch %s, skipping", s, natType)
		return false
	}

	return s.stunKeepAlive(addr, extAddr)
}

func (s *Service) stunKeepAlive(addr string, extAddr *Host) (tryNext bool) {
	var err error
	nextSleep := time.Duration(s.cfg.Options().StunKeepaliveStartS) * time.Second

	l.Debugf("%s starting stun keepalive via %s, next sleep %s", s, addr, nextSleep)

	for {
		if s.addr == nil || s.addr != extAddr {
			s.subscriber.OnExternalAddressChanged(extAddr, addr)
			// If the port has changed (addresses are not equal but the hosts are equal),
			// we're probably spending too much time between keepalives, reduce the sleep.
			if s.addr != nil && s.addr.IP() == extAddr.IP() {
				nextSleep /= 2
				l.Debugf("%s stun port change, next sleep %s", s, nextSleep)
			}
			s.addr = extAddr

			// The stun server is probably stuffed, we've gone beyond min timeout, yet the address keeps changing.
			minSleep := time.Duration(s.cfg.Options().StunKeepaliveMinS) * time.Second
			if nextSleep < minSleep {
				l.Debugf("%s keepalive aborting, sleep below min: %s < %s", s, nextSleep, minSleep)
				return true
			}
		}

		// Adjust the keepalives to fire only nextSleep after last write.
		lastWrite := s.writeTrackingPacketConn.getLastWrite()
		minSleep := time.Duration(s.cfg.Options().StunKeepaliveMinS) * time.Second
		if nextSleep < minSleep {
			nextSleep = minSleep
		}
	tryLater:
		sleepFor := nextSleep
		now := time.Now()

		nextKeepalive := lastWrite.Add(sleepFor)
		if nextKeepalive.After(now) {
			sleepFor = nextKeepalive.Sub(time.Now())
		}

		l.Debugf("%s stun sleeping for %s", s, sleepFor)

		select {
		case <-time.After(sleepFor):
		case <-s.stop:
			l.Debugf("%s stopping, aborting stun", s)
			return false
		}

		if s.cfg.Options().IsStunDisabled() {
			// Disabled, give up
			l.Debugf("%s disabled, aborting stun ", s)
			return false
		}

		// Check if any writes happened while we were sleeping, if they did, sleep again
		lastWrite = s.writeTrackingPacketConn.getLastWrite()
		if gap := time.Now().Sub(lastWrite); gap < nextSleep {
			l.Debugf("%s stun last write gap less than next sleep: %s < %s. Will try later", s, gap, nextSleep)
			goto tryLater
		}

		l.Debugf("%s stun keepalive", s)

		extAddr, err = s.client.Keepalive()
		if err != nil {
			l.Debugf("%s stun keepalive on %s: %s (%v)", s, addr, err, extAddr)
			return true
		}
	}
}

func (s *Service) setNATType(natType NATType) {
	if natType != s.natType {
		s.subscriber.OnNATTypeChanged(natType)
	}
	s.natType = natType
}

func (s *Service) setExternalAddress(addr *Host, via string) {
	if addr != s.addr {
		s.subscriber.OnExternalAddressChanged(addr, via)
	}
	s.addr = addr
}

func (s *Service) String() string {
	return s.name
}

func (s *Service) isCurrentNATTypePunchable() bool {
	return s.natType == NATNone || s.natType == NATPortRestricted || s.natType == NATRestricted || s.natType == NATFull
}

// Copyright © 2016 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package udp

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/TheThingsNetwork/ttn/core"
	"github.com/TheThingsNetwork/ttn/semtech"
	"github.com/TheThingsNetwork/ttn/utils/errors"
	"github.com/TheThingsNetwork/ttn/utils/stats"
	"github.com/apex/log"
	"golang.org/x/net/context"
)

type adapter struct {
	Components
}

// Components defines a structure to make the instantiation easier to read
type Components struct {
	Ctx    log.Interface
	Router core.RouterServer
}

// Options defines a structure to make the instantiation easier to read
type Options struct {
	// NetAddr refers to the udp address + port the adapter will have to listen
	NetAddr string
	// MaxReconnectionDelay defines the delay of the longest attempt to reconnect a lost connection
	// before giving up.
	MaxReconnectionDelay time.Duration
}

// replier is an alias used by methods herebelow
type replier func(data []byte) error

// Start constructs and launches a new udp adapter
func Start(c Components, o Options) (err error) {
	// Create the udp connection and start listening with a goroutine
	var udpConn *net.UDPConn
	if udpConn, err = tryConnect(o.NetAddr); err != nil {
		c.Ctx.WithError(err).Error("Unable to start server")
		return errors.New(errors.Operational, fmt.Sprintf("Invalid bind address %v", o.NetAddr))
	}

	c.Ctx.WithField("bind", o.NetAddr).Info("Starting Server")
	a := adapter{Components: c}
	go a.listen(o.NetAddr, udpConn, o.MaxReconnectionDelay)
	return nil
}

// makeReply curryfies a writing to udp connection by binding the address and connection
func makeReply(addr *net.UDPAddr, conn *net.UDPConn) replier {
	return func(data []byte) error {
		_, err := conn.WriteToUDP(data, addr)
		return err
	}
}

// tryConnect attempt to connect to a udp connection
func tryConnect(netAddr string) (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp", netAddr)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp", addr)
}

// listen Handle incoming packets and forward them.
//
// Runs in its own goroutine.
func (a adapter) listen(netAddr string, conn *net.UDPConn, maxReconnectionDelay time.Duration) {
	var err error
	a.Ctx.WithField("address", conn.LocalAddr()).Debug("Starting accept loop")
	for {
		// Read on the UDP connection
		buf := make([]byte, 5000)
		n, addr, fault := conn.ReadFromUDP(buf)
		if fault != nil { // Problem with the connection
			for delay := time.Millisecond * 25; delay < maxReconnectionDelay; delay *= 10 {
				a.Ctx.Infof("UDP connection lost. Trying to reconnect in %s", delay)
				<-time.After(delay)
				conn, err = tryConnect(netAddr)
				if err == nil {
					a.Ctx.Info("UDP connection recovered")
					break
				}
			}
			a.Ctx.WithError(fault).Error("Unable to restore UDP connection")
			break
		}

		// Handle the incoming datagram
		a.Ctx.Debug("Incoming datagram")
		go func(data []byte, reply replier) {
			pkt := new(semtech.Packet)
			if err := pkt.UnmarshalBinary(data); err != nil {
				a.Ctx.WithError(err).Debug("Unable to handle datagram")
			}

			switch pkt.Identifier {
			case semtech.PULL_DATA:
				err = a.handlePullData(*pkt, reply)
			case semtech.PUSH_DATA:
				err = a.handlePushData(*pkt, reply)
			default:
				err = errors.New(errors.Implementation, "Unhandled packet type")
			}
			if err != nil {
				a.Ctx.WithError(err).Debug("Unable to handle datagram")
			}
		}(buf[:n], makeReply(addr, conn))
	}

	if conn != nil {
		_ = conn.Close()
	}
}

// Handle a PULL_DATA packet coming from a gateway
func (a adapter) handlePullData(pkt semtech.Packet, reply replier) error {
	stats.MarkMeter("semtech_adapter.pull_data")
	stats.MarkMeter(fmt.Sprintf("semtech_adapter.gateways.%X.pull_data", pkt.GatewayId))
	stats.SetString(fmt.Sprintf("semtech_adapter.gateways.%X.last_pull_data", pkt.GatewayId), "date", time.Now().UTC().Format(time.RFC3339))

	data, err := semtech.Packet{
		Version:    semtech.VERSION,
		Token:      pkt.Token,
		Identifier: semtech.PULL_ACK,
	}.MarshalBinary()

	if err != nil {
		return errors.New(errors.Structural, err)
	}

	return reply(data)
}

// Handle a PUSH_DATA packet coming from a gateway
func (a adapter) handlePushData(pkt semtech.Packet, reply replier) error {
	stats.MarkMeter("semtech_adapter.push_data")
	stats.MarkMeter(fmt.Sprintf("semtech_adapter.gateways.%X.push_data", pkt.GatewayId))
	stats.SetString(fmt.Sprintf("semtech_adapter.gateways.%X.last_push_data", pkt.GatewayId), "date", time.Now().UTC().Format(time.RFC3339))

	// AckNowledge with a PUSH_ACK
	data, err := semtech.Packet{
		Version:    semtech.VERSION,
		Token:      pkt.Token,
		Identifier: semtech.PUSH_ACK,
	}.MarshalBinary()
	if err != nil || reply(data) != nil || pkt.Payload == nil {
		return errors.New(errors.Operational, "Unable to process PUSH_DATA packet")
	}

	// Process Stat payload
	if pkt.Payload.Stat != nil {
		go a.Router.HandleStats(context.Background(), &core.StatsReq{
			GatewayID: pkt.GatewayId,
			Metadata:  extractMetadata(*pkt.Payload.Stat, new(core.StatsMetadata)).(*core.StatsMetadata),
		})
	}

	// Process rxpks payloads
	cherr := make(chan error, len(pkt.Payload.RXPK))
	wait := sync.WaitGroup{}
	wait.Add(len(pkt.Payload.RXPK))
	for _, rxpk := range pkt.Payload.RXPK {
		go func(rxpk semtech.RXPK) {
			defer wait.Done()
			if err := a.handleDataUp(rxpk, pkt.GatewayId, reply); err != nil {
				cherr <- err
			}
		}(rxpk)
	}

	// Retrieve any errors
	wait.Wait()
	close(cherr)
	return <-cherr
}

func (a adapter) handleDataUp(rxpk semtech.RXPK, gid []byte, reply replier) error {
	dataRouterReq, err := newDataRouterReq(rxpk, gid, a.Ctx)
	if err != nil {
		return errors.New(errors.Structural, err)
	}
	resp, err := a.Router.HandleData(context.Background(), dataRouterReq)
	if err != nil {
		errors.New(errors.Operational, err)
	}
	return a.handleDataDown(resp, reply)
}

func (a adapter) handleDataDown(resp *core.DataRouterRes, reply replier) error {
	if resp == nil { // No response
		return nil
	}

	txpk, err := newTXPK(*resp, a.Ctx)
	if err != nil {
		return errors.New(errors.Structural, err)
	}

	data, err := semtech.Packet{
		Version:    semtech.VERSION,
		Identifier: semtech.PULL_RESP,
		Payload:    &semtech.Payload{TXPK: &txpk},
	}.MarshalBinary()
	if err != nil {
		return errors.New(errors.Structural, err)
	}
	return reply(data)
}

// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/stretchr/testify/require"
)

// As a regression we left this test very low-level and network-ey, even after
// we abstracted the transport. We added some basic network-free transport tests
// in transport_test.go to prove that we didn't hard code some network stuff
// outside of NetTransport.

func TestHandleCompoundPing(t *testing.T) {
	m := GetMemberlist(t, func(c *Config) {
		c.EnableCompression = false
	})
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	udp := listenUDP(t)
	defer func() {
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	udpAddr := udp.LocalAddr().(*net.UDPAddr)

	// Encode a ping
	ping := ping{
		SeqNo:      42,
		SourceAddr: udpAddr.IP,
		SourcePort: uint16(udpAddr.Port),
		SourceNode: "test",
	}
	buf, err := encode(pingMsg, ping, m.config.MsgpackUseNewTimeFormat)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Make a compound message
	compound := makeCompoundMessage([][]byte{buf.Bytes(), buf.Bytes(), buf.Bytes()})

	// Send compound version
	addr := &net.UDPAddr{IP: net.ParseIP(m.config.BindAddr), Port: m.config.BindPort}
	_, err = udp.WriteTo(compound.Bytes(), addr)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Wait for responses
	doneCh := make(chan struct{}, 1)
	go func() {
		select {
		case <-doneCh:
		case <-time.After(2 * time.Second):
			panic("timeout")
		}
	}()

	for i := 0; i < 3; i++ {
		in := make([]byte, 1500)
		n, _, err := udp.ReadFrom(in)
		if err != nil {
			t.Fatalf("unexpected err %s", err)
		}
		in = in[0:n]

		msgType := messageType(in[0])
		if msgType != ackRespMsg {
			t.Fatalf("bad response %v", in)
		}

		var ack ackResp
		if err := decode(in[1:], &ack); err != nil {
			t.Fatalf("unexpected err %s", err)
		}

		if ack.SeqNo != 42 {
			t.Fatalf("bad sequence no")
		}
	}

	doneCh <- struct{}{}
}

func TestHandlePing(t *testing.T) {
	m := GetMemberlist(t, func(c *Config) {
		c.EnableCompression = false
	})
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	udp := listenUDP(t)
	defer func() {
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	udpAddr := udp.LocalAddr().(*net.UDPAddr)

	// Encode a ping
	ping := ping{
		SeqNo:      42,
		SourceAddr: udpAddr.IP,
		SourcePort: uint16(udpAddr.Port),
		SourceNode: "test",
	}
	buf, err := encode(pingMsg, ping, m.config.MsgpackUseNewTimeFormat)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Send
	addr := &net.UDPAddr{IP: net.ParseIP(m.config.BindAddr), Port: m.config.BindPort}
	_, err = udp.WriteTo(buf.Bytes(), addr)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Wait for response
	doneCh := make(chan struct{}, 1)
	go func() {
		select {
		case <-doneCh:
		case <-time.After(2 * time.Second):
			panic("timeout")
		}
	}()

	in := make([]byte, 1500)
	n, _, err := udp.ReadFrom(in)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	in = in[0:n]

	msgType := messageType(in[0])
	if msgType != ackRespMsg {
		t.Fatalf("bad response %v", in)
	}

	var ack ackResp
	if err := decode(in[1:], &ack); err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	if ack.SeqNo != 42 {
		t.Fatalf("bad sequence no")
	}

	doneCh <- struct{}{}
}

func TestHandlePing_WrongNode(t *testing.T) {
	m := GetMemberlist(t, func(c *Config) {
		c.EnableCompression = false
	})
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	udp := listenUDP(t)
	defer func() {
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	udpAddr := udp.LocalAddr().(*net.UDPAddr)

	// Encode a ping, wrong node!
	ping := ping{
		SeqNo:      42,
		Node:       m.config.Name + "-bad",
		SourceAddr: udpAddr.IP,
		SourcePort: uint16(udpAddr.Port),
		SourceNode: "test",
	}
	buf, err := encode(pingMsg, ping, m.config.MsgpackUseNewTimeFormat)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Send
	addr := &net.UDPAddr{IP: net.ParseIP(m.config.BindAddr), Port: m.config.BindPort}
	_, err = udp.WriteTo(buf.Bytes(), addr)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Wait for response
	_ = udp.SetDeadline(time.Now().Add(50 * time.Millisecond))
	in := make([]byte, 1500)
	_, _, err = udp.ReadFrom(in)

	// Should get an i/o timeout
	if err == nil {
		t.Fatalf("expected err %s", err)
	}
}

func TestHandleIndirectPing(t *testing.T) {
	m := GetMemberlist(t, func(c *Config) {
		c.EnableCompression = false
	})
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	udp := listenUDP(t)
	defer func() {
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	udpAddr := udp.LocalAddr().(*net.UDPAddr)

	// Encode an indirect ping
	ind := indirectPingReq{
		SeqNo:      100,
		Target:     net.ParseIP(m.config.BindAddr),
		Port:       uint16(m.config.BindPort),
		Node:       m.config.Name,
		SourceAddr: udpAddr.IP,
		SourcePort: uint16(udpAddr.Port),
		SourceNode: "test",
	}
	buf, err := encode(indirectPingMsg, &ind, m.config.MsgpackUseNewTimeFormat)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Send
	addr := &net.UDPAddr{IP: net.ParseIP(m.config.BindAddr), Port: m.config.BindPort}
	_, err = udp.WriteTo(buf.Bytes(), addr)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Wait for response
	doneCh := make(chan struct{}, 1)
	go func() {
		select {
		case <-doneCh:
		case <-time.After(2 * time.Second):
			panic("timeout")
		}
	}()

	in := make([]byte, 1500)
	n, _, err := udp.ReadFrom(in)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	in = in[0:n]

	msgType := messageType(in[0])
	if msgType != ackRespMsg {
		t.Fatalf("bad response %v", in)
	}

	var ack ackResp
	if err := decode(in[1:], &ack); err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	if ack.SeqNo != 100 {
		t.Fatalf("bad sequence no")
	}

	doneCh <- struct{}{}
}

func TestTCPPing(t *testing.T) {
	var tcp *net.TCPListener
	var tcpAddr *net.TCPAddr
	for port := 60000; port < 61000; port++ {
		tcpAddr = &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
		tcpLn, err := net.ListenTCP("tcp", tcpAddr)
		if err == nil {
			tcp = tcpLn
			break
		}
	}
	if tcp == nil {
		t.Fatalf("no tcp listener")
	}

	tcpAddr2 := Address{Addr: tcpAddr.String(), Name: "test"}

	// Note that tcp gets closed in the last test, so we avoid a deferred
	// Close() call here.

	m := GetMemberlist(t, nil)
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	pingTimeout := m.config.ProbeInterval
	pingTimeMax := m.config.ProbeInterval + 10*time.Millisecond

	// Do a normal round trip.
	pingOut := ping{SeqNo: 23, Node: "mongo"}
	pingErrCh := make(chan error, 1)
	go func() {
		_ = tcp.SetDeadline(time.Now().Add(pingTimeMax))
		conn, err := tcp.AcceptTCP()
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to connect: %s", err)
			return
		}
		defer func() {
			_ = conn.Close()
		}()

		msgType, _, dec, err := m.readStream(conn, "")
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to read ping: %s", err)
			return
		}

		if msgType != pingMsg {
			pingErrCh <- fmt.Errorf("expecting ping, got message type (%d)", msgType)
			return
		}

		var pingIn ping
		if err := dec.Decode(&pingIn); err != nil {
			pingErrCh <- fmt.Errorf("failed to decode ping: %s", err)
			return
		}

		if pingIn.SeqNo != pingOut.SeqNo {
			pingErrCh <- fmt.Errorf("sequence number isn't correct (%d) vs (%d)", pingIn.SeqNo, pingOut.SeqNo)
			return
		}

		if pingIn.Node != pingOut.Node {
			pingErrCh <- fmt.Errorf("node name isn't correct (%s) vs (%s)", pingIn.Node, pingOut.Node)
			return
		}

		ack := ackResp{pingIn.SeqNo, nil}
		out, err := encode(ackRespMsg, &ack, m.config.MsgpackUseNewTimeFormat)
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to encode ack: %s", err)
			return
		}

		err = m.rawSendMsgStream(conn, out.Bytes(), "")
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to send ack: %s", err)
			return
		}
		pingErrCh <- nil
	}()
	deadline := time.Now().Add(pingTimeout)
	didContact, err := m.sendPingAndWaitForAck(tcpAddr2, pingOut, deadline)
	if err != nil {
		t.Fatalf("error trying to ping: %s", err)
	}
	if !didContact {
		t.Fatalf("expected successful ping")
	}
	if err = <-pingErrCh; err != nil {
		t.Fatal(err)
	}

	// Make sure a mis-matched sequence number is caught.
	go func() {
		_ = tcp.SetDeadline(time.Now().Add(pingTimeMax))
		conn, err := tcp.AcceptTCP()
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to connect: %s", err)
			return
		}
		defer func() {
			_ = conn.Close()
		}()

		_, _, dec, err := m.readStream(conn, "")
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to read ping: %s", err)
			return
		}

		var pingIn ping
		if err := dec.Decode(&pingIn); err != nil {
			pingErrCh <- fmt.Errorf("failed to decode ping: %s", err)
			return
		}

		ack := ackResp{pingIn.SeqNo + 1, nil}
		out, err := encode(ackRespMsg, &ack, m.config.MsgpackUseNewTimeFormat)
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to encode ack: %s", err)
			return
		}

		err = m.rawSendMsgStream(conn, out.Bytes(), "")
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to send ack: %s", err)
			return
		}
		pingErrCh <- nil
	}()
	deadline = time.Now().Add(pingTimeout)
	didContact, err = m.sendPingAndWaitForAck(tcpAddr2, pingOut, deadline)
	if err == nil || !strings.Contains(err.Error(), "sequence number") {
		t.Fatalf("expected an error from mis-matched sequence number")
	}
	if didContact {
		t.Fatalf("expected failed ping")
	}
	if err = <-pingErrCh; err != nil {
		t.Fatal(err)
	}

	// Make sure an unexpected message type is handled gracefully.
	go func() {
		_ = tcp.SetDeadline(time.Now().Add(pingTimeMax))
		conn, err := tcp.AcceptTCP()
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to connect: %s", err)
			return
		}
		defer func() {
			_ = conn.Close()
		}()

		_, _, _, err = m.readStream(conn, "")
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to read ping: %s", err)
			return
		}

		bogus := indirectPingReq{}
		out, err := encode(indirectPingMsg, &bogus, m.config.MsgpackUseNewTimeFormat)
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to encode bogus msg: %s", err)
			return
		}

		err = m.rawSendMsgStream(conn, out.Bytes(), "")
		if err != nil {
			pingErrCh <- fmt.Errorf("failed to send bogus msg: %s", err)
			return
		}
		pingErrCh <- nil
	}()
	deadline = time.Now().Add(pingTimeout)
	didContact, err = m.sendPingAndWaitForAck(tcpAddr2, pingOut, deadline)
	if err == nil || !strings.Contains(err.Error(), "unexpected msgType") {
		t.Fatalf("expected an error from bogus message")
	}
	if didContact {
		t.Fatalf("expected failed ping")
	}
	if err = <-pingErrCh; err != nil {
		t.Fatal(err)
	}

	// Make sure failed I/O respects the deadline. In this case we try the
	// common case of the receiving node being totally down.
	_ = tcp.Close()
	deadline = time.Now().Add(pingTimeout)
	startPing := time.Now()
	didContact, err = m.sendPingAndWaitForAck(tcpAddr2, pingOut, deadline)
	pingTime := time.Since(startPing)
	if err != nil {
		t.Fatalf("expected no error during ping on closed socket, got: %s", err)
	}
	if didContact {
		t.Fatalf("expected failed ping")
	}
	if pingTime > pingTimeMax {
		t.Fatalf("took too long to fail ping, %9.6f", pingTime.Seconds())
	}
}

func TestTCPPushPull(t *testing.T) {
	m := GetMemberlist(t, nil)
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	m.nodes = append(m.nodes, &nodeState{
		Node: Node{
			Name: "Test 0",
			Addr: net.ParseIP(m.config.BindAddr),
			Port: uint16(m.config.BindPort),
		},
		Incarnation: 0,
		State:       StateSuspect,
		StateChange: time.Now().Add(-1 * time.Second),
	})

	addr := net.JoinHostPort(m.config.BindAddr, strconv.Itoa(m.config.BindPort))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	localNodes := make([]pushNodeState, 3)
	localNodes[0].Name = "Test 0"
	localNodes[0].Addr = net.ParseIP(m.config.BindAddr)
	localNodes[0].Port = uint16(m.config.BindPort)
	localNodes[0].Incarnation = 1
	localNodes[0].State = StateAlive
	localNodes[1].Name = "Test 1"
	localNodes[1].Addr = net.ParseIP(m.config.BindAddr)
	localNodes[1].Port = uint16(m.config.BindPort)
	localNodes[1].Incarnation = 1
	localNodes[1].State = StateAlive
	localNodes[2].Name = "Test 2"
	localNodes[2].Addr = net.ParseIP(m.config.BindAddr)
	localNodes[2].Port = uint16(m.config.BindPort)
	localNodes[2].Incarnation = 1
	localNodes[2].State = StateAlive

	// Send our node state
	header := pushPullHeader{Nodes: 3}
	hd := codec.MsgpackHandle{}
	hd.TimeNotBuiltin = !m.config.MsgpackUseNewTimeFormat

	enc := codec.NewEncoder(conn, &hd)

	// Send the push/pull indicator
	_, _ = conn.Write([]byte{byte(pushPullMsg)})

	if err := enc.Encode(&header); err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	for i := 0; i < header.Nodes; i++ {
		if err := enc.Encode(&localNodes[i]); err != nil {
			t.Fatalf("unexpected err %s", err)
		}
	}

	// Read the message type
	var msgType messageType
	if err := binary.Read(conn, binary.BigEndian, &msgType); err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	var bufConn io.Reader = conn
	msghd := codec.MsgpackHandle{}
	msghd.TimeNotBuiltin = !m.config.MsgpackUseNewTimeFormat

	dec := codec.NewDecoder(bufConn, &msghd)

	// Check if we have a compressed message
	if msgType == compressMsg {
		var c compress
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("unexpected err %s", err)
		}
		decomp, err := decompressBuffer(&c)
		if err != nil {
			t.Fatalf("unexpected err %s", err)
		}

		// Reset the message type
		msgType = messageType(decomp[0])

		// Create a new bufConn
		bufConn = bytes.NewReader(decomp[1:])

		// Create a new decoder
		dec = codec.NewDecoder(bufConn, &hd)
	}

	// Quit if not push/pull
	if msgType != pushPullMsg {
		t.Fatalf("bad message type")
	}

	if err := dec.Decode(&header); err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Allocate space for the transfer
	remoteNodes := make([]pushNodeState, header.Nodes)

	// Try to decode all the states
	for i := 0; i < header.Nodes; i++ {
		if err := dec.Decode(&remoteNodes[i]); err != nil {
			t.Fatalf("unexpected err %s", err)
		}
	}

	if len(remoteNodes) != 1 {
		t.Fatalf("bad response")
	}

	n := &remoteNodes[0]
	if n.Name != "Test 0" {
		t.Fatalf("bad name")
	}
	if !bytes.Equal(n.Addr, net.ParseIP(m.config.BindAddr)) {
		t.Fatal("bad addr")
	}
	if n.Incarnation != 0 {
		t.Fatal("bad incarnation")
	}
	if n.State != StateSuspect {
		t.Fatal("bad state")
	}
}

func TestSendMsg_Piggyback(t *testing.T) {
	m := GetMemberlist(t, nil)
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	// Add a message to be broadcast
	a := alive{
		Incarnation: 10,
		Node:        "rand",
		Addr:        []byte{127, 0, 0, 255},
		Meta:        nil,
		Vsn: []uint8{
			ProtocolVersionMin, ProtocolVersionMax, ProtocolVersionMin,
			1, 1, 1,
		},
	}
	m.encodeAndBroadcast("rand", aliveMsg, &a)

	udp := listenUDP(t)
	defer func() {
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	udpAddr := udp.LocalAddr().(*net.UDPAddr)

	// Encode a ping
	ping := ping{
		SeqNo:      42,
		SourceAddr: udpAddr.IP,
		SourcePort: uint16(udpAddr.Port),
		SourceNode: "test",
	}
	buf, err := encode(pingMsg, ping, m.config.MsgpackUseNewTimeFormat)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Send
	addr := &net.UDPAddr{IP: net.ParseIP(m.config.BindAddr), Port: m.config.BindPort}
	_, err = udp.WriteTo(buf.Bytes(), addr)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	// Wait for response
	doneCh := make(chan struct{}, 1)
	go func() {
		select {
		case <-doneCh:
		case <-time.After(2 * time.Second):
			panic("timeout")
		}
	}()

	in := make([]byte, 1500)
	n, _, err := udp.ReadFrom(in)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	in = in[0:n]

	msgType := messageType(in[0])
	if msgType != compoundMsg {
		t.Fatalf("bad response %v", in)
	}

	// get the parts
	trunc, parts, err := decodeCompoundMessage(in[1:])
	if trunc != 0 {
		t.Fatalf("unexpected truncation")
	}
	if len(parts) != 2 {
		t.Fatalf("unexpected parts %v", parts)
	}
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	var ack ackResp
	if err := decode(parts[0][1:], &ack); err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	if ack.SeqNo != 42 {
		t.Fatalf("bad sequence no")
	}

	var aliveout alive
	if err := decode(parts[1][1:], &aliveout); err != nil {
		t.Fatalf("unexpected err %s", err)
	}

	if aliveout.Node != "rand" || aliveout.Incarnation != 10 {
		t.Fatalf("bad mesg")
	}

	doneCh <- struct{}{}
}

func TestEncryptDecryptState(t *testing.T) {
	state := []byte("this is our internal state...")
	config := &Config{
		SecretKey:       []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		ProtocolVersion: ProtocolVersionMax,
	}
	sink := registerInMemorySink(t)

	m, err := Create(config)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	crypt, err := m.encryptLocalState(state, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Create reader, seek past the type byte
	buf := bytes.NewReader(crypt)
	if _, err := buf.Seek(1, 0); err != nil {
		t.Fatalf("err: %v", err)
	}

	plain, err := m.decryptRemoteState(buf, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	verifySampleExists(t, "consul.usage.test.memberlist.size.remote", sink)

	if !reflect.DeepEqual(state, plain) {
		t.Fatalf("Decrypt failed: %v", plain)
	}
}

func TestRawSendUdp_CRC(t *testing.T) {
	m := GetMemberlist(t, func(c *Config) {
		c.EnableCompression = false
	})
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	udp := listenUDP(t)
	defer func() {
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	a := Address{
		Addr: udp.LocalAddr().String(),
		Name: "test",
	}

	// Pass a nil node with no nodes registered, should result in no checksum
	payload := []byte{3, 3, 3, 3}
	if err := m.rawSendMsgPacket(a, nil, payload); err != nil {
		t.Fatalf("err: %v", err)
	}

	in := make([]byte, 1500)
	n, _, err := udp.ReadFrom(in)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	in = in[0:n]

	if len(in) != 4 {
		t.Fatalf("bad: %v", in)
	}

	// Pass a non-nil node with PMax >= 5, should result in a checksum
	if err := m.rawSendMsgPacket(a, &Node{PMax: 5}, payload); err != nil {
		t.Fatalf("err: %v", err)
	}

	in = make([]byte, 1500)
	n, _, err = udp.ReadFrom(in)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	in = in[0:n]

	if len(in) != 9 {
		t.Fatalf("bad: %v", in)
	}

	// Register a node with PMax >= 5 to be looked up, should result in a checksum
	m.nodeMap["127.0.0.1"] = &nodeState{
		Node: Node{PMax: 5},
	}
	if err := m.rawSendMsgPacket(a, nil, payload); err != nil {
		t.Fatal(err)
	}

	in = make([]byte, 1500)
	n, _, err = udp.ReadFrom(in)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	in = in[0:n]

	if len(in) != 9 {
		t.Fatalf("bad: %v", in)
	}
}

func TestIngestPacket_CRC(t *testing.T) {
	m := GetMemberlist(t, func(c *Config) {
		c.EnableCompression = false
	})
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	udp := listenUDP(t)
	defer func() {
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	a := Address{
		Addr: udp.LocalAddr().String(),
		Name: "test",
	}

	// Get a message with a checksum
	payload := []byte{3, 3, 3, 3}
	if err := m.rawSendMsgPacket(a, &Node{PMax: 5}, payload); err != nil {
		t.Fatal(err)
	}

	in := make([]byte, 1500)
	n, _, err := udp.ReadFrom(in)
	if err != nil {
		t.Fatalf("unexpected err %s", err)
	}
	in = in[0:n]

	if len(in) != 9 {
		t.Fatalf("bad: %v", in)
	}

	// Corrupt the checksum
	in[1] <<= 1

	logs := &bytes.Buffer{}
	logger := log.New(logs, "", 0)
	m.logger = logger
	m.ingestPacket(in, udp.LocalAddr(), time.Now())

	if !strings.Contains(logs.String(), "invalid checksum") {
		t.Fatalf("bad: %s", logs.String())
	}
}

func TestIngestPacket_ExportedFunc_EmptyMessage(t *testing.T) {
	m := GetMemberlist(t, func(c *Config) {
		c.EnableCompression = false
	})
	defer func() {
		if err := m.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	udp := listenUDP(t)
	defer func() {
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	emptyConn := &emptyReadNetConn{}

	logs := &bytes.Buffer{}
	logger := log.New(logs, "", 0)
	m.logger = logger

	type ingestionAwareTransport interface {
		IngestPacket(conn net.Conn, addr net.Addr, now time.Time, shouldClose bool) error
	}

	err := m.transport.(ingestionAwareTransport).IngestPacket(emptyConn, udp.LocalAddr(), time.Now(), true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "packet too short")
}

type emptyReadNetConn struct {
	net.Conn
}

func (c *emptyReadNetConn) Read(b []byte) (n int, err error) {
	return 0, io.EOF
}

func (c *emptyReadNetConn) Close() error {
	return nil
}

func TestGossip_MismatchedKeys(t *testing.T) {
	// Create two agents with different gossip keys
	c1 := testConfig(t)
	c1.SecretKey = []byte("4W6DGn2VQVqDEceOdmuRTQ==")

	m1, err := Create(c1)
	require.NoError(t, err)
	defer func() {
		if err := m1.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	bindPort := m1.config.BindPort

	c2 := testConfig(t)
	c2.BindPort = bindPort
	c2.SecretKey = []byte("XhX/w702/JKKK7/7OtM9Ww==")

	m2, err := Create(c2)
	require.NoError(t, err)
	defer func() {
		if err := m2.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}()

	// Make sure we get this error on the joining side
	_, err = m2.Join([]string{c1.Name + "/" + c1.BindAddr})
	if err == nil || !strings.Contains(err.Error(), "no installed keys could decrypt the message") {
		t.Fatalf("bad: %s", err)
	}
}

func listenUDP(t *testing.T) *net.UDPConn {
	var udp *net.UDPConn
	for port := 60000; port < 61000; port++ {
		udpAddr := fmt.Sprintf("127.0.0.1:%d", port)
		udpLn, err := net.ListenPacket("udp", udpAddr)
		if err == nil {
			udp = udpLn.(*net.UDPConn)
			break
		}
	}
	if udp == nil {
		t.Fatalf("no udp listener")
	}
	return udp
}

func TestHandleCommand(t *testing.T) {
	var buf bytes.Buffer
	m := Memberlist{
		logger: log.New(&buf, "", 0),
	}
	m.handleCommand(nil, &net.TCPAddr{Port: 12345}, time.Now())
	require.Contains(t, buf.String(), "missing message type byte")
}

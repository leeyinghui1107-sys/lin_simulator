package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestTryAcquireConnHonorsPerPortLimit(t *testing.T) {
	port := &PortInfo{}

	if !tryAcquireConn(port, 2) {
		t.Fatal("first connection was rejected")
	}
	if !tryAcquireConn(port, 2) {
		t.Fatal("second connection was rejected")
	}
	if tryAcquireConn(port, 2) {
		t.Fatal("third connection was accepted, want rejection")
	}
	if got := port.activeConns.Load(); got != 2 {
		t.Fatalf("activeConns = %d, want 2", got)
	}

	releaseConn(port)
	if !tryAcquireConn(port, 2) {
		t.Fatal("connection was rejected after a slot was released")
	}
}

func TestTryAcquireConnAllowsUnlimitedWhenMaxConnIsZero(t *testing.T) {
	port := &PortInfo{}

	for i := 0; i < 10; i++ {
		if !tryAcquireConn(port, 0) {
			t.Fatal("unlimited connection mode rejected a connection")
		}
	}
	if got := port.activeConns.Load(); got != 10 {
		t.Fatalf("activeConns = %d, want 10", got)
	}
}

func TestLogRejectedConnectionsResetsCounters(t *testing.T) {
	port := &PortInfo{Addr: "0.0.0.0:4001"}
	port.rejectedConns.Add(3)

	logRejectedConnections(map[int]*PortInfo{4001: port})

	if got := port.rejectedConns.Load(); got != 0 {
		t.Fatalf("rejectedConns = %d, want 0", got)
	}
}

func TestRetryBackoffIsCapped(t *testing.T) {
	tests := []struct {
		failCount int
		want      string
	}{
		{failCount: 0, want: "1s"},
		{failCount: 5, want: "32s"},
		{failCount: 6, want: "1m0s"},
		{failCount: 60, want: "1m0s"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := retryBackoff(tt.failCount); got.String() != tt.want {
				t.Fatalf("retryBackoff(%d) = %s, want %s", tt.failCount, got, tt.want)
			}
		})
	}
}

func TestHandleConnMatchesSplitAndCoalescedCommands(t *testing.T) {
	port := &PortInfo{Addr: "pipe"}
	port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x01})
	port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x02})
	port.addCommandResponse([]byte{0xCC}, "CC", []byte{0x03})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		handleConn(ctx, serverConn, port, 0)
		close(done)
	}()

	if err := clientConn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := clientConn.Write([]byte{0xAA}); err != nil {
		t.Fatalf("write split prefix: %v", err)
	}
	if _, err := clientConn.Write([]byte{0xBB, 0xCC, 0xAA, 0xBB}); err != nil {
		t.Fatalf("write coalesced commands: %v", err)
	}

	got := make([]byte, 3)
	if _, err := io.ReadFull(clientConn, got); err != nil {
		t.Fatalf("read responses: %v", err)
	}
	want := []byte{0x01, 0x03, 0x02}
	if string(got) != string(want) {
		t.Fatalf("responses = % X, want % X", got, want)
	}

	cancel()
	clientConn.Close()
	<-done
}

func TestHandleConnDropsGarbageAndResynchronizes(t *testing.T) {
	port := &PortInfo{Addr: "pipe"}
	port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x01})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		handleConn(ctx, serverConn, port, 0)
		close(done)
	}()

	if err := clientConn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := clientConn.Write([]byte{0x00, 0xAA, 0xBB}); err != nil {
		t.Fatalf("write garbage plus command: %v", err)
	}

	got := make([]byte, 1)
	if _, err := io.ReadFull(clientConn, got); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got[0] != 0x01 {
		t.Fatalf("response = % X, want 01", got)
	}

	cancel()
	clientConn.Close()
	<-done
}

func TestHandleConnClosesWhenPendingExceedsLimit(t *testing.T) {
	command := bytes.Repeat([]byte{0xAA}, maxPendingBytes+2)
	port := &PortInfo{Addr: "pipe"}
	port.addCommandResponse(command, BytesToHex(command), []byte{0x01})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		handleConn(ctx, serverConn, port, 0)
		close(done)
	}()

	writeDone := make(chan error, 1)
	go func() {
		_, err := clientConn.Write(command[:maxPendingBytes+1])
		writeDone <- err
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConn did not close an oversized pending buffer")
	}

	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client write did not unblock after server closed the connection")
	}
}

func TestProcessBufferedCommandsRecoversAfterLargeGarbagePrefix(t *testing.T) {
	port := &PortInfo{Addr: "pipe"}
	port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x01})
	conn := &recordingConn{}
	state := connState{}
	pending := bytes.Repeat([]byte{0x00}, readBufferSize*2)
	pending = append(pending, 0xAA, 0xBB)

	remaining, ok := processBufferedCommands(context.Background(), conn, port, 0, pending, &state, false)
	if !ok {
		t.Fatal("processBufferedCommands returned false")
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining pending length = %d, want 0", len(remaining))
	}
	if got := conn.buf.Bytes(); !bytes.Equal(got, []byte{0x01}) {
		t.Fatalf("written response = % X, want 01", got)
	}
}

func TestProcessBufferedCommandsReusesConsumedBuffer(t *testing.T) {
	port := &PortInfo{Addr: "pipe"}
	port.addCommandResponse([]byte{0xAA}, "AA", []byte{0x01})
	conn := &recordingConn{}
	state := connState{}
	pending := make([]byte, 1, readBufferSize)
	pending[0] = 0xAA

	remaining, ok := processBufferedCommands(context.Background(), conn, port, 0, pending, &state, false)
	if !ok {
		t.Fatal("processBufferedCommands returned false")
	}
	if len(remaining) != 0 || cap(remaining) != readBufferSize {
		t.Fatalf("remaining = len %d cap %d, want len 0 cap %d", len(remaining), cap(remaining), readBufferSize)
	}
}

func TestProcessBufferedCommandsCompactsTrailingPrefix(t *testing.T) {
	port := &PortInfo{Addr: "pipe"}
	port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x01})
	conn := &recordingConn{}
	state := connState{}
	pending := make([]byte, 2, readBufferSize)
	copy(pending, []byte{0x00, 0xAA})

	remaining, ok := processBufferedCommands(context.Background(), conn, port, 0, pending, &state, false)
	if !ok {
		t.Fatal("processBufferedCommands returned false")
	}
	if !bytes.Equal(remaining, []byte{0xAA}) || cap(remaining) != readBufferSize {
		t.Fatalf("remaining = % X cap %d, want AA cap %d", remaining, cap(remaining), readBufferSize)
	}

	remaining = append(remaining, 0xBB)
	remaining, ok = processBufferedCommands(context.Background(), conn, port, 0, remaining, &state, false)
	if !ok || len(remaining) != 0 || !bytes.Equal(conn.buf.Bytes(), []byte{0x01}) {
		t.Fatalf("completed prefix = remaining % X written % X ok %v, want empty, 01, true", remaining, conn.buf.Bytes(), ok)
	}
}

func TestMatchCommandPrefersLongestKnownCommand(t *testing.T) {
	port := &PortInfo{Addr: "pipe"}
	port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x02})
	port.addCommandResponse([]byte{0xAA}, "AA", []byte{0x01})

	cmd, cmdLen, _ := matchCommand(port, []byte{0xAA, 0xBB})
	if cmd == nil {
		t.Fatal("matchCommand returned nil")
	}
	if cmdLen != 2 || cmd.HexKey != "AABB" {
		t.Fatalf("matched %s length %d, want AABB length 2", cmd.HexKey, cmdLen)
	}
}

func TestMatchCommandReportsCompleteCommandsAndPrefixes(t *testing.T) {
	port := &PortInfo{Addr: "pipe"}
	port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x01})

	if cmd, _, prefix := matchCommand(port, []byte{0xAA}); cmd != nil || !prefix {
		t.Fatal("matchCommand rejected a valid command prefix")
	}
	if cmd, _, _ := matchCommand(port, []byte{0xAA, 0xBB}); cmd == nil {
		t.Fatal("matchCommand rejected a complete command")
	}
	if cmd, _, prefix := matchCommand(port, []byte{0xAA, 0xBC}); cmd != nil || prefix {
		t.Fatal("matchCommand accepted an invalid prefix")
	}
}

func TestConnStateNextResponseRotatesOnlyMultiResponseCommands(t *testing.T) {
	state := connState{}
	cmd := &CommandInfo{
		Responses: [][]byte{
			{0x01},
			{0x02},
			{0x03},
		},
	}

	for i, want := range [][]byte{{0x01}, {0x02}, {0x03}, {0x01}, {0x02}} {
		if got := state.nextResponse(cmd); !bytes.Equal(got, want) {
			t.Fatalf("response %d = % X, want % X", i, got, want)
		}
	}
	if state.responseIndex[cmd] != 2 {
		t.Fatalf("next response index = %d, want 2", state.responseIndex[cmd])
	}

	singleState := connState{}
	single := &CommandInfo{Responses: [][]byte{{0xAA}}}
	if got := singleState.nextResponse(single); !bytes.Equal(got, []byte{0xAA}) {
		t.Fatalf("single response = % X, want AA", got)
	}
	if singleState.responseIndex != nil {
		t.Fatal("single-response command allocated responseIndex")
	}

	independentState := connState{}
	cmdA := &CommandInfo{HexKey: "AA", Responses: [][]byte{{0x10}, {0x11}}}
	cmdB := &CommandInfo{HexKey: "BB", Responses: [][]byte{{0x20}, {0x21}, {0x22}}}
	sequence := []struct {
		cmd  *CommandInfo
		want []byte
	}{
		{cmd: cmdA, want: []byte{0x10}},
		{cmd: cmdB, want: []byte{0x20}},
		{cmd: cmdA, want: []byte{0x11}},
		{cmd: cmdB, want: []byte{0x21}},
		{cmd: cmdA, want: []byte{0x10}},
	}
	for i, step := range sequence {
		if got := independentState.nextResponse(step.cmd); !bytes.Equal(got, step.want) {
			t.Fatalf("independent response %d = % X, want % X", i, got, step.want)
		}
	}
}

func TestHandleConnClosesOnContextCancel(t *testing.T) {
	port := &PortInfo{Addr: "pipe"}

	ctx, cancel := context.WithCancel(context.Background())
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		handleConn(ctx, serverConn, port, 0)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConn did not return after context cancel")
	}
}

func TestSetPortRetryIncrementsFailCountWithStatusSnapshot(t *testing.T) {
	port := &PortInfo{}

	setPortRetry(port)
	status, failCount, nextRetry := portStatusSnapshot(port)
	if status != portStatusRetry {
		t.Fatalf("status = %d, want %d", status, portStatusRetry)
	}
	if failCount != 1 {
		t.Fatalf("failCount = %d, want 1", failCount)
	}
	if nextRetry.IsZero() {
		t.Fatal("nextRetry was not set")
	}

	setPortStatus(port, portStatusListening, 0, time.Time{})
	status, failCount, nextRetry = portStatusSnapshot(port)
	if status != portStatusListening || failCount != 0 || !nextRetry.IsZero() {
		t.Fatalf("snapshot after reset = status:%d failCount:%d nextRetry:%s, want normal zero retry", status, failCount, nextRetry)
	}
}

func TestWaitResponseDelayHonorsPortDelayAndContext(t *testing.T) {
	start := time.Now()
	if !waitResponseDelay(context.Background(), 20*time.Millisecond) {
		t.Fatal("waitResponseDelay returned false without cancellation")
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("waitResponseDelay returned too early: %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitResponseDelay(ctx, 20*time.Millisecond) {
		t.Fatal("waitResponseDelay returned true after context cancellation")
	}

	if !waitResponseDelay(context.Background(), 0) {
		t.Fatal("waitResponseDelay returned false for non-positive delay")
	}
}

func TestCompactPendingKeepsAppendHeadroom(t *testing.T) {
	pending := make([]byte, readBufferSize*4, maxPendingBytes+1)
	compacted := compactPending(pending)

	if len(compacted) != len(pending) {
		t.Fatalf("len(compacted) = %d, want %d", len(compacted), len(pending))
	}
	if cap(compacted) <= len(compacted) {
		t.Fatalf("cap(compacted) = %d, want append headroom above len %d", cap(compacted), len(compacted))
	}
	if cap(compacted) > maxPendingBytes {
		t.Fatalf("cap(compacted) = %d, want <= %d", cap(compacted), maxPendingBytes)
	}

	if compacted := compactPending(nil); compacted != nil {
		t.Fatalf("compactPending(nil) = %v, want nil", compacted)
	}

	reusable := make([]byte, 0, readBufferSize)
	if compacted := compactPending(reusable); cap(compacted) != readBufferSize {
		t.Fatalf("cap(compactPending(reusable)) = %d, want %d", cap(compacted), readBufferSize)
	}
}

func TestWriteAllHandlesShortWrites(t *testing.T) {
	conn := &recordingConn{maxWrite: 1}

	if err := writeAll(conn, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("writeAll returned error: %v", err)
	}
	if got := conn.buf.Bytes(); !bytes.Equal(got, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("written bytes = % X, want 01 02 03", got)
	}
}

func TestWriteAllReturnsDeadlineError(t *testing.T) {
	wantErr := errors.New("deadline failed")
	conn := &recordingConn{writeDeadlineErr: wantErr}

	if err := writeAll(conn, []byte{0x01}); !errors.Is(err, wantErr) {
		t.Fatalf("writeAll error = %v, want %v", err, wantErr)
	}
}

func TestBuildServerMarksPortForRetryWhenListenFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	port := &PortInfo{Addr: listener.Addr().String()}
	BuildServer(context.Background(), port, 0, 1)

	status, failCount, nextRetry := portStatusSnapshot(port)
	if status != portStatusRetry {
		t.Fatalf("status = %d, want %d", status, portStatusRetry)
	}
	if failCount != 1 {
		t.Fatalf("failCount = %d, want 1", failCount)
	}
	if nextRetry.IsZero() {
		t.Fatal("nextRetry was not set")
	}
}

type recordingConn struct {
	buf              bytes.Buffer
	maxWrite         int
	writeDeadlineErr error
}

func (c *recordingConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *recordingConn) Write(p []byte) (int, error) {
	n := len(p)
	if c.maxWrite > 0 && n > c.maxWrite {
		n = c.maxWrite
	}
	c.buf.Write(p[:n])
	return n, nil
}

func (c *recordingConn) Close() error {
	return nil
}

func (c *recordingConn) LocalAddr() net.Addr {
	return testAddr("local")
}

func (c *recordingConn) RemoteAddr() net.Addr {
	return testAddr("remote")
}

func (c *recordingConn) SetDeadline(time.Time) error {
	return nil
}

func (c *recordingConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *recordingConn) SetWriteDeadline(time.Time) error {
	return c.writeDeadlineErr
}

type testAddr string

func (a testAddr) Network() string {
	return string(a)
}

func (a testAddr) String() string {
	return string(a)
}

var (
	benchmarkMatchedCommand *CommandInfo
	benchmarkMatchedLen     int
	benchmarkMatchedPrefix  bool
)

func BenchmarkMatchCommand(b *testing.B) {
	cases := []struct {
		name       string
		add        func(*PortInfo)
		pending    []byte
		wantHexKey string
		wantLen    int
		wantPrefix bool
	}{
		{
			name: "longest",
			add: func(port *PortInfo) {
				port.addCommandResponse([]byte{0xAA}, "AA", []byte{0x01})
				port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x02})
				port.addCommandResponse([]byte{0xAA, 0xBB, 0xCC, 0xDD}, "AABBCCDD", []byte{0x03})
				port.addCommandResponse([]byte{0xEE, 0xFF}, "EEFF", []byte{0x04})
			},
			pending:    []byte{0xAA, 0xBB, 0xCC, 0xDD},
			wantHexKey: "AABBCCDD",
			wantLen:    4,
			wantPrefix: true,
		},
		{
			name: "root-miss",
			add: func(port *PortInfo) {
				port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x01})
			},
			pending: []byte{0x00},
		},
		{
			name: "prefix",
			add: func(port *PortInfo) {
				port.addCommandResponse([]byte{0xAA, 0xBB}, "AABB", []byte{0x01})
			},
			pending:    []byte{0xAA},
			wantPrefix: true,
		},
		{
			name: "short-match",
			add: func(port *PortInfo) {
				port.addCommandResponse([]byte{0xAA}, "AA", []byte{0x01})
			},
			pending:    []byte{0xAA},
			wantHexKey: "AA",
			wantLen:    1,
			wantPrefix: true,
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			port := &PortInfo{Addr: "bench"}
			tc.add(port)

			b.ReportAllocs()
			b.ResetTimer()

			var cmd *CommandInfo
			var cmdLen int
			var prefix bool
			for i := 0; i < b.N; i++ {
				cmd, cmdLen, prefix = matchCommand(port, tc.pending)
			}

			benchmarkMatchedCommand = cmd
			benchmarkMatchedLen = cmdLen
			benchmarkMatchedPrefix = prefix

			if tc.wantHexKey == "" {
				if cmd != nil || cmdLen != 0 || prefix != tc.wantPrefix {
					b.Fatalf("matchCommand = (%v, %d, %v), want nil, 0, %v", cmd, cmdLen, prefix, tc.wantPrefix)
				}
				return
			}
			if cmd == nil || cmd.HexKey != tc.wantHexKey || cmdLen != tc.wantLen || prefix != tc.wantPrefix {
				b.Fatalf("matchCommand = (%v, %d, %v), want %s, %d, %v", cmd, cmdLen, prefix, tc.wantHexKey, tc.wantLen, tc.wantPrefix)
			}
		})
	}
}

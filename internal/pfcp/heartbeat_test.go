package pfcp

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

func TestSendHeartbeatRequest(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	upfRecoveryTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	peerAddr, peerDone := startHeartbeatResponsePeer(t, func(req *message.HeartbeatRequest) (message.Message, error) {
		if req.RecoveryTimeStamp == nil {
			return nil, fmt.Errorf("Heartbeat Request is missing Recovery Time Stamp")
		}
		smfRecoveryTime, err := req.RecoveryTimeStamp.RecoveryTimeStamp()
		if err != nil {
			return nil, fmt.Errorf("decode request Recovery Time Stamp: %w", err)
		}
		if got, want := smfRecoveryTime.Unix(), s.RecoveryTime().Unix(); got != want {
			return nil, fmt.Errorf("request Recovery Time Stamp = %d, want %d", got, want)
		}
		return message.NewHeartbeatResponse(
			req.Sequence(),
			ie.NewRecoveryTimeStamp(upfRecoveryTime),
		), nil
	})

	response, err := s.SendHeartbeatRequest(context.Background(), peerAddr)
	if err != nil {
		t.Fatalf("SendHeartbeatRequest() error: %v", err)
	}
	if err = <-peerDone; err != nil {
		t.Fatalf("UPF peer error: %v", err)
	}
	recoveryTime, err := response.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		t.Fatalf("decode response Recovery Time Stamp: %v", err)
	}
	if got, want := recoveryTime.Unix(), upfRecoveryTime.Unix(); got != want {
		t.Fatalf("response Recovery Time Stamp = %d, want %d", got, want)
	}
}

func TestSendHeartbeatRequestRejectsMissingRecoveryTimeStamp(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	peerAddr, peerDone := startHeartbeatResponsePeer(t, func(req *message.HeartbeatRequest) (message.Message, error) {
		return message.NewHeartbeatResponse(req.Sequence(), nil), nil
	})

	_, err := s.SendHeartbeatRequest(context.Background(), peerAddr)
	if err == nil || !strings.Contains(err.Error(), "missing Recovery Time Stamp") {
		t.Fatalf("SendHeartbeatRequest() error = %v, want missing Recovery Time Stamp", err)
	}
	if peerErr := <-peerDone; peerErr != nil {
		t.Fatalf("UPF peer error: %v", peerErr)
	}
}

func TestSendHeartbeatRequestRejectsUnexpectedResponse(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	peerAddr, peerDone := startHeartbeatResponsePeer(t, func(req *message.HeartbeatRequest) (message.Message, error) {
		return message.NewAssociationUpdateResponse(req.Sequence()), nil
	})

	_, err := s.SendHeartbeatRequest(context.Background(), peerAddr)
	if err == nil || !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("SendHeartbeatRequest() error = %v, want unexpected response", err)
	}
	if peerErr := <-peerDone; peerErr != nil {
		t.Fatalf("UPF peer error: %v", peerErr)
	}
}

func TestSendHeartbeatRequestTimeout(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	blackHole := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}

	_, err := s.SendHeartbeatRequest(context.Background(), blackHole)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("SendHeartbeatRequest() error = %v, want timeout", err)
	}
}

func TestSendHeartbeatRequestRejectsStoppedServer(t *testing.T) {
	s, wg := startTestPfcpServer(t)
	s.Stop()
	wg.Wait()

	peer := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: PfcpPort}
	_, err := s.SendHeartbeatRequest(context.Background(), peer)
	if err == nil || !strings.Contains(err.Error(), "server is stopped") {
		t.Fatalf("SendHeartbeatRequest() error = %v, want server stopped", err)
	}
}

func TestSendHeartbeatRequestRejectsUnspecifiedDestination(t *testing.T) {
	s := NewPfcpServer(nil, "127.0.0.1")
	for _, addr := range []*net.UDPAddr{nil, {IP: net.IPv4zero, Port: PfcpPort}} {
		if _, err := s.SendHeartbeatRequest(context.Background(), addr); err == nil {
			t.Fatalf("SendHeartbeatRequest(%v) succeeded, want destination error", addr)
		}
	}
}

func startHeartbeatResponsePeer(
	t *testing.T,
	buildResponse func(*message.HeartbeatRequest) (message.Message, error),
) (*net.UDPAddr, <-chan error) {
	t.Helper()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP() error: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	done := make(chan error, 1)
	go func() {
		packet := make([]byte, 1500)
		n, smfAddr, readErr := peer.ReadFromUDP(packet)
		if readErr != nil {
			done <- readErr
			return
		}
		parsed, parseErr := message.Parse(packet[:n])
		if parseErr != nil {
			done <- parseErr
			return
		}
		request, ok := parsed.(*message.HeartbeatRequest)
		if !ok {
			done <- fmt.Errorf("received %T, want *message.HeartbeatRequest", parsed)
			return
		}
		response, buildErr := buildResponse(request)
		if buildErr != nil {
			done <- buildErr
			return
		}
		responsePacket := make([]byte, response.MarshalLen())
		if marshalErr := response.MarshalTo(responsePacket); marshalErr != nil {
			done <- marshalErr
			return
		}
		_, writeErr := peer.WriteToUDP(responsePacket, smfAddr)
		done <- writeErr
	}()
	return peer.LocalAddr().(*net.UDPAddr), done
}

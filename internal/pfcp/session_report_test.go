package pfcp

import (
	"testing"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

type fakeSessionReportHandler struct {
	cause      uint8
	remoteSEID uint64
	request    *message.SessionReportRequest
}

func (f *fakeSessionReportHandler) HandleSessionReportRequest(
	request *message.SessionReportRequest,
) (uint8, uint64) {
	f.request = request
	return f.cause, f.remoteSEID
}

func TestHandleSessionReportRequestUsesProcedureHandler(t *testing.T) {
	handler := &fakeSessionReportHandler{
		cause:      ie.CauseRequestAccepted,
		remoteSEID: 0x0102030405060708,
	}
	server := NewPfcpServer(nil, "127.0.0.1")
	server.SetSessionReportHandler(handler)
	request := message.NewSessionReportRequest(
		0, 0, 99, 0x123456, 0,
		ie.NewReportType(0, 0, 1, 0),
	)

	response := server.handleSessionReportRequest(request)
	if handler.request != request {
		t.Fatal("Session Report procedure did not receive the original concrete request")
	}
	assertCause(t, response.Cause, ie.CauseRequestAccepted)
	if got, want := response.SEID(), handler.remoteSEID; got != want {
		t.Fatalf("response SEID = %d, want %d", got, want)
	}
	if got, want := response.Sequence(), request.Sequence(); got != want {
		t.Fatalf("response sequence = %#x, want %#x", got, want)
	}
}

func TestHandleSessionReportRequestWithoutHandler(t *testing.T) {
	server := NewPfcpServer(nil, "127.0.0.1")
	request := message.NewSessionReportRequest(0, 0, 99, 7, 0, ie.NewReportType(0, 0, 1, 0))

	response := server.handleSessionReportRequest(request)
	assertCause(t, response.Cause, ie.CauseServiceNotSupported)
	if response.SEID() != 0 {
		t.Fatalf("response SEID = %d, want zero when no SMF procedure handled the request", response.SEID())
	}
}

package processor

import (
	"net"
	"strings"
	"testing"

	"github.com/wmnsk/go-pfcp/ie"

	"github.com/free5gc/smf/internal/context"
)

func TestApplyCreatedPDRsUpdatesUPFAllocatedFTEID(t *testing.T) {
	pdr := &context.PDR{PDRID: 7}
	sessionContext := &context.PFCPSessionContext{
		PDRs: map[uint16]*context.PDR{7: pdr},
	}
	createdPDR := ie.NewCreatedPDR(
		ie.NewPDRID(7),
		ie.NewFTEID(0x01, 0x10203040, net.ParseIP("192.0.2.20").To4(), nil, 0),
	)

	if err := applyCreatedPDRs([]*ie.IE{createdPDR}, sessionContext, nil); err != nil {
		t.Fatalf("applyCreatedPDRs() error: %v", err)
	}
	got := pdr.PDI.LocalFTeid
	if got == nil {
		t.Fatal("LocalFTeid was not updated")
	}
	if got.Teid != 0x10203040 || !got.V4 || got.V6 {
		t.Fatalf("LocalFTeid = %+v, want IPv4 TEID %#x", got, uint32(0x10203040))
	}
	if !got.Ipv4Address.Equal(net.ParseIP("192.0.2.20")) {
		t.Fatalf("LocalFTeid IPv4 = %v, want 192.0.2.20", got.Ipv4Address)
	}
}

func TestApplyCreatedPDRsAllowsNoAllocatedFTEID(t *testing.T) {
	pdr := &context.PDR{PDRID: 7}
	sessionContext := &context.PFCPSessionContext{
		PDRs: map[uint16]*context.PDR{7: pdr},
	}
	if err := applyCreatedPDRs(
		[]*ie.IE{ie.NewCreatedPDR(ie.NewPDRID(7))}, sessionContext, nil,
	); err != nil {
		t.Fatalf("applyCreatedPDRs() error: %v", err)
	}
	if pdr.PDI.LocalFTeid != nil {
		t.Fatalf("LocalFTeid = %+v, want unchanged nil", pdr.PDI.LocalFTeid)
	}
}

func TestApplyCreatedPDRsRejectsUnknownPDR(t *testing.T) {
	sessionContext := &context.PFCPSessionContext{PDRs: make(map[uint16]*context.PDR)}
	err := applyCreatedPDRs([]*ie.IE{
		ie.NewCreatedPDR(
			ie.NewPDRID(99),
			ie.NewFTEID(0x01, 1, net.ParseIP("192.0.2.20").To4(), nil, 0),
		),
	}, sessionContext, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown PDR ID 99") {
		t.Fatalf("applyCreatedPDRs() error = %v, want unknown PDR error", err)
	}
}

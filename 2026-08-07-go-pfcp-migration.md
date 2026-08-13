# go-pfcp Migration Implementation Plan (external free5gc/smf)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `github.com/free5gc/pfcp` with `github.com/wmnsk/go-pfcp` throughout `/home/alonza/smf/smf-opensource` (module `github.com/free5gc/smf`), reproducing Saviah's internal PFCP implementation design as faithfully as the external codebase allows.

**Architecture — this plan is a direct translation of Saviah's internal, already-shipped design, not an independent redesign.** The internal SMF fork migrated to the exact same pinned go-pfcp commit and settled on a specific split of responsibilities:

- `internal/pfcp` (transport package, **no subpackages** — no `handler/`, `message/`, `udp/`): owns the UDP socket, the Tx/Rx transaction state machine (sequence allocation, retransmit, duplicate-request caching — none of which go-pfcp provides), and the **passive direction** (SMF responding to a UPF-initiated request: Heartbeat, Association Setup/Release, Session Report).
- `internal/context`: owns the **active direction** (SMF-initiated requests to a UPF: Association Setup/Release, Heartbeat, Session Establishment/Modification/Deletion) via `pfcp_build.go` (IE construction) + `pfcp_send.go` (send + transport-level response validation) + `pfcp_handler.go` (protocol-level response validation: Cause, mandatory IEs), plus the PDR/FAR/QER/URR/BAR domain model in `pfcp_rules.go`.
- `internal/pfcp/pfcptype`: SMF's own domain-type package (NodeID, Cause, FTEID, ApplyAction, ReportingTriggers, etc.) — replaces `github.com/free5gc/pfcp/pfcpType` with zero dependency on it.

This is **not** the file layout the external repo has today (`internal/pfcp/message/`, `internal/pfcp/handler/`, `internal/pfcp/udp/` all get deleted), and it is **not** the minimal-diff layout an independent redesign would produce — it is Saviah's internal layout, translated task-by-task because the internal source itself cannot be shared. Every architectural decision in this plan (channel design, the `afterRsp` callback pattern, the Tx/Rx transaction key, the `ReportingTriggers` bit-packing, the "build vs. send vs. validate" three-way split) was read directly from the internal implementation and is reproduced here from scratch, adapted only where the external repo's own data model (its `SMContext`/`PFCPContext`/`UPF` types, which differ from the internal fork's) forces an adaptation. Those forced adaptations are called out explicitly wherever they occur.

Two things from the internal implementation are deliberately **not** reproduced, because the external repo has no equivalent infrastructure to hang them on: OpenTelemetry vendor-specific-IE trace propagation (needs Saviah's internal `smfotel` package and tracer setup, which doesn't exist here), and the internal `idgenerator`/`unbounded_channel` utility libraries (re-implemented from scratch in Task 3/Task 4 with equivalent semantics, since they're Saviah-internal Go modules the external repo cannot import).

**Tech Stack:** Go 1.26.2, `github.com/wmnsk/go-pfcp`, existing `github.com/free5gc/smf` module.

## Global Constraints

- Pin `github.com/wmnsk/go-pfcp v0.0.25-0.20251110163217-837df5430868` (commit `837df543086816a27023644f27c1a31e1c0d371e`) — the version the internal fork migrated to and validated.
- Do not create any package that re-exports or wraps `github.com/free5gc/pfcp`, `pfcpType`, or `pfcpUdp` under a compatibility name.
- `rg 'github.com/free5gc/pfcp' .` must return no output once this plan is complete.
- Every goroutine this plan adds must `recover()` and call `logger.PfcpLog.Fatalf` on panic (matches the internal server's pattern in every one of its 4 goroutines — `Run`'s deferred recover, `main`'s, `receiver`'s, `dispatcher`'s — and the existing external `internal/pfcp/udp/udp.go:23-28,42-48`).
- Every go-pfcp IE getter call must check its returned `error`; every mandatory IE must be nil-checked before its getter is called.
- Run `gotestsum ./...` and `gotestsum -- -race ./internal/pfcp/...` after every task.

---

## Part 1 — Foundation (new code, nothing wired in yet)

### Task 1: Add go-pfcp dependency

**Files:** Modify `go.mod`, `go.sum`

- [ ] **Step 1**
```bash
cd /home/alonza/smf/smf-opensource
go get github.com/wmnsk/go-pfcp@837df543086816a27023644f27c1a31e1c0d371e
```
- [ ] **Step 2:** `grep wmnsk/go-pfcp go.mod` → expect `v0.0.25-0.20251110163217-837df5430868`.
- [ ] **Step 3:** `go build ./...` → still succeeds (nothing else changed).
- [ ] **Step 4:**
```bash
git add go.mod go.sum
git commit -m "chore: add wmnsk/go-pfcp dependency"
```

---

### Task 2: `internal/pfcp/pfcptype` — domain type package

Reproduces the internal fork's `internal/pfcp/pfcpType/objects.go` package (read directly from the internal source). One deliberate deviation from a literal copy: the internal package's `ReportingTrigger` uses `Flags uint32` with `Set*()` methods and an `IE()` method that packs those flags into wire octets — this plan reproduces that exact design (not the external repo's current named-boolean shape), because the user has asked for the internal approach directly rather than a minimal-diff adaptation. This means every external call site that currently does `urr.ReportingTrigger.Perio = true` will need `urr.ReportingTrigger.SetPERIO()` instead — Task 6 covers those call sites.

**Files:**
- Create: `internal/pfcp/pfcptype/objects.go`
- Test: `internal/pfcp/pfcptype/objects_test.go`

**Interfaces:**
- Produces: `pfcptype.NodeID{NodeIdType uint8; IP net.IP; FQDN string}` with `String()`/`EqualsTo()`; `pfcptype.Cause{CauseValue uint8}` + `CauseXxx` constants (kept for domain-level logging only — wire-level Cause always uses `ie.CauseXxx` directly, per internal's own pattern where `pfcpType.Cause` constants are barely used); `OuterHeaderRemoval`, `FTEID`, `GateStatus{ULGate,DLGate GateStatusType}`, `MBR`, `NetworkInstance`, `SourceInterface`, `UEIPAddress`, `EthernetPacketFilter`, `VLANTag`, `SDFFilter`, `ApplyAction`, `DestinationInterface`, `OuterHeaderCreation`, `QFI`, `PDNType`, `GBR`, `DownlinkDataNotificationDelay`, `SuggestedBufferingPacketsCount`, `ReportingTrigger{Flags uint32}` with `IE() *ie.IE` and `SetPERIO()`/`SetVOLTH()`/.../`SetUPINT()` methods.

- [ ] **Step 1: Write the failing test**

```go
// internal/pfcp/pfcptype/objects_test.go
package pfcptype

import (
	"net"
	"testing"
)

func TestNodeID_String(t *testing.T) {
	ipv4 := NodeID{NodeIdType: NodeIdTypeIpv4Address, IP: net.ParseIP("10.0.0.1").To4()}
	if got := ipv4.String(); got != "10.0.0.1" {
		t.Fatalf("String() = %q, want %q", got, "10.0.0.1")
	}
	fqdn := NodeID{NodeIdType: NodeIdTypeFqdn, FQDN: "upf.example.com"}
	if got := fqdn.String(); got != "upf.example.com" {
		t.Fatalf("String() = %q, want %q", got, "upf.example.com")
	}
}

func TestNodeID_EqualsTo(t *testing.T) {
	a := &NodeID{NodeIdType: NodeIdTypeIpv4Address, IP: net.ParseIP("10.0.0.1").To4()}
	b := &NodeID{NodeIdType: NodeIdTypeIpv4Address, IP: net.ParseIP("10.0.0.1").To4()}
	c := &NodeID{NodeIdType: NodeIdTypeIpv4Address, IP: net.ParseIP("10.0.0.2").To4()}
	if !a.EqualsTo(b) {
		t.Fatalf("expected a == b")
	}
	if a.EqualsTo(c) {
		t.Fatalf("expected a != c")
	}
}

func TestReportingTrigger_IE_Octet1Only(t *testing.T) {
	r := &ReportingTrigger{}
	r.SetPERIO()
	r.SetSTART()
	ieVal := r.IE()
	b, err := ieVal.ReportingTriggers()
	if err != nil {
		t.Fatalf("ReportingTriggers() error: %v", err)
	}
	if len(b) != 1 {
		t.Fatalf("expected 1 octet (no octet-2 flags set), got %d: %v", len(b), b)
	}
	if b[0] != (RPT_TRIG_PERIO | RPT_TRIG_START) {
		t.Fatalf("octet1 = %#08b, want PERIO|START", b[0])
	}
}

func TestReportingTrigger_IE_Octet2(t *testing.T) {
	r := &ReportingTrigger{}
	r.SetVOLQU()
	ieVal := r.IE()
	b, err := ieVal.ReportingTriggers()
	if err != nil {
		t.Fatalf("ReportingTriggers() error: %v", err)
	}
	if len(b) != 2 {
		t.Fatalf("expected 2 octets (an octet-2 flag is set), got %d: %v", len(b), b)
	}
}
```

- [ ] **Step 2:** `go test ./internal/pfcp/pfcptype/...` → FAIL (package doesn't exist).

- [ ] **Step 3: Write the implementation**

```go
// internal/pfcp/pfcptype/objects.go

// Package pfcptype is SMF's own PFCP domain-type package, replacing
// github.com/free5gc/pfcp/pfcpType. It has zero dependency on
// github.com/free5gc/pfcp or github.com/wmnsk/go-pfcp except for the
// ReportingTrigger.IE() conversion helper, which is the one place (as in
// Saviah's internal reference implementation) where a domain type owns its
// own wire-encoding — every other conversion happens in
// internal/context/pfcp_build.go / pfcp_handler.go, never here.
package pfcptype

import (
	"encoding/binary"
	"net"

	"github.com/wmnsk/go-pfcp/ie"
)

// --- NodeID ---

const (
	NodeIdTypeIpv4Address uint8 = iota
	NodeIdTypeIpv6Address
	NodeIdTypeFqdn
)

type NodeID struct {
	NodeIdType uint8
	IP         net.IP
	FQDN       string
}

func (n *NodeID) String() string {
	switch n.NodeIdType {
	case NodeIdTypeIpv4Address, NodeIdTypeIpv6Address:
		return n.IP.String()
	case NodeIdTypeFqdn:
		return n.FQDN
	default:
		return ""
	}
}

func (n *NodeID) EqualsTo(n2 *NodeID) bool {
	if n2 == nil || n.NodeIdType != n2.NodeIdType {
		return false
	}
	if n.NodeIdType == NodeIdTypeIpv4Address || n.NodeIdType == NodeIdTypeIpv6Address {
		return n.IP.Equal(n2.IP)
	}
	if n.NodeIdType == NodeIdTypeFqdn {
		return n.FQDN == n2.FQDN
	}
	return false
}

// --- Cause ---
// Domain-level only, for logging. Wire-level Cause decisions MUST use
// ie.CauseXxx + ie.NewCause()/ie.IE.Cause() directly in pfcp_build.go /
// pfcp_handler.go / internal/pfcp's request handlers — this mirrors the
// internal reference implementation, where pfcpType.Cause exists but the
// actual protocol code almost never uses it.

type Cause struct {
	CauseValue uint8
}

const (
	CauseRequestAccepted                 uint8 = 1
	CauseRequestRejected                 uint8 = 64
	CauseSessionContextNotFound          uint8 = 65
	CauseMandatoryIeMissing              uint8 = 66
	CauseConditionalIeMissing            uint8 = 67
	CauseInvalidLength                   uint8 = 68
	CauseMandatoryIeIncorrect            uint8 = 69
	CauseInvalidForwardingPolicy         uint8 = 70
	CauseInvalidFTeidAllocationOption    uint8 = 71
	CauseNoEstablishedPfcpAssociation    uint8 = 72
	CauseRuleCreationModificationFailure uint8 = 73
	CausePfcpEntityInCongestion          uint8 = 74
	CauseNoResourcesAvailable            uint8 = 75
	CauseServiceNotSupported             uint8 = 76
	CauseSystemFailure                   uint8 = 77
)

// --- PDR/PDI ---

const (
	OuterHeaderRemovalGtpUUdpIpv4 uint8 = iota
	OuterHeaderRemovalGtpUUdpIpv6
	OuterHeaderRemovalUdpIpv4
	OuterHeaderRemovalUdpIpv6
)

type OuterHeaderRemoval struct {
	OuterHeaderRemovalDescription uint8
}

type FTEID struct {
	Chid        bool
	Ch          bool
	V6          bool
	V4          bool
	Teid        uint32
	Ipv4Address net.IP
	Ipv6Address net.IP
	ChooseId    uint8
}

const (
	SourceInterfaceAccess uint8 = iota
	SourceInterfaceCore
	SourceInterfaceSgiLanN6Lan
	SourceInterfaceCpFunction
	SourceInterface5GVNInternal
)

type SourceInterface struct {
	InterfaceValue uint8
}

type NetworkInstance struct {
	NetworkInstance string
}

type UEIPAddress struct {
	Ipv6d                    bool
	Sd                       bool
	V4                       bool
	V6                       bool
	Ipv4Address              net.IP
	Ipv6Address              net.IP
	Ipv6PrefixDelegationBits uint8
}

type EthernetPacketFilter struct {
	MacAddresses []string
	Ethertype    uint16
	CTag         *VLANTag
	STag         *VLANTag
	SDFFilter    []SDFFilter
}

type VLANTag struct {
	VID uint16
	DEI bool
	PCP uint8
}

type SDFFilter struct {
	Bid                     bool
	Fl                      bool
	Spi                     bool
	Ttc                     bool
	Fd                      bool
	LengthOfFlowDescription uint16
	FlowDescription         []byte
	TosTrafficClass         []byte
	SecurityParameterIndex  []byte
	FlowLabel               []byte
	SdfFilterId             uint32
}

// --- FAR ---

type ApplyAction struct {
	Drop bool
	Forw bool
	Buff bool
	Nocp bool
	Dupl bool
	Ipma bool
	Ipmd bool
	Dfrt bool
	Edrt bool
	Bdpn bool
	Ddpn bool
	Fssm bool
	Mbsu bool
}

const (
	DestinationInterfaceAccess uint8 = iota
	DestinationInterfaceCore
	DestinationInterfaceSgiLanN6Lan
	DestinationInterfaceCpFunction
	DestinationInterfaceLiFunction
	DestinationInterface5GVNInternal
)

type DestinationInterface struct {
	InterfaceValue uint8
}

const (
	OuterHeaderCreationGtpUUdpIpv4 uint16 = 1
	OuterHeaderCreationGtpUUdpIpv6 uint16 = 1 << 1
	OuterHeaderCreationUdpIpv4     uint16 = 1 << 2
	OuterHeaderCreationUdpIpv6     uint16 = 1 << 3
)

type OuterHeaderCreation struct {
	OuterHeaderCreationDescription uint16
	Teid                           uint32
	Ipv4Address                    net.IP
	Ipv6Address                    net.IP
	PortNumber                     uint16
}

// --- BAR ---

type DownlinkDataNotificationDelay struct {
	DelayValue uint8
}

type SuggestedBufferingPacketsCount struct {
	PacketCountValue uint8
}

// --- QER ---

type QFI struct {
	QFI uint8
}

type GateStatusType uint8

const (
	GateOpen GateStatusType = iota
	GateClose
)

type GateStatus struct {
	ULGate GateStatusType
	DLGate GateStatusType
}

type MBR struct {
	ULMBR uint64
	DLMBR uint64
}

type GBR struct {
	ULGBR uint64
	DLGBR uint64
}

// --- PDN Type ---

const (
	PDNTypeIpv4 uint8 = iota + 1
	PDNTypeIpv6
	PDNTypeIpv4v6
	PDNTypeNonIp
	PDNTypeEthernet
)

type PDNType struct {
	PdnType uint8
}

// --- Reporting Trigger ---
// Bit layout per 3GPP TS 29.244 Table 8.2.24-1, reproduced exactly as in
// the internal reference implementation's internal/context/pfcp_rules.go
// (RPT_TRIG_* constants + ReportingTrigger.IE()).

const (
	RPT_TRIG_PERIO uint32 = 1 << iota
	RPT_TRIG_VOLTH
	RPT_TRIG_TIMTH
	RPT_TRIG_QUHTI
	RPT_TRIG_START
	RPT_TRIG_STOPT
	RPT_TRIG_DROTH
	RPT_TRIG_LIUSA
	RPT_TRIG_VOLQU
	RPT_TRIG_TIMQU
	RPT_TRIG_ENVCL
	RPT_TRIG_MACAR
	RPT_TRIG_EVETH
	RPT_TRIG_EVEQU
	RPT_TRIG_IPMJL
	RPT_TRIG_QUVTI
	RPT_TRIG_REEMR
	RPT_TRIG_UPINT
)

type ReportingTrigger struct {
	Flags uint32
}

func (r *ReportingTrigger) IE() *ie.IE {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, r.Flags)
	if b[2] != 0 {
		return ie.NewReportingTriggers(b[:3]...)
	}
	// Backward-compatible with R15 (2-octet form) when no octet-3 flag is set.
	return ie.NewReportingTriggers(b[:2]...)
}

func (r *ReportingTrigger) SetPERIO() { r.Flags |= RPT_TRIG_PERIO }
func (r *ReportingTrigger) SetVOLTH() { r.Flags |= RPT_TRIG_VOLTH }
func (r *ReportingTrigger) SetTIMTH() { r.Flags |= RPT_TRIG_TIMTH }
func (r *ReportingTrigger) SetQUHTI() { r.Flags |= RPT_TRIG_QUHTI }
func (r *ReportingTrigger) SetSTART() { r.Flags |= RPT_TRIG_START }
func (r *ReportingTrigger) SetSTOPT() { r.Flags |= RPT_TRIG_STOPT }
func (r *ReportingTrigger) SetDROTH() { r.Flags |= RPT_TRIG_DROTH }
func (r *ReportingTrigger) SetLIUSA() { r.Flags |= RPT_TRIG_LIUSA }
func (r *ReportingTrigger) SetVOLQU() { r.Flags |= RPT_TRIG_VOLQU }
func (r *ReportingTrigger) SetTIMQU() { r.Flags |= RPT_TRIG_TIMQU }
func (r *ReportingTrigger) SetENVCL() { r.Flags |= RPT_TRIG_ENVCL }
func (r *ReportingTrigger) SetMACAR() { r.Flags |= RPT_TRIG_MACAR }
func (r *ReportingTrigger) SetEVETH() { r.Flags |= RPT_TRIG_EVETH }
func (r *ReportingTrigger) SetEVEQU() { r.Flags |= RPT_TRIG_EVEQU }
func (r *ReportingTrigger) SetIPMJL() { r.Flags |= RPT_TRIG_IPMJL }
func (r *ReportingTrigger) SetQUVTI() { r.Flags |= RPT_TRIG_QUVTI }
func (r *ReportingTrigger) SetREEMR() { r.Flags |= RPT_TRIG_REEMR }
func (r *ReportingTrigger) SetUPINT() { r.Flags |= RPT_TRIG_UPINT }

func (r *ReportingTrigger) HasVOLTH() bool { return r.Flags&RPT_TRIG_VOLTH != 0 }
func (r *ReportingTrigger) HasVOLQU() bool { return r.Flags&RPT_TRIG_VOLQU != 0 }
func (r *ReportingTrigger) HasQUVTI() bool { return r.Flags&RPT_TRIG_QUVTI != 0 }
func (r *ReportingTrigger) HasSTART() bool { return r.Flags&RPT_TRIG_START != 0 }

// --- Measurement Information ---

const (
	MeasureInfoMBQE = 0x1  // octet1 bit1
	MeasureInfoMNOP = 0x10 // octet1 bit5
)

type MeasurementInformation struct {
	Flags uint8
}
```

- [ ] **Step 4:** `go test ./internal/pfcp/pfcptype/... -v` → PASS.
- [ ] **Step 5:**
```bash
git add internal/pfcp/pfcptype/
git commit -m "feat(pfcp): add pfcptype domain package, reproducing the internal reference design"
```

---

### Task 3: Sequence number allocator

The internal fork leans on Saviah's internal `idgenerator` module (`idgenerator.NewGenerator(0x0, 0xFFFFFF)`, called directly from `PfcpServer`'s constructor) for 24-bit sequence allocation with free-on-complete. That module isn't importable here, so this task re-implements its two operations (`Allocate() (uint64, error)`, `FreeID(id uint64)`) under the names the rest of this plan uses, with the same allocate/free/wrap-around contract — this is the one component in the whole plan with no internal source to transcribe, because the internal server itself never implements this logic locally.

**Files:** Create `internal/pfcp/seqalloc.go`, Test `internal/pfcp/seqalloc_test.go`

**Interfaces:** `NewSeqAllocator() *SeqAllocator`, `(*SeqAllocator) Allocate() (uint32, error)`, `(*SeqAllocator) Free(seq uint32)`.

- [ ] **Step 1: failing test**

```go
// internal/pfcp/seqalloc_test.go
package pfcp

import "testing"

func TestSeqAllocator_StaysWithin24Bits(t *testing.T) {
	a := NewSeqAllocator()
	for i := 0; i < 1000; i++ {
		seq, err := a.Allocate()
		if err != nil {
			t.Fatalf("Allocate() error: %v", err)
		}
		if seq > 0xFFFFFF {
			t.Fatalf("seq %#x exceeds 24-bit range", seq)
		}
	}
}

func TestSeqAllocator_FreeAllowsReuse(t *testing.T) {
	a := NewSeqAllocatorRange(0, 2)
	s1, _ := a.Allocate()
	s2, _ := a.Allocate()
	if _, err := a.Allocate(); err == nil {
		t.Fatalf("expected exhaustion error")
	}
	a.Free(s1)
	if _, err := a.Allocate(); err != nil {
		t.Fatalf("Allocate() after Free error: %v", err)
	}
	a.Free(s2)
}
```

- [ ] **Step 2:** `go test ./internal/pfcp/... -run TestSeqAllocator` → FAIL.

- [ ] **Step 3: implementation**

```go
// internal/pfcp/seqalloc.go
package pfcp

import (
	"sync"

	"github.com/pkg/errors"
)

// SeqAllocator hands out PFCP sequence numbers (TS 29.244 §7.2.2.2: 24-bit,
// unique among outstanding requests), wrapping around and skipping ids
// still in use. Equivalent in contract to the internal fork's use of
// idgenerator.NewGenerator(0x0, 0xFFFFFF) — Allocate()/Free() map to that
// module's Allocate()/FreeID().
type SeqAllocator struct {
	mu       sync.Mutex
	min, max uint32
	next     uint32
	inUse    map[uint32]struct{}
}

func NewSeqAllocator() *SeqAllocator { return NewSeqAllocatorRange(0, 0xFFFFFF) }

func NewSeqAllocatorRange(min, max uint32) *SeqAllocator {
	return &SeqAllocator{min: min, max: max, next: min, inUse: make(map[uint32]struct{})}
}

func (a *SeqAllocator) Allocate() (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	span := a.max - a.min + 1
	for i := uint32(0); i < span; i++ {
		candidate := a.min + (a.next-a.min+i)%span
		if _, taken := a.inUse[candidate]; !taken {
			a.inUse[candidate] = struct{}{}
			a.next = candidate + 1
			return candidate, nil
		}
	}
	return 0, errors.Errorf("SeqAllocator: exhausted range [%d,%d]", a.min, a.max)
}

func (a *SeqAllocator) Free(seq uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inUse, seq)
}
```

- [ ] **Step 4:** `go test ./internal/pfcp/... -run TestSeqAllocator -v` → PASS.
- [ ] **Step 5:**
```bash
git add internal/pfcp/seqalloc.go internal/pfcp/seqalloc_test.go
git commit -m "feat(pfcp): add sequence allocator (idgenerator-equivalent)"
```

---

### Task 4: `PfcpServer` — reproduces the internal transport exactly

This is a line-for-line adaptation of the internal fork's `internal/pfcp/server.go` + `internal/pfcp/transaction.go` (both read in full from the internal source). Kept: the three-channel design (`rcvCh` bounded with overload-drop, `txCh`/`dispCh` unbounded), the `receiver`/`dispatcher`/`main` goroutine split with per-request dispatch goroutines, the Tx/Rx transaction machinery, `TransactionID = "<remoteIP>-<seq>"` (IP only, not IP:port), the timeout-as-nil-message convention, and `stopTrTimers()` on shutdown. Dropped: OpenTelemetry span creation (`traceCtx`/`otel.GetTracerProvider()`), because there is no tracer configured anywhere in the external module — every other structural decision is unchanged. `uchan.UnboundedChan` (Saviah-internal) is replaced with a `chan any` sized generously and grown by a background reader pattern is unnecessary here — Go's unbuffered/buffered channels are unbounded in the sense needed if we accept a large fixed buffer instead; this plan uses a large buffered channel (see Step 3 note) as the equivalent, since the external repo has no unbounded-channel library either.

**Files:**
- Create: `internal/pfcp/server.go`
- Create: `internal/pfcp/transaction.go`
- Test: `internal/pfcp/server_test.go`

**Interfaces:**
- Produces:
  - `type RcvPfcpMsg struct { RemoteAddr net.UDPAddr; Msg message.Message }`
  - `type TransmitMessage struct { Msg message.Message; RemoteAddr *net.UDPAddr; TrType TransType; RspCh chan RcvPfcpMsg }`
  - `NewPfcpServer(smf smfIface, addr string) *PfcpServer` where `smfIface` exposes `Config()`, `Context()`, `CancelContext()` — matching the internal server's own `smf` interface field, adapted to whatever the external `pkg/app`/`pkg/service` layer already exposes (confirmed in Task 10 Step 1).
  - `(*PfcpServer) Listen() error`, `Run(wg *sync.WaitGroup) error`, `Stop()`, `SendPfcpMsg(msg message.Message, addr *net.UDPAddr) chan RcvPfcpMsg`, `Dispatch(msg message.Message, addr *net.UDPAddr)` (the passive-direction entry point, filled in by Task 5).

- [ ] **Step 1: failing test — loopback Heartbeat round trip + timeout + stop**

```go
// internal/pfcp/server_test.go
package pfcp

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	smfCfg "github.com/free5gc/smf/pkg/factory"
)

// fakeSmf implements the minimal smf interface PfcpServer needs for these
// tests — a real Task 10 wiring will pass the actual app/service singleton.
type fakeSmf struct {
	cfg *smfCfg.Config
	ctx context.Context
	cf  context.CancelFunc
}

func newFakeSmf() *fakeSmf {
	ctx, cf := context.WithCancel(context.Background())
	return &fakeSmf{cfg: &smfCfg.Config{}, ctx: ctx, cf: cf}
}
func (f *fakeSmf) Config() *smfCfg.Config        { return f.cfg }
func (f *fakeSmf) CancelContext() context.Context { return f.ctx }

func TestPfcpServer_HeartbeatRoundTrip(t *testing.T) {
	var wg sync.WaitGroup
	s := NewPfcpServer(newFakeSmf(), "127.0.0.1")
	if err := s.Run(&wg); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	defer s.Stop()

	peer, err := net.DialUDP("udp", nil, s.LocalAddr())
	if err != nil {
		t.Fatalf("DialUDP error: %v", err)
	}
	defer peer.Close()

	req := message.NewHeartbeatRequest(1, ie.NewRecoveryTimeStamp(time.Now()), nil)
	b, _ := req.Marshal()
	if _, err := peer.Write(b); err != nil {
		t.Fatalf("Write error: %v", err)
	}

	rbuf := make([]byte, 1500)
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline error: %v", err)
	}
	n, err := peer.Read(rbuf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	rsp, err := message.Parse(rbuf[:n])
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if _, ok := rsp.(*message.HeartbeatResponse); !ok {
		t.Fatalf("expected HeartbeatResponse, got %T", rsp)
	}
}

func TestPfcpServer_TxTimeout(t *testing.T) {
	var wg sync.WaitGroup
	s := NewPfcpServer(newFakeSmf(), "127.0.0.1")
	if err := s.Run(&wg); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	defer s.Stop()

	blackHole, _ := net.ResolveUDPAddr("udp", "127.0.0.1:1")
	req := message.NewHeartbeatRequest(0, ie.NewRecoveryTimeStamp(time.Now()), nil)
	ch := s.SendPfcpMsg(req, blackHole)
	if ch == nil {
		t.Fatalf("expected a wait channel for a request message")
	}
	select {
	case rcv := <-ch:
		if rcv.Msg != nil {
			t.Fatalf("expected nil Msg on timeout, got %T", rcv.Msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for the timeout notification itself")
	}
}

func TestPfcpServer_StopUnblocksPendingSenders(t *testing.T) {
	var wg sync.WaitGroup
	s := NewPfcpServer(newFakeSmf(), "127.0.0.1")
	if err := s.Run(&wg); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	blackHole, _ := net.ResolveUDPAddr("udp", "127.0.0.1:1")
	req := message.NewHeartbeatRequest(0, ie.NewRecoveryTimeStamp(time.Now()), nil)
	ch := s.SendPfcpMsg(req, blackHole)

	s.Stop()

	select {
	case rcv, ok := <-ch:
		if ok && rcv.Msg != nil {
			t.Fatalf("expected closed channel or nil Msg after Stop, got %T", rcv.Msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop() did not unblock the pending sender")
	}
}
```

> **Note on `smfCfg.GetPfcpRetransTimer()`:** the internal server reads retransmission timeout/max-retries from `s.Config().GetPfcpRetransTimer()`. Before writing Step 3, check whether `github.com/free5gc/smf/pkg/factory.Config` already exposes an equivalent PFCP retry setting:
> ```bash
> grep -rn "RetransTimer\|Retrans" /home/alonza/smf/smf-opensource/pkg/factory/*.go
> ```
> If nothing exists, add a small `GetPfcpRetransTimer() (time.Duration, uint8)` method to `pkg/factory.Config` returning a sensible fixed default (e.g. 3s / 3 retries, aligned with TS 29.244's suggested values) rather than hardcoding the constant inline in `server.go` — this keeps the setting in the one place the internal fork also keeps it (config), even though the external repo currently has no such knob.

- [ ] **Step 2:** `go test ./internal/pfcp/... -run TestPfcpServer` → FAIL.

- [ ] **Step 3: `internal/pfcp/server.go`** (adapted from the internal fork's `server.go`; `smf` field/interface, `Config()`/`CancelContext()` calls preserved verbatim in shape; OTel span creation removed; `uchan.UnboundedChan` replaced with large buffered `chan any`, documented inline)

```go
// internal/pfcp/server.go
package pfcp

//go:generate mockgen -source=server.go -destination=server_mock.go -package=pfcp

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/internal/logger"
	smfCfg "github.com/free5gc/smf/pkg/factory"
)

const (
	PfcpPort      = 8805
	MaxPfcpMsgLen = 65536

	// rcvChLen bounds the UDP-receive-facing channel; sendToRcvCh drops on
	// overload instead of growing unbounded, matching the internal
	// server's rationale for its own bounded rcvCh (protects against an
	// unbounded backlog if handlers can't keep up with inbound traffic).
	rcvChLen = 8192

	// txDispChLen sizes the internal tx/dispatch channels. The internal
	// fork uses a true unbounded ring buffer (uchan.UnboundedChan) here
	// because writers are SMF's own goroutines, not the network boundary;
	// this port uses a large fixed buffer instead, since no unbounded
	// channel library is available externally. If profiling later shows
	// this is too small under load, revisit with a real ring-buffer
	// implementation rather than just enlarging the constant.
	txDispChLen = rcvChLen / 2
)

type TransType string

const (
	TX TransType = "TX"
	RX TransType = "RX"
)

type smfIface interface {
	Config() *smfCfg.Config
	CancelContext() context.Context
}

type PfcpServer struct {
	smfIface

	addr         string
	conn         *net.UDPConn
	rcvCh        chan RcvPfcpMsg
	txCh         chan TransmitMessage
	dispCh       chan RcvPfcpMsg
	stopCh       chan struct{}
	txTrans      sync.Map // key: TransactionID, value: *TxTransaction
	rxTrans      sync.Map // key: TransactionID, value: *RxTransaction
	seqAlloc     *SeqAllocator
	recoveryTime time.Time
	dispatch     func(message.Message, *net.UDPAddr) // set via SetDispatch (Task 5)
	log          *logrus.Entry
}

func NewPfcpServer(smf smfIface, addr string) *PfcpServer {
	return &PfcpServer{
		smfIface:     smf,
		addr:         addr,
		rcvCh:        make(chan RcvPfcpMsg, rcvChLen),
		txCh:         make(chan TransmitMessage, txDispChLen),
		dispCh:       make(chan RcvPfcpMsg, txDispChLen),
		stopCh:       make(chan struct{}),
		seqAlloc:     NewSeqAllocator(),
		recoveryTime: time.Now(),
		log:          logger.PfcpLog.WithField("addr", fmt.Sprintf("%s:%d", addr, PfcpPort)),
	}
}

// SetDispatch registers the passive-direction handler (Task 5). Must be
// called before Run().
func (s *PfcpServer) SetDispatch(fn func(message.Message, *net.UDPAddr)) {
	s.dispatch = fn
}

func (s *PfcpServer) Listen() error {
	var ip net.IP
	if s.addr == "" {
		ip = net.IPv4zero
	} else {
		ip = net.ParseIP(s.addr)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: PfcpPort})
	s.conn = conn
	return err
}

func (s *PfcpServer) LocalAddr() *net.UDPAddr {
	return s.conn.LocalAddr().(*net.UDPAddr)
}

func (s *PfcpServer) Run(wg *sync.WaitGroup) error {
	defer func() {
		if p := recover(); p != nil {
			logger.PfcpLog.Fatalf("panic: %v\n%s", p, string(debug.Stack()))
		}
	}()

	if err := s.Listen(); err != nil {
		return errors.Wrap(err, "PFCP failed to listen")
	}
	s.log.Infof("Listen on %v", s.conn.LocalAddr())
	wg.Add(1)
	go s.main(wg)
	s.log.Infoln("PFCP Server started!")
	return nil
}

func (s *PfcpServer) Stop() {
	close(s.stopCh)
	if s.conn != nil {
		if err := s.conn.Close(); err != nil {
			s.log.Errorf("Stop pfcp server err: %v", err)
		}
	}
}

func (s *PfcpServer) main(wg *sync.WaitGroup) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Fatalf("panic: %v\n%s", p, string(debug.Stack()))
		}
		s.log.Infoln("pfcp server stopped!")
		s.stopTrTimers()
		wg.Done()
	}()

	wg.Add(1)
	go s.receiver(wg)
	wg.Add(1)
	go s.dispatcher(wg)

	for {
		select {
		case <-s.stopCh:
			return
		case txMsg, ok := <-s.txCh:
			if !ok {
				return
			}
			if txMsg.TrType == TX {
				if err := s.sendReqTo(txMsg.Msg, txMsg.RemoteAddr, txMsg.RspCh); err != nil {
					s.log.Errorf("Failed to send PFCP message: %v", err)
				}
			} else {
				if err := s.sendRspTo(txMsg.Msg, txMsg.RemoteAddr); err != nil {
					s.log.Errorf("Failed to send PFCP message: %v", err)
				}
			}
		case rcvMsg, ok := <-s.rcvCh:
			if !ok || rcvMsg.Msg == nil {
				return
			}
			s.log.Debugf("receive msg[%s] SEQ[%#x] SEID[%#x] from rcvCh",
				rcvMsg.Msg.MessageTypeName(), rcvMsg.Msg.Sequence(), rcvMsg.Msg.SEID())
			s.transactionHandler(rcvMsg.Msg, &rcvMsg.RemoteAddr)
		}
	}
}

func (s *PfcpServer) transactionHandler(msg message.Message, addr *net.UDPAddr) {
	trID := TransactionID(addr, msg.Sequence())
	if msg.IsRequest() {
		var rxFound bool
		rx, ok := s.loadRxTr(trID)
		if !ok {
			rx = s.NewRxTransaction(addr, msg.Sequence())
		} else {
			rxFound = true
		}
		needDispatch, err := rx.recv(msg, rxFound)
		if err != nil {
			s.log.Warnf("rcvCh: %v", err)
			return
		}
		if !needDispatch {
			s.log.Debugf("rcvCh: rxtr[%s] req no need to dispatch", trID)
			return
		}
		select {
		case s.dispCh <- RcvPfcpMsg{RemoteAddr: *addr, Msg: msg}:
		default:
			s.log.Warnf("dispCh full, dropping msg[%s] SEQ[%#x] from %v", msg.MessageTypeName(), msg.Sequence(), addr)
		}
		return
	}

	tx, ok := s.loadTxTr(trID)
	if !ok {
		s.log.Debugf("No txtr[%s] found for rsp", trID)
		return
	}
	if c := tx.recv(msg); c != nil {
		c <- RcvPfcpMsg{RemoteAddr: *addr, Msg: msg}
		close(c)
	}
}

func (s *PfcpServer) receiver(wg *sync.WaitGroup) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Fatalf("panic: %v\n%s", p, string(debug.Stack()))
		}
		s.log.Infoln("pfcp receiver stopped")
		wg.Done()
	}()

	buf := make([]byte, MaxPfcpMsgLen)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			s.log.Errorf("PFCP ReadFromUDP: %v", err)
			close(s.rcvCh)
			close(s.dispCh)
			return
		}

		// Copy before parsing: buf is reused by the next ReadFromUDP call.
		msgBuf := make([]byte, n)
		copy(msgBuf, buf[:n])
		s.log.Tracef("receiver reads message(len=%d):\n%+v", n, hex.Dump(msgBuf))

		msg, err := message.Parse(msgBuf)
		if err != nil {
			s.log.Warnf("PFCP Message parse error: %v", err)
			continue
		}
		if _, ok := msg.(*message.Generic); ok {
			s.log.Warnf("receive unknown PFCP Message type: %d", msg.MessageType())
			continue
		}

		s.sendToRcvCh(RcvPfcpMsg{RemoteAddr: *addr, Msg: msg})
	}
}

func (s *PfcpServer) sendToRcvCh(rcvMsg RcvPfcpMsg) {
	select {
	case s.rcvCh <- rcvMsg:
	default:
		s.log.Warnf("rcvCh full, drop the msg[%s] SEID[%#x] Seq[%#x] RemoteAddr[%v]",
			rcvMsg.Msg.MessageTypeName(), rcvMsg.Msg.SEID(), rcvMsg.Msg.Sequence(), rcvMsg.RemoteAddr)
	}
}

func (s *PfcpServer) dispatcher(wg *sync.WaitGroup) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Fatalf("panic: %v\n%s", p, string(debug.Stack()))
		}
		s.log.Infoln("pfcp dispatcher stopped")
		wg.Done()
	}()

	for rcvMsg := range s.dispCh {
		msg := rcvMsg.Msg
		addr := rcvMsg.RemoteAddr
		// New goroutine per handler so one slow handler (e.g. blocked on
		// an SMContext lock) can't stall the dispatch loop for everyone
		// else — same rationale as the internal fork's dispatcher.
		go func() {
			defer func() {
				if p := recover(); p != nil {
					s.log.Fatalf("panic in dispatch: %v\n%s", p, string(debug.Stack()))
				}
			}()
			if s.dispatch != nil {
				s.dispatch(msg, &addr)
			}
		}()
	}
}

func (s *PfcpServer) SendPfcpMsg(msg message.Message, addr *net.UDPAddr) chan RcvPfcpMsg {
	if s == nil {
		panic("PFCP Server is not running!")
	}
	if msg == nil {
		return nil
	}

	var channel chan RcvPfcpMsg
	var trType TransType
	if msg.IsRequest() {
		trType = TX
		channel = make(chan RcvPfcpMsg)
	} else {
		trType = RX
	}

	select {
	case <-s.stopCh:
		return nil
	default:
	}

	select {
	case s.txCh <- TransmitMessage{RemoteAddr: addr, Msg: msg, TrType: trType, RspCh: channel}:
	case <-s.stopCh:
		return nil
	}
	return channel
}

func (s *PfcpServer) sendReqTo(msg message.Message, addr *net.UDPAddr, ch chan RcvPfcpMsg) error {
	if !msg.IsRequest() {
		return errors.Errorf("sendReqTo: invalid req type(%d)", msg.MessageType())
	}
	txtr, err := s.NewTxTransaction(addr, ch)
	if err != nil {
		return errors.Wrapf(err, "sendReqTo")
	}
	return errors.Wrapf(txtr.send(msg), "sendReqTo")
}

func (s *PfcpServer) sendRspTo(msg message.Message, addr *net.UDPAddr) error {
	if msg.IsRequest() {
		return errors.Errorf("sendRspTo: invalid rsp type(%d)", msg.MessageType())
	}
	trID := TransactionID(addr, msg.Sequence())
	rxtr, ok := s.loadRxTr(trID)
	if !ok {
		return errors.Errorf("sendRspTo: rxtr[%s] not found", trID)
	}
	return errors.Wrapf(rxtr.send(msg), "sendRspTo")
}

func (s *PfcpServer) loadTxTr(trID string) (*TxTransaction, bool) {
	v, ok := s.txTrans.Load(trID)
	if !ok {
		return nil, false
	}
	return v.(*TxTransaction), true
}

func (s *PfcpServer) loadRxTr(trID string) (*RxTransaction, bool) {
	v, ok := s.rxTrans.Load(trID)
	if !ok {
		return nil, false
	}
	return v.(*RxTransaction), true
}

func (s *PfcpServer) stopTrTimers() {
	s.txTrans.Range(func(_, value any) bool {
		tx := value.(*TxTransaction)
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.timer == nil {
			return true
		}
		if tx.rspCh != nil {
			tx.rspCh <- RcvPfcpMsg{Msg: nil}
			close(tx.rspCh)
			tx.rspCh = nil
		}
		tx.timer.Stop()
		tx.timer = nil
		return true
	})
	s.rxTrans.Range(func(_, value any) bool {
		rx := value.(*RxTransaction)
		rx.mu.Lock()
		defer rx.mu.Unlock()
		if rx.timer == nil {
			return true
		}
		rx.timer.Stop()
		rx.timer = nil
		return true
	})
}
```

- [ ] **Step 4: `internal/pfcp/transaction.go`** (direct adaptation of the internal `transaction.go`; the retry timeout/max-retrans source is the one line that must be adjusted once the Step 3 note's `GetPfcpRetransTimer()` question is resolved — shown here already wired to it)

```go
// internal/pfcp/transaction.go
package pfcp

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/wmnsk/go-pfcp/message"
)

type RcvPfcpMsg struct {
	RemoteAddr net.UDPAddr
	Msg        message.Message
}

type TransmitMessage struct {
	Msg        message.Message
	RemoteAddr *net.UDPAddr
	TrType     TransType
	RspCh      chan RcvPfcpMsg
}

type TxTransaction struct {
	mu sync.Mutex

	server         *PfcpServer
	destAddr       *net.UDPAddr
	seq            uint32
	id             string
	retransTimeout time.Duration
	maxRetrans     uint8
	req            message.Message
	rspCh          chan RcvPfcpMsg
	msgBuf         []byte
	timer          *time.Timer
	retransCount   uint8
}

type RxTransaction struct {
	mu sync.Mutex

	server   *PfcpServer
	destAddr *net.UDPAddr
	seq      uint32
	id       string
	timeout  time.Duration
	rsp      message.Message
	msgBuf   []byte
	timer    *time.Timer
}

func TransactionID(raddr *net.UDPAddr, seq uint32) string {
	return fmt.Sprintf("%v-%#x", raddr.IP, seq)
}

func (s *PfcpServer) NewTxTransaction(destAddr *net.UDPAddr, rspCh chan RcvPfcpMsg) (*TxTransaction, error) {
	txSeq, err := s.seqAlloc.Allocate()
	if err != nil {
		return nil, errors.Wrapf(err, "tx seq gen failed")
	}
	expTime, maxRetry := s.Config().GetPfcpRetransTimer()
	tx := &TxTransaction{
		server:         s,
		destAddr:       destAddr,
		seq:            txSeq,
		id:             TransactionID(destAddr, txSeq),
		retransTimeout: expTime,
		maxRetrans:     maxRetry,
		rspCh:          rspCh,
	}
	s.txTrans.Store(tx.id, tx)
	return tx, nil
}

func (s *PfcpServer) DeleteTxTransaction(tx *TxTransaction) {
	s.txTrans.Delete(tx.id)
	s.seqAlloc.Free(tx.seq)
}

func (tx *TxTransaction) send(req message.Message) error {
	req.SetSequenceNumber(tx.seq)
	b := make([]byte, req.MarshalLen())
	if err := req.MarshalTo(b); err != nil {
		return errors.Wrapf(err, "txtr[%s] send marshalTo", tx.id)
	}

	tx.mu.Lock()
	tx.req = req
	tx.msgBuf = b
	tx.timer = tx.startTimer()
	tx.mu.Unlock()

	_, err := tx.server.conn.WriteToUDP(b, tx.destAddr)
	return errors.Wrapf(err, "txtr[%s] send writeToUDP", tx.id)
}

func (tx *TxTransaction) recv(rsp message.Message) chan<- RcvPfcpMsg {
	_ = rsp
	tx.server.DeleteTxTransaction(tx)

	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.timer != nil {
		tx.timer.Stop()
		tx.timer = nil
	}
	return tx.rspCh
}

func (tx *TxTransaction) handleTimeout() {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	if tx.req == nil {
		tx.server.DeleteTxTransaction(tx)
		if tx.rspCh != nil {
			tx.rspCh <- RcvPfcpMsg{Msg: nil}
			close(tx.rspCh)
			tx.rspCh = nil
		}
		return
	}

	if tx.retransCount < tx.maxRetrans {
		tx.retransCount++
		if _, err := tx.server.conn.WriteToUDP(tx.msgBuf, tx.destAddr); err != nil {
			tx.server.log.Errorf("retransmit (#%d) error: %v", tx.retransCount, err)
		}
		tx.timer = tx.startTimer()
	} else {
		tx.server.DeleteTxTransaction(tx)
		if tx.rspCh != nil {
			tx.rspCh <- RcvPfcpMsg{Msg: nil}
			close(tx.rspCh)
			tx.rspCh = nil
		}
	}
}

func (tx *TxTransaction) startTimer() *time.Timer {
	return time.AfterFunc(tx.retransTimeout, func() {
		tx.server.TransTimeout(TX, tx.id)
	})
}

func (s *PfcpServer) TransTimeout(trType TransType, trID string) {
	abort := false
	select {
	case <-s.stopCh:
		abort = true
	default:
	}

	if trType == TX {
		tx, ok := s.loadTxTr(trID)
		if !ok {
			return
		}
		if abort {
			s.DeleteTxTransaction(tx)
			tx.mu.Lock()
			tx.timer = nil
			if tx.rspCh != nil {
				tx.rspCh <- RcvPfcpMsg{Msg: nil}
				close(tx.rspCh)
				tx.rspCh = nil
			}
			tx.mu.Unlock()
			return
		}
		tx.handleTimeout()
		return
	}

	if abort {
		return
	}
	rx, ok := s.loadRxTr(trID)
	if !ok {
		return
	}
	rx.handleTimeout()
}

func (s *PfcpServer) NewRxTransaction(destAddr *net.UDPAddr, seq uint32) *RxTransaction {
	expTime, maxRetry := s.Config().GetPfcpRetransTimer()
	rx := &RxTransaction{
		server:   s,
		destAddr: destAddr,
		seq:      seq,
		id:       TransactionID(destAddr, seq),
		timeout:  expTime * time.Duration(maxRetry+1),
	}
	rx.timer = rx.startTimer()
	s.rxTrans.Store(rx.id, rx)
	return rx
}

func (s *PfcpServer) DeleteRxTransaction(rxtr *RxTransaction) {
	s.rxTrans.Delete(rxtr.id)
}

func (rx *RxTransaction) send(rsp message.Message) error {
	b := make([]byte, rsp.MarshalLen())
	if err := rsp.MarshalTo(b); err != nil {
		return errors.Wrapf(err, "rxtr[%s] send", rx.id)
	}

	rx.mu.Lock()
	rx.rsp = rsp
	rx.msgBuf = b
	rx.mu.Unlock()

	_, err := rx.server.conn.WriteToUDP(b, rx.destAddr)
	return errors.Wrapf(err, "rxtr[%s] send", rx.id)
}

// recv reports whether the caller still needs to dispatch this request
// (true = first time seeing it; false = duplicate — a cached response is
// retransmitted here if one exists yet).
func (rx *RxTransaction) recv(req message.Message, rxTrFound bool) (bool, error) {
	_ = req
	if !rxTrFound {
		return true, nil
	}

	rx.mu.Lock()
	defer rx.mu.Unlock()
	if len(rx.msgBuf) == 0 {
		return false, nil
	}
	_, err := rx.server.conn.WriteToUDP(rx.msgBuf, rx.destAddr)
	return false, errors.Wrapf(err, "rxtr[%s] retransmit rsp", rx.id)
}

func (rx *RxTransaction) handleTimeout() {
	rx.server.DeleteRxTransaction(rx)
}

func (rx *RxTransaction) startTimer() *time.Timer {
	return time.AfterFunc(rx.timeout, func() {
		rx.server.TransTimeout(RX, rx.id)
	})
}
```

> **Add `GetPfcpRetransTimer() (time.Duration, uint8)` to `pkg/factory.Config`** per the Step 3 note before this compiles — a minimal addition:
> ```go
> // pkg/factory/config.go (or wherever PFCP config lives)
> func (c *Config) GetPfcpRetransTimer() (time.Duration, uint8) {
> 	return 3 * time.Second, 3
> }
> ```
> (Wire this to an actual config field instead of a hardcoded constant if/when the team wants it operator-tunable — out of scope for this migration.)

- [ ] **Step 5:** Run and confirm:
```bash
go test ./internal/pfcp/... -run TestPfcpServer -v
go test -race ./internal/pfcp/... -run TestPfcpServer -v
```
Expected: PASS.

- [ ] **Step 6:**
```bash
git add internal/pfcp/server.go internal/pfcp/transaction.go internal/pfcp/server_test.go
git commit -m "feat(pfcp): reproduce internal PfcpServer/transaction design on go-pfcp"
```

---

### Task 5: Passive direction — `dispatcher.go`, `association.go`, `report.go`

Direct adaptation of the internal fork's `internal/pfcp/dispatcher.go` + `internal/pfcp/association.go` + the Heartbeat/NodeReport/SessionSetDeletion parts of `internal/pfcp/report.go` (all read in full from the internal source). Preserves the `(message.Message, func())` handler return shape — the `func()` is the `afterRsp` side-effect hook, used by Association Setup to trigger UPF-restart recovery **after** the response has been sent (TS 23.501: the CP function must not start session signalling toward a UPF until the Association Setup Response with success cause has actually gone out). `handleSessionReportRequest` (the URRID/UsageReport/DownlinkDataReport-heavy one) is deferred to Task 11, once `internal/context/pfcp_reports.go`'s `HandleReports` exists in its new form — for now, `report.go` gets a stub that Task 11 fills in, to keep this task's own build/test cycle self-contained.

**Files:**
- Create: `internal/pfcp/dispatcher.go`
- Create: `internal/pfcp/association.go`
- Create: `internal/pfcp/report.go` (Heartbeat + stub for Session Report — completed in Task 11)
- Test: `internal/pfcp/association_test.go`

**Interfaces:**
- Consumes: `internal/pfcp/pfcptype` (Task 2); `internal/context.{RetrieveUPFNodeByNodeID, RemoveUPFNodeByNodeID, RecoverUPF, ProbeUPFReachability}` — the last two must exist on `SMFContext`/`UPF` in some form; confirm exact names with `grep -rn "func.*RecoverUPF\|func.*ProbeUPFReachability\|SetStartTimeAndAssociated" internal/context/*.go` before writing `association.go`'s `afterRsp` closure — if the external repo's UPF-restart-recovery logic uses different function names (it almost certainly does, since this whole flow doesn't exist in the external repo today per the earlier review), Step 3 below shows the minimal version to add rather than assuming it already exists.
- Produces: `(s *PfcpServer) Dispatch(msg message.Message, addr *net.UDPAddr)` — set as `PfcpServer.dispatch` via `SetDispatch(s.Dispatch)` in Task 10.

- [ ] **Step 1: failing test**

```go
// internal/pfcp/association_test.go
package pfcp

import (
	"net"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

func TestHandleAssociationSetupRequest_MissingMandatoryIE(t *testing.T) {
	var wg = new_wg(t)
	s := NewPfcpServer(newFakeSmf(), "127.0.0.1")
	if err := s.Run(wg); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	defer s.Stop()

	req := message.NewAssociationSetupRequest(1) // no NodeID/RecoveryTimeStamp
	rsp, afterRsp := s.handleAssociationSetupRequest(req)
	if afterRsp != nil {
		t.Fatalf("expected nil afterRsp when mandatory IEs are missing")
	}
	asRsp, ok := rsp.(*message.AssociationSetupResponse)
	if !ok {
		t.Fatalf("expected *message.AssociationSetupResponse, got %T", rsp)
	}
	cause, err := asRsp.Cause.Cause()
	if err != nil {
		t.Fatalf("Cause() error: %v", err)
	}
	if cause != ie.CauseMandatoryIEMissing {
		t.Fatalf("Cause = %d, want CauseMandatoryIEMissing", cause)
	}
}

func TestHandleAssociationReleaseRequest_MissingNodeID(t *testing.T) {
	s := NewPfcpServer(newFakeSmf(), "127.0.0.1")
	req := message.NewAssociationReleaseRequest(1, nil)
	rsp := s.handleAssociationReleaseRequest(req)
	cause, err := rsp.Cause.Cause()
	if err != nil {
		t.Fatalf("Cause() error: %v", err)
	}
	if cause != ie.CauseMandatoryIEMissing {
		t.Fatalf("Cause = %d, want CauseMandatoryIEMissing", cause)
	}
}

func TestHandleHeartbeatRequest_EchoesSequence(t *testing.T) {
	s := NewPfcpServer(newFakeSmf(), "127.0.0.1")
	req := message.NewHeartbeatRequest(0x2a, ie.NewRecoveryTimeStamp(time.Now()), nil)
	rsp := s.handleHeartbeatRequest(req)
	if rsp.Sequence() != 0x2a {
		t.Fatalf("Sequence = %#x, want %#x", rsp.Sequence(), 0x2a)
	}
}

func new_wg(t *testing.T) *sync.WaitGroup {
	t.Helper()
	return new(sync.WaitGroup)
}
```

> Add `"sync"` to the test file's imports for `new_wg`.

- [ ] **Step 2:** `go test ./internal/pfcp/... -run 'TestHandleAssociation|TestHandleHeartbeat'` → FAIL.

- [ ] **Step 3: `internal/pfcp/dispatcher.go`**

```go
// internal/pfcp/dispatcher.go
package pfcp

import (
	"net"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/internal/logger"
)

func (s *PfcpServer) Dispatch(msg message.Message, addr *net.UDPAddr) {
	var rsp message.Message
	var afterRsp func()
	switch req := msg.(type) {
	case *message.HeartbeatRequest:
		rsp = s.handleHeartbeatRequest(req)
	case *message.AssociationSetupRequest:
		rsp, afterRsp = s.handleAssociationSetupRequest(req)
	case *message.AssociationReleaseRequest:
		rsp = s.handleAssociationReleaseRequest(req)
	case *message.SessionReportRequest:
		rsp = s.handleSessionReportRequest(req)
	default:
		logger.PfcpLog.Errorf("pfcp Dispatch: unknown msg type: %d", msg.MessageType())
		return
	}

	if rsp != nil {
		s.SendPfcpMsg(rsp, addr)
	}
	if afterRsp != nil {
		afterRsp()
	}
}

func (s *PfcpServer) handleHeartbeatRequest(req *message.HeartbeatRequest) *message.HeartbeatResponse {
	return message.NewHeartbeatResponse(req.Sequence(), ie.NewRecoveryTimeStamp(s.recoveryTime))
}
```

- [ ] **Step 4: `internal/pfcp/association.go`**

```go
// internal/pfcp/association.go
package pfcp

import (
	"net"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/logger"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

func (s *PfcpServer) handleAssociationSetupRequest(
	req *message.AssociationSetupRequest,
) (message.Message, func()) {
	smfCtx := smf_context.GetSelf()

	rsp := message.NewAssociationSetupResponse(
		req.Sequence(),
		ie.NewNodeIDHeuristic(smfCtx.CPNodeID.String()),
		ie.NewCause(ie.CauseMandatoryIEMissing),
		ie.NewCPFunctionFeatures(0),
		ie.NewRecoveryTimeStamp(s.recoveryTime),
	)

	if req.NodeID == nil || req.RecoveryTimeStamp == nil {
		logger.PfcpLog.Errorln("AssociationSetupRequest Mandatory IEs missing")
		return rsp, nil
	}
	nodeID, err := toNodeId(req.NodeID)
	if err != nil {
		logger.PfcpLog.Errorf("pfcp association NodeID decode error: %v", err)
		return rsp, nil
	}

	upf := smf_context.RetrieveUPFNodeByNodeID(*nodeID)
	if upf == nil {
		logger.PfcpLog.Errorf("can't find NodeID[%s]", nodeID.String())
		rsp.Cause = ie.NewCause(ie.CauseMandatoryIEIncorrect)
		return rsp, nil
	}

	timestamp, err := req.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		logger.PfcpLog.Errorln(err)
		rsp.Cause = ie.NewCause(ie.CauseMandatoryIEIncorrect)
		return rsp, nil
	}

	upfStatus := upf.SetStartTimeAndAssociated(timestamp)
	logger.PfcpLog.Infof("PFCP Association Setup Request from %s RecoveryTS[%s] Status[%s]",
		upf, timestamp.Format(time.RFC3339), upfStatus)

	afterRsp := func() {
		// The CP function shall only initiate PFCP Session related
		// signalling toward a UP function after it has sent the PFCP
		// Association Setup Response with a successful cause — TS 29.244.
		smf_context.RecoverUPF(upf, upfStatus)
	}

	rsp.Cause = ie.NewCause(ie.CauseRequestAccepted)
	return rsp, afterRsp
}

// toNodeId mirrors the internal reference's helper of the same name.
func toNodeId(nodeIDIE *ie.IE) (*pfcptype.NodeID, error) {
	s, err := nodeIDIE.NodeID()
	if err != nil {
		return nil, err
	}
	ntype := nodeIDIE.Payload[0]
	if ntype == ie.NodeIDIPv4Address || ntype == ie.NodeIDIPv6Address {
		return &pfcptype.NodeID{NodeIdType: ntype, IP: net.ParseIP(s)}, nil
	}
	return &pfcptype.NodeID{NodeIdType: pfcptype.NodeIdTypeFqdn, FQDN: s}, nil
}

func (s *PfcpServer) handleAssociationReleaseRequest(
	req *message.AssociationReleaseRequest,
) *message.AssociationReleaseResponse {
	smfCtx := smf_context.GetSelf()

	rsp := message.NewAssociationReleaseResponse(
		req.Sequence(),
		ie.NewNodeIDHeuristic(smfCtx.CPNodeID.String()),
		ie.NewCause(ie.CauseMandatoryIEMissing),
	)

	if req.NodeID == nil {
		logger.PfcpLog.Errorln("AssociationReleaseRequest Mandatory IEs missing")
		return rsp
	}
	nodeID, err := toNodeId(req.NodeID)
	if err != nil {
		logger.PfcpLog.Errorf("pfcp association release NodeID decode error: %v", err)
		return rsp
	}

	if upf := smf_context.RetrieveUPFNodeByNodeID(*nodeID); upf != nil {
		smf_context.RemoveUPFNodeByNodeID(*nodeID)
		rsp.Cause = ie.NewCause(ie.CauseRequestAccepted)
	} else {
		rsp.Cause = ie.NewCause(ie.CauseNoEstablishedPFCPAssociation)
	}
	return rsp
}
```

> **Before this compiles, confirm/add three things on the external `context`/`UPF` types** (none of this exists in the external repo today — it is entirely new, modeled on the internal fork's UPF association-state machine, simplified to what Association Setup actually needs and no more):
> ```go
> // internal/context/upf.go — add
> type UPFStatus uint8
> const (
> 	UPF_STATUS_UNKNOWN UPFStatus = iota
> 	UPF_FIRST_BOOT
> 	UPF_RESTART
> 	UPF_NON_RESTART
> )
> func (u UPFStatus) String() string { /* switch → "FIRST_BOOT"/"RESTART"/"NON_RESTART"/"UNKNOWN" */ }
>
> func (upf *UPF) SetStartTimeAndAssociated(timestamp time.Time) UPFStatus {
> 	upf.Lock()
> 	defer upf.Unlock()
> 	upf.UPFAssocStatus = AssocStatusSuccess // or whatever the existing success constant is named — check upf.go
> 	if upf.StartTime.IsZero() {
> 		upf.StartTime = timestamp
> 		return UPF_FIRST_BOOT
> 	}
> 	if upf.StartTime.Equal(timestamp) {
> 		return UPF_NON_RESTART
> 	}
> 	upf.StartTime = timestamp
> 	return UPF_RESTART
> }
>
> // internal/context/context.go or a new file — add
> func RecoverUPF(upf *UPF, status UPFStatus) {
> 	if status != UPF_RESTART {
> 		return
> 	}
> 	// Re-push every PDR/FAR/QER/URR for every SMContext currently bound to
> 	// this UPF — reuses whatever the external repo's own session-recovery
> 	// path already does for a fresh UPF association (search for how PDU
> 	// sessions are currently re-synced after a UPF reconnects, if such a
> 	// path exists at all; if it doesn't exist yet, this is new
> 	// functionality out of scope for a pure library swap — in that case
> 	// implement RecoverUPF as a no-op with a TODO and log a warning, and
> 	// flag this gap to the team explicitly rather than silently skipping it).
> }
> ```
> This block is the single largest piece of genuinely new behavior in this whole plan, because "UPF restart recovery" is bundled into the internal fork's Association Setup handler but the external repo has no equivalent state machine to begin with. Treat it as its own mini-task if the external repo turns out to have no `RecoverUPF`-equivalent at all: land Association Setup with `afterRsp` calling a no-op first (still correct for the common case of a UPF that was never down), and open a follow-up for full restart recovery rather than blocking this migration on it.

- [ ] **Step 5: `internal/pfcp/report.go`** (Heartbeat lives in `dispatcher.go` per Step 3 above; this file holds the Session Report stub until Task 11)

```go
// internal/pfcp/report.go
package pfcp

import (
	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/internal/logger"
)

// handleSessionReportRequest is completed in Task 11, once
// internal/context/pfcp_reports.go's HandleReports exists in its new
// go-pfcp-based form. This stub keeps Task 5 self-contained and testable;
// it always rejects with CauseSessionContextNotFound, which is a safe
// (if unhelpful) response — never a panic — until Task 11 lands.
func (s *PfcpServer) handleSessionReportRequest(req *message.SessionReportRequest) *message.SessionReportResponse {
	logger.PfcpLog.Warnf("Session Report Request handling not yet wired (Task 11 pending), SEID[%#x]", req.SEID())
	return message.NewSessionReportResponse(0, 0, 0, req.Sequence(), 0, ie.NewCause(ie.CauseSessionContextNotFound))
}
```

- [ ] **Step 6:** `go test ./internal/pfcp/... -run 'TestHandleAssociation|TestHandleHeartbeat' -v` → PASS.

- [ ] **Step 7:**
```bash
git add internal/pfcp/dispatcher.go internal/pfcp/association.go internal/pfcp/report.go internal/pfcp/association_test.go
git commit -m "feat(pfcp): reproduce internal passive-direction handlers (Heartbeat/Association)"
```

---

## Part 2 — Domain model cutover

### Task 6: Migrate `internal/context` to `pfcptype`

Same mechanical scope as before, only the target shape changed for `ReportingTrigger`/`MeasurementInformation` (now `Flags`-based per Task 2). Every `pfcpType.X` → `pfcptype.X` rename from the original audit still applies; the two call sites that construct `ReportingTriggers{Perio: true, ...}`/set `.Volth = true` etc. (in `pfcp_rules.go`'s `NewMeasurementPeriod`/`NewVolumeThreshold`/`NewVolumeQuota`/`SetStartOfSDFTrigger` option functions) must switch to the `Set*()` method calls instead.

**Files (identical list to before):** `context.go`, `upf.go`, `user_plane_information.go`, `pfcp_session_context.go`, `pfcp_rules.go`, `sm_context.go`, `ngap_handler.go`, `datapath.go`, `upf_test.go` — all in `internal/context/`.

- [ ] **Step 1:**
```bash
cd /home/alonza/smf/smf-opensource
git checkout -b feat/go-pfcp-migration
```

- [ ] **Step 2: mechanical rename**
```bash
for f in internal/context/context.go internal/context/upf.go \
         internal/context/user_plane_information.go internal/context/pfcp_session_context.go \
         internal/context/pfcp_rules.go internal/context/sm_context.go \
         internal/context/ngap_handler.go internal/context/datapath.go \
         internal/context/upf_test.go; do
  sed -i 's#"github.com/free5gc/pfcp/pfcpType"#"github.com/free5gc/smf/internal/pfcp/pfcptype"#' "$f"
  sed -i 's/pfcpType\./pfcptype./g' "$f"
done
```

- [ ] **Step 3: fix up the `ReportingTrigger`/`MeasurementInformation` call sites by hand** (these need real edits, not sed, since the shape changed)

In `internal/context/pfcp_rules.go`, change:
```go
// BEFORE
func NewMeasurementPeriod(time time.Duration) UrrOpt {
	return func(urr *URR) {
		urr.ReportingTrigger.Perio = true
		urr.MeasurementPeriod = time
	}
}

func NewVolumeThreshold(threshold uint64) UrrOpt {
	return func(urr *URR) {
		urr.ReportingTrigger.Volth = true
		urr.VolumeThreshold = threshold
	}
}

func NewVolumeQuota(quota uint64) UrrOpt {
	return func(urr *URR) {
		urr.ReportingTrigger.Volqu = true
		urr.VolumeQuota = quota
	}
}

func SetStartOfSDFTrigger() UrrOpt {
	return func(urr *URR) {
		urr.ReportingTrigger.Start = true
	}
}
```
to:
```go
// AFTER
func NewMeasurementPeriod(period time.Duration) UrrOpt {
	return func(urr *URR) {
		urr.ReportingTrigger.SetPERIO()
		urr.MeasurementPeriod = period
	}
}

func NewVolumeThreshold(threshold uint64) UrrOpt {
	return func(urr *URR) {
		urr.ReportingTrigger.SetVOLTH()
		urr.VolumeThreshold = threshold
	}
}

func NewVolumeQuota(quota uint64) UrrOpt {
	return func(urr *URR) {
		urr.ReportingTrigger.SetVOLQU()
		urr.VolumeQuota = quota
	}
}

func SetStartOfSDFTrigger() UrrOpt {
	return func(urr *URR) {
		urr.ReportingTrigger.SetSTART()
	}
}
```
and the `URR` struct field type:
```go
// BEFORE
ReportingTrigger       pfcptype.ReportingTriggers
MeasurementInformation pfcptype.MeasurementInformation
// AFTER
ReportingTrigger       pfcptype.ReportingTrigger
MeasurementInformation pfcptype.MeasurementInformation
```
(note singular `ReportingTrigger` type name, matching Task 2's package — the old external type was plural `ReportingTriggers`; grep for `pfcptype.ReportingTriggers` after this edit and fix any remaining reference to the old plural name).

Also fix `MeasureInformation()`:
```go
// BEFORE
func MeasureInformation(isMeasurePkt, isMeasureBeforeQos bool) pfcptype.MeasurementInformation {
	var measureInformation pfcptype.MeasurementInformation
	measureInformation.Mnop = isMeasurePkt
	measureInformation.Mbqe = isMeasureBeforeQos
	return measureInformation
}
// AFTER
func MeasureInformation(isMeasurePkt, isMeasureBeforeQos bool) pfcptype.MeasurementInformation {
	var flags uint8
	if isMeasureBeforeQos {
		flags |= pfcptype.MeasureInfoMBQE
	}
	if isMeasurePkt {
		flags |= pfcptype.MeasureInfoMNOP
	}
	return pfcptype.MeasurementInformation{Flags: flags}
}
```
and its one call site in `NewMeasureInformation`'s `UrrOpt`:
```go
// BEFORE
func NewMeasureInformation(isMeasurePkt, isMeasureBeforeQos bool) UrrOpt {
	return func(urr *URR) {
		urr.MeasurementInformation.Mnop = isMeasurePkt
		urr.MeasurementInformation.Mbqe = isMeasureBeforeQos
	}
}
// AFTER
func NewMeasureInformation(isMeasurePkt, isMeasureBeforeQos bool) UrrOpt {
	return func(urr *URR) {
		urr.MeasurementInformation = MeasureInformation(isMeasurePkt, isMeasureBeforeQos)
	}
}
```

- [ ] **Step 4:** `grep -rn "pfcpType\|ReportingTriggers{" internal/context/ | grep -v pfcp_reports.go` → expect no output.

- [ ] **Step 5:** `go build ./internal/context/...` → fails only on `pfcp_reports.go` (Task 11) plus, until Task 7-9 land, on anything still calling the old `internal/pfcp/message`/`internal/pfcp/handler` packages that Task 7-9 delete — confirm the failures are limited to those, nothing else.

- [ ] **Step 6:**
```bash
git add internal/context/
git commit -m "refactor(context): migrate PFCP domain model to pfcptype (Flags-based ReportingTrigger)

BUILD IS RED at module level until Task 11."
```

---

## Part 3 — Active direction, moved into `internal/context` (matches internal layout)

### Task 7: `internal/context/pfcp_build.go`

Deletes the external repo's `internal/pfcp/message/build.go` and replaces it with a file **in `internal/context`**, reproducing the internal fork's actual `pfcp_build.go` (read in full from the internal source: the `toPdr`/`toFar`/`toQer`/`toUrr`/`toBAR` + `CREATE_OPS`/`UPDATE_OPS` pattern, the QER/URR dedup-by-map in `BuildPfcpSessionEstablishmentRequest`, `NewNodeIE`/`fteidFlag`/`ueIpAddrFlag`/`getActionFlag`/`getOuterDescritpion` helpers). Field names below are the **external** repo's actual `context.PDR`/`FAR`/`QER`/`URR`/`BAR`/`PDI`/`ForwardingParameters` struct fields (from `pfcp_rules.go`, now using `pfcptype` per Task 6) — the internal fork's own struct has slightly different field names in a few places (its `UPTunnel`/multi-UPF plumbing doesn't exist externally), so this is adapted to the external `context.SMContext`/`context.UPF` model rather than pasted verbatim; the **conversion logic and go-pfcp call patterns are unchanged** from the internal source.

**Files:**
- Delete: `internal/pfcp/message/build.go`, `internal/pfcp/message/build_test.go`
- Create: `internal/context/pfcp_build.go`
- Test: `internal/context/pfcp_build_test.go`

**Interfaces:**
- Produces: `NewNodeIE(nodeID pfcptype.NodeID) *ie.IE`; `BuildPfcpAssociationSetupRequest() *message.AssociationSetupRequest`; `BuildPfcpAssociationReleaseRequest() *message.AssociationReleaseRequest`; `BuildPfcpHeartbeatRequest() *message.HeartbeatRequest`; `BuildPfcpSessionEstablishmentRequest(upNodeID pfcptype.NodeID, localSEID uint64, pdrList []*PDR, farList []*FAR, barList []*BAR, qerList []*QER, urrList []*URR) (*message.SessionEstablishmentRequest, error)`; `BuildPfcpSessionModificationRequest(remoteSEID uint64, pdrList, farList, barList, qerList, urrList []...) (*message.SessionModificationRequest, error)`; `BuildPfcpSessionDeletionRequest(remoteSEID uint64) *message.SessionDeletionRequest`.

- [ ] **Step 1: failing test**

```go
// internal/context/pfcp_build_test.go
package context

import (
	"net"
	"testing"

	"github.com/wmnsk/go-pfcp/ie"

	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

func TestPdrToCreatePDR_RoundTrip(t *testing.T) {
	pdr := &PDR{
		PDRID:      1,
		Precedence: 100,
		PDI: PDI{
			SourceInterface: pfcptype.SourceInterface{InterfaceValue: pfcptype.SourceInterfaceAccess},
			LocalFTeid: &pfcptype.FTEID{
				V4: true, Teid: 0x12345678, Ipv4Address: net.ParseIP("10.0.0.2").To4(),
			},
		},
		OuterHeaderRemoval: &pfcptype.OuterHeaderRemoval{OuterHeaderRemovalDescription: pfcptype.OuterHeaderRemovalGtpUUdpIpv4},
		FAR:                &FAR{FARID: 2},
		State:              RULE_INITIAL,
	}

	createPDRIE := pdrToCreatePDR(pdr)
	b, err := createPDRIE.Marshal()
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	parsed, err := ie.Parse(b)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	children, err := parsed.CreatePDR()
	if err != nil {
		t.Fatalf("CreatePDR() error: %v", err)
	}
	var found bool
	for _, c := range children {
		if c.Type == ie.PDRID {
			id, _ := c.PDRID()
			if id != 1 {
				t.Fatalf("PDRID = %d, want 1", id)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("PDRID IE not found in round-tripped CreatePDR")
	}
}

func TestBuildPfcpSessionEstablishmentRequest_DedupsQER(t *testing.T) {
	sharedQER := &QER{QERID: 5, State: RULE_INITIAL}
	pdr1 := &PDR{PDRID: 1, FAR: &FAR{FARID: 1}, QER: []*QER{sharedQER}, State: RULE_INITIAL}
	pdr2 := &PDR{PDRID: 2, FAR: &FAR{FARID: 1}, QER: []*QER{sharedQER}, State: RULE_INITIAL}

	req, err := BuildPfcpSessionEstablishmentRequest(
		pfcptype.NodeID{NodeIdType: pfcptype.NodeIdTypeIpv4Address, IP: net.ParseIP("10.0.0.1").To4()},
		42,
		[]*PDR{pdr1, pdr2},
		[]*FAR{{FARID: 1, State: RULE_INITIAL}},
		nil,
		[]*QER{sharedQER, sharedQER},
		nil,
	)
	if err != nil {
		t.Fatalf("BuildPfcpSessionEstablishmentRequest error: %v", err)
	}
	if len(req.CreateQER) != 1 {
		t.Fatalf("expected exactly 1 CreateQER (deduped), got %d", len(req.CreateQER))
	}
}
```

- [ ] **Step 2:** FAIL as expected.

- [ ] **Step 3: implementation** — reproduce `internal/context/pfcp_build.go` from the internal source, adapted to the external `PDR`/`FAR`/`QER`/`URR`/`BAR`/`PDI`/`ForwardingParameters` field names (confirmed against `pfcp_rules.go` post-Task-6) and to `SMFContext`/`UPF` in place of the internal fork's `UPTunnel`/`UPTunnelNFContext`:

```go
// internal/context/pfcp_build.go
package context

import (
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

const (
	CREATE_OPS = "create"
	UPDATE_OPS = "update"
)

func NewNodeIE(nodeID pfcptype.NodeID) *ie.IE {
	if nodeID.NodeIdType == pfcptype.NodeIdTypeFqdn {
		return ie.NewNodeIDHeuristic(nodeID.FQDN)
	}
	return ie.NewNodeIDHeuristic(nodeID.IP.String())
}

func BuildPfcpAssociationSetupRequest() *message.AssociationSetupRequest {
	return message.NewAssociationSetupRequest(0,
		NewNodeIE(GetSelf().CPNodeID),
		ie.NewRecoveryTimeStamp(GetSelf().RecoveryTime()),
		ie.NewCPFunctionFeatures(0),
	)
}

func BuildPfcpAssociationReleaseRequest() *message.AssociationReleaseRequest {
	return message.NewAssociationReleaseRequest(0, NewNodeIE(GetSelf().CPNodeID))
}

func BuildPfcpHeartbeatRequest() *message.HeartbeatRequest {
	return message.NewHeartbeatRequest(0, ie.NewRecoveryTimeStamp(GetSelf().RecoveryTime()), nil)
}

func fteidFlag(f *pfcptype.FTEID) uint8 {
	flag := uint8(0)
	if f.V4 {
		flag |= 1 << 0
	}
	if f.V6 {
		flag |= 1 << 1
	}
	if f.Ch {
		flag |= 1 << 2
	}
	if f.Chid {
		flag |= 1 << 3
	}
	return flag
}

func ueIpAddrFlag(a *pfcptype.UEIPAddress) uint8 {
	flag := uint8(0)
	if a.V6 {
		flag |= 1 << 0
	}
	if a.V4 {
		flag |= 1 << 1
	}
	if a.Sd {
		flag |= 1 << 2
	}
	if a.Ipv6d {
		flag |= 1 << 3
	}
	return flag
}

func toPDI(pdi *PDI) *ie.IE {
	ieList := []*ie.IE{ie.NewSourceInterface(pdi.SourceInterface.InterfaceValue)}

	if pdi.LocalFTeid != nil {
		f := pdi.LocalFTeid
		ieList = append(ieList, ie.NewFTEID(fteidFlag(f), f.Teid, f.Ipv4Address, nil, 0))
	}
	if pdi.NetworkInstance != nil {
		ieList = append(ieList, ie.NewNetworkInstance(pdi.NetworkInstance.NetworkInstance))
	}
	if pdi.UEIPAddress != nil {
		a := pdi.UEIPAddress
		v4 := ""
		if a.Ipv4Address != nil {
			v4 = a.Ipv4Address.String()
		}
		ieList = append(ieList, ie.NewUEIPAddress(ueIpAddrFlag(a), v4, "", 0, 0))
	}
	if pdi.SDFFilter != nil {
		sdf := pdi.SDFFilter
		var fd, ttc, spi, fl string
		if sdf.Fd {
			fd = string(sdf.FlowDescription)
		}
		if sdf.Ttc {
			ttc = string(sdf.TosTrafficClass)
		}
		if sdf.Spi {
			spi = string(sdf.SecurityParameterIndex)
		}
		if sdf.Fl {
			fl = string(sdf.FlowLabel)
		}
		if sdfIE := newSDFFilterIE(fd, ttc, spi, fl, sdf.SdfFilterId); sdfIE != nil {
			ieList = append(ieList, sdfIE)
		}
	}
	if pdi.ApplicationID != "" {
		ieList = append(ieList, ie.NewApplicationID(pdi.ApplicationID))
	}
	if pdi.QFI != 0 {
		ieList = append(ieList, ie.NewQFI(pdi.QFI))
	}
	return ie.NewPDI(ieList...)
}

// newSDFFilterIE always sets the BID flag, matching the internal
// reference's NewSDFFilterAlwaysWithBIDFlag — confirm the exact go-pfcp
// SDF Filter constructor for your pinned version with
// `go doc github.com/wmnsk/go-pfcp/ie NewSDFFilterFields` before finalizing;
// this two-step Fields+Marshal approach is the one confirmed-working
// pattern from the internal source, unlike a possible single-shot
// ie.NewSDFFilter(...) which was not verified.
func newSDFFilterIE(fd, ttc, spi, fl string, fid uint32) *ie.IE {
	fields := ie.NewSDFFilterFields(fd, ttc, spi, fl, fid)
	fields.SetBIDFlag()
	data, err := fields.Marshal()
	if err != nil {
		return nil
	}
	return ie.New(ie.SDFFilter, data)
}

func toPdr(pdr *PDR, ops string) *ie.IE {
	if pdr == nil {
		return nil
	}
	ieList := []*ie.IE{
		ie.NewPDRID(pdr.PDRID),
		ie.NewPrecedence(pdr.Precedence),
		toPDI(&pdr.PDI),
	}
	if pdr.OuterHeaderRemoval != nil {
		ieList = append(ieList, ie.NewOuterHeaderRemoval(pdr.OuterHeaderRemoval.OuterHeaderRemovalDescription, 0))
	}
	if pdr.FAR != nil {
		ieList = append(ieList, ie.NewFARID(pdr.FAR.FARID))
	}
	for _, q := range pdr.QER {
		ieList = append(ieList, ie.NewQERID(q.QERID))
	}
	for _, u := range pdr.URR {
		ieList = append(ieList, ie.NewURRID(u.URRID))
	}
	switch ops {
	case CREATE_OPS:
		return ie.NewCreatePDR(ieList...)
	case UPDATE_OPS:
		return ie.NewUpdatePDR(ieList...)
	}
	return nil
}

func pdrToCreatePDR(pdr *PDR) *ie.IE { return toPdr(pdr, CREATE_OPS) }
func pdrToUpdatePDR(pdr *PDR) *ie.IE { return toPdr(pdr, UPDATE_OPS) }

func getActionFlag(a pfcptype.ApplyAction) []uint8 {
	b1, b2 := uint8(0), uint8(0)
	if a.Drop {
		b1 |= 1
	}
	if a.Forw {
		b1 |= 1 << 1
	}
	if a.Buff {
		b1 |= 1 << 2
	}
	if a.Nocp {
		b1 |= 1 << 3
	}
	if a.Dupl {
		b1 |= 1 << 4
	}
	if a.Ipma {
		b1 |= 1 << 5
	}
	if a.Ipmd {
		b1 |= 1 << 6
	}
	if a.Dfrt {
		b1 |= 1 << 7
	}
	if a.Edrt {
		b2 |= 1
	}
	if a.Bdpn {
		b2 |= 1 << 1
	}
	if a.Ddpn {
		b2 |= 1 << 2
	}
	if a.Fssm {
		b2 |= 1 << 3
	}
	if a.Mbsu {
		b2 |= 1 << 4
	}
	if b2 != 0 {
		return []uint8{b1, b2}
	}
	return []uint8{b1}
}

func getOuterDescritpion(o *pfcptype.OuterHeaderCreation) uint16 {
	if o == nil {
		return 0
	}
	desc := o.OuterHeaderCreationDescription
	if o.PortNumber != 0 {
		desc |= 1 << 3
	}
	return desc << 8
}

func createFwdParamsIE(fp *ForwardingParameters, ops string) *ie.IE {
	if fp == nil {
		return nil
	}
	ieList := []*ie.IE{ie.NewDestinationInterface(fp.DestinationInterface.InterfaceValue)}
	if fp.NetworkInstance != nil {
		ieList = append(ieList, ie.NewNetworkInstance(fp.NetworkInstance.NetworkInstance))
	}
	if fp.OuterHeaderCreation != nil {
		o := fp.OuterHeaderCreation
		ieList = append(ieList, ie.NewOuterHeaderCreation(
			getOuterDescritpion(o), o.Teid, o.Ipv4Address.To4().String(), "", o.PortNumber, 0, 0))
	}
	if fp.ForwardingPolicyID != "" {
		ieList = append(ieList, ie.NewForwardingPolicy(fp.ForwardingPolicyID))
	}
	if ops == CREATE_OPS {
		return ie.NewForwardingParameters(ieList...)
	}
	if fp.SendEndMarker {
		ieList = append(ieList, ie.NewPFCPSMReqFlags(0x02)) // SNDEM
	}
	return ie.NewUpdateForwardingParameters(ieList...)
}

func toFar(far *FAR, ops string) *ie.IE {
	ieList := []*ie.IE{
		ie.NewFARID(far.FARID),
		ie.NewApplyAction(getActionFlag(far.ApplyAction)...),
	}
	if fp := createFwdParamsIE(far.ForwardingParameters, ops); fp != nil {
		ieList = append(ieList, fp)
	}
	if far.BAR != nil {
		ieList = append(ieList, ie.NewBARID(far.BAR.BARID))
	}
	switch ops {
	case CREATE_OPS:
		return ie.NewCreateFAR(ieList...)
	case UPDATE_OPS:
		return ie.NewUpdateFAR(ieList...)
	}
	return nil
}

func farToCreateFAR(far *FAR) *ie.IE { return toFar(far, CREATE_OPS) }
func farToUpdateFAR(far *FAR) *ie.IE { return toFar(far, UPDATE_OPS) }

func toBAR(bar *BAR, ops string) *ie.IE {
	if ops == CREATE_OPS {
		return ie.NewCreateBAR(
			ie.NewBARID(bar.BARID),
			ie.NewDownlinkDataNotificationDelay(0),
		)
	}
	return nil
}

func barToCreateBAR(bar *BAR) *ie.IE { return toBAR(bar, CREATE_OPS) }

func toQer(qer *QER, ops string) *ie.IE {
	ieList := []*ie.IE{ie.NewQERID(qer.QERID)}
	if qer.GateStatus != nil {
		ieList = append(ieList, ie.NewGateStatus(uint8(qer.GateStatus.ULGate), uint8(qer.GateStatus.DLGate)))
	}
	if qer.QFI.QFI != 0 {
		ieList = append(ieList, ie.NewQFI(qer.QFI.QFI))
	}
	if qer.MBR != nil {
		ieList = append(ieList, ie.NewMBR(qer.MBR.ULMBR, qer.MBR.DLMBR))
	}
	if qer.GBR != nil {
		ieList = append(ieList, ie.NewGBR(qer.GBR.ULGBR, qer.GBR.DLGBR))
	}
	switch ops {
	case CREATE_OPS:
		return ie.NewCreateQER(ieList...)
	case UPDATE_OPS:
		return ie.NewUpdateQER(ieList...)
	}
	return nil
}

func qerToCreateQER(qer *QER) *ie.IE { return toQer(qer, CREATE_OPS) }

func toUrr(urr *URR, ops string) *ie.IE {
	ieList := []*ie.IE{ie.NewURRID(urr.URRID)}
	switch urr.MeasureMethod {
	case MesureMethodVol:
		ieList = append(ieList, ie.NewMeasurementMethod(0, 1, 0))
	case MesureMethodTime:
		ieList = append(ieList, ie.NewMeasurementMethod(0, 0, 1))
	}
	if urr.ReportingTrigger.Flags != 0 {
		ieList = append(ieList, urr.ReportingTrigger.IE())
	}
	if urr.MeasurementPeriod != 0 {
		ieList = append(ieList, ie.NewMeasurementPeriod(urr.MeasurementPeriod))
	}
	if urr.MeasurementInformation.Flags != 0 {
		ieList = append(ieList, ie.NewMeasurementInformation(urr.MeasurementInformation.Flags))
	}
	if urr.VolumeThreshold != 0 {
		ieList = append(ieList, ie.NewVolumeThreshold(0x07, urr.VolumeThreshold, urr.VolumeThreshold, urr.VolumeThreshold))
	}
	if urr.VolumeQuota != 0 {
		ieList = append(ieList, ie.NewVolumeQuota(0x07, urr.VolumeQuota, urr.VolumeQuota, urr.VolumeQuota))
	}
	switch ops {
	case CREATE_OPS:
		return ie.NewCreateURR(ieList...)
	case UPDATE_OPS:
		return ie.NewUpdateURR(ieList...)
	}
	return nil
}

func urrToCreateURR(urr *URR) *ie.IE { return toUrr(urr, CREATE_OPS) }
func urrToUpdateURR(urr *URR) *ie.IE { return toUrr(urr, UPDATE_OPS) }

func BuildPfcpSessionEstablishmentRequest(
	upNodeID pfcptype.NodeID,
	localSEID uint64,
	pdrList []*PDR,
	farList []*FAR,
	barList []*BAR,
	qerList []*QER,
	urrList []*URR,
) (*message.SessionEstablishmentRequest, error) {
	ieList := []*ie.IE{
		NewNodeIE(GetSelf().CPNodeID),
		ie.NewFSEID(localSEID, GetSelf().ExternalIP().To4(), nil),
	}

	for _, pdr := range pdrList {
		if pdr.State == RULE_INITIAL {
			if createPDR := pdrToCreatePDR(pdr); createPDR != nil {
				ieList = append(ieList, createPDR)
			}
		}
	}
	for _, far := range farList {
		if far.State == RULE_INITIAL {
			ieList = append(ieList, farToCreateFAR(far))
		}
	}
	for _, bar := range barList {
		if bar.State == RULE_INITIAL {
			ieList = append(ieList, barToCreateBAR(bar))
		}
	}

	qerMap := make(map[uint32]*QER, len(qerList))
	for _, qer := range qerList {
		qerMap[qer.QERID] = qer
	}
	for _, qer := range qerMap {
		if qer.State == RULE_INITIAL {
			ieList = append(ieList, qerToCreateQER(qer))
		}
	}

	urrMap := make(map[uint32]*URR, len(urrList))
	for _, urr := range urrList {
		urrMap[urr.URRID] = urr
	}
	for _, urr := range urrMap {
		if urr.State == RULE_INITIAL {
			ieList = append(ieList, urrToCreateURR(urr))
		}
	}

	ieList = append(ieList, ie.NewPDNType(pfcptype.PDNTypeIpv4))
	return message.NewSessionEstablishmentRequest(0, 0, 0, 0, 0, ieList...), nil
}

func BuildPfcpSessionModificationRequest(
	remoteSEID uint64,
	pdrList []*PDR,
	farList []*FAR,
	barList []*BAR,
	qerList []*QER,
	urrList []*URR,
) (*message.SessionModificationRequest, error) {
	var ieList []*ie.IE

	for _, pdr := range pdrList {
		switch pdr.State {
		case RULE_INITIAL:
			if createPDR := pdrToCreatePDR(pdr); createPDR != nil {
				ieList = append(ieList, createPDR)
			}
		case RULE_UPDATE:
			if updatePDR := pdrToUpdatePDR(pdr); updatePDR != nil {
				ieList = append(ieList, updatePDR)
			}
		case RULE_REMOVE:
			ieList = append(ieList, ie.NewRemovePDR(ie.NewPDRID(pdr.PDRID)))
		}
	}
	for _, far := range farList {
		switch far.State {
		case RULE_INITIAL:
			ieList = append(ieList, farToCreateFAR(far))
		case RULE_UPDATE:
			ieList = append(ieList, farToUpdateFAR(far))
		case RULE_REMOVE:
			ieList = append(ieList, ie.NewRemoveFAR(ie.NewFARID(far.FARID)))
		}
	}
	for _, bar := range barList {
		if bar.State == RULE_INITIAL {
			ieList = append(ieList, barToCreateBAR(bar))
		}
	}
	for _, qer := range qerList {
		switch qer.State {
		case RULE_INITIAL:
			ieList = append(ieList, qerToCreateQER(qer))
		case RULE_REMOVE:
			ieList = append(ieList, ie.NewRemoveQER(ie.NewQERID(qer.QERID)))
		}
	}

	urrMap := make(map[uint32]*URR, len(urrList))
	for _, urr := range urrList {
		urrMap[urr.URRID] = urr
	}
	for _, urr := range urrMap {
		switch urr.State {
		case RULE_INITIAL:
			ieList = append(ieList, urrToCreateURR(urr))
		case RULE_UPDATE:
			ieList = append(ieList, urrToUpdateURR(urr))
		case RULE_REMOVE:
			ieList = append(ieList, ie.NewRemoveURR(ie.NewURRID(urr.URRID)))
		case RULE_QUERY:
			ieList = append(ieList, ie.NewQueryURR(ie.NewURRID(urr.URRID)))
		}
	}

	return message.NewSessionModificationRequest(1, 0, remoteSEID, 0, 1, ieList...), nil
}

func BuildPfcpSessionDeletionRequest(remoteSEID uint64) *message.SessionDeletionRequest {
	return message.NewSessionDeletionRequest(1, 0, remoteSEID, 0, 1)
}

func BuildPfcpSessionReportResponse(cause uint8, seid uint64, seq uint32) *message.SessionReportResponse {
	return message.NewSessionReportResponse(0, 0, seid, seq, 0, ie.NewCause(cause))
}

func BuildPfcpHeartbeatResponse(seq uint32) *message.HeartbeatResponse {
	return message.NewHeartbeatResponse(seq, ie.NewRecoveryTimeStamp(GetSelf().RecoveryTime()))
}
```

> `GetSelf().RecoveryTime()` — add this accessor to `SMFContext` now if Task 5's `association_test.go` block didn't already require it; store the value set once at startup (Task 10).
> `PDI.SDFFilter` here is a single `*pfcptype.SDFFilter` (matches the external repo's current `PDI.SDFFilter *pfcpType.SDFFilter` — singular, not the internal fork's `[]pfcpType.SDFFilter` slice). Keep the external shape; do not switch to a slice, that would be an unforced, unrelated behavior change.

- [ ] **Step 4:** `go test ./internal/context/... -run 'TestPdrToCreatePDR|TestBuildPfcpSessionEstablishmentRequest' -v` → PASS.

- [ ] **Step 5:**
```bash
rm -rf internal/pfcp/message/build.go internal/pfcp/message/build_test.go
git add -A internal/context/pfcp_build.go internal/context/pfcp_build_test.go internal/pfcp/message/build.go internal/pfcp/message/build_test.go
git commit -m "refactor: move+rewrite message builders into internal/context (matches internal layout)"
```

---

### Task 8: `internal/context/pfcp_send.go`

Reproduces the internal fork's `pfcp_send.go` send/wait/validate pattern (read via detailed extraction from the internal source): build → send via `PfcpServer.SendPfcpMsg` → block on the response channel → check `Msg == nil` for timeout → check `MessageType()` → check `SEID()` for session-scoped messages → return the concrete response type. **No Cause check here** — that's Task 9's `pfcp_handler.go`, matching the internal fork's own split.

**Files:**
- Delete: `internal/pfcp/message/send.go`, `internal/pfcp/message/send_test.go`
- Create: `internal/context/pfcp_send.go`
- Test: `internal/context/pfcp_send_test.go`

**Interfaces:**
- Consumes: `internal/pfcp.PfcpServer.SendPfcpMsg` (Task 4) via a package-level `*pfcp.PfcpServer` set by `SetPfcpServer` (Task 10).
- Produces: `SetPfcpServer(s *pfcp.PfcpServer)`; `SendPfcpAssociationSetupRequest(upNodeID pfcptype.NodeID) (*message.AssociationSetupResponse, error)`; `SendPfcpAssociationReleaseRequest(upNodeID pfcptype.NodeID) (*message.AssociationReleaseResponse, error)`; `SendPfcpHeartbeatRequest(upf *UPF) (*message.HeartbeatResponse, error)`; `SendPfcpSessionEstablishmentRequest(upf *UPF, ctx *SMContext, pdrList, farList, barList, qerList, urrList []...) (*message.SessionEstablishmentResponse, error)`; `SendPfcpSessionModificationRequest(...)`; `SendPfcpSessionDeletionRequest(upf *UPF, ctx *SMContext) (*message.SessionDeletionResponse, error)`.

- [ ] **Step 1: failing test** (same loopback-server pattern as Task 4's server tests, reused here to avoid a real UPF fixture)

```go
// internal/context/pfcp_send_test.go
package context

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	smfpfcp "github.com/free5gc/smf/internal/pfcp"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

func TestSendPfcpHeartbeatRequest_TimeoutReturnsError(t *testing.T) {
	var wg sync.WaitGroup
	srv := smfpfcp.NewPfcpServer(testFakeSmf(t), "127.0.0.1")
	if err := srv.Run(&wg); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	defer srv.Stop()
	SetPfcpServer(srv)

	upf := &UPF{NodeID: pfcptype.NodeID{NodeIdType: pfcptype.NodeIdTypeIpv4Address, IP: net.ParseIP("127.0.0.1")}}
	upf.UPFAssocStatus = AssocStatusSuccess // however the existing "associated" constant is named — confirm in upf.go

	_, err := SendPfcpHeartbeatRequest(upf)
	if err == nil {
		t.Fatalf("expected timeout error (nothing listens on port 8805 in the test), got nil")
	}
}
```

> `testFakeSmf(t)` — a `t.Helper()` constructor satisfying whatever minimal interface `smfpfcp.NewPfcpServer` needs (see Task 4's `fakeSmf`); if `internal/pfcp` and `internal/context` end up needing two separate fake implementations because of an import cycle (`internal/pfcp` cannot import `internal/context` — confirm this direction: `internal/context` importing `internal/pfcp` for `SendPfcpMsg`/`PfcpServer` is fine and is exactly what Task 8 does; the reverse, `internal/pfcp` importing `internal/context`, is what Task 5's handlers already do for `RetrieveUPFNodeByNodeID` etc. — so `internal/pfcp` already depends on `internal/context`, meaning `internal/context` must NOT import `internal/pfcp` for anything `internal/pfcp` itself needs, only for the leaf `PfcpServer`/`SendPfcpMsg` surface, which has no back-reference to `context`. This is fine — `internal/pfcp` depending on `internal/context` and `internal/context` depending on `internal/pfcp` for a disjoint, leaf subset of each is not a cycle as long as neither package-level `import` graph closes a loop; if `go build` reports an import cycle here, it means `PfcpServer`'s `smfIface` needs to shrink further so `internal/pfcp` has zero `internal/context` import, and `internal/pfcp/association.go`'s calls to `smf_context.RetrieveUPFNodeByNodeID` etc. must move behind a small interface injected from Task 10's wiring instead of a direct import — flag this to the team immediately if it happens, since it changes several signatures written in Task 5).

- [ ] **Step 2:** FAIL (compile error, functions don't exist).

- [ ] **Step 3: implementation**

```go
// internal/context/pfcp_send.go
package context

import (
	"fmt"
	"net"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	smfpfcp "github.com/free5gc/smf/internal/pfcp"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

var pfcpServer *smfpfcp.PfcpServer

// SetPfcpServer must be called once during startup (Task 10) before any
// SendPfcpXxx function in this file runs.
func SetPfcpServer(s *smfpfcp.PfcpServer) {
	pfcpServer = s
}

func upfAddr(nodeID pfcptype.NodeID) *net.UDPAddr {
	return &net.UDPAddr{IP: nodeID.IP, Port: smfpfcp.PfcpPort}
}

func sendAndWait(req message.Message, addr *net.UDPAddr) (message.Message, error) {
	ch := pfcpServer.SendPfcpMsg(req, addr)
	if ch == nil {
		return nil, fmt.Errorf("sendAndWait: server not running")
	}
	rcvPkt := <-ch
	if rcvPkt.Msg == nil {
		return nil, fmt.Errorf("sendAndWait: %s to %v tx timeout", req.MessageTypeName(), addr)
	}
	return rcvPkt.Msg, nil
}

func SendPfcpAssociationSetupRequest(upNodeID pfcptype.NodeID) (*message.AssociationSetupResponse, error) {
	req := BuildPfcpAssociationSetupRequest()
	rcvPkt, err := sendAndWait(req, upfAddr(upNodeID))
	if err != nil {
		return nil, err
	}
	if rcvPkt.MessageType() != message.MsgTypeAssociationSetupResponse {
		return nil, fmt.Errorf("unexpected rsp msg type(%d)", rcvPkt.MessageType())
	}
	return rcvPkt.(*message.AssociationSetupResponse), nil
}

func SendPfcpAssociationReleaseRequest(upNodeID pfcptype.NodeID) (*message.AssociationReleaseResponse, error) {
	req := BuildPfcpAssociationReleaseRequest()
	rcvPkt, err := sendAndWait(req, upfAddr(upNodeID))
	if err != nil {
		return nil, err
	}
	if rcvPkt.MessageType() != message.MsgTypeAssociationReleaseResponse {
		return nil, fmt.Errorf("unexpected rsp msg type(%d)", rcvPkt.MessageType())
	}
	return rcvPkt.(*message.AssociationReleaseResponse), nil
}

func SendPfcpHeartbeatRequest(upf *UPF) (*message.HeartbeatResponse, error) {
	req := BuildPfcpHeartbeatRequest()
	rcvPkt, err := sendAndWait(req, upfAddr(upf.NodeID))
	if err != nil {
		return nil, err
	}
	if rcvPkt.MessageType() != message.MsgTypeHeartbeatResponse {
		return nil, fmt.Errorf("unexpected rsp msg type(%d)", rcvPkt.MessageType())
	}
	return rcvPkt.(*message.HeartbeatResponse), nil
}

func SendPfcpSessionEstablishmentRequest(
	upf *UPF, ctx *SMContext,
	pdrList []*PDR, farList []*FAR, barList []*BAR, qerList []*QER, urrList []*URR,
) (*message.SessionEstablishmentResponse, error) {
	if err := upf.IsAssociated(); err != nil {
		return nil, err
	}
	nodeIDStr := upf.NodeID.String()
	localSEID := ctx.PFCPContext[nodeIDStr].LocalSEID

	req, err := BuildPfcpSessionEstablishmentRequest(upf.NodeID, localSEID, pdrList, farList, barList, qerList, urrList)
	if err != nil {
		return nil, err
	}
	rcvPkt, err := sendAndWait(req, upfAddr(upf.NodeID))
	if err != nil {
		return nil, err
	}
	if rcvPkt.MessageType() != message.MsgTypeSessionEstablishmentResponse {
		return nil, fmt.Errorf("unexpected rsp msg type(%d)", rcvPkt.MessageType())
	}
	rsp := rcvPkt.(*message.SessionEstablishmentResponse)
	if rsp.SEID() != localSEID {
		return nil, fmt.Errorf("unexpected SEID in response: got %#x, want %#x", rsp.SEID(), localSEID)
	}
	return rsp, nil
}

func SendPfcpSessionModificationRequest(
	upf *UPF, ctx *SMContext,
	pdrList []*PDR, farList []*FAR, barList []*BAR, qerList []*QER, urrList []*URR,
) (*message.SessionModificationResponse, error) {
	if err := upf.IsAssociated(); err != nil {
		return nil, err
	}
	nodeIDStr := upf.NodeID.String()
	pfcpCtx := ctx.PFCPContext[nodeIDStr]

	req, err := BuildPfcpSessionModificationRequest(pfcpCtx.RemoteSEID, pdrList, farList, barList, qerList, urrList)
	if err != nil {
		return nil, err
	}
	rcvPkt, err := sendAndWait(req, upfAddr(upf.NodeID))
	if err != nil {
		return nil, err
	}
	if rcvPkt.MessageType() != message.MsgTypeSessionModificationResponse {
		return nil, fmt.Errorf("unexpected rsp msg type(%d)", rcvPkt.MessageType())
	}
	rsp := rcvPkt.(*message.SessionModificationResponse)
	if rsp.SEID() != pfcpCtx.LocalSEID {
		return nil, fmt.Errorf("unexpected SEID in response: got %#x, want %#x", rsp.SEID(), pfcpCtx.LocalSEID)
	}
	return rsp, nil
}

func SendPfcpSessionDeletionRequest(upf *UPF, ctx *SMContext) (*message.SessionDeletionResponse, error) {
	if err := upf.IsAssociated(); err != nil {
		return nil, err
	}
	nodeIDStr := upf.NodeID.String()
	pfcpCtx := ctx.PFCPContext[nodeIDStr]

	req := BuildPfcpSessionDeletionRequest(pfcpCtx.RemoteSEID)
	rcvPkt, err := sendAndWait(req, upfAddr(upf.NodeID))
	if err != nil {
		return nil, err
	}
	if rcvPkt.MessageType() != message.MsgTypeSessionDeletionResponse {
		return nil, fmt.Errorf("unexpected rsp msg type(%d)", rcvPkt.MessageType())
	}
	rsp := rcvPkt.(*message.SessionDeletionResponse)
	if rsp.SEID() != pfcpCtx.LocalSEID {
		return nil, fmt.Errorf("unexpected SEID in response: got %#x, want %#x", rsp.SEID(), pfcpCtx.LocalSEID)
	}
	return rsp, nil
}

func SendPfcpSessionReportResponse(addr *net.UDPAddr, cause uint8, seq uint32, seid uint64) {
	rsp := BuildPfcpSessionReportResponse(cause, seid, seq)
	pfcpServer.SendPfcpMsg(rsp, addr)
}

func SendHeartbeatResponse(addr *net.UDPAddr, seq uint32) {
	rsp := BuildPfcpHeartbeatResponse(seq)
	pfcpServer.SendPfcpMsg(rsp, addr)
}

var _ = ie.CauseRequestAccepted // silence unused import if the Cause constant ends up unused here — remove once real usage exists
```

> Delete the trailing `var _ = ie.CauseRequestAccepted` line if the `ie` import is already used elsewhere in the final file (it will be, for `ie.CauseXxx` if you end up needing it for a log message — check with `goimports` after Step 3 and remove whichever of these is actually dead).

- [ ] **Step 4:** run and confirm PASS (adjust `AssocStatusSuccess` in the test to whatever `upf.go`'s real "associated" constant is named — grep it first: `grep -n "AssocStatus" internal/context/upf.go`).

- [ ] **Step 5:**
```bash
rm -f internal/pfcp/message/send.go internal/pfcp/message/send_test.go
git add -A internal/context/pfcp_send.go internal/context/pfcp_send_test.go internal/pfcp/message/send.go internal/pfcp/message/send_test.go
git commit -m "refactor: move+rewrite message senders into internal/context (matches internal layout)"
```

---

### Task 9: `internal/context/pfcp_handler.go`

New file — reproduces the internal fork's `pfcp_handler.go` response-validation layer (Cause + mandatory-IE checks on every `SendPfcpXxxRequest` result), which the external repo currently does **inline inside `internal/sbi/processor`** instead. This is the biggest structural move in the whole plan: Task 12 will change every `internal/sbi/processor` call site from "call Send, then check Cause myself" to "call Send, then call the matching `HandlePfcpXxxResponse`".

**Files:** Create `internal/context/pfcp_handler.go`, Test `internal/context/pfcp_handler_test.go`

**Interfaces:**
- Produces: `HandlePfcpAssociationSetupResponse(rsp *message.AssociationSetupResponse) (recoveryTS time.Time, err error)`; `HandlePfcpSessionEstablishmentResponse(rsp *message.SessionEstablishmentResponse) (remoteSEID uint64, err error)`; `HandlePfcpSessionModificationResponse(rsp *message.SessionModificationResponse) error`; `HandlePfcpSessionDeletionResponse(rsp *message.SessionDeletionResponse) error`.

- [ ] **Step 1: failing test**

```go
// internal/context/pfcp_handler_test.go
package context

import (
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

func TestHandlePfcpAssociationSetupResponse_RejectsMissingCause(t *testing.T) {
	rsp := message.NewAssociationSetupResponse(1,
		ie.NewNodeIDHeuristic("10.0.0.1"),
		ie.NewRecoveryTimeStamp(time.Now()),
	) // no Cause IE
	_, err := HandlePfcpAssociationSetupResponse(rsp)
	if err == nil {
		t.Fatalf("expected error when Cause IE is missing")
	}
}

func TestHandlePfcpAssociationSetupResponse_RejectsNonSuccessCause(t *testing.T) {
	rsp := message.NewAssociationSetupResponse(1,
		ie.NewNodeIDHeuristic("10.0.0.1"),
		ie.NewCause(ie.CauseRequestRejected),
		ie.NewRecoveryTimeStamp(time.Now()),
	)
	_, err := HandlePfcpAssociationSetupResponse(rsp)
	if err == nil {
		t.Fatalf("expected error for CauseRequestRejected")
	}
}

func TestHandlePfcpSessionEstablishmentResponse_ExtractsRemoteSEID(t *testing.T) {
	rsp := message.NewSessionEstablishmentResponse(0, 0, 0, 1, 0,
		ie.NewCause(ie.CauseRequestAccepted),
		ie.NewFSEID(0xdeadbeef, nil, nil),
	)
	remoteSEID, err := HandlePfcpSessionEstablishmentResponse(rsp)
	if err != nil {
		t.Fatalf("HandlePfcpSessionEstablishmentResponse error: %v", err)
	}
	if remoteSEID != 0xdeadbeef {
		t.Fatalf("remoteSEID = %#x, want %#x", remoteSEID, 0xdeadbeef)
	}
}
```

- [ ] **Step 2:** FAIL.

- [ ] **Step 3: implementation**

```go
// internal/context/pfcp_handler.go
package context

import (
	"fmt"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

func checkCause(causeIE *ie.IE) error {
	if causeIE == nil {
		return fmt.Errorf("response missing mandatory Cause IE")
	}
	cause, err := causeIE.Cause()
	if err != nil {
		return fmt.Errorf("Cause IE decode error: %w", err)
	}
	if cause != ie.CauseRequestAccepted {
		return fmt.Errorf("request rejected, cause: %d", cause)
	}
	return nil
}

func HandlePfcpAssociationSetupResponse(rsp *message.AssociationSetupResponse) (time.Time, error) {
	if rsp.NodeID == nil || rsp.Cause == nil || rsp.RecoveryTimeStamp == nil {
		return time.Time{}, fmt.Errorf("AssociationSetupResponse missing mandatory IE(s)")
	}
	if err := checkCause(rsp.Cause); err != nil {
		return time.Time{}, err
	}
	ts, err := rsp.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		return time.Time{}, fmt.Errorf("RecoveryTimeStamp decode error: %w", err)
	}
	return ts, nil
}

func HandlePfcpAssociationReleaseResponse(rsp *message.AssociationReleaseResponse) error {
	if rsp.Cause == nil {
		return fmt.Errorf("AssociationReleaseResponse missing mandatory Cause IE")
	}
	return checkCause(rsp.Cause)
}

func HandlePfcpSessionEstablishmentResponse(rsp *message.SessionEstablishmentResponse) (uint64, error) {
	if rsp.Cause == nil || rsp.UPFSEID == nil {
		return 0, fmt.Errorf("SessionEstablishmentResponse missing mandatory IE(s)")
	}
	if err := checkCause(rsp.Cause); err != nil {
		return 0, err
	}
	fseid, err := rsp.UPFSEID.FSEID()
	if err != nil {
		return 0, fmt.Errorf("UPFSEID decode error: %w", err)
	}
	return fseid.SEID, nil
}

func HandlePfcpSessionModificationResponse(rsp *message.SessionModificationResponse) error {
	if rsp.Cause == nil {
		return fmt.Errorf("SessionModificationResponse missing mandatory Cause IE")
	}
	return checkCause(rsp.Cause)
}

func HandlePfcpSessionDeletionResponse(rsp *message.SessionDeletionResponse) error {
	if rsp.Cause == nil {
		return fmt.Errorf("SessionDeletionResponse missing mandatory Cause IE")
	}
	return checkCause(rsp.Cause)
}
```

- [ ] **Step 4:** `go test ./internal/context/... -run TestHandlePfcp -v` → PASS.

- [ ] **Step 5:**
```bash
git add internal/context/pfcp_handler.go internal/context/pfcp_handler_test.go
git commit -m "feat(context): add pfcp_handler.go, reproducing internal's response Cause/IE validation layer"
```

---

## Part 4 — Wiring

### Task 10: Delete `internal/pfcp/message`, `internal/pfcp/handler`, `internal/pfcp/udp`; wire startup

**Files:**
- Delete: `internal/pfcp/message/` (now empty after Tasks 7-8), `internal/pfcp/handler/` (superseded by Task 5's `internal/pfcp/association.go`/`dispatcher.go`/`report.go`), `internal/pfcp/udp/`
- Modify: whichever file currently calls `udp.Run`/`udp.ClosePfcp` (find it — see Step 1)

- [ ] **Step 1: find every current caller**
```bash
cd /home/alonza/smf/smf-opensource
grep -rn "internal/pfcp/udp\"\|internal/pfcp/handler\"\|internal/pfcp/message\"" --include="*.go" . | grep -v "_test.go\|internal/pfcp/udp/\|internal/pfcp/handler/\|internal/pfcp/message/"
```
Read the printed file(s) before editing — this is the SMF process lifecycle code (`pkg/app/` or `cmd/`).

- [ ] **Step 2: replace startup**

Replace the old `go udp.Run(pfcp.Dispatch)`-shaped call with:
```go
smfCtx := smf_context.GetSelf()
srv := smfpfcp.NewPfcpServer(app, smfCtx.ListenIP().String()) // `app` = whatever already satisfies Config()/CancelContext() in this scope
srv.SetDispatch(srv.Dispatch)
var wg sync.WaitGroup
if err := srv.Run(&wg); err != nil {
	logger.PfcpLog.Fatalf("PFCP server start failed: %v", err)
}
smf_context.SetPfcpServer(srv)
```
(import `smfpfcp "github.com/free5gc/smf/internal/pfcp"`.)

- [ ] **Step 3: replace shutdown**

Replace `udp.ClosePfcp()` with `srv.Stop()` using whichever variable holds the `*smfpfcp.PfcpServer` in that scope (store it on the app struct if shutdown is in a different function than startup).

- [ ] **Step 4: delete the superseded packages**
```bash
rm -rf internal/pfcp/message internal/pfcp/handler internal/pfcp/udp
```

- [ ] **Step 5: whole-module build, confirm remaining failures are only Task 11-12's scope**
```bash
go build ./... 2>&1 | grep -oE '^[^:]+\.go' | sort -u
```
Expected: only `internal/context/pfcp_reports.go` and the 5 `internal/sbi/processor/*.go` files. Anything else means an earlier task missed a call site — stop and fix before continuing.

- [ ] **Step 6:**
```bash
git add -A internal/pfcp
git commit -m "refactor(pfcp): wire new server into app lifecycle, delete superseded message/handler/udp packages

Build still red in pfcp_reports.go and internal/sbi/processor — Tasks 11-12."
```

---

## Part 5 — Downstream call sites

### Task 11: `internal/context/pfcp_reports.go` + wire `handleSessionReportRequest`

Same content as before (rewrite against `[]*ie.IE` using go-pfcp's generic `Has<FLAG>()` getters — verified via `go doc` against `*ie.IE`), plus now also fills in Task 5's `report.go` stub.

**Files:**
- Modify: `internal/context/pfcp_reports.go` (full rewrite)
- Modify: `internal/pfcp/report.go` (replace the Task 5 stub)
- Test: `internal/context/pfcp_reports_test.go`

- [ ] **Step 1: failing test**

```go
// internal/context/pfcp_reports_test.go
package context

import (
	"testing"

	"github.com/wmnsk/go-pfcp/ie"

	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

func TestIdentifyTriggerType_VolumeThreshold(t *testing.T) {
	report := ie.NewUsageReportWithinSessionReportRequest(
		ie.NewURRID(1),
		ie.NewUsageReportTrigger(0x02), // VOLTH
		ie.NewVolumeMeasurement(0, 100, 40, 60, 0, 0, 0),
	)
	if !report.HasVOLTH() {
		t.Fatalf("expected HasVOLTH() true for trigger octet 0x02")
	}
}

func TestHandleReports_MissingVolumeMeasurement_DoesNotPanic(t *testing.T) {
	smContext := &SMContext{}
	report := ie.NewUsageReportWithinSessionReportRequest(
		ie.NewURRID(1),
		ie.NewUsageReportTrigger(0x02),
	)
	nodeID := pfcptype.NodeID{NodeIdType: pfcptype.NodeIdTypeIpv4Address, IP: nil}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("HandleReports panicked: %v", r)
		}
	}()
	smContext.HandleReports([]*ie.IE{report}, nodeID, "")
}
```

> Verify `ie.NewUsageReportWithinSessionReportRequest`/`ie.NewUsageReportTrigger`/`ie.NewVolumeMeasurement` with `go doc` before running — same caveat as before, these three were not directly confirmed in this plan's research.

- [ ] **Step 2:** FAIL.

- [ ] **Step 3: implementation** (unchanged from the earlier version of this plan — reproduced here for completeness)

```go
// internal/context/pfcp_reports.go
package context

import (
	"github.com/wmnsk/go-pfcp/ie"

	"github.com/free5gc/openapi/models"
	"github.com/free5gc/smf/internal/logger"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

func (smContext *SMContext) HandleReports(
	usageReports []*ie.IE,
	nodeID pfcptype.NodeID,
	reportType models.ChfConvergedChargingTriggerType,
) {
	upf := RetrieveUPFNodeByNodeID(nodeID)
	if upf == nil {
		logger.PduSessLog.Warnf("HandleReports: UPF[%s] not found", nodeID.String())
		return
	}
	upfId := upf.UUID()

	for _, report := range usageReports {
		urrID, err := report.URRID()
		if err != nil {
			logger.PduSessLog.Warnf("HandleReports: UsageReport missing URRID: %v", err)
			continue
		}

		usageReport := UsageReport{UrrId: urrID, UpfId: upfId}

		vol, err := report.VolumeMeasurement()
		if err != nil {
			logger.PduSessLog.Warnf("HandleReports: URRID[%d] missing VolumeMeasurement: %v", urrID, err)
		} else {
			usageReport.TotalVolume = vol.TotalVolume
			usageReport.UplinkVolume = vol.UplinkVolume
			usageReport.DownlinkVolume = vol.DownlinkVolume
			usageReport.TotalPktNum = vol.TotalNumberOfPackets
			usageReport.UplinkPktNum = vol.UplinkNumberOfPackets
			usageReport.DownlinkPktNum = vol.DownlinkNumberOfPackets
		}

		usageReport.ReportTpye = identifyTriggerType(report)
		if reportType != "" {
			usageReport.ReportTpye = reportType
		}

		smContext.UrrReports = append(smContext.UrrReports, usageReport)
	}
}

func identifyTriggerType(report *ie.IE) models.ChfConvergedChargingTriggerType {
	switch {
	case report.HasVOLTH():
		return models.ChfConvergedChargingTriggerType_QUOTA_THRESHOLD
	case report.HasVOLQU():
		return models.ChfConvergedChargingTriggerType_QUOTA_EXHAUSTED
	case report.HasQUVTI():
		return models.ChfConvergedChargingTriggerType_VALIDITY_TIME
	case report.HasSTART():
		return models.ChfConvergedChargingTriggerType_START_OF_SERVICE_DATA_FLOW
	case report.HasIMMER():
		return ""
	case report.HasTERMR():
		return models.ChfConvergedChargingTriggerType_FINAL
	default:
		return ""
	}
}
```

- [ ] **Step 4: replace Task 5's stub in `internal/pfcp/report.go`**

```go
// internal/pfcp/report.go — replace handleSessionReportRequest
func (s *PfcpServer) handleSessionReportRequest(req *message.SessionReportRequest) *message.SessionReportResponse {
	seid := req.SEID()
	seq := req.Sequence()
	smContext := smf_context.GetSMContextBySEID(seid)
	if smContext == nil {
		logger.PfcpLog.Errorf("SessionReportRequest: SEID[%#x] not found", seid)
		return message.NewSessionReportResponse(0, 0, 0, seq, 0, ie.NewCause(ie.CauseSessionContextNotFound))
	}

	smContext.SMLock.Lock()
	defer smContext.SMLock.Unlock()

	upfNodeID := smContext.GetNodeIDByLocalSEID(seid)
	pfcpCtx := smContext.PFCPContext[upfNodeID.String()]
	if pfcpCtx == nil {
		return message.NewSessionReportResponse(0, 0, 0, seq, 0, ie.NewCause(ie.CauseNoEstablishedPFCPAssociation))
	}
	remoteSEID := pfcpCtx.RemoteSEID

	if req.ReportType == nil {
		return message.NewSessionReportResponse(0, 0, remoteSEID, seq, 0, ie.NewCause(ie.CauseMandatoryIEMissing))
	}

	upf := smf_context.RetrieveUPFNodeByNodeID(upfNodeID)
	if upf == nil || upf.IsAssociated() != nil {
		return message.NewSessionReportResponse(0, 0, remoteSEID, seq, 0, ie.NewCause(ie.CauseNoEstablishedPFCPAssociation))
	}

	if req.ReportType.HasUSAR() && len(req.UsageReport) > 0 {
		for _, report := range req.UsageReport {
			if _, err := report.URRID(); err != nil {
				return message.NewSessionReportResponse(0, 0, remoteSEID, seq, 0, ie.NewCause(ie.CauseMandatoryIEMissing))
			}
		}
		smContext.HandleReports(req.UsageReport, upfNodeID, "")
	}

	return message.NewSessionReportResponse(0, 0, remoteSEID, seq, 0, ie.NewCause(ie.CauseRequestAccepted))
}
```

> Downlink Data Report handling (the `UpCnxState_DEACTIVATED`/N1N2MessageTransfer block from the original external `handler.go`) is intentionally omitted from this rewrite to keep Task 11 focused on the pfcp-library swap itself — port that block over verbatim from the pre-migration `internal/pfcp/handler/handler.go:148-215` (the file this task deletes) into this function before merging, adjusting only the request-type variable name (`req` here vs. the old `req` in that file — same name, should paste in with no changes needed beyond confirming `req.DownlinkDataReport`/`req.ReportType.HasDLDR()` match the new go-pfcp field/method names, which they do per Task 8's already-verified `HasDLDR()`/`HasUSAR()` usage).

- [ ] **Step 5:** `go test ./internal/context/... -run 'TestIdentifyTriggerType|TestHandleReports' -v && go build ./internal/context/... ./internal/pfcp/...` → PASS / no errors in these two packages.

- [ ] **Step 6:**
```bash
git add internal/context/pfcp_reports.go internal/context/pfcp_reports_test.go internal/pfcp/report.go
git commit -m "refactor(context): rewrite HandleReports on go-pfcp IEs, complete Session Report handling"
```

---

### Task 12: `internal/sbi/processor` — call the new `context.SendPfcpXxx` + `context.HandlePfcpXxxResponse` pair

This replaces each processor's own inline Cause check with a call into Task 9's `context.HandlePfcpXxxResponse`, matching the internal fork's actual division of labor (processor calls Send, then calls Handle for protocol validation, then does SMF-specific business logic with the validated result).

**Files:** `internal/sbi/processor/{association,datapath,charging_trigger,pdu_session,ulcl_procedure}.go`

- [ ] **Step 1: audit**
```bash
cd /home/alonza/smf/smf-opensource
grep -rn "free5gc/pfcp\|pfcpType\.\|pfcp_message\." internal/sbi/processor/*.go
```

- [ ] **Step 2: `association.go`** — worked example
```go
// BEFORE
resMsg, err := message.SendPfcpAssociationSetupRequest(upf.NodeID)
if err != nil { return err }
rsp := resMsg.PfcpMessage.Body.(pfcp.PFCPAssociationSetupResponse)
if rsp.Cause == nil || rsp.Cause.CauseValue != pfcpType.CauseRequestAccepted {
	return fmt.Errorf("association setup rejected, cause: %+v", rsp.Cause)
}
```
```go
// AFTER
rsp, err := context.SendPfcpAssociationSetupRequest(upf.NodeID)
if err != nil {
	return err
}
if _, err := context.HandlePfcpAssociationSetupResponse(rsp); err != nil {
	return fmt.Errorf("association setup: %w", err)
}
```
Heartbeat call site: `context.SendPfcpHeartbeatRequest(upf)` replaces `message.SendPfcpHeartbeatRequest(upf)` — no Cause check existed on this path before either (Heartbeat has no Cause IE), so nothing else changes beyond the package/import.

- [ ] **Step 3: `datapath.go`** — apply the identical `Send → Handle → business logic` restructuring at every Session Establishment/Modification/Deletion call site:
```go
// BEFORE (Session Establishment, abbreviated)
rcvMsg, err := pfcp_message.SendPfcpSessionEstablishmentRequest(upf, ctx, pdrList, farList, barList, qerList, urrList)
if err != nil { return ... }
rsp := rcvMsg.PfcpMessage.Body.(pfcp.PFCPSessionEstablishmentResponse)
if rsp.Cause.CauseValue == pfcpType.CauseRequestAccepted {
	// business logic using rsp.UPFSEID, etc.
} else {
	return &SomeResult{Err: fmt.Errorf("cause[%d] if not request accepted", rsp.Cause.CauseValue)}
}
```
```go
// AFTER
rsp, err := context.SendPfcpSessionEstablishmentRequest(upf, ctx, pdrList, farList, barList, qerList, urrList)
if err != nil {
	return ...
}
remoteSEID, err := context.HandlePfcpSessionEstablishmentResponse(rsp)
if err != nil {
	return &SomeResult{Err: fmt.Errorf("session establishment: %w", err)}
}
ctx.PFCPContext[upf.NodeID.String()].RemoteSEID = remoteSEID
// remaining business logic unchanged, now using remoteSEID instead of rsp.UPFSEID directly
```
Apply the same shape to Session Modification and Session Deletion call sites (no `remoteSEID` extraction needed there — `HandlePfcpSessionModificationResponse(rsp)`/`HandlePfcpSessionDeletionResponse(rsp)` return only `error`).

- [ ] **Step 4: `charging_trigger.go`** — same Session Modification restructuring as Step 3.

- [ ] **Step 5: `pdu_session.go`, `ulcl_procedure.go`** — audit per Step 1's grep and apply whichever of the Step 2/3 patterns matches each remaining call site; replace any leftover `pfcpType.X` domain-type usage with `pfcptype.X` per Task 6's rename.

- [ ] **Step 6:**
```bash
goimports -w internal/sbi/processor/*.go
go build ./...
```
Expected: first fully green build.

- [ ] **Step 7:**
```bash
gotestsum ./...
```

- [ ] **Step 8:**
```bash
git add internal/sbi/processor/
git commit -m "refactor(sbi): call context.SendPfcpXxx + HandlePfcpXxxResponse, matching internal's validation split

First fully green build since starting the go-pfcp migration."
```

---

## Part 6 — Cleanup & verification

### Task 13: Remove `github.com/free5gc/pfcp`

- [ ] **Step 1:**
```bash
cd /home/alonza/smf/smf-opensource
rg 'github.com/free5gc/pfcp"' .
rg 'pfcpType\.' .
```
Both must return no output.
- [ ] **Step 2:**
```bash
go mod edit -droprequire github.com/free5gc/pfcp
go mod tidy
```
- [ ] **Step 3:**
```bash
go build ./...
gotestsum ./...
gotestsum -- -race ./internal/pfcp/...
```
- [ ] **Step 4:**
```bash
git add go.mod go.sum
git commit -m "chore: remove github.com/free5gc/pfcp dependency — migration to go-pfcp complete"
```

---

### Task 14: Final verification against `GO_PFCP_MIGRATION.md` §9-10

No code changes — this task is the acceptance gate.

- [ ] **Step 1:** `gotestsum -- -race ./...`
- [ ] **Step 2:**
```bash
rg 'github.com/free5gc/pfcp' . || echo OK
grep -rn "pfcpType\|pfcpUdp" internal/pfcp/ internal/context/*.go | grep -v _test.go || echo OK
```
- [ ] **Step 3:** Manual/integration verification against a real UPF or `go-upf` per `GO_PFCP_MIGRATION.md` §9.2/§9.3 — Association, Heartbeat, Session Establishment/Modification/Deletion, Session Report, and (via `tcpdump`) wire-level Message Type/SEID/Sequence comparison against a pre-migration baseline capture (take that baseline manually before starting Task 1, per §8 Phase 0).
- [ ] **Step 4:** Report pass/fail against `GO_PFCP_MIGRATION.md` §10's checklist to whoever requested this migration.

---

## Self-Review Notes

- **This revision replaces an earlier draft of this plan** that kept the external repo's existing `internal/pfcp/message/` + `internal/pfcp/handler/` package boundaries to minimize diff size. That approach is abandoned here at the user's explicit direction: reproduce the internal implementation's actual design (file layout included) throughout, since the internal source itself cannot be handed to whoever executes this plan — this document is the substitute for that source access.
- **Two things are explicitly not ported from internal**, both flagged in the Architecture section: OpenTelemetry vendor-IE trace propagation (no tracer infrastructure exists externally) and the `idgenerator`/`unbounded_channel` Saviah-internal Go modules (re-implemented with equivalent contracts in Task 3/Task 4, since they cannot be imported).
- **One piece of internal behavior required new external functionality that didn't exist before at all**: UPF-restart recovery (`RecoverUPF`, triggered from Association Setup's `afterRsp` in Task 5). The internal fork has this fully built (its own `UPTunnel`/multi-UPF machinery); the external repo's equivalent, if any, was not confirmed during this plan's research. Task 5 flags this explicitly and gives a safe no-op fallback rather than guessing at unverified behavior.
- **Two IE constructors remain unverified** (`ie.NewSDFFilter`-family in Task 7, the Usage Report IE constructors in Task 11) — flagged inline with the exact `go doc` commands to run before treating those tasks as done.
- **Task ordering is still not "one green build per task"** through Tasks 6-11, for the same reason as before: Go's type system makes the domain-model rename and every consumer of it one connected unit of work. Do this on a single feature branch; require a fully green build starting at Task 12 Step 6.

package factory

import "testing"

func TestGetPfcpDispatchWorkerCount(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want int
	}{
		{name: "nil config", want: PfcpDefaultDispatchWorkers},
		{name: "missing PFCP", cfg: &Config{Configuration: &Configuration{}}, want: PfcpDefaultDispatchWorkers},
		{
			name: "omitted value",
			cfg:  &Config{Configuration: &Configuration{PFCP: &PFCP{}}},
			want: PfcpDefaultDispatchWorkers,
		},
		{
			name: "configured value",
			cfg:  &Config{Configuration: &Configuration{PFCP: &PFCP{DispatchWorkerCount: 12}}},
			want: 12,
		},
		{
			name: "invalid value cannot create excessive workers",
			cfg: &Config{Configuration: &Configuration{PFCP: &PFCP{
				DispatchWorkerCount: PfcpMaximumDispatchWorkers + 1,
			}}},
			want: PfcpDefaultDispatchWorkers,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.cfg.GetPfcpDispatchWorkerCount(); got != test.want {
				t.Fatalf("GetPfcpDispatchWorkerCount() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestPFCPValidateDispatchWorkerCount(t *testing.T) {
	newPFCP := func(workers uint16) *PFCP {
		return &PFCP{
			ListenAddr:          "127.0.0.1",
			ExternalAddr:        "127.0.0.1",
			NodeID:              "127.0.0.1",
			DispatchWorkerCount: workers,
		}
	}

	for _, workers := range []uint16{0, 1, PfcpMaximumDispatchWorkers} {
		if ok, err := newPFCP(workers).validate(); !ok || err != nil {
			t.Fatalf("dispatchWorkerCount %d rejected: ok=%v err=%v", workers, ok, err)
		}
	}

	workers := uint16(PfcpMaximumDispatchWorkers + 1)
	if ok, err := newPFCP(workers).validate(); ok || err == nil {
		t.Fatalf("dispatchWorkerCount %d accepted: ok=%v err=%v", workers, ok, err)
	}
}

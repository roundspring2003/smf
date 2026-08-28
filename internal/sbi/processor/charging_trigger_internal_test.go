package processor

import (
	"testing"

	"github.com/free5gc/openapi/models"
	smf_context "github.com/free5gc/smf/internal/context"
)

func TestBuildMultiUnitUsageIncludesFinalUsageReports(t *testing.T) {
	const urrID = uint32(77)
	smContext := &smf_context.SMContext{
		RequestedUnit: 4096,
		ChargingInfo: map[uint32]*smf_context.ChargingInfo{
			urrID: {
				ChargingMethod: models.Chf_ConvCharging_QuotaManagementIndicator_ONLINE_CHARGING,
				ChargingLevel:  smf_context.PduSessionCharging,
				RatingGroup:    9,
				UpfId:          "192.0.2.10",
			},
		},
		UrrReports: []smf_context.UsageReport{
			{
				UrrId:          urrID,
				UpfId:          "192.0.2.10",
				TotalVolume:    300,
				UplinkVolume:   100,
				DownlinkVolume: 200,
				ReportTpye:     models.Chf_ConvCharging_TriggerType_FINAL,
			},
		},
	}

	usages := buildMultiUnitUsageFromUsageReport(smContext)
	if len(usages) != 1 {
		t.Fatalf("MultipleUnitUsage count = %d, want 1", len(usages))
	}
	if got := usages[0].RatingGroup; got != 9 {
		t.Errorf("RatingGroup = %d, want 9", got)
	}
	if len(usages[0].UsedUnitContainer) != 1 {
		t.Fatalf("UsedUnitContainer count = %d, want 1", len(usages[0].UsedUnitContainer))
	}
	used := usages[0].UsedUnitContainer[0]
	if used.TotalVolume != 300 || used.UplinkVolume != 100 || used.DownlinkVolume != 200 {
		t.Errorf("reported volume = total:%d uplink:%d downlink:%d, want 300/100/200",
			used.TotalVolume, used.UplinkVolume, used.DownlinkVolume)
	}
	if len(used.Triggers) != 1 ||
		used.Triggers[0].TriggerType != models.Chf_ConvCharging_TriggerType_FINAL {
		t.Errorf("charging triggers = %+v, want FINAL", used.Triggers)
	}
	if len(smContext.UrrReports) != 0 {
		t.Errorf("cached Usage Reports count after conversion = %d, want 0", len(smContext.UrrReports))
	}
}

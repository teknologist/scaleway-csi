package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/scaleway/scaleway-csi/pkg/scaleway"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestControllerService(zone scw.Zone, servers []*instance.Server) *controllerService {
	return &controllerService{
		scaleway: scaleway.NewFake(servers, zone),
		config:   &DriverConfig{},
	}
}

// TestControllerPublishVolume_VolumeLimit tests Fix 3: defensive >= check.
func TestControllerPublishVolume_VolumeLimit(t *testing.T) {
	t.Parallel()
	zone := scw.ZoneFrPar1
	serverID := "server-1"

	// Pre-fill server with MaxVolumesPerNode volumes.
	volumes := make(map[string]*instance.VolumeServer)
	for i := 0; i < scaleway.MaxVolumesPerNode; i++ {
		key := string(rune('0' + i))
		volumes[key] = &instance.VolumeServer{
			ID:         "existing-vol-" + key,
			VolumeType: instance.VolumeServerVolumeType("sbs_volume"),
		}
	}

	server := &instance.Server{
		ID:      serverID,
		Name:    "full-server",
		Zone:    zone,
		Volumes: volumes,
	}

	cs := newTestControllerService(zone, []*instance.Server{server})

	vol, err := cs.scaleway.CreateVolume(context.Background(), "one-more-vol", "", scaleway.MinVolumeSize, nil, zone)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	_, err = cs.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: expandZonalID(vol.ID, zone),
		NodeId:   expandZonalID(serverID, zone),
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})
	if err == nil {
		t.Fatal("expected ResourceExhausted error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted, got %s: %s", st.Code(), st.Message())
	}
}

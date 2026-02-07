package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestNodeService(zone scw.Zone, server *instance.Server) *nodeService {
	return &nodeService{
		diskUtils: newFakeDiskUtils(server),
		nodeID:    server.ID,
		nodeZone:  zone,
	}
}

// TestNodeUnstageVolume_DeviceGone tests Fix 4 Bug A: when the device path is gone,
// NodeUnstageVolume should return success per CSI spec MUST requirement.
func TestNodeUnstageVolume_DeviceGone(t *testing.T) {
	t.Parallel()
	zone := scw.ZoneFrPar1
	server := &instance.Server{
		ID:      "test-server",
		Name:    "test",
		Zone:    zone,
		Volumes: map[string]*instance.VolumeServer{},
	}

	ns := newTestNodeService(zone, server)

	// Use a temp dir as staging path.
	stagingPath := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(stagingPath, 0o755); err != nil {
		t.Fatalf("failed to create staging path: %v", err)
	}

	// Volume ID that has no corresponding device in the fake (simulates device gone).
	volumeID := expandZonalID("nonexistent-volume-id", zone)

	resp, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume should return success when device is gone, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

// TestNodeUnstageVolume_StagingPathGone tests Fix 4 Bug B: when the staging path is gone,
// NodeUnstageVolume should return success per CSI spec MUST requirement.
func TestNodeUnstageVolume_StagingPathGone(t *testing.T) {
	t.Parallel()
	zone := scw.ZoneFrPar1
	volumeID := "test-volume-1234"
	server := &instance.Server{
		ID:   "test-server",
		Name: "test",
		Zone: zone,
		Volumes: map[string]*instance.VolumeServer{
			"0": {
				ID:         volumeID,
				VolumeType: instance.VolumeServerVolumeType("sbs_volume"),
			},
		},
	}

	ns := newTestNodeService(zone, server)

	// Use a path that doesn't exist as staging path.
	stagingPath := filepath.Join(t.TempDir(), "nonexistent-staging")

	resp, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          expandZonalID(volumeID, zone),
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume should return success when staging path is gone, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

// TestNodeUnstageVolume_MissingVolumeID tests that missing volumeID returns InvalidArgument.
func TestNodeUnstageVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	zone := scw.ZoneFrPar1
	server := &instance.Server{
		ID:      "test-server",
		Name:    "test",
		Zone:    zone,
		Volumes: map[string]*instance.VolumeServer{},
	}

	ns := newTestNodeService(zone, server)

	_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "",
		StagingTargetPath: "/tmp/staging",
	})
	if err == nil {
		t.Fatal("expected error for empty volumeID")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", st.Code())
	}
}

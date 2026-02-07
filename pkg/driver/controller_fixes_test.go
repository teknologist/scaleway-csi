package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/scaleway/scaleway-csi/pkg/scaleway"
	block "github.com/scaleway/scaleway-sdk-go/api/block/v1"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

func newTestControllerService(zone scw.Zone, servers []*instance.Server) *controllerService {
	return &controllerService{
		scaleway: scaleway.NewFake(servers, zone),
		config:   &DriverConfig{},
	}
}

// TestControllerUnpublishVolume_DeletedServer tests Fix 1: when the server is deleted,
// ControllerUnpublishVolume should detach the volume and return success.
func TestControllerUnpublishVolume_DeletedServer(t *testing.T) {
	t.Parallel()
	zone := scw.ZoneFrPar1
	serverID := "server-alive-1234"
	deletedServerID := "server-deleted-5678"

	server := &instance.Server{
		ID:      serverID,
		Name:    "alive-server",
		Zone:    zone,
		Volumes: map[string]*instance.VolumeServer{},
	}

	cs := newTestControllerService(zone, []*instance.Server{server})

	// Create and attach a volume to the alive server.
	vol, err := cs.scaleway.CreateVolume(context.Background(), "test-vol", "", scaleway.MinVolumeSize, nil, zone)
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	if err := cs.scaleway.AttachVolume(context.Background(), serverID, vol.ID, zone); err != nil {
		t.Fatalf("AttachVolume failed: %v", err)
	}

	// Detach and re-attach to set up the ghost-attach scenario.
	if err := cs.scaleway.DetachVolume(context.Background(), vol.ID, zone); err != nil {
		t.Fatalf("DetachVolume failed: %v", err)
	}
	if err := cs.scaleway.AttachVolume(context.Background(), serverID, vol.ID, zone); err != nil {
		t.Fatalf("AttachVolume (re-attach) failed: %v", err)
	}

	// Verify volume is in use.
	volCheck, err := cs.scaleway.GetVolume(context.Background(), vol.ID, zone)
	if err != nil {
		t.Fatalf("GetVolume failed: %v", err)
	}
	if volCheck.Status != block.VolumeStatusInUse {
		t.Fatalf("expected volume status InUse, got %s", volCheck.Status)
	}

	// Call ControllerUnpublishVolume with a server that doesn't exist.
	resp, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: expandZonalID(vol.ID, zone),
		NodeId:   expandZonalID(deletedServerID, zone),
	})
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume with deleted server should succeed, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Volume should now be available (detached).
	volAfter, err := cs.scaleway.GetVolume(context.Background(), vol.ID, zone)
	if err != nil {
		t.Fatalf("GetVolume after unpublish failed: %v", err)
	}
	if volAfter.Status != block.VolumeStatusAvailable {
		t.Errorf("expected volume status Available after unpublish from deleted server, got %s", volAfter.Status)
	}
}

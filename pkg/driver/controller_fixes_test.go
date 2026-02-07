package driver

import (
	"context"
	"fmt"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/scaleway/scaleway-csi/pkg/scaleway"
	block "github.com/scaleway/scaleway-sdk-go/api/block/v1"
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

func TestControllerPublishVolume_ForceDetachDeletedServer(t *testing.T) {
	t.Parallel()
	zone := scw.ZoneFrPar1

	deadServer := &instance.Server{
		ID:      "dead-server-id",
		Name:    "dead-server",
		Volumes: map[string]*instance.VolumeServer{},
		Zone:    zone,
	}
	aliveServer := &instance.Server{
		ID:      "alive-server-id",
		Name:    "alive-server",
		Volumes: map[string]*instance.VolumeServer{},
		Zone:    zone,
	}

	cs := newTestControllerService(zone, []*instance.Server{deadServer, aliveServer})

	// Create a volume and attach it to the dead server.
	ctx := context.Background()
	vol, err := cs.scaleway.CreateVolume(ctx, "test-vol", "", scaleway.MinVolumeSize, nil, zone)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := cs.scaleway.AttachVolume(ctx, deadServer.ID, vol.ID, zone); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	// Simulate the dead server being deleted (without cleaning up volume refs).
	cs.scaleway.(*scaleway.Fake).RemoveServer(deadServer.ID)

	// Now publish the volume to the alive server — should succeed via force-detach.
	resp, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: expandZonalID(vol.ID, zone),
		NodeId:   expandZonalID(aliveServer.ID, zone),
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume should succeed after force-detach, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify the volume is now attached to the alive server.
	updatedVol, err := cs.scaleway.GetVolume(ctx, vol.ID, zone)
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if len(updatedVol.References) == 0 {
		t.Fatal("expected volume to have references after publish")
	}
	if updatedVol.References[0].ProductResourceID != aliveServer.ID {
		t.Errorf("expected volume attached to %s, got %s", aliveServer.ID, updatedVol.References[0].ProductResourceID)
	}
}

func TestControllerPublishVolume_GenuineConflict(t *testing.T) {
	t.Parallel()
	zone := scw.ZoneFrPar1

	server1 := &instance.Server{
		ID:      "server-1-id",
		Name:    "server-1",
		Volumes: map[string]*instance.VolumeServer{},
		Zone:    zone,
	}
	server2 := &instance.Server{
		ID:      "server-2-id",
		Name:    "server-2",
		Volumes: map[string]*instance.VolumeServer{},
		Zone:    zone,
	}

	cs := newTestControllerService(zone, []*instance.Server{server1, server2})

	// Create a volume and attach it to server1.
	ctx := context.Background()
	vol, err := cs.scaleway.CreateVolume(ctx, "test-vol", "", scaleway.MinVolumeSize, nil, zone)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := cs.scaleway.AttachVolume(ctx, server1.ID, vol.ID, zone); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	// Try to publish to server2 — server1 still exists, so this should fail.
	_, err = cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: expandZonalID(vol.ID, zone),
		NodeId:   expandZonalID(server2.ID, zone),
		VolumeCapability: &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err == nil {
		t.Fatal("expected FailedPrecondition error when old server still exists")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("expected code %s, got %s: %s", codes.FailedPrecondition, st.Code(), st.Message())
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
		key := fmt.Sprintf("%d", i)
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

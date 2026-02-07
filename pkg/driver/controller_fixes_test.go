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

func TestControllerPublishVolume_ForceDetachDeletedServer(t *testing.T) {
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

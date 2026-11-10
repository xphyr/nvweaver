package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Identity says what the driver is. Probe asks the API, when there is one, as
// the controller is no use without it. The node plugin needs no API.
type Identity struct {
	csi.UnimplementedIdentityServer
	cfg Config
	api API // nil on the node plugin
}

func NewIdentity(cfg Config, api API) *Identity { return &Identity{cfg: cfg, api: api} }

func (s *Identity) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: s.cfg.Name, VendorVersion: s.cfg.Version}, nil
}

func (s *Identity) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	caps := []*csi.PluginCapability{
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE}}},
		{Type: &csi.PluginCapability_VolumeExpansion_{VolumeExpansion: &csi.PluginCapability_VolumeExpansion{Type: csi.PluginCapability_VolumeExpansion_ONLINE}}},
	}
	if s.cfg.SnapshotMetadata && s.api != nil {
		// Only where it's served: the controller, when asked for.
		caps = append(caps, &csi.PluginCapability{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{
			Type: csi.PluginCapability_Service_SNAPSHOT_METADATA_SERVICE}}})
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: caps}, nil
}

func (s *Identity) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	if s.api == nil {
		return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
	}
	if _, err := s.api.Health(ctx); err != nil {
		return nil, apiError(err, "nvweaver's API")
	}
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}

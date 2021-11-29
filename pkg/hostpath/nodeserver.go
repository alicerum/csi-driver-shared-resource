/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hostpath

import (
	"fmt"
	"os"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	sharev1alpha1 "github.com/openshift/api/sharedresource/v1alpha1"
	"github.com/openshift/csi-driver-shared-resource/pkg/client"
	"github.com/openshift/csi-driver-shared-resource/pkg/consts"
	"github.com/openshift/csi-driver-shared-resource/pkg/metrics"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/klog/v2"
	"k8s.io/utils/mount"
)

var (
	listers client.Listers
)

type nodeServer struct {
	nodeID            string
	maxVolumesPerNode int64
	hp                HostPathDriver
	readOnlyMounter   FileSystemMounter
	readWriteMounter  FileSystemMounter
	mounter           mount.Interface
	alwaysReadOnly    bool
}

func NewNodeServer(hp *hostPath, alwaysReadOnly bool) *nodeServer {
	return &nodeServer{
		nodeID:            hp.nodeID,
		maxVolumesPerNode: hp.maxVolumesPerNode,
		hp:                hp,
		mounter:           mount.New(""),
		readOnlyMounter:   &WriteOnceReadMany{},
		readWriteMounter:  &ReadWriteMany{},
		alwaysReadOnly:    alwaysReadOnly,
	}
}

func getPodDetails(volumeContext map[string]string) (string, string, string, string) {
	podName, _ := volumeContext[CSIPodName]
	podNamespace, _ := volumeContext[CSIPodNamespace]
	podSA, _ := volumeContext[CSIPodSA]
	podUID, _ := volumeContext[CSIPodUID]
	return podNamespace, podName, podUID, podSA

}

func (ns *nodeServer) validateShare(req *csi.NodePublishVolumeRequest) (*sharev1alpha1.SharedConfigMap, *sharev1alpha1.SharedSecret, error) {
	configMapShareName, cmok := req.GetVolumeContext()[SharedConfigMapShareKey]
	secretShareName, sok := req.GetVolumeContext()[SharedSecretShareKey]
	if (!cmok && !sok) || (len(strings.TrimSpace(configMapShareName)) == 0 && len(strings.TrimSpace(secretShareName)) == 0) {
		return nil, nil, status.Errorf(codes.InvalidArgument,
			"the csi driver reference is missing the volumeAttribute %q and %q", SharedSecretShareKey, SharedConfigMapShareKey)
	}
	if (cmok && sok) || (len(strings.TrimSpace(configMapShareName)) > 0 && len(strings.TrimSpace(secretShareName)) > 0) {
		return nil, nil, status.Errorf(codes.InvalidArgument,
			"a single volume cannot support both a SharedConfigMap reference %q and SharedSecret reference %q",
			configMapShareName, secretShareName)
	}

	var cmShare *sharev1alpha1.SharedConfigMap
	var sShare *sharev1alpha1.SharedSecret
	var err error
	allowed := false
	if len(configMapShareName) > 0 {
		cmShare, err = client.GetListers().SharedConfigMaps.Get(configMapShareName)
		if err != nil {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"the csi driver volumeAttribute %q reference had an error: %s", configMapShareName, err.Error())
		}
	}
	if len(secretShareName) > 0 {
		sShare, err = client.GetListers().SharedSecrets.Get(secretShareName)
		if err != nil {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"the csi driver volumeAttribute %q reference had an error: %s", secretShareName, err.Error())
		}
	}

	if sShare == nil && cmShare == nil {
		return nil, nil, status.Errorf(codes.InvalidArgument,
			"volumeAttributes did not reference a valid SharedSecret or SharedConfigMap")
	}

	podNamespace, podName, _, podSA := getPodDetails(req.GetVolumeContext())
	shareName := ""
	kind := consts.ResourceReferenceTypeConfigMap
	if cmShare != nil {
		if len(strings.TrimSpace(cmShare.Spec.ConfigMapRef.Namespace)) == 0 {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"the SharedConfigMap %q backing resource namespace needs to be set", configMapShareName)
		}
		if len(strings.TrimSpace(cmShare.Spec.ConfigMapRef.Name)) == 0 {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"the SharedConfigMap %q backing resource name needs to be set", configMapShareName)
		}
		shareName = configMapShareName
	}
	if sShare != nil {
		kind = consts.ResourceReferenceTypeSecret
		if len(strings.TrimSpace(sShare.Spec.SecretRef.Namespace)) == 0 {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"the SharedSecret %q backing resource namespace needs to be set", secretShareName)
		}
		if len(strings.TrimSpace(sShare.Spec.SecretRef.Name)) == 0 {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"the SharedSecret %q backing resource name needs to be set", secretShareName)
		}
		shareName = secretShareName
	}

	allowed, err = client.ExecuteSAR(shareName, podNamespace, podName, podSA, kind)
	if allowed {
		return cmShare, sShare, nil
	}
	return nil, nil, err
}

// validateVolumeContext return values:
func (ns *nodeServer) validateVolumeContext(req *csi.NodePublishVolumeRequest) error {

	podNamespace, podName, podUID, podSA := getPodDetails(req.GetVolumeContext())
	klog.V(4).Infof("NodePublishVolume pod %s ns %s sa %s uid %s",
		podName,
		podNamespace,
		podSA,
		podUID)

	if len(podName) == 0 || len(podNamespace) == 0 || len(podUID) == 0 || len(podSA) == 0 {
		return status.Error(codes.InvalidArgument,
			fmt.Sprintf("Volume attributes missing required set for pod: namespace: %s name: %s uid: %s, sa: %s",
				podNamespace, podName, podUID, podSA))
	}
	ephemeralVolume := req.GetVolumeContext()[CSIEphemeral] == "true" ||
		req.GetVolumeContext()[CSIEphemeral] == "" // Kubernetes 1.15 doesn't have csi.storage.k8s.io/ephemeral.

	if !ephemeralVolume {
		return status.Error(codes.InvalidArgument, "Non-ephemeral request made")
	}

	if req.GetVolumeCapability().GetMount() == nil {
		return status.Error(codes.InvalidArgument, "only support mount access type")
	}
	return nil
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	var kubeletTargetPath string

	// Check arguments
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}
	if req.VolumeContext == nil || len(req.GetVolumeContext()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume attributes missing in request")
	}

	err := ns.validateVolumeContext(req)
	if err != nil {
		return nil, err
	}

	cmShare, sShare, err := ns.validateShare(req)
	if err != nil {
		return nil, err
	}

	kubeletTargetPath = req.GetTargetPath()
	// when on always-read-only mode it will make sure the volume mounts won't be writable at all
	readOnly := ns.alwaysReadOnly || req.GetReadonly()

	vol, err := ns.hp.createHostpathVolume(req.GetVolumeId(), kubeletTargetPath, readOnly, req.GetVolumeContext(), cmShare, sShare, maxStorageCapacity, mountAccess)
	if err != nil && !os.IsExist(err) {
		klog.Error("ephemeral mode failed to create volume: ", err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	klog.V(4).Infof("NodePublishVolume created volume: %s (read-only=%v)", kubeletTargetPath, readOnly)

	notMnt, err := mount.IsNotMountPoint(ns.mounter, kubeletTargetPath)

	if err != nil {
		if os.IsNotExist(err) {
			if err = os.MkdirAll(kubeletTargetPath, 0750); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	// this means the mount.Mounter call has already happened
	if !notMnt {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	fsType := req.GetVolumeCapability().GetMount().GetFsType()

	deviceId := ""
	if req.GetPublishContext() != nil {
		deviceId = req.GetPublishContext()[deviceID]
	}

	volumeId := req.GetVolumeId()
	attrib := req.GetVolumeContext()
	mountFlags := req.GetVolumeCapability().GetMount().GetMountFlags()

	klog.V(4).Infof("NodePublishVolume %v\nfstype %v\ndevice %v\nvolumeId %v\nattributes %v\nmountflags %v\n",
		kubeletTargetPath, fsType, deviceId, volumeId, attrib, mountFlags)

	anchorDir, bindDir := ns.hp.getVolumePath(req.GetVolumeId(), req.GetVolumeContext())
	switch {
	case readOnly:
		if err := ns.readOnlyMounter.makeFSMounts(anchorDir, bindDir, kubeletTargetPath, ns.mounter); err != nil {
			return nil, err
		}
	default:
		if err := ns.readWriteMounter.makeFSMounts(anchorDir, bindDir, kubeletTargetPath, ns.mounter); err != nil {
			return nil, err
		}

	}

	metrics.IncMountCounter()

	// here is what initiates that necessary copy now with *NOT* using bind on the mount so each pod gets its own tmpfs
	if err := ns.hp.mapVolumeToPod(vol); err != nil {
		metrics.IncMountFailedCounter()
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to populate mount device: %s at %s: %s",
			bindDir,
			kubeletTargetPath,
			err.Error()))
	}

	if err := storeVolMapToDisk(); err != nil {
		metrics.IncMountFailedCounter()
		klog.Errorf("failed to persist driver volume metadata to disk: %s", err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}
	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()

	hpv := ns.hp.getHostpathVolume(volumeID)
	if hpv == nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("unpublish volume %s already gone", volumeID))
	}
	var err error
	switch {
	case hpv.IsReadOnly():
		err = ns.readOnlyMounter.removeFSMounts(hpv.GetVolPathAnchorDir(), hpv.GetVolPathBindMountDir(), targetPath, ns.mounter)
	default:
		err = ns.readWriteMounter.removeFSMounts(hpv.GetVolPathAnchorDir(), hpv.GetVolPathBindMountDir(), targetPath, ns.mounter)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("error removing %s: %s", targetPath, err.Error()))

	}

	klog.V(4).Infof("volume %s at path %s has been unpublished.", volumeID, targetPath)

	if err := ns.hp.deleteHostpathVolume(volumeID); err != nil && !os.IsNotExist(err) {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to delete volume: %s", err))
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {

	topology := &csi.Topology{
		Segments: map[string]string{TopologyKeyNode: ns.nodeID},
	}

	return &csi.NodeGetInfoResponse{
		NodeId:             ns.nodeID,
		MaxVolumesPerNode:  ns.maxVolumesPerNode,
		AccessibleTopology: topology,
	}, nil
}

func (ns *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{},
	}, nil
}

func (ns *nodeServer) NodeGetVolumeStats(ctx context.Context, in *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodeExpandVolume is only implemented so the driver can be used for e2e testing.
func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

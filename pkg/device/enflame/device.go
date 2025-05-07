/*
Copyright 2024 The HAMi Authors.

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

package enflame

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/Project-HAMi/HAMi/pkg/util"
	"github.com/Project-HAMi/HAMi/pkg/util/nodelock"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type EnflameDevices struct {
}

const (
	EnflameGPUDevice     = "Enflame"
	EnflameGPUCommonWord = "Enflame"

	NodeLockEnflame = "hami.io/mutex.lock"

	EnflameUseUUID   = "enflame.com/use-gpuuuid"
	EnflameNoUseUUID = "enflame.com/nouse-gpuuuid"

	NodeHandshakeAnnos = "hami.io/node-handshake-vgcu"
	NodeRegisterAnnos  = "hami.io/node-vgcu-register"

	PodDevicesToAllocate = "hami.io/vgcu-devices-to-allocate"
	PodDevicesAllocated  = "hami.io/vgcu-devices-allocated"

	ResourceCountName  = "enflame.com/vgcu"
	ResourceMemoryName = "enflame.com/vgcu-memory"
	ResourceCoreName   = "enflame.com/vgcu-core"

	coresPerEnflameGCU = 24
)

var (
	EnflameResourceCount  string
	EnflameResourceMemory string
	EnflameResourceCore   string
)

type EnflameConfig struct {
	ResourceCountName  string `yaml:"resourceCountName"`
	ResourceMemoryName string `yaml:"resourceMemoryName"`
	ResourceCoreName   string `yaml:"resourceCoreName"`
}

func InitEnflameDevice(config EnflameConfig) *EnflameDevices {
	EnflameResourceCount = config.ResourceCountName
	util.InRequestDevices[EnflameGPUDevice] = PodDevicesToAllocate
	util.SupportDevices[EnflameGPUDevice] = PodDevicesAllocated
	util.HandshakeAnnos[EnflameGPUDevice] = NodeHandshakeAnnos
	return &EnflameDevices{}
}

func (dev *EnflameDevices) CommonWord() string {
	return EnflameGPUCommonWord
}

func ParseConfig(fs *flag.FlagSet) {
	fs.StringVar(&EnflameResourceCount, "enflame-name", ResourceCountName, "enflame resource count name")
	fs.StringVar(&EnflameResourceMemory, "enflame-resource-memory-name", ResourceMemoryName, "enflame resource memory name")
	fs.StringVar(&EnflameResourceCore, "enflame-resource-core-name", ResourceCoreName, "enflame resource core name")
}

func (dev *EnflameDevices) MutateAdmission(ctr *corev1.Container, p *corev1.Pod) (bool, error) {
	count, ok := ctr.Resources.Limits[corev1.ResourceName(EnflameResourceCount)]
	if ok {
		if count.Value() > 1 {
			ctr.Resources.Limits[corev1.ResourceName(EnflameResourceMemory)] = *resource.NewQuantity(0, resource.DecimalSI)
			ctr.Resources.Limits[corev1.ResourceName(EnflameResourceCore)] = *resource.NewQuantity(count.Value()*coresPerEnflameGCU, resource.DecimalSI)
			ctr.Resources.Requests[corev1.ResourceName(EnflameResourceMemory)] = *resource.NewQuantity(0, resource.DecimalSI)
			ctr.Resources.Requests[corev1.ResourceName(EnflameResourceCore)] = *resource.NewQuantity(count.Value()*coresPerEnflameGCU, resource.DecimalSI)
			return ok, nil
		}
		mem, memok := ctr.Resources.Limits[corev1.ResourceName(EnflameResourceMemory)]
		if !memok || mem.Value() == 0 {
			ctr.Resources.Limits[corev1.ResourceName(EnflameResourceMemory)] = *resource.NewQuantity(0, resource.DecimalSI)
			ctr.Resources.Limits[corev1.ResourceName(EnflameResourceCore)] = *resource.NewQuantity(coresPerEnflameGCU, resource.DecimalSI)
			ctr.Resources.Requests[corev1.ResourceName(EnflameResourceMemory)] = *resource.NewQuantity(0, resource.DecimalSI)
			ctr.Resources.Requests[corev1.ResourceName(EnflameResourceCore)] = *resource.NewQuantity(coresPerEnflameGCU, resource.DecimalSI)
			return ok, nil
		}
		_, coreok := ctr.Resources.Limits[corev1.ResourceName(EnflameResourceCore)]
		if !coreok {
			ctr.Resources.Limits[corev1.ResourceName(EnflameResourceCore)] = *resource.NewQuantity(0, resource.DecimalSI)
			ctr.Resources.Requests[corev1.ResourceName(EnflameResourceCore)] = *resource.NewQuantity(0, resource.DecimalSI)
			return ok, nil
		}
	}
	return ok, nil
}

func (dev *EnflameDevices) GetNodeDevices(n corev1.Node) ([]*util.DeviceInfo, error) {
	anno, ok := n.Annotations[NodeRegisterAnnos]
	if !ok {
		return []*util.DeviceInfo{}, fmt.Errorf("annos not found %s", NodeRegisterAnnos)
	}
	nodeDevices, err := util.UnMarshalNodeDevices(anno)
	if err != nil {
		klog.ErrorS(err, "failed to unmarshal node devices", "node", n.Name, "device annotation", anno)
		return []*util.DeviceInfo{}, err
	}
	if len(nodeDevices) == 0 {
		klog.InfoS("no gpu device found", "node", n.Name, "device annotation", anno)
		return []*util.DeviceInfo{}, errors.New("no device found on node")
	}
	return nodeDevices, nil
}

func (dev *EnflameDevices) PatchAnnotations(annoinput *map[string]string, pd util.PodDevices) map[string]string {
	devlist, ok := pd[EnflameGPUDevice]
	if ok && len(devlist) > 0 {
		(*annoinput)[util.InRequestDevices[EnflameGPUDevice]] = util.EncodePodSingleDevice(devlist)
		(*annoinput)[util.SupportDevices[EnflameGPUDevice]] = util.EncodePodSingleDevice(devlist)
	}
	return *annoinput
}

func (dev *EnflameDevices) LockNode(n *corev1.Node, p *corev1.Pod) error {
	found := false
	for _, val := range p.Spec.Containers {
		if (dev.GenerateResourceRequests(&val).Nums) > 0 {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	return nodelock.LockNode(n.Name, NodeLockEnflame, p)
}

func (dev *EnflameDevices) ReleaseNodeLock(n *corev1.Node, p *corev1.Pod) error {
	found := false
	for _, val := range p.Spec.Containers {
		if (dev.GenerateResourceRequests(&val).Nums) > 0 {
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	return nodelock.ReleaseNodeLock(n.Name, NodeLockEnflame, p, false)
}

func (dev *EnflameDevices) NodeCleanUp(nn string) error {
	return util.MarkAnnotationsToDelete(NodeHandshakeAnnos, nn)
}

func (dev *EnflameDevices) CheckType(annos map[string]string, d util.DeviceUsage, n util.ContainerDeviceRequest) (bool, bool, bool) {
	if strings.Compare(n.Type, EnflameGPUDevice) == 0 {
		return true, true, false
	}
	return false, false, false
}

func (dev *EnflameDevices) CheckUUID(annos map[string]string, d util.DeviceUsage) bool {
	useUUID, ok := annos[EnflameUseUUID]
	if ok {
		klog.V(5).Infof("check uuid for Enflame user uuid [%s], device id is %s", useUUID, d.ID)
		useUUIDs := strings.Split(useUUID, ",")
		for _, uuid := range useUUIDs {
			if d.ID == uuid {
				return true
			}
		}
		return false
	}

	nouseUUID, ok := annos[EnflameNoUseUUID]
	if ok {
		klog.V(5).Infof("check uuid for Enflame not user uuid [%s], device id is %s", nouseUUID, d.ID)
		nouseUUIDs := strings.Split(nouseUUID, ",")
		for _, uuid := range nouseUUIDs {
			if d.ID == uuid {
				return false
			}
		}
		return true
	}
	return true
}

func (dev *EnflameDevices) CheckHealth(devType string, n *corev1.Node) (bool, bool) {
	return util.CheckHealth(devType, n)
}

func (dev *EnflameDevices) GenerateResourceRequests(ctr *corev1.Container) util.ContainerDeviceRequest {
	klog.Info("Start to count enflame devices for container ", ctr.Name)
	countResource := corev1.ResourceName(EnflameResourceCount)
	memoryResource := corev1.ResourceName(EnflameResourceMemory)
	coreResource := corev1.ResourceName(EnflameResourceCore)
	res := util.ContainerDeviceRequest{}
	count, ok := ctr.Resources.Limits[countResource]
	memory, memOk := ctr.Resources.Limits[memoryResource]
	core, coreOk := ctr.Resources.Limits[coreResource]
	if !ok || !memOk || !coreOk {
		return res
	}
	countNum, ok := count.AsInt64()
	memoryNum, memOk := memory.AsInt64()
	coreNum, coreOk := core.AsInt64()
	if !ok || !memOk || !coreOk {
		return res
	}
	klog.Info("Found enflame devices")
	memoryNum = memoryNum * 1024
	res.Type = EnflameGPUDevice
	res.Nums = int32(countNum)
	res.Memreq = int32(memoryNum) / int32(countNum)
	res.Coresreq = int32(coreNum) / int32(countNum)
	if res.Memreq == 0 {
		res.MemPercentagereq = 100
	}
	return res
}

func (dev *EnflameDevices) CustomFilterRule(allocated *util.PodDevices, request util.ContainerDeviceRequest, toAllocate util.ContainerDevices, device *util.DeviceUsage) bool {
	return true
}

func (dev *EnflameDevices) ScoreNode(node *corev1.Node, podDevices util.PodSingleDevice, policy string) float32 {
	return 0
}

func (dev *EnflameDevices) AddResourceUsage(n *util.DeviceUsage, ctr *util.ContainerDevice) error {
	n.Used++
	n.Usedcores += ctr.Usedcores
	n.Usedmem += ctr.Usedmem
	return nil
}

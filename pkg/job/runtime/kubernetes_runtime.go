/*
Copyright (c) 2022 PaddlePaddle Authors. All Rights Reserve.

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

package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/jinzhu/copier"
	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	busv1alpha1 "volcano.sh/apis/pkg/apis/bus/v1alpha1"
	"volcano.sh/apis/pkg/apis/helpers"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	"paddleflow/pkg/apiserver/models"
	"paddleflow/pkg/common/config"
	"paddleflow/pkg/common/k8s"
	"paddleflow/pkg/common/schema"
	"paddleflow/pkg/job/api"
	"paddleflow/pkg/job/runtime/kubernetes/controller"
	"paddleflow/pkg/job/runtime/kubernetes/executor"
)

type KubeRuntime struct {
	schema.Cluster
	clientset        kubernetes.Interface
	dynamicClientOpt *k8s.DynamicClientOption
}

func NewKubeRuntime(cluster schema.Cluster) RuntimeService {
	kr := &KubeRuntime{
		Cluster: cluster,
	}
	return kr
}

func (kr *KubeRuntime) Name() string {
	return fmt.Sprintf("kubernetes runtime for cluster: %s", kr.Cluster.Name)
}

func (kr *KubeRuntime) BuildConfig() (*rest.Config, error) {
	var cfg *rest.Config
	// decode credential base64 string to []byte
	configBytes, decodeErr := base64.StdEncoding.DecodeString(kr.Cluster.ClientOpt.Config)
	if decodeErr != nil {
		err := fmt.Errorf("decode cluster[%s] credential base64 string error! msg: %s",
			kr.Cluster.Name, decodeErr.Error())
		return nil, err
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(configBytes)
	if err != nil {
		log.Errorf("Failed to build kube config from kubeConfBytes[%s], err:[%v]", string(configBytes[:]), err)
		return nil, err
	}

	// set qps, burst
	cfg.QPS = kr.Cluster.ClientOpt.QPS
	cfg.Burst = kr.Cluster.ClientOpt.Burst
	return cfg, nil
}

func (kr *KubeRuntime) Init() error {
	config, err := kr.BuildConfig()
	if err != nil {
		log.Errorf("build config failed. error:%s", err)
		return err
	}
	kr.dynamicClientOpt, err = k8s.CreateDynamicClientOpt(config)
	if err != nil {
		log.Errorf("init dynamic client failed. error:%s", err)
		return err
	}
	// new kubernetes typed client
	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Errorf("create kubernetes client failed, err: %v", err)
		return err
	}
	kr.clientset = k8sClient
	return nil
}

func (kr *KubeRuntime) SubmitJob(jobInfo *api.PFJob) error {
	log.Infof("submit job[%v] to cluster[%s] queue[%s]", jobInfo.ID, kr.Cluster.ID, jobInfo.QueueID)
	// prepare kubernetes storage
	if len(jobInfo.FSID) != 0 {
		pvName, err := kr.CreatePV(jobInfo.Namespace, jobInfo.FSID, jobInfo.UserName)
		if err != nil {
			log.Errorf("create pv for job[%s] failed, err: %v", jobInfo.ID, err)
			return err
		}
		err = kr.CreatePVC(jobInfo.Namespace, jobInfo.FSID, pvName)
		if err != nil {
			log.Errorf("create pvc for job[%s] failed, err: %v", jobInfo.ID, err)
			return err
		}
	}
	// submit job
	job, err := executor.NewKubeJob(jobInfo, kr.dynamicClientOpt)
	if err != nil {
		log.Warnf("new kubernetes job[%s] failed, err: %v", jobInfo.Name, err)
		return err
	}
	jobID, err := job.CreateJob()
	if err != nil {
		log.Warnf("create kubernetes job[%s] failed, err: %v", jobInfo.Name, err)
		return err
	}
	log.Debugf("submit job[%s] successful", jobID)
	return nil
}

func (kr *KubeRuntime) StopJob(jobInfo *api.PFJob) error {
	log.Infof("stop job[%v] on cluster[%s] queue[%s]", jobInfo.ID, kr.Cluster.ID, jobInfo.ID)
	job, err := executor.NewKubeJob(jobInfo, kr.dynamicClientOpt)
	if err != nil {
		log.Warnf("stop kubernetes job[%s] failed, err: %v", jobInfo.Name, err)
		return err
	}
	err = job.StopJobByID(jobInfo.ID)
	if err != nil && !k8serrors.IsNotFound(err) {
		log.Warnf("stop kubernetes job[%s] failed, err: %v", jobInfo.Name, err)
		return err
	}
	log.Debugf("stop job[%s] successful", jobInfo.ID)
	return nil
}

func (kr *KubeRuntime) SyncJob(stopCh <-chan struct{}) {
	log.Infof("start job sync loop for cluster[%s]", kr.Cluster.ID)

	syncController, err := controller.New(controller.JobSyncControllerName, kr.dynamicClientOpt.Config)
	if err != nil {
		log.Errorf("init sync controller failed, err: %v", err)
		return
	}
	go syncController.Run(stopCh)
}

func (kr *KubeRuntime) GCJob(stopCh <-chan struct{}) {
	log.Infof("start job gc loop for cluster[%s]", kr.Cluster.ID)

	gcController, err := controller.New(controller.JobGCControllerName, kr.dynamicClientOpt.Config)
	if err != nil {
		log.Errorf("init sync controller failed, err: %v", err)
		return
	}
	go gcController.Run(stopCh)
}

func (kr *KubeRuntime) SyncQueue(stopCh <-chan struct{}) {
	log.Infof("start queue sync loop for cluster[%s]", kr.Cluster.ID)

	queueController, err := controller.New(controller.QueueSyncControllerName, kr.dynamicClientOpt.Config)
	if err != nil {
		log.Errorf("init queue sync controller failed, err: %v", err)
		return
	}
	go queueController.Run(stopCh)

}

func (kr *KubeRuntime) CreateQueue(q *models.Queue) error {
	resourceList := apiv1.ResourceList{}
	resourceList[apiv1.ResourceCPU] = resource.MustParse(q.Cpu)
	resourceList[apiv1.ResourceMemory] = resource.MustParse(q.Mem)
	for k, v := range q.ScalarResources {
		resourceList[apiv1.ResourceName(k)] = resource.MustParse(v)
	}
	log.Debugf("CreateQueue resourceList[%v]", resourceList)

	queue := &schedulingv1beta1.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: q.Name,
		},
		Spec: schedulingv1beta1.QueueSpec{
			Capability: resourceList,
		},
		Status: schedulingv1beta1.QueueStatus{
			State: schedulingv1beta1.QueueStateOpen,
		},
	}
	log.Debugf("CreateQueue queue info:%#v", queue)
	if err := executor.Create(queue, k8s.VCQueueGVK, kr.dynamicClientOpt); err != nil {
		log.Errorf("CreateQueue error. queueName:[%s], error:[%s]", q.Name, err.Error())
		return err

	}
	return nil
}

func (kr *KubeRuntime) DeleteQueue(q *models.Queue) error {
	err := executor.Delete("", q.Name, k8s.VCQueueGVK, kr.dynamicClientOpt)
	if err != nil && !k8serrors.IsNotFound(err) {
		log.Errorf("DeleteQueue error. queueName:[%s], error:[%s]", q.Name, err.Error())
		return err
	}
	return nil
}

func (kr *KubeRuntime) CloseQueue(q *models.Queue) error {
	return kr.executeQueueAction(q, busv1alpha1.CloseQueueAction)
}

func (kr *KubeRuntime) executeQueueAction(q *models.Queue, action busv1alpha1.Action) error {
	obj, err := executor.Get("", q.Name, k8s.VCQueueGVK, kr.dynamicClientOpt)
	if err != nil {
		log.Errorf("execute queue action get queue failed. queueName:[%s]", q.Name)
		return err
	}

	ctrlRef := metav1.NewControllerRef(obj, helpers.V1beta1QueueKind)
	cmd := &busv1alpha1.Command{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%s-",
				obj.GetName(), strings.ToLower(string(action))),
			OwnerReferences: []metav1.OwnerReference{
				*ctrlRef,
			},
		},
		TargetObject: ctrlRef,
		Action:       string(action),
	}
	if err = executor.Create(cmd, k8s.VCQueueGVK, kr.dynamicClientOpt); err != nil {
		log.Errorf("execute queue action failed. queueName:[%s] err:[%s]", q.Name, err.Error())
		return err
	}
	return nil
}

func (kr *KubeRuntime) UpdateQueue(q *models.Queue) error {
	// TODO: add update queue logic
	return nil
}

func (kr *KubeRuntime) CreatePV(namespace, fsId, userName string) (string, error) {
	pv := config.DefaultPV
	// format pvname to fsid
	pvName := strings.Replace(pv.Name, schema.FSIDFormat, fsId, -1)
	pvName = strings.Replace(pvName, schema.NameSpaceFormat, namespace, -1)
	// check pv existence
	if _, err := kr.getPersistentVolume(pvName, metav1.GetOptions{}); err == nil {
		return "", nil
	} else if !k8serrors.IsNotFound(err) {
		return "", err
	}
	// construct a new pv
	newPV := &apiv1.PersistentVolume{}
	if err := copier.Copy(newPV, pv); err != nil {
		return "", err
	}
	newPV.Name = pvName
	csi := newPV.Spec.CSI
	if csi != nil && csi.VolumeAttributes != nil {
		if _, ok := csi.VolumeAttributes[schema.FSID]; ok {
			newPV.Spec.CSI.VolumeAttributes[schema.FSID] = fsId
			newPV.Spec.CSI.VolumeHandle = pvName
		}
		if _, ok := csi.VolumeAttributes[schema.PFSUserName]; ok {
			newPV.Spec.CSI.VolumeAttributes[schema.PFSUserName] = userName
		}
		if _, ok := csi.VolumeAttributes[schema.PFSServer]; ok {
			newPV.Spec.CSI.VolumeAttributes[schema.PFSServer] = fmt.Sprintf("%s:%d",
				config.GlobalServerConfig.Fs.K8sServiceName, config.GlobalServerConfig.Fs.K8sServicePort)
		}
	}
	// create pv in k8s
	if _, err := kr.createPersistentVolume(newPV); err != nil {
		return "", err
	}
	return pvName, nil
}

func (kr *KubeRuntime) CreatePVC(namespace, fsId, pv string) error {
	pvc := config.DefaultPVC
	pvcName := strings.Replace(pvc.Name, schema.FSIDFormat, fsId, -1)
	// check pvc existence
	if _, err := kr.getPersistentVolumeClaim(namespace, pvcName, metav1.GetOptions{}); err == nil {
		return nil
	} else if !k8serrors.IsNotFound(err) {
		return err
	}
	// construct a new pvc
	newPVC := &apiv1.PersistentVolumeClaim{}
	if err := copier.Copy(newPVC, pvc); err != nil {
		return err
	}
	newPVC.Namespace = namespace
	newPVC.Name = pvcName
	newPVC.Spec.VolumeName = pv
	// create pvc in k8s
	if _, err := kr.createPersistentVolumeClaim(namespace, newPVC); err != nil {
		return err
	}
	return nil
}

func (kr *KubeRuntime) GetJobLog(jobLogRequest schema.JobLogRequest) (schema.JobLogInfo, error) {
	return getKubernetesLogs(kr.clientset, jobLogRequest)
}

func (kr *KubeRuntime) createPersistentVolume(pv *apiv1.PersistentVolume) (*apiv1.PersistentVolume, error) {
	return kr.clientset.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
}

func (kr *KubeRuntime) deletePersistentVolume(name string, deleteOptions *metav1.DeleteOptions) error {
	return kr.clientset.CoreV1().PersistentVolumes().Delete(context.TODO(), name, *deleteOptions)
}

func (kr *KubeRuntime) getPersistentVolume(name string, getOptions metav1.GetOptions) (*apiv1.PersistentVolume, error) {
	return kr.clientset.CoreV1().PersistentVolumes().Get(context.TODO(), name, getOptions)
}

func (kr *KubeRuntime) createPersistentVolumeClaim(namespace string, pvc *apiv1.PersistentVolumeClaim) (*apiv1.
	PersistentVolumeClaim, error) {
	return kr.clientset.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
}

func (kr *KubeRuntime) deletePersistentVolumeClaim(namespace string, name string,
	deleteOptions *metav1.DeleteOptions) error {
	return kr.clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(context.TODO(), name, *deleteOptions)
}

func (kr *KubeRuntime) getPersistentVolumeClaim(namespace, name string, getOptions metav1.GetOptions) (*apiv1.
	PersistentVolumeClaim, error) {
	return kr.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), name, getOptions)
}

func (kr *KubeRuntime) listNodes(listOptions metav1.ListOptions) (*v1.NodeList, error) {
	return kr.clientset.CoreV1().Nodes().List(context.TODO(), listOptions)
}

func (kr *KubeRuntime) listPods(namespace string, listOptions metav1.ListOptions) (*v1.PodList, error) {
	return kr.clientset.CoreV1().Pods(namespace).List(context.TODO(), listOptions)
}

func (kr *KubeRuntime) getNodeQuotaListImpl(subQuotaFn func(r *schema.Resource, pod *apiv1.Pod) error) (schema.QuotaSummary, []schema.NodeQuotaInfo, error) {
	result := []schema.NodeQuotaInfo{}
	summary := schema.QuotaSummary{
		TotalQuota: *k8s.NewResource(v1.ResourceList{}),
		IdleQuota:  *k8s.NewResource(v1.ResourceList{}),
	}
	nodes, _ := kr.listNodes(metav1.ListOptions{})
	log.Infof("ListNodeQuota nodes Items len: %d", len(nodes.Items))

	for _, node := range nodes.Items {
		nodeSchedulable := !node.Spec.Unschedulable
		// 过滤掉不能调度的节点
		if !nodeSchedulable {
			continue
		}
		totalQuota := k8s.NewResource(node.Status.Allocatable)
		idleQuota := k8s.NewResource(node.Status.Allocatable)
		nodeName := node.ObjectMeta.Name
		log.Infof("nodeName: %s, totalQuota: %v, idleQuota: %v", nodeName, totalQuota, idleQuota)

		fieldSelector := "status.phase!=Succeeded,status.phase!=Failed," +
			"status.phase!=Unknown,spec.nodeName=" + nodeName

		pods, _ := kr.listPods("", metav1.ListOptions{
			FieldSelector: fieldSelector,
		})
		for _, pod := range pods.Items {
			err := subQuotaFn(idleQuota, &pod)
			if err != nil {
				return summary, result, err
			}
		}

		nodeQuota := schema.NodeQuotaInfo{
			NodeName:    nodeName,
			Schedulable: nodeSchedulable,
			Total:       *totalQuota,
			Idle:        *idleQuota,
		}
		result = append(result, nodeQuota)
		summary.TotalQuota.Add(totalQuota)
		summary.IdleQuota.Add(idleQuota)
	}

	return summary, result, nil
}

// 返回quota信息
func (kr *KubeRuntime) ListNodeQuota() (schema.QuotaSummary, []schema.NodeQuotaInfo, error) {
	return kr.getNodeQuotaListImpl(k8s.SubQuota)
}
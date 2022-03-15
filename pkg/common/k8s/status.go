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

package k8s

import (
	"fmt"

	paddlejobv1 "github.com/paddleflow/paddle-operator/api/v1"
	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	sparkoperatorv1beta2 "paddleflow/pkg/apis/spark-operator/sparkoperator.k8s.io/v1beta2"
	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"

	"paddleflow/pkg/common/schema"
)

func ConvertToStatus(obj interface{}, jobType schema.JobType) (interface{}, error) {
	var realStatus interface{}
	switch jobType {
	case schema.TypeSparkJob:
		realStatus = &sparkoperatorv1beta2.SparkApplicationStatus{}
	case schema.TypeVcJob:
		realStatus = &batchv1alpha1.JobStatus{}
	case schema.TypePaddleJob:
		realStatus = &paddlejobv1.PaddleJobStatus{}
	case schema.TypePodJob:
		realStatus = &v1.PodStatus{}
	default:
		return nil, fmt.Errorf("job type %s is not supported", jobType)
	}
	if obj == nil {
		return realStatus, nil
	}
	// Get status from unstructured object
	jobObj := obj.(*unstructured.Unstructured)
	status, ok, unerr := unstructured.NestedFieldCopy(jobObj.Object, "status")
	if !ok {
		if unerr != nil {
			log.Error(unerr, "NestedFieldCopy unstructured to status error")
			return realStatus, unerr
		}
		log.Info("NestedFieldCopy unstructured to status error: Status is not found in job")
		return realStatus, fmt.Errorf("get status from unstructured object failed")
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(status.(map[string]interface{}), realStatus); err != nil {
		log.Errorf("convert unstructured object[%#v] to [%s] status failed. error:[%s]", obj, jobType, err.Error())
		return nil, err
	}
	return realStatus, nil
}

// SparkAppStatus get spark application status, message from interface{}, and covert to JobStatus
func SparkAppStatus(obj interface{}) (StatusInfo, error) {
	status, err := ConvertToStatus(obj, schema.TypeSparkJob)
	if err != nil {
		return StatusInfo{}, err
	}
	jobStatus := status.(*sparkoperatorv1beta2.SparkApplicationStatus)
	state, err := getSparkJobStatus(jobStatus.AppState.State)
	if err != nil {
		log.Errorf("convert VCJob status to JobStatus failed, err[%v]", err)
		return StatusInfo{}, err
	}
	log.Infof("Spark application status[%s]", state)
	return StatusInfo{
		OriginStatus: string(jobStatus.AppState.State),
		Status:       state,
		Message:      jobStatus.AppState.ErrorMessage,
	}, nil
}

func getSparkJobStatus(state sparkoperatorv1beta2.ApplicationStateType) (schema.JobStatus, error) {
	status := schema.JobStatus("")
	switch state {
	case sparkoperatorv1beta2.NewState, sparkoperatorv1beta2.SubmittedState:
		status = schema.StatusJobPending
	case sparkoperatorv1beta2.RunningState, sparkoperatorv1beta2.SucceedingState, sparkoperatorv1beta2.FailingState,
		sparkoperatorv1beta2.InvalidatingState, sparkoperatorv1beta2.PendingRerunState:
		status = schema.StatusJobRunning
	case sparkoperatorv1beta2.CompletedState:
		status = schema.StatusJobSucceeded
	case sparkoperatorv1beta2.FailedState, sparkoperatorv1beta2.FailedSubmissionState, sparkoperatorv1beta2.UnknownState:
		status = schema.StatusJobFailed
	default:
		return status, fmt.Errorf("unexpected spark application status: %s", state)
	}
	return status, nil
}

// VCJobStatus get vc job status, message from interface{}, and covert to JobStatus
func VCJobStatus(obj interface{}) (StatusInfo, error) {
	status, err := ConvertToStatus(obj, schema.TypeVcJob)
	if err != nil {
		log.Errorf("convert VCJob status failed, err[%v]", err)
		return StatusInfo{}, err
	}
	jobStatus := status.(*batchv1alpha1.JobStatus)
	state, err := getVCJobStatus(jobStatus.State.Phase)
	if err != nil {
		log.Errorf("convert VCJob status to JobStatus failed, err[%v]", err)
		return StatusInfo{}, err
	}
	log.Infof("VCJob status[%s]", state)

	return StatusInfo{
		OriginStatus: string(jobStatus.State.Phase),
		Status:       state,
		Message:      jobStatus.State.Message,
	}, nil
}

func getVCJobStatus(phase batchv1alpha1.JobPhase) (schema.JobStatus, error) {
	status := schema.JobStatus("")
	switch phase {
	case batchv1alpha1.Pending:
		status = schema.StatusJobPending
	case batchv1alpha1.Running, batchv1alpha1.Restarting, batchv1alpha1.Completing:
		status = schema.StatusJobRunning
	case batchv1alpha1.Terminating, batchv1alpha1.Aborting:
		status = schema.StatusJobTerminating
	case batchv1alpha1.Completed:
		status = schema.StatusJobSucceeded
	case batchv1alpha1.Aborted:
		status = schema.StatusJobTerminated
	case batchv1alpha1.Failed, batchv1alpha1.Terminated:
		status = schema.StatusJobFailed
	default:
		return status, fmt.Errorf("unexpected vcjob status: %s", phase)
	}
	return status, nil
}

// PaddleJobStatus get paddle job status, message from interface{}, and covert to JobStatus
func PaddleJobStatus(obj interface{}) (StatusInfo, error) {
	status, err := ConvertToStatus(obj, schema.TypePaddleJob)
	if err != nil {
		log.Errorf("convert PaddleJob status failed, err[%v]", err)
		return StatusInfo{}, err
	}
	jobStatus := status.(*paddlejobv1.PaddleJobStatus)
	state, err := getPaddleJobStatus(jobStatus.Phase)
	if err != nil {
		log.Errorf("get PaddleJob status failed, err[%v]", err)
		return StatusInfo{}, err
	}
	log.Infof("Paddle job status[%s]", state)
	return StatusInfo{
		OriginStatus: string(jobStatus.Phase),
		Status:       state,
		Message:      "",
	}, nil
}

func getPaddleJobStatus(phase paddlejobv1.PaddleJobPhase) (schema.JobStatus, error) {
	status := schema.JobStatus("")
	switch phase {
	case paddlejobv1.Starting, paddlejobv1.Pending:
		status = schema.StatusJobPending
	case paddlejobv1.Running, paddlejobv1.Restarting, paddlejobv1.Completing, paddlejobv1.Scaling:
		status = schema.StatusJobRunning
	case paddlejobv1.Terminating, paddlejobv1.Aborting:
		status = schema.StatusJobTerminating
	case paddlejobv1.Completed, paddlejobv1.Succeed:
		status = schema.StatusJobSucceeded
	case paddlejobv1.Aborted:
		status = schema.StatusJobTerminated
	case paddlejobv1.Failed, paddlejobv1.Terminated, paddlejobv1.Unknown:
		status = schema.StatusJobFailed
	default:
		return status, fmt.Errorf("unexpected paddlejob status: %s", phase)
	}
	return status, nil
}
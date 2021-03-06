// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cloudstack

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tsuru/monsterqueue"
	"github.com/tsuru/tsuru/log"
)

const (
	machineCreateTaskName = "cloudstack-machine-create"
	machineDeleteTaskName = "cloudstack-machine-delete"
)

type machineCreate struct {
	iaas *CloudstackIaaS
}

type machineDelete struct {
	iaas *CloudstackIaaS
}

type cloudstackTag struct {
	Key   string
	Value string
}

func (t *machineCreate) Name() string {
	return t.iaas.taskName(machineCreateTaskName)
}

func (t *machineCreate) Run(job monsterqueue.Job) {
	params := job.Parameters()
	jobId := params["jobId"].(string)
	vmId := params["vmId"].(string)
	projectId := params["projectId"].(string)
	ip, err := t.iaas.waitVMIsCreated(jobId, vmId, projectId)
	if err != nil {
		_, qErr := job.Queue().Enqueue(t.iaas.taskName(machineDeleteTaskName), monsterqueue.JobParams{
			"vmId":      vmId,
			"projectId": projectId,
		})
		if qErr != nil {
			job.Error(fmt.Errorf("error trying to enqueue deletion: %s caused by: %s", qErr, err))
			return
		}
		job.Error(err)
		return
	}
	if tags, ok := params["tags"]; ok {
		var cloudTags []*cloudstackTag
		tagList := strings.Split(tags.(string), ",")
		cloudTags = make([]*cloudstackTag, 0, len(tagList))
		for _, tag := range tagList {
			if strings.Contains(tag, ":") {
				parts := strings.SplitN(tag, ":", 2)
				cloudTags = append(cloudTags, &cloudstackTag{
					Key:   string(parts[0]),
					Value: string(parts[1]),
				})
			}
		}
		if len(cloudTags) > 0 {
			param := make(map[string]string)
			param["resourceids"] = vmId
			param["resourcetype"] = "UserVm"
			for index, tag := range cloudTags {
				param["tags["+strconv.Itoa(index+1)+"].key"] = tag.Key
				param["tags["+strconv.Itoa(index+1)+"].value"] = tag.Value
			}
			param["projectId"] = projectId
			var result CreateTagsResponse
			err = t.iaas.do("createTags", param, &result)
			if err != nil {
				job.Error(err)
				return
			}
		}
	}
	notified, _ := job.Success(ip)
	if !notified {
		_, err = job.Queue().Enqueue(t.iaas.taskName(machineDeleteTaskName), monsterqueue.JobParams{
			"vmId":      vmId,
			"projectId": projectId,
		})
		if err != nil {
			log.Errorf("could not enqueue delete unnotified vm: %s", err)
			return
		}
	}
}

func (t *machineDelete) Name() string {
	return t.iaas.taskName(machineDeleteTaskName)
}

func (t *machineDelete) Run(job monsterqueue.Job) {
	params := job.Parameters()
	vmId := params["vmId"].(string)
	projectId := params["projectId"].(string)
	var volumesRsp ListVolumesResponse
	err := t.iaas.do("listVolumes", ApiParams{
		"virtualmachineid": vmId,
		"projectid":        projectId,
	}, &volumesRsp)
	if err != nil {
		job.Error(err)
		return
	}
	var destroyData DestroyVirtualMachineResponse
	err = t.iaas.do("destroyVirtualMachine", ApiParams{
		"id": vmId,
	}, &destroyData)
	if err != nil {
		job.Error(err)
		return
	}
	_, err = t.iaas.waitForAsyncJob(destroyData.DestroyVirtualMachineResponse.JobID)
	if err != nil {
		job.Error(err)
		return
	}
	for _, vol := range volumesRsp.ListVolumesResponse.Volume {
		if vol.Type != diskDataDisk {
			continue
		}
		var detachRsp DetachVolumeResponse
		err = t.iaas.do("detachVolume", ApiParams{"id": vol.ID}, &detachRsp)
		if err != nil {
			job.Error(err)
			return
		}
		_, err = t.iaas.waitForAsyncJob(detachRsp.DetachVolumeResponse.JobID)
		if err != nil {
			job.Error(err)
			return
		}
		err = t.iaas.do("deleteVolume", ApiParams{"id": vol.ID}, nil)
		if err != nil {
			job.Error(err)
			return
		}
	}
	job.Success(nil)
}

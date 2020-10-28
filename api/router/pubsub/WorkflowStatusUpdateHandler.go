/*
 * Copyright (c) 2020 Devtron Labs
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package pubsub

import (
	"github.com/devtron-labs/devtron/api/bean"
	client "github.com/devtron-labs/devtron/client/events"
	"github.com/devtron-labs/devtron/client/pubsub"
	"github.com/devtron-labs/devtron/internal/sql/repository/pipelineConfig"
	"github.com/devtron-labs/devtron/pkg/pipeline"
	"github.com/devtron-labs/devtron/util/event"
	"encoding/json"
	"github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"github.com/nats-io/stan"
	"go.uber.org/zap"
	"time"
)

type WorkflowStatusUpdateHandler interface {
	Subscribe() error
}

type WorkflowStatusUpdateHandlerImpl struct {
	logger               *zap.SugaredLogger
	pubsubClient         *pubsub.PubSubClient
	ciHandler            pipeline.CiHandler
	cdHandler            pipeline.CdHandler
	eventFactory         client.EventFactory
	eventClient          client.EventClient
	cdWorkflowRepository pipelineConfig.CdWorkflowRepository
}

const workflowStatusUpdate = "WORKFLOW_STATUS_UPDATE"
const workflowStatusUpdateGroup = "WORKFLOW_STATUS_UPDATE_GROUP-1"
const workflowStatusUpdateDurable = "WORKFLOW_STATUS_UPDATE_DURABLE-1"

const cdWorkflowStatusUpdate = "CD_WORKFLOW_STATUS_UPDATE"
const cdWorkflowStatusUpdateGroup = "CD_WORKFLOW_STATUS_UPDATE_GROUP-1"
const cdWorkflowStatusUpdateDurable = "CD_WORKFLOW_STATUS_UPDATE_DURABLE-1"

func NewWorkflowStatusUpdateHandlerImpl(logger *zap.SugaredLogger, pubsubClient *pubsub.PubSubClient, ciHandler pipeline.CiHandler, cdHandler pipeline.CdHandler,
	eventFactory client.EventFactory, eventClient client.EventClient, cdWorkflowRepository pipelineConfig.CdWorkflowRepository) *WorkflowStatusUpdateHandlerImpl {
	workflowStatusUpdateHandlerImpl := &WorkflowStatusUpdateHandlerImpl{
		logger:               logger,
		pubsubClient:         pubsubClient,
		ciHandler:            ciHandler,
		cdHandler:            cdHandler,
		eventFactory:         eventFactory,
		eventClient:          eventClient,
		cdWorkflowRepository: cdWorkflowRepository,
	}
	err := workflowStatusUpdateHandlerImpl.Subscribe()
	if err != nil {
		logger.Error("err", err)
		return nil
	}
	err = workflowStatusUpdateHandlerImpl.SubscribeCD()
	if err != nil {
		logger.Error("err", err)
		return nil
	}
	return workflowStatusUpdateHandlerImpl
}

func (impl *WorkflowStatusUpdateHandlerImpl) Subscribe() error {
	_, err := impl.pubsubClient.Conn.QueueSubscribe(workflowStatusUpdate, workflowStatusUpdateGroup, func(msg *stan.Msg) {
		impl.logger.Debug("received wf update request")
		defer msg.Ack()
		wfStatus := v1alpha1.WorkflowStatus{}
		err := json.Unmarshal([]byte(string(msg.Data)), &wfStatus)
		if err != nil {
			impl.logger.Errorw("error on wf status update", "err", err, "msg", string(msg.Data))
			return
		}
		_, err = impl.ciHandler.UpdateWorkflow(wfStatus)
		if err != nil {
			impl.logger.Errorw("error on update workflow status", "err", err, "msg", string(msg.Data))
			return
		}
	}, stan.DurableName(workflowStatusUpdateDurable), stan.StartWithLastReceived(), stan.AckWait(time.Duration(impl.pubsubClient.AckDuration)*time.Second), stan.SetManualAckMode(), stan.MaxInflight(1))

	if err != nil {
		impl.logger.Error("err", err)
		return err
	}
	return nil
}

func (impl *WorkflowStatusUpdateHandlerImpl) SubscribeCD() error {
	_, err := impl.pubsubClient.Conn.QueueSubscribe(cdWorkflowStatusUpdate, cdWorkflowStatusUpdateGroup, func(msg *stan.Msg) {
		impl.logger.Debug("received cd wf update request")
		defer msg.Ack()
		wfStatus := v1alpha1.WorkflowStatus{}
		err := json.Unmarshal([]byte(string(msg.Data)), &wfStatus)
		if err != nil {
			impl.logger.Error("err", err)
			return
		}
		impl.logger.Debugw("received cd wf update request body", "body", wfStatus)
		wfrId, wfrStatus, err := impl.cdHandler.UpdateWorkflow(wfStatus)
		impl.logger.Debug(wfrId)
		if err != nil {
			impl.logger.Error("err", err)
			return
		}

		wfr, err := impl.cdWorkflowRepository.FindWorkflowRunnerById(wfrId)
		if err != nil {
			impl.logger.Errorw("could not get wf runner", "err", err)
			return
		}
		if wfrStatus == string(v1alpha1.NodeSucceeded) ||
			wfrStatus == string(v1alpha1.NodeFailed) || wfrStatus == string(v1alpha1.NodeError) {
			eventType := util.EventType(0)
			if wfrStatus == string(v1alpha1.NodeSucceeded) {
				eventType = util.Success
			} else if wfrStatus == string(v1alpha1.NodeFailed) || wfrStatus == string(v1alpha1.NodeError) {
				eventType = util.Fail
			}
			if wfr.WorkflowType == bean.CD_WORKFLOW_TYPE_PRE {
				event := impl.eventFactory.Build(eventType, &wfr.CdWorkflow.PipelineId, wfr.CdWorkflow.Pipeline.AppId, &wfr.CdWorkflow.Pipeline.EnvironmentId, util.CD)
				impl.logger.Debugw("event pre stage", "event", event)
				event = impl.eventFactory.BuildExtraCDData(event, wfr, 0, bean.CD_WORKFLOW_TYPE_PRE)
				_, evtErr := impl.eventClient.WriteEvent(event)
				if evtErr != nil {
					impl.logger.Errorw("CD stage post fail or success event unable to sent", "error", evtErr)
				}

			} else if wfr.WorkflowType == bean.CD_WORKFLOW_TYPE_POST {
				event := impl.eventFactory.Build(eventType, &wfr.CdWorkflow.PipelineId, wfr.CdWorkflow.Pipeline.AppId, &wfr.CdWorkflow.Pipeline.EnvironmentId, util.CD)
				impl.logger.Debugw("event post stage", "event", event)
				event = impl.eventFactory.BuildExtraCDData(event, wfr, 0, bean.CD_WORKFLOW_TYPE_POST)
				_, evtErr := impl.eventClient.WriteEvent(event)
				if evtErr != nil {
					impl.logger.Errorw("CD stage post fail or success event not sent", "error", evtErr)
				}
			}
		}
	}, stan.DurableName(cdWorkflowStatusUpdateDurable), stan.StartWithLastReceived(), stan.AckWait(time.Duration(impl.pubsubClient.AckDuration)*time.Second), stan.SetManualAckMode(), stan.MaxInflight(1))

	if err != nil {
		impl.logger.Error("err", err)
		return err
	}
	return nil
}
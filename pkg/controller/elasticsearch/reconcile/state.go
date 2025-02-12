// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package reconcile

import (
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"

	esv1 "github.com/elastic/cloud-on-k8s/pkg/apis/elasticsearch/v1"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/events"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/version"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/hints"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/label"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/observer"
	"github.com/elastic/cloud-on-k8s/pkg/utils/k8s"
	ulog "github.com/elastic/cloud-on-k8s/pkg/utils/log"
)

var log = ulog.Log.WithName("elasticsearch-controller")

// State holds the accumulated state during the reconcile loop including the response and a pointer to an
// Elasticsearch resource for status updates.
type State struct {
	*events.Recorder
	cluster esv1.Elasticsearch
	status  esv1.ElasticsearchStatus
	hints   hints.OrchestrationsHints
}

// NewState creates a new reconcile state based on the given cluster
func NewState(c esv1.Elasticsearch) (*State, error) {
	hints, err := hints.NewFromAnnotations(c.Annotations)
	if err != nil {
		return nil, err
	}
	return &State{Recorder: events.NewRecorder(), cluster: c, status: *c.Status.DeepCopy(), hints: hints}, nil
}

// MustNewState like NewState but panics on error. Use recommended only in test code.
func MustNewState(c esv1.Elasticsearch) *State {
	state, err := NewState(c)
	if err != nil {
		panic(err)
	}
	return state
}

// AvailableElasticsearchNodes filters a slice of pods for the ones that are ready.
func AvailableElasticsearchNodes(pods []corev1.Pod) []corev1.Pod {
	var nodesAvailable []corev1.Pod
	for _, pod := range pods {
		if k8s.IsPodReady(pod) {
			nodesAvailable = append(nodesAvailable, pod)
		}
	}
	return nodesAvailable
}

func (s *State) fetchMinRunningVersion(resourcesState ResourcesState) (*version.Version, error) {
	minPodVersion, err := version.MinInPods(resourcesState.AllPods, label.VersionLabelName)
	if err != nil {
		log.Error(err, "failed to parse running Pods version", "namespace", s.cluster.Namespace, "es_name", s.cluster.Name)
		return nil, err
	}
	minSsetVersion, err := version.MinInStatefulSets(resourcesState.StatefulSets, label.VersionLabelName)
	if err != nil {
		log.Error(err, "failed to parse running Pods version", "namespace", s.cluster.Namespace, "es_name", s.cluster.Name)
		return nil, err
	}

	if minPodVersion == nil {
		return minSsetVersion, nil
	}
	if minSsetVersion == nil {
		return minPodVersion, nil
	}

	if minPodVersion.GT(*minSsetVersion) {
		return minSsetVersion, nil
	}

	return minPodVersion, nil
}

func (s *State) updateWithPhase(
	phase esv1.ElasticsearchOrchestrationPhase,
	resourcesState ResourcesState,
	observedState observer.State,
) *State {
	s.status.AvailableNodes = int32(len(AvailableElasticsearchNodes(resourcesState.CurrentPods)))
	s.status.Phase = phase

	lowestVersion, err := s.fetchMinRunningVersion(resourcesState)
	if err != nil {
		// error already handled in fetchMinRunningVersion, move on with the status update
	} else if lowestVersion != nil {
		s.status.Version = lowestVersion.String()
	}

	s.status.Health = esv1.ElasticsearchUnknownHealth
	if observedState.ClusterHealth != nil && observedState.ClusterHealth.Status != "" {
		s.status.Health = observedState.ClusterHealth.Status
	}
	return s
}

// UpdateElasticsearchState updates the Elasticsearch section of the state resource status based on the given pods.
func (s *State) UpdateElasticsearchState(
	resourcesState ResourcesState,
	observedState observer.State,
) *State {
	return s.updateWithPhase(s.status.Phase, resourcesState, observedState)
}

// UpdateElasticsearchReady marks Elasticsearch as being ready in the resource status.
func (s *State) UpdateElasticsearchReady(
	resourcesState ResourcesState,
	observedState observer.State,
) *State {
	return s.updateWithPhase(esv1.ElasticsearchReadyPhase, resourcesState, observedState)
}

// IsElasticsearchReady reports if Elasticsearch is ready.
func (s *State) IsElasticsearchReady(observedState observer.State) bool {
	return s.status.Phase == esv1.ElasticsearchReadyPhase
}

// UpdateElasticsearchApplyingChanges marks Elasticsearch as being the applying changes phase in the resource status.
func (s *State) UpdateElasticsearchApplyingChanges(pods []corev1.Pod) *State {
	s.status.AvailableNodes = int32(len(AvailableElasticsearchNodes(pods)))
	s.status.Phase = esv1.ElasticsearchApplyingChangesPhase
	s.status.Health = esv1.ElasticsearchRedHealth
	return s
}

// UpdateElasticsearchMigrating marks Elasticsearch as being in the data migration phase in the resource status.
func (s *State) UpdateElasticsearchMigrating(
	resourcesState ResourcesState,
	observedState observer.State,
) *State {
	s.AddEvent(
		corev1.EventTypeNormal,
		events.EventReasonDelayed,
		"Requested topology change delayed by data migration. Ensure index settings allow node removal.",
	)
	return s.updateWithPhase(esv1.ElasticsearchMigratingDataPhase, resourcesState, observedState)
}

func (s *State) UpdateElasticsearchShutdownStalled(
	resourcesState ResourcesState,
	observedState observer.State,
	reasonDetail string,
) *State {
	s.AddEvent(
		corev1.EventTypeWarning,
		events.EventReasonStalled,
		fmt.Sprintf("Requested topology change is stalled. User intervention maybe required if this condition persists. %s", reasonDetail),
	)
	return s.updateWithPhase(esv1.ElasticsearchNodeShutdownStalledPhase, resourcesState, observedState)
}

// Apply takes the current Elasticsearch status, compares it to the previous status, and updates the status accordingly.
// It returns the events to emit and an updated version of the Elasticsearch cluster resource with
// the current status applied to its status sub-resource.
func (s *State) Apply() ([]events.Event, *esv1.Elasticsearch) {
	previous := s.cluster.Status
	current := s.status
	if reflect.DeepEqual(previous, current) {
		return s.Events(), nil
	}
	if current.IsDegraded(previous) {
		s.AddEvent(corev1.EventTypeWarning, events.EventReasonUnhealthy, "Elasticsearch cluster health degraded")
	}
	s.cluster.Status = current
	return s.Events(), &s.cluster
}

func (s *State) UpdateElasticsearchInvalid(err error) {
	s.status.Phase = esv1.ElasticsearchResourceInvalid
	s.AddEvent(corev1.EventTypeWarning, events.EventReasonValidation, err.Error())
}

func (s *State) UpdateElasticsearchStatusPhase(orchPhase esv1.ElasticsearchOrchestrationPhase) {
	s.status.Phase = orchPhase
}

// UpdateOrchestrationHints updates the orchestration hints collected so far with the hints in hint.
func (s *State) UpdateOrchestrationHints(hint hints.OrchestrationsHints) {
	s.hints = s.hints.Merge(hint)
}

// OrchestrationHints returns the current annotation hints as maintained in reconciliation state. Initially these will be
// populated from the Elasticsearch resource. But after calls to UpdateOrchestrationHints they can deviate from the state
// stored in the API server.
func (s *State) OrchestrationHints() hints.OrchestrationsHints {
	return s.hints
}

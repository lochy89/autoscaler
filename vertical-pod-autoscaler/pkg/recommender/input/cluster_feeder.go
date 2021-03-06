/*
Copyright 2018 The Kubernetes Authors.

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

package input

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1beta2"
	vpa_clientset "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/clientset/versioned"
	vpa_api "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/clientset/versioned/typed/autoscaling.k8s.io/v1beta2"
	vpa_lister "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/listers/autoscaling.k8s.io/v1beta2"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/input/history"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/input/metrics"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/input/oom"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/input/spec"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/recommender/model"
	target "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/target"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
	"k8s.io/client-go/informers"
	kube_client "k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	v1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	resourceclient "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
)

const (
	defaultResyncPeriod time.Duration = 10 * time.Minute
)

// ClusterStateFeeder can update state of ClusterState object.
type ClusterStateFeeder interface {

	// InitFromHistoryProvider loads historical pod spec into clusterState.
	InitFromHistoryProvider(historyProvider history.HistoryProvider)

	// InitFromCheckpoints loads historical checkpoints into clusterState.
	InitFromCheckpoints()

	// LoadVPAs updates clusterState with current state of VPAs.
	LoadVPAs()

	// LoadPods updates slusterState with current specification of Pods and their Containers.
	LoadPods()

	// LoadRealTimeMetrics updates clusterState with current usage metrics of containers.
	LoadRealTimeMetrics()

	// GarbageCollectCheckpoints removes historical checkpoints that don't have a matching VPA.
	GarbageCollectCheckpoints()
}

// ClusterStateFeederFactory makes instances of ClusterStateFeeder.
type ClusterStateFeederFactory struct {
	ClusterState          *model.ClusterState
	KubeClient            kube_client.Interface
	MetricsClient         metrics.MetricsClient
	VpaCheckpointClient   vpa_api.VerticalPodAutoscalerCheckpointsGetter
	VpaLister             vpa_lister.VerticalPodAutoscalerLister
	PodLister             v1lister.PodLister
	OOMObserver           oom.Observer
	LegacySelectorFetcher target.VpaTargetSelectorFetcher
	SelectorFetcher       target.VpaTargetSelectorFetcher
}

// Make creates new ClusterStateFeeder with internal data providers, based on kube client.
func (m ClusterStateFeederFactory) Make() *clusterStateFeeder {
	return &clusterStateFeeder{
		coreClient:            m.KubeClient.CoreV1(),
		metricsClient:         m.MetricsClient,
		oomChan:               m.OOMObserver.GetObservedOomsChannel(),
		vpaCheckpointClient:   m.VpaCheckpointClient,
		vpaLister:             m.VpaLister,
		clusterState:          m.ClusterState,
		specClient:            spec.NewSpecClient(m.PodLister),
		legacySelectorFetcher: m.LegacySelectorFetcher,
		selectorFetcher:       m.SelectorFetcher,
	}
}

// NewClusterStateFeeder creates new ClusterStateFeeder with internal data providers, based on kube client config.
// Deprecated; Use ClusterStateFeederFactory instead.
func NewClusterStateFeeder(config *rest.Config, clusterState *model.ClusterState) ClusterStateFeeder {
	kubeClient := kube_client.NewForConfigOrDie(config)
	podLister, oomObserver := NewPodListerAndOOMObserver(kubeClient)
	factory := informers.NewSharedInformerFactory(kubeClient, defaultResyncPeriod)
	return ClusterStateFeederFactory{
		PodLister:             podLister,
		OOMObserver:           oomObserver,
		KubeClient:            kubeClient,
		MetricsClient:         newMetricsClient(config),
		VpaCheckpointClient:   vpa_clientset.NewForConfigOrDie(config).AutoscalingV1beta2(),
		VpaLister:             vpa_api_util.NewAllVpasLister(vpa_clientset.NewForConfigOrDie(config), make(chan struct{})),
		ClusterState:          clusterState,
		LegacySelectorFetcher: target.NewBeta1TargetSelectorFetcher(config),
		SelectorFetcher:       target.NewVpaTargetSelectorFetcher(config, kubeClient, factory),
	}.Make()
}

func newMetricsClient(config *rest.Config) metrics.MetricsClient {
	metricsGetter := resourceclient.NewForConfigOrDie(config)
	return metrics.NewMetricsClient(metricsGetter)
}

// WatchEvictionEventsWithRetries watches new Events with reason=Evicted and passes them to the observer.
func WatchEvictionEventsWithRetries(kubeClient kube_client.Interface, observer oom.Observer) {
	go func() {
		options := metav1.ListOptions{
			FieldSelector: "reason=Evicted",
		}

		for {
			watchInterface, err := kubeClient.CoreV1().Events("").Watch(options)
			if err != nil {
				klog.Errorf("Cannot initialize watching events. Reason %v", err)
				continue
			}
			watchEvictionEvents(watchInterface.ResultChan(), observer)
		}
	}()
}

func watchEvictionEvents(evictedEventChan <-chan watch.Event, observer oom.Observer) {
	for {
		evictedEvent, ok := <-evictedEventChan
		if !ok {
			klog.V(3).Infof("Eviction event chan closed")
			return
		}
		if evictedEvent.Type == watch.Added {
			evictedEvent, ok := evictedEvent.Object.(*apiv1.Event)
			if !ok {
				continue
			}
			observer.OnEvent(evictedEvent)
		}
	}
}

// Creates clients watching pods: PodLister (listing only not terminated pods).
func newPodClients(kubeClient kube_client.Interface, resourceEventHandler cache.ResourceEventHandler) v1lister.PodLister {
	selector := fields.ParseSelectorOrDie("status.phase!=" + string(apiv1.PodPending))
	podListWatch := cache.NewListWatchFromClient(kubeClient.CoreV1().RESTClient(), "pods", apiv1.NamespaceAll, selector)
	indexer, controller := cache.NewIndexerInformer(
		podListWatch,
		&apiv1.Pod{},
		time.Hour,
		resourceEventHandler,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	podLister := v1lister.NewPodLister(indexer)
	stopCh := make(chan struct{})
	go controller.Run(stopCh)
	return podLister
}

// NewPodListerAndOOMObserver creates pair of pod lister and OOM observer.
func NewPodListerAndOOMObserver(kubeClient kube_client.Interface) (v1lister.PodLister, oom.Observer) {
	oomObserver := oom.NewObserver()
	podLister := newPodClients(kubeClient, oomObserver)
	WatchEvictionEventsWithRetries(kubeClient, oomObserver)
	return podLister, oomObserver
}

type clusterStateFeeder struct {
	coreClient            corev1.CoreV1Interface
	specClient            spec.SpecClient
	metricsClient         metrics.MetricsClient
	oomChan               <-chan oom.OomInfo
	vpaCheckpointClient   vpa_api.VerticalPodAutoscalerCheckpointsGetter
	vpaLister             vpa_lister.VerticalPodAutoscalerLister
	clusterState          *model.ClusterState
	legacySelectorFetcher target.VpaTargetSelectorFetcher
	selectorFetcher       target.VpaTargetSelectorFetcher
}

func (feeder *clusterStateFeeder) InitFromHistoryProvider(historyProvider history.HistoryProvider) {
	klog.V(3).Info("Initializing VPA from history provider")
	clusterHistory, err := historyProvider.GetClusterHistory()
	if err != nil {
		klog.Errorf("Cannot get cluster history: %v", err)
	}
	for podID, podHistory := range clusterHistory {
		klog.V(4).Infof("Adding pod %v with labels %v", podID, podHistory.LastLabels)
		feeder.clusterState.AddOrUpdatePod(podID, podHistory.LastLabels, apiv1.PodUnknown)
		for containerName, sampleList := range podHistory.Samples {
			containerID := model.ContainerID{
				PodID:         podID,
				ContainerName: containerName}
			klog.V(4).Infof("Adding %d samples for container %v", len(sampleList), containerID)
			for _, sample := range sampleList {
				feeder.clusterState.AddSample(
					&model.ContainerUsageSampleWithKey{
						ContainerUsageSample: sample,
						Container:            containerID})
			}
		}
	}
}

func (feeder *clusterStateFeeder) setVpaCheckpoint(checkpoint *vpa_types.VerticalPodAutoscalerCheckpoint) error {
	vpaID := model.VpaID{Namespace: checkpoint.Namespace, VpaName: checkpoint.Spec.VPAObjectName}
	vpa, exists := feeder.clusterState.Vpas[vpaID]
	if !exists {
		return fmt.Errorf("cannot load checkpoint to missing VPA object %+v", vpaID)
	}

	cs := model.NewAggregateContainerState()
	err := cs.LoadFromCheckpoint(&checkpoint.Status)
	if err != nil {
		return fmt.Errorf("cannot load checkpoint for VPA %+v. Reason: %v", vpa.ID, err)
	}
	vpa.ContainersInitialAggregateState[checkpoint.Spec.ContainerName] = cs
	return nil
}

func (feeder *clusterStateFeeder) InitFromCheckpoints() {
	klog.V(3).Info("Initializing VPA from checkpoints")
	feeder.LoadVPAs()

	namespaces := make(map[string]bool)
	for _, v := range feeder.clusterState.Vpas {
		namespaces[v.ID.Namespace] = true
	}

	for namespace := range namespaces {
		klog.V(3).Infof("Fetching checkpoints from namespace %s", namespace)
		checkpointList, err := feeder.vpaCheckpointClient.VerticalPodAutoscalerCheckpoints(namespace).List(metav1.ListOptions{})

		if err != nil {
			klog.Errorf("Cannot list VPA checkpoints from namespace %v. Reason: %+v", namespace, err)
		}
		for _, checkpoint := range checkpointList.Items {

			klog.V(3).Infof("Loading VPA %s/%s checkpoint for %s", checkpoint.ObjectMeta.Namespace, checkpoint.Spec.VPAObjectName, checkpoint.Spec.ContainerName)
			err = feeder.setVpaCheckpoint(&checkpoint)
			if err != nil {
				klog.Errorf("Error while loading checkpoint. Reason: %+v", err)
			}

		}
	}
}

func (feeder *clusterStateFeeder) GarbageCollectCheckpoints() {
	klog.V(3).Info("Starting garbage collection of checkpoints")
	feeder.LoadVPAs()

	namspaceList, err := feeder.coreClient.Namespaces().List(metav1.ListOptions{})
	if err != nil {
		klog.Errorf("Cannot list namespaces. Reason: %+v", err)
		return
	}

	for _, namespaceItem := range namspaceList.Items {
		namespace := namespaceItem.Name
		checkpointList, err := feeder.vpaCheckpointClient.VerticalPodAutoscalerCheckpoints(namespace).List(metav1.ListOptions{})
		if err != nil {
			klog.Errorf("Cannot list VPA checkpoints from namespace %v. Reason: %+v", namespace, err)
		}
		for _, checkpoint := range checkpointList.Items {
			vpaID := model.VpaID{Namespace: checkpoint.Namespace, VpaName: checkpoint.Spec.VPAObjectName}
			_, exists := feeder.clusterState.Vpas[vpaID]
			if !exists {
				err = feeder.vpaCheckpointClient.VerticalPodAutoscalerCheckpoints(namespace).Delete(checkpoint.Name, &metav1.DeleteOptions{})
				if err == nil {
					klog.V(3).Infof("Orphaned VPA checkpoint cleanup - deleting %v/%v.", namespace, checkpoint.Name)
				} else {
					klog.Errorf("Cannot delete VPA checkpoint %v/%v. Reason: %+v", namespace, checkpoint.Name, err)
				}
			}
		}
	}
}

// Fetch VPA objects and load them into the cluster state.
func (feeder *clusterStateFeeder) LoadVPAs() {
	// List VPA API objects.
	vpaCRDs, err := feeder.vpaLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Cannot list VPAs. Reason: %+v", err)
		return
	}
	klog.V(3).Infof("Fetched %d VPAs.", len(vpaCRDs))
	// Add or update existing VPAs in the model.
	vpaKeys := make(map[model.VpaID]bool)
	for _, vpaCRD := range vpaCRDs {
		vpaID := model.VpaID{
			Namespace: vpaCRD.Namespace,
			VpaName:   vpaCRD.Name}

		selector, conditions := feeder.getSelector(vpaCRD)
		glog.Infof("Using selector %s for VPA %s/%s", selector.String(), vpaCRD.Namespace, vpaCRD.Name)

		if feeder.clusterState.AddOrUpdateVpa(vpaCRD, selector) == nil {
			// Successfully added VPA to the model.
			vpaKeys[vpaID] = true

			legacySelector, _ := feeder.legacySelectorFetcher.Fetch(vpaCRD)
			feeder.clusterState.Vpas[vpaID].IsV1Beta1API = legacySelector != nil

			for _, condition := range conditions {
				if condition.delete {
					delete(feeder.clusterState.Vpas[vpaID].Conditions, condition.conditionType)
				} else {
					feeder.clusterState.Vpas[vpaID].Conditions.Set(condition.conditionType, true, "", condition.message)
				}
			}
		}
	}
	// Delete non-existent VPAs from the model.
	for vpaID := range feeder.clusterState.Vpas {
		if _, exists := vpaKeys[vpaID]; !exists {
			klog.V(3).Infof("Deleting VPA %v", vpaID)
			feeder.clusterState.DeleteVpa(vpaID)
		}
	}
	feeder.clusterState.ObservedVpas = vpaCRDs
}

// Load pod into the cluster state.
func (feeder *clusterStateFeeder) LoadPods() {
	podSpecs, err := feeder.specClient.GetPodSpecs()
	if err != nil {
		klog.Errorf("Cannot get SimplePodSpecs. Reason: %+v", err)
	}
	pods := make(map[model.PodID]*spec.BasicPodSpec)
	for _, spec := range podSpecs {
		pods[spec.ID] = spec
	}
	for key := range feeder.clusterState.Pods {
		if _, exists := pods[key]; !exists {
			klog.V(3).Infof("Deleting Pod %v", key)
			feeder.clusterState.DeletePod(key)
		}
	}
	for _, pod := range pods {
		feeder.clusterState.AddOrUpdatePod(pod.ID, pod.PodLabels, pod.Phase)
		for _, container := range pod.Containers {
			feeder.clusterState.AddOrUpdateContainer(container.ID, container.Request)
		}
	}
}

func (feeder *clusterStateFeeder) LoadRealTimeMetrics() {
	containersMetrics, err := feeder.metricsClient.GetContainersMetrics()
	if err != nil {
		klog.Errorf("Cannot get ContainerMetricsSnapshot from MetricsClient. Reason: %+v", err)
	}

	sampleCount := 0
	for _, containerMetrics := range containersMetrics {
		for _, sample := range newContainerUsageSamplesWithKey(containerMetrics) {
			feeder.clusterState.AddSample(sample)
			sampleCount++
		}
	}
	klog.V(3).Infof("ClusterSpec fed with #%v ContainerUsageSamples for #%v containers", sampleCount, len(containersMetrics))

Loop:
	for {
		select {
		case oomInfo := <-feeder.oomChan:
			klog.V(3).Infof("OOM detected %+v", oomInfo)
			feeder.clusterState.RecordOOM(oomInfo.ContainerID, oomInfo.Timestamp, oomInfo.Memory)
		default:
			break Loop
		}
	}
}

func newContainerUsageSamplesWithKey(metrics *metrics.ContainerMetricsSnapshot) []*model.ContainerUsageSampleWithKey {
	var samples []*model.ContainerUsageSampleWithKey

	for metricName, resourceAmount := range metrics.Usage {
		sample := &model.ContainerUsageSampleWithKey{
			Container: metrics.ID,
			ContainerUsageSample: model.ContainerUsageSample{
				MeasureStart: metrics.SnapshotTime,
				Resource:     metricName,
				Usage:        resourceAmount,
			},
		}
		samples = append(samples, sample)
	}
	return samples
}

type condition struct {
	conditionType vpa_types.VerticalPodAutoscalerConditionType
	delete        bool
	message       string
}

func (feeder *clusterStateFeeder) getSelector(vpa *vpa_types.VerticalPodAutoscaler) (labels.Selector, []condition) {
	legacySelector, fetchLegacyErr := feeder.legacySelectorFetcher.Fetch(vpa)
	if fetchLegacyErr != nil {
		glog.Errorf("Error while fetching legacy selector. Reason: %+v", fetchLegacyErr)
	}
	selector, fetchErr := feeder.selectorFetcher.Fetch(vpa)
	if fetchErr != nil {
		glog.Errorf("Cannot get target selector from VPA's targetRef. Reason: %+v", fetchErr)
	}
	if selector != nil {
		if legacySelector != nil {
			return labels.Nothing(), []condition{
				{conditionType: vpa_types.ConfigUnsupported, delete: false, message: "Both targetRef and label selector defined. Please remove label selector"},
				{conditionType: vpa_types.ConfigDeprecated, delete: true},
			}
		}
		return selector, []condition{
			{conditionType: vpa_types.ConfigUnsupported, delete: true},
			{conditionType: vpa_types.ConfigDeprecated, delete: true},
		}
	}
	if legacySelector != nil {
		return labels.Nothing(), []condition{
			{conditionType: vpa_types.ConfigUnsupported, delete: false, message: "Label selector is no longer supported, please migrate to targetRef"},
			{conditionType: vpa_types.ConfigDeprecated, delete: true},
		}
	}
	msg := "Cannot read targetRef"
	if fetchErr != nil {
		msg = fmt.Sprintf("Cannot read targetRef. Reason: %s", fetchErr.Error())
	}
	return labels.Nothing(), []condition{
		{conditionType: vpa_types.ConfigUnsupported, delete: false, message: msg},
		{conditionType: vpa_types.ConfigDeprecated, delete: true},
	}
}

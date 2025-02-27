package status

import (
	"context"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/karmada-io/karmada/pkg/apis/cluster/v1alpha1"
	"github.com/karmada-io/karmada/pkg/util"
)

const (
	// ControllerName is the controller name that will be used when reporting events.
	ControllerName            = "cluster-status-controller"
	clusterReady              = "ClusterReady"
	clusterHealthy            = "cluster is reachable and health endpoint responded with ok"
	clusterNotReady           = "ClusterNotReady"
	clusterUnhealthy          = "cluster is reachable but health endpoint responded without ok"
	clusterNotReachableReason = "ClusterNotReachable"
	clusterNotReachableMsg    = "cluster is not reachable"
	// clusterStatusRetryInterval specifies the interval between two retries.
	clusterStatusRetryInterval = 500 * time.Millisecond
	// clusterStatusRetryTimeout specifies the maximum time to wait for cluster status.
	clusterStatusRetryTimeout = 2 * time.Second
)

// ClusterStatusController is to sync status of Cluster.
type ClusterStatusController struct {
	client.Client        // used to operate Cluster resources.
	EventRecorder        record.EventRecorder
	PredicateFunc        predicate.Predicate
	ClusterClientSetFunc func(c *v1alpha1.Cluster, client client.Client) (*util.ClusterClient, error)

	// ClusterStatusUpdateFrequency is the frequency that controller computes cluster status.
	// If cluster lease feature is not enabled, it is also the frequency that controller posts cluster status
	// to karmada-apiserver.
	ClusterStatusUpdateFrequency metav1.Duration
}

// Reconcile syncs status of the given member cluster.
// The Controller will requeue the Request to be processed again if an error is non-nil or
// Result.Requeue is true, otherwise upon completion it will requeue the reconcile key after the duration.
func (c *ClusterStatusController) Reconcile(req controllerruntime.Request) (controllerruntime.Result, error) {
	klog.V(4).Infof("Syncing cluster status: %s", req.NamespacedName.String())

	cluster := &v1alpha1.Cluster{}
	if err := c.Client.Get(context.TODO(), req.NamespacedName, cluster); err != nil {
		// The resource may no longer exist, in which case we stop processing.
		if errors.IsNotFound(err) {
			return controllerruntime.Result{}, nil
		}

		return controllerruntime.Result{Requeue: true}, err
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return controllerruntime.Result{}, nil
	}

	// start syncing status only when the finalizer is present on the given Cluster to
	// avoid conflict with cluster controller.
	if !controllerutil.ContainsFinalizer(cluster, util.ClusterControllerFinalizer) {
		klog.V(2).Infof("waiting finalizer present for member cluster: %s", cluster.Name)
		return controllerruntime.Result{Requeue: true}, nil
	}

	return c.syncClusterStatus(cluster)
}

// SetupWithManager creates a controller and register to controller manager.
func (c *ClusterStatusController) SetupWithManager(mgr controllerruntime.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).For(&v1alpha1.Cluster{}).WithEventFilter(c.PredicateFunc).Complete(c)
}

func (c *ClusterStatusController) syncClusterStatus(cluster *v1alpha1.Cluster) (controllerruntime.Result, error) {
	// create a ClusterClient for the given member cluster
	clusterClient, err := c.ClusterClientSetFunc(cluster, c.Client)
	if err != nil {
		klog.Errorf("Failed to create a ClusterClient for the given member cluster: %v, err is : %v", cluster.Name, err)
		return controllerruntime.Result{Requeue: true}, err
	}

	var currentClusterStatus = v1alpha1.ClusterStatus{}

	// get the health status of member cluster
	online, healthy := getClusterHealthStatus(clusterClient)

	// in case of cluster offline, retry a few times to avoid network unstable problems.
	// Note: retry timeout should not be too long, otherwise will block other cluster reconcile.
	if !online {
		err := wait.Poll(clusterStatusRetryInterval, clusterStatusRetryTimeout, func() (done bool, err error) {
			online, healthy = getClusterHealthStatus(clusterClient)
			if !online {
				return false, nil
			}
			klog.V(2).Infof("Cluster(%s) back to online after retry.", cluster.Name)
			return true, nil
		})
		// error indicates that retry timeout, update cluster status immediately and return.
		if err != nil {
			currentClusterStatus.Conditions = generateReadyCondition(false, false)
			setTransitionTime(&cluster.Status, &currentClusterStatus)
			return c.updateStatusIfNeeded(cluster, currentClusterStatus)
		}
	}

	clusterVersion, err := getKubernetesVersion(clusterClient)
	if err != nil {
		klog.Errorf("Failed to get server version of the member cluster: %v, err is : %v", cluster.Name, err)
		return controllerruntime.Result{Requeue: true}, err
	}

	// get the list of APIs installed in the member cluster
	apiEnables, err := getAPIEnablements(clusterClient)
	if err != nil {
		klog.Errorf("Failed to get APIs installed in the member cluster: %v, err is : %v", cluster.Name, err)
		return controllerruntime.Result{Requeue: true}, err
	}

	// get the summary of nodes status in the member cluster
	nodeSummary, err := getNodeSummary(clusterClient)
	if err != nil {
		klog.Errorf("Failed to get summary of nodes status in the member cluster: %v, err is : %v", cluster.Name, err)
		return controllerruntime.Result{Requeue: true}, err
	}

	currentClusterStatus.Conditions = generateReadyCondition(online, healthy)
	setTransitionTime(&cluster.Status, &currentClusterStatus)
	currentClusterStatus.KubernetesVersion = clusterVersion
	currentClusterStatus.APIEnablements = apiEnables
	currentClusterStatus.NodeSummary = nodeSummary

	return c.updateStatusIfNeeded(cluster, currentClusterStatus)
}

// updateStatusIfNeeded calls updateStatus only if the status of the member cluster is not the same as the old status
func (c *ClusterStatusController) updateStatusIfNeeded(cluster *v1alpha1.Cluster, currentClusterStatus v1alpha1.ClusterStatus) (controllerruntime.Result, error) {
	if !equality.Semantic.DeepEqual(cluster.Status, currentClusterStatus) {
		klog.V(4).Infof("Start to update cluster status: %s", cluster.Name)
		cluster.Status = currentClusterStatus
		err := c.Client.Status().Update(context.TODO(), cluster)
		if err != nil {
			klog.Errorf("Failed to update health status of the member cluster: %v, err is : %v", cluster.Name, err)
			return controllerruntime.Result{Requeue: true}, err
		}
	}

	return controllerruntime.Result{RequeueAfter: c.ClusterStatusUpdateFrequency.Duration}, nil
}

func getClusterHealthStatus(clusterClient *util.ClusterClient) (online, healthy bool) {
	healthStatus, err := healthEndpointCheck(clusterClient.KubeClient, "/readyz")
	if err != nil && healthStatus == http.StatusNotFound {
		// do health check with healthz endpoint if the readyz endpoint is not installed in member cluster
		healthStatus, err = healthEndpointCheck(clusterClient.KubeClient, "/healthz")
	}

	if err != nil {
		klog.Errorf("Failed to do cluster health check for cluster %v, err is : %v ", clusterClient.ClusterName, err)
		return false, false
	}

	if healthStatus != http.StatusOK {
		klog.Infof("Member cluster %v isn't healthy", clusterClient.ClusterName)
		return true, false
	}

	return true, true
}

func healthEndpointCheck(client *kubernetes.Clientset, path string) (int, error) {
	var healthStatus int
	resp := client.DiscoveryClient.RESTClient().Get().AbsPath(path).Do(context.TODO()).StatusCode(&healthStatus)
	return healthStatus, resp.Error()
}

func generateReadyCondition(online, healthy bool) []metav1.Condition {
	var conditions []metav1.Condition
	currentTime := metav1.Now()

	newClusterOfflineCondition := metav1.Condition{
		Type:               v1alpha1.ClusterConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             clusterNotReachableReason,
		Message:            clusterNotReachableMsg,
		LastTransitionTime: currentTime,
	}

	newClusterReadyCondition := metav1.Condition{
		Type:               v1alpha1.ClusterConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             clusterReady,
		Message:            clusterHealthy,
		LastTransitionTime: currentTime,
	}

	newClusterNotReadyCondition := metav1.Condition{
		Type:               v1alpha1.ClusterConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             clusterNotReady,
		Message:            clusterUnhealthy,
		LastTransitionTime: currentTime,
	}

	if !online {
		conditions = append(conditions, newClusterOfflineCondition)
	} else {
		if !healthy {
			conditions = append(conditions, newClusterNotReadyCondition)
		} else {
			conditions = append(conditions, newClusterReadyCondition)
		}
	}

	return conditions
}

func setTransitionTime(oldClusterStatus, newClusterStatus *v1alpha1.ClusterStatus) {
	// preserve the last transition time if the status of member cluster not changed
	if util.IsClusterReady(oldClusterStatus) == util.IsClusterReady(newClusterStatus) {
		if len(oldClusterStatus.Conditions) != 0 {
			for i := 0; i < len(newClusterStatus.Conditions); i++ {
				newClusterStatus.Conditions[i].LastTransitionTime = oldClusterStatus.Conditions[0].LastTransitionTime
			}
		}
	}
}

func getKubernetesVersion(clusterClient *util.ClusterClient) (string, error) {
	clusterVersion, err := clusterClient.KubeClient.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}

	return clusterVersion.GitVersion, nil
}

func getAPIEnablements(clusterClient *util.ClusterClient) ([]v1alpha1.APIEnablement, error) {
	_, apiResourceList, err := clusterClient.KubeClient.Discovery().ServerGroupsAndResources()
	if err != nil {
		return nil, err
	}

	var apiEnablements []v1alpha1.APIEnablement

	for _, list := range apiResourceList {
		var apiResource []string
		for _, resource := range list.APIResources {
			apiResource = append(apiResource, resource.Name)
		}
		apiEnablements = append(apiEnablements, v1alpha1.APIEnablement{GroupVersion: list.GroupVersion, Resources: apiResource})
	}

	return apiEnablements, nil
}

func getNodeSummary(clusterClient *util.ClusterClient) (v1alpha1.NodeSummary, error) {
	var nodeSummary = v1alpha1.NodeSummary{}
	nodeList, err := clusterClient.KubeClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nodeSummary, err
	}

	totalNum := len(nodeList.Items)
	readyNum := 0

	for _, node := range nodeList.Items {
		if getReadyStatusForNode(node.Status) {
			readyNum++
		}
	}

	allocatable := getClusterAllocatable(nodeList)

	podList, err := clusterClient.KubeClient.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nodeSummary, err
	}

	usedResource := getUsedResource(podList)

	nodeSummary.TotalNum = totalNum
	nodeSummary.ReadyNum = readyNum
	nodeSummary.Allocatable = allocatable
	nodeSummary.Used = usedResource

	return nodeSummary, nil
}

func getReadyStatusForNode(nodeStatus corev1.NodeStatus) bool {
	for _, condition := range nodeStatus.Conditions {
		if condition.Type == "Ready" {
			if condition.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

func getClusterAllocatable(nodeList *corev1.NodeList) (allocatable corev1.ResourceList) {
	allocatable = make(corev1.ResourceList)
	for _, node := range nodeList.Items {
		for key, val := range node.Status.Allocatable {
			tmpCap, ok := allocatable[key]
			if ok {
				tmpCap.Add(val)
			} else {
				tmpCap = val
			}
			allocatable[key] = tmpCap
		}
	}

	return allocatable
}

func getUsedResource(podList *corev1.PodList) corev1.ResourceList {
	var requestCPU, requestMem int64
	for _, pod := range podList.Items {
		if pod.Status.Phase == "Running" {
			for _, c := range pod.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					podRes := addPodRequestResource(&pod)
					requestCPU += podRes.MilliCPU
					requestMem += podRes.Memory
				}
			}
		}
	}

	usedResource := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(requestCPU, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(requestMem, resource.BinarySI),
	}

	return usedResource
}

func addPodRequestResource(pod *corev1.Pod) requestResource {
	res := calculateResource(pod)
	return res
}

func calculateResource(pod *corev1.Pod) (res requestResource) {
	resPtr := &res
	for _, c := range pod.Spec.Containers {
		resPtr.addResource(c.Resources.Requests)
	}
	return
}

// requestResource is a collection of compute resource.
type requestResource struct {
	MilliCPU int64
	Memory   int64
}

func (r *requestResource) addResource(rl corev1.ResourceList) {
	if r == nil {
		return
	}

	for rName, rQuant := range rl {
		switch rName {
		case corev1.ResourceCPU:
			r.MilliCPU += rQuant.MilliValue()
		case corev1.ResourceMemory:
			r.Memory += rQuant.Value()
		default:
			continue
		}
	}
}

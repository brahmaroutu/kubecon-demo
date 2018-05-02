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

package main

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	clientset "github.com/Huang-Wei/kubecon-demo/pkg/client/clientset/versioned"
	samplescheme "github.com/Huang-Wei/kubecon-demo/pkg/client/clientset/versioned/scheme"
	barlisters "github.com/Huang-Wei/kubecon-demo/pkg/client/listers/samplecontrollee/v1alpha1"
	informers "github.com/Huang-Wei/kubecon-demo/pkg/client/informers/externalversions"
	controlleev1alpha1 "github.com/Huang-Wei/kubecon-demo/pkg/apis/samplecontrollee/v1alpha1"
)

const controllerAgentName = "kubecon-demo"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Bar is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a Bar fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by Bar"
	// MessageResourceSynced is the message used for an Event fired when a Bar
	// is synced successfully
	MessageResourceSynced = "Bar synced successfully"
)

// Controller is the controller implementation for Bar resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// sampleclientset is a clientset for our own API group
	sampleclientset clientset.Interface

	deploymentsLister appslisters.DeploymentLister
	deploymentsSynced cache.InformerSynced

	barLister barlisters.BarLister
	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new sample controller
func NewController(
	kubeclientset kubernetes.Interface,
	sampleclientset clientset.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory,
	sampleInformerFactory informers.SharedInformerFactory) *Controller {

	// obtain references to shared index informers for the Deployment and Bar
	// types.
	deploymentInformer := kubeInformerFactory.Apps().V1().Deployments()
	barInformer := sampleInformerFactory.Samplecontrollee().V1alpha1().Bars()

	// Create event broadcaster
	// Add sample-controller types to the default Kubernetes Scheme so Events can be
	// logged for sample-controller types.
	samplescheme.AddToScheme(scheme.Scheme)
	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:     kubeclientset,
		sampleclientset:   sampleclientset,
		deploymentsLister: deploymentInformer.Lister(),
		deploymentsSynced: deploymentInformer.Informer().HasSynced,
		barLister:         barInformer.Lister(),
		workqueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Bars"),
		recorder:          recorder,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when Bar resources change
	barInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueBar,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueBar(new)
		},
	})
	// Set up an event handler for when Deployment resources change. This
	// handler will lookup the owner of the given Deployment, and if it is
	// owned by a Bar resource will enqueue that Bar resource for
	// processing. This way, we don't need to implement custom logic for
	// handling Deployment resources. More info on this pattern:
	// https://github.com/kubernetes/community/blob/8cafef897a22026d42f5e5bb3f104febe7e29830/contributors/devel/controllers.md
	deploymentInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newDepl := new.(*appsv1.Deployment)
			oldDepl := old.(*appsv1.Deployment)
			if newDepl.ResourceVersion == oldDepl.ResourceVersion {
				// Periodic resync will send update events for all known Deployments.
				// Two different versions of the same Deployment will always have different RVs.
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting Deployer controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.deploymentsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	// Launch two workers to process Bar resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {

		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Bar resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Bar resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	if namespace  != "demo" {
		fmt.Println("Only process deployments in demo")
		return nil
	}
	// Get the deployment with the name specified in Foo.spec
	deployment, err := c.deploymentsLister.Deployments(namespace).Get(name)
	if err != nil {
		// The deploymen resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("deployment '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	//  See if there is a service for this deployment, using same name
	//  Get the Bar object that this bar should be associated with
	bar, err := c.barLister.Bars(namespace).Get(name)
	if err != nil {
var port int32 = 8080;
if  (len(deployment.Spec.Template.Spec.Containers[0].Ports) > 0) {
  port = deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort
} 

		// The Bar resource may no exist yet, in which case we create the resource.
		// Bar Controller will then process this resource.
		if errors.IsNotFound(err) {
			barName := deployment.Name + "-1"
			labels :=  deployment.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}
			labels["svcport"] = fmt.Sprint(port)
			barObject := &controlleev1alpha1.Bar{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "samplecontrollee.k8s.io/v1alpha1",
					Kind:       "Bar",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      barName,
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						*metav1.NewControllerRef(deployment, schema.GroupVersionKind{
							Group:   controlleev1alpha1.SchemeGroupVersion.Group,
							Version: controlleev1alpha1.SchemeGroupVersion.Version,
							Kind:    "deployment",
						}),
					},
				},
				Spec: controlleev1alpha1.BarSpec{
					DeploymentName: barName,
					Selector: &metav1.LabelSelector{
						MatchLabels: labels,
					},
				},
			}
			_, err := c.sampleclientset.SamplecontrolleeV1alpha1().Bars(namespace).Create(barObject)
			if err != nil {
				runtime.HandleError(fmt.Errorf("bar '%s' in work queue no longer exists", key))
				return nil
			}
			return nil
		}

		return err
	}

	// If an error occurs during Update, we'll requeue the item so we can
	// attempt processing again later. THis could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return err
	}

	c.recorder.Event(bar, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

func (c *Controller) updateBarStatus(bar *controlleev1alpha1.Bar, deployment *appsv1.Deployment) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	barCopy := bar.DeepCopy()
	// Until #38113 is merged, we must use Update instead of UpdateStatus to
	// update the Status block of the Bar resource. UpdateStatus will not
	// allow changes to the Spec of the resource, which is ideal for ensuring
	// nothing other than resource status has been updated.
	_, err := c.sampleclientset.SamplecontrolleeV1alpha1().Bars(bar.Namespace).Update(barCopy)
	return err
}

// enqueueBar takes a Bar resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Bar.
func (c *Controller) enqueueBar(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

// handleObject will take any resource implementing metav1.Object and attempt
// to find the Bar resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that Bar resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.
func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		glog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	glog.V(4).Infof("Processing object: %s", object.GetName())
        if object.GetNamespace() != "demo" {
		//we are interested in processing object in demo namespace
		glog.V(4).Infof("ignoring  object '%s' of namespace '%s'", object.GetName(), object.GetNamespace())
		return
	}

	if ownerRef := metav1.GetControllerOf(object); ownerRef == nil {
		// If this object is not owned by a Foo, we should not do anything more
		// with it.
		c.enqueueBar(object)
		return
	}
}

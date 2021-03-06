package controller

import (
	"fmt"
	"time"

	istio "istio.io/client-go/pkg/clientset/versioned"
	istionetworkinginformers "istio.io/client-go/pkg/informers/externalversions/networking/v1beta1"
	istionetworkinglisters "istio.io/client-go/pkg/listers/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	networkinginformers "k8s.io/client-go/informers/networking/v1beta1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	networkinglisters "k8s.io/client-go/listers/networking/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const controllerAgentName = "ingress-istio-controller"

// Controller responds to new resources and applies the necessary configuration
type Controller struct {
	kubeclientset  kubernetes.Interface
	istioclientset istio.Interface

	clusterDomain  string
	defaultGateway string
	ingressClass   string
	defaultWeight  int

	ingressesLister  networkinglisters.IngressLister
	ingressesSynched cache.InformerSynced

	servicesLister  corev1listers.ServiceLister
	servicesSynched cache.InformerSynced

	virtualServicesListers istionetworkinglisters.VirtualServiceLister
	virtualServicesSynched cache.InformerSynced

	workqueue workqueue.RateLimitingInterface
	recorder  record.EventRecorder
}

// NewController creates a new Controller object.
func NewController(
	kubeclientset kubernetes.Interface,
	istioclientset istio.Interface,
	clusterDomain string,
	defaultGateway string,
	ingressClass string,
	defaultWeight int,
	ingressesInformer networkinginformers.IngressInformer,
	servicesInformer corev1informers.ServiceInformer,
	virtualServicesInformer istionetworkinginformers.VirtualServiceInformer) *Controller {

	// Create event broadcaster
	klog.V(4).Info("creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:          kubeclientset,
		istioclientset:         istioclientset,
		clusterDomain:          clusterDomain,
		defaultGateway:         defaultGateway,
		ingressClass:           ingressClass,
		defaultWeight:          defaultWeight,
		ingressesLister:        ingressesInformer.Lister(),
		ingressesSynched:       ingressesInformer.Informer().HasSynced,
		servicesLister:         servicesInformer.Lister(),
		servicesSynched:        servicesInformer.Informer().HasSynced,
		virtualServicesListers: virtualServicesInformer.Lister(),
		virtualServicesSynched: virtualServicesInformer.Informer().HasSynced,
		workqueue:              workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "IngressIstio"),
		recorder:               recorder,
	}

	klog.Info("setting up event handlers")
	ingressesInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueIngress,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueIngress(new)
		},
	})

	return controller
}

// Run runs the controller.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.Info("starting controller")

	klog.Info("waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.ingressesSynched, c.servicesSynched, c.virtualServicesSynched); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("starting workers")
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("started workers")
	<-stopCh
	klog.Info("shutting down workers")

	return nil
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool

		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		if err := c.syncHandler(key); err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error synching %q: %v, requeing", key, err)
		}

		c.workqueue.Forget(obj)
		klog.Infof("successfully synched %q", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the ingress object
	ingress, err := c.ingressesLister.Ingresses(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("ingress %q in work queue no longer exists", key))
			return nil
		}

		return err
	}

	// Handle the VirtualService
	err = c.handleVirtualService(ingress)
	if err != nil {
		klog.Errorf("failed to handle virtual service: %v", err)
		return err
	}

	return nil
}

func (c *Controller) enqueueIngress(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}

	c.workqueue.Add(key)
}

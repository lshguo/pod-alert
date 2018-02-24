package main

import (
	"flag"
	"os/exec"
//	"reflect"
	//"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
//	"k8s.io/client-go/pkg/api/errors"
	"k8s.io/client-go/pkg/api/v1"
//	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	meta_v1 "k8s.io/client-go/pkg/apis/meta/v1"
	"k8s.io/client-go/pkg/runtime"
	utilruntime "k8s.io/client-go/pkg/util/runtime"
	//"k8s.io/client-go/pkg/util/wait"
	"k8s.io/client-go/pkg/util/workqueue"
	"k8s.io/client-go/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	// Only required to authenticate against GKE clusters
	//_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	//"strings"
	//"github.com/golang/glog"
	"github.com/golang/glog"
	"io/ioutil"
	"net/http"
	"strings"
)

func getClientsetOrDie(kubeconfig string) *kubernetes.Clientset {
	// Create the client config. Use kubeconfig if given, otherwise assume in-cluster.
	if kubeconfig != ""{
		glog.Info("build k8s client with configFile:", kubeconfig)
	}else{
		glog.Info("build k8s client with clusterConfig")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	glog.Info("got clientset of k8s")
	return clientset
}

//查询自己当前所在的namespace(project)
func getNamespaceInsidePod()string{
	//pod里存放namespace的文件位置
	namespaceFile := "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	cmd := exec.Command("cat", namespaceFile)
	if b, err := cmd.Output();err != nil{
		panic("cat namespace file failed:" + err.Error())
	}else{
		return string(b)
	}
}

func main() {
	kubeconfig := flag.String("kubeconfig", "", "Path to a kube config. Only required if out-of-cluster.")
	flag.Parse()
	controller := newAlertController(*kubeconfig)
	var stopCh <-chan struct{}
	controller.Run(1, stopCh)
}
/*
	监控pod的创建与删除，并在prometheus中生成或删除"容器CPU使用率"的告警规则
*/
type AlertRuleController struct {
	kubeClient *kubernetes.Clientset
	// A cache of pods
	podStore cache.StoreToPodLister
	// Watches changes to all pods
	podController *cache.Controller

	podsQueue      workqueue.RateLimitingInterface

	//
	alertRules       *AlertRules
}

func (c *AlertRuleController) enqueuePod(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	c.podsQueue.Add(key)

	//fmt.Println("enqueuePod: ", key)
}

func (c *AlertRuleController) podWorker() {
	glog.Infoln("podWorker start...")
	workFunc := func() bool {
		key, quit := c.podsQueue.Get()
		if quit {
			glog.Infoln("WTF! podQueue quit?!")
			return true
		}
		defer c.podsQueue.Done(key)

		obj, exists, err := c.podStore.Indexer.GetByKey(key.(string))
		//pod是被删除
		if !exists {
			glog.V(4).Info("pod deleted:", key.(string))
			c.alertRules.AddToList(getDeletedContainersFromPodKey(key.(string)))
			return false
		}
		if err != nil {
			glog.Errorf("cannot get pod: %v\n", key)
			return false
		}

		//pod是被创建
		glog.V(4).Infoln("pod created:",key.(string))
		pod := obj.(*v1.Pod)

		c.alertRules.AddToList(getCreatedConatinersFromPodObj(pod))
		return false
	}
	for {
		if quit := workFunc(); quit {
			glog.Infoln("pod worker shutting down")
			continue
		}
	}
}

func (c *AlertRuleController) ruleWorker(){
	glog.Infoln("ruleWorker start...")
	for {
		//等待信号
		select{
		case <- c.alertRules.Full:
			glog.Info("List is full")
		case <- c.alertRules.Timer.C:
			glog.Info("time to process list")
		}

		//生成新的rules.yml文件
		c.alertRules.UpdateAlertRulesFile()
		//创建新的configmap
		namespace := getNamespaceInsidePod()
		if "" == namespace{
			panic("can not get namespace where I'm running in")
		}
		cmap := new(v1.ConfigMap)
		cmap.Name = "alert-rules-config"
		cmap.Namespace = namespace
		b, err := ioutil.ReadFile(c.alertRules.FileName)
		if err != nil{
			glog.Error("read alert rules file failed")
			continue
		}
		cmap.Data = make(map[string]string)
		cmap.Data["instance_cpu_alert_rules.yml"] = string(b)

		if _, err := c.kubeClient.CoreV1().ConfigMaps(namespace).Get(cmap.Name, meta_v1.GetOptions{});err != nil{
			_, err = c.kubeClient.CoreV1().ConfigMaps(namespace).Create(cmap)
		}else {
			_, err = c.kubeClient.CoreV1().ConfigMaps(namespace).Update(cmap)
		}

		if err != nil{
			glog.Error("create configmap alert-rules-config failed: ", err.Error())
			continue
		}

		//通知prometheus对rules做reload
		resp, err := http.Post(c.alertRules.PrometheusUrl + "/-/reload",
			"text/plain",
			strings.NewReader("reload rules"))
		if err!= nil{
			glog.Error("inform Prometheus to reload rules failed: " + err.Error())
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
}


func newAlertController(kubeconfig string) *AlertRuleController {
	//fmt.Println("ENV")
	//fmt.Println("	CONTAINER_CPU_THRESHOLD: ",os.Getenv("CONTAINER_CPU_THRESHOLD"))
	//fmt.Println("	CONTAINER_CPU_DURATION: ",os.Getenv("CONTAINER_CPU_DURATION"))

	c := &AlertRuleController{
		kubeClient: getClientsetOrDie(kubeconfig),
		podsQueue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "pods"),
		alertRules: NewAlertRules(),
	}

	c.podStore.Indexer, c.podController = cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				return c.kubeClient.CoreV1().Pods(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				return c.kubeClient.CoreV1().Pods(api.NamespaceAll).Watch(options)
			},
		},
		&v1.Pod{},
		// resync is not needed
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueuePod,
			//UpdateFunc: c.updatePod,
			DeleteFunc: c.enqueuePod,
		},
		cache.Indexers{},
	)


	return c
}

// Run begins watching and syncing.
func (c *AlertRuleController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	glog.Infoln("Starting alertRule Manager")

	go c.podController.Run(stopCh)
	// wait for the controller to List. This help avoid churns during start up.
	if !cache.WaitForCacheSync(stopCh, c.podController.HasSynced) {
		glog.Error("can not sync")
		return
	}else{
		glog.Infoln("pod sync completed")
	}

	//处理pod的创建与删除事件
	go c.podWorker()
	//for i := 0; i < workers; i++ {
	//	go wait.Until(c.podWorker, time.Second, stopCh)
	//}

	//处理rule的创建与删除事件
	go c.ruleWorker()
	//for i := 0; i < workers; i++ {
	//	go wait.Until(c.ruleWorker, time.Second, stopCh)
	//}

	//启动计时器
	c.alertRules.RestartTimer()

	<-stopCh
	glog.Infoln("Shutting down alertRule Manager")
	c.podsQueue.ShutDown()
}

# pod-alert

部署于k8s或openshift平台

prometheus的告警配置通过configmap挂载进来，而pod-alert根据pod的创建与删除事件，相应的更新此configmap，然后通知prometheus reload配置

pod-alert为每个pod单独创建一条告警规则

pod-alert借助了client-go项目来监控pod的相关事件


目前只做了cpu使用超标的告警策略

package main

import (
	"os"
	"strings"
	"sync"
	"io/ioutil"
	"regexp"
	"github.com/golang/glog"
	"os/exec"
	"time"
	"k8s.io/client-go/pkg/api/v1"
	"strconv"
)


// 以下字符串中的空格非常重要，不可乱改
// 其中5m和0.7意思是5分钟内Cpu使用率超过70%
// alert_rule.yml由一个开头和若干条rule组成，形如：
const FILE_HEAD  string = "groups:\n" +
						  "- name: cpu-usage\n" +
						  "  rules:\n"
const RULE_START string = "  - alert: $NAMESPACE/$POD_NAME/$CONTAINER_NAME/CPU\n"
const RULE_EXPR  string = "    expr: sum(irate(container_cpu_usage_seconds_total{namespace=\"$NAMESPACE\",pod_name=\"$POD_NAME\",container_name=\"$CONTAINER_NAME\"}[5m])) / $CPU_QUATA > 0.8 \n"
const RULE_END   string = "    labels:\n" +
						  "      severity: webhook\n" +
						  "    annotations:\n" +
						  "      summary: $NAMESPACE/$POD_NAME/$CONTAINER_NAME/CPU\n"
// 三段字符串组成一条alert rule
const ALERT_RULE string = RULE_START + RULE_EXPR + RULE_END

//为了避免prometheus频繁reload告警规则，pods创建后其对应rule先在内存里缓存一下
//但数量不超过WAITING_NUMBER_MAX个, 时间不超过WAITING_SECOND_MAX秒
const WAITING_NUMBER_MAX int = 60
const WAITING_SECOND_MAX int = 300


type AlertRules struct{
	//*_alert_rules.yml文件
	FileName string
	//
	AlertDescription string
	//
	PrometheusUrl string

	//待添加rule的对象
	ContainerList []*ContainerInfo
	ListLock sync.Mutex

	//rule对象最多在List中缓存WAITING_NUMBER_MAX个
	Full chan bool
	//rule对象最多在List中缓存WAITING_SECOND_MAX秒
	Timer *time.Timer
}
func NewAlertRules()(*AlertRules){
	fileName := getAlertRulesFileName()
	f, err := os.Create(fileName)
	if err != nil{
		panic("create file " + fileName + " failed: " + err.Error())
	}

	_, err = f.WriteString(FILE_HEAD)
	if err != nil{
		panic("write file " + fileName + " failed: " + err.Error())
	}

	//f.Close()

	rules := new(AlertRules)
	rules.FileName = fileName

	rules.PrometheusUrl = getPrometheusUrl()

	rules.ContainerList = make([]*ContainerInfo, 0)
	//同步chan，用来通知ruleWorker处理List
	rules.Full = make(chan bool, 0)

	rules.RestartTimer()
	glog.Info("new AlertRules obj")

	return rules
}
func getAlertRulesFileName()(fileName string){
	dir := os.Getenv("RULES_DIR")
	if dir == ""{
		glog.Info("Env:RULES_DIR is nil, use '/mnt' as default")
		dir = "/mnt"
	}
	return dir + "/instance_cpu_alert_rules.yml"
}
func getPrometheusUrl()string{
	url := os.Getenv("PROMETHEUS_URL")
	if url == ""{
		glog.Info("Env:PROMETHEUS_URL is nil, use 'http://prometheus:9090' as default")
		url = "http://prometheus:9090"
	}
	return url
}
//从被创建或删除的Pod里获取到的原始信息
type ContainerInfo struct{
	//用来生成一条alert rule的原始信息
	Namespace string
	PodName string
	ContainerName string
	//Duration string
	CpuQuata string
	//操作标志：增加or删除. true：只有Namespace和PodName
	Deleted bool
}
func (c ContainerInfo)BuildAlertRuleStrings()(rule_start, rule_expr, rule_end string){
	stringRelaceFunc := func(src string)string{
		s := src
		s = strings.Replace(s,"$NAMESPACE", c.Namespace, -1)
		s = strings.Replace(s,"$POD_NAME", c.PodName, -1)
		s = strings.Replace(s,"$CONTAINER_NAME", c.ContainerName, -1)
		//s = strings.Replace(s,"$DURATION", c.Duration, -1)
		s = strings.Replace(s,"$CPU_QUATA", c.CpuQuata, -1)

		return s
	}

	return stringRelaceFunc(RULE_START),stringRelaceFunc(RULE_EXPR),stringRelaceFunc(RULE_END)
}
func (c ContainerInfo)deleteMeFromAlertRuleString(pStr *string){
	//rule由三个部分组成，每个部分里都有如下关键词
	keys := c.Namespace + ".*" + c.PodName + ".*" + c.ContainerName + ".*\n"

	//抹除rule_start
	re, err := regexp.Compile("  - alert:.*" + keys)
	if err != nil{
		glog.Error(keys + " regexp Error: " + err.Error())
		return
	}
	re.ReplaceAllString(*pStr, "")

	//抹除rule_expr
	re, err = regexp.Compile("    expr:.*" + keys)
	if err != nil{
		glog.Error(keys + " regexp Error: " + err.Error())
		return
	}
	re.ReplaceAllString(*pStr, "")

	//抹除rule_end
	re, err = regexp.Compile("    labels:.*\n" + ".*severity:.*\n"+ ".*annotations:.*\n" + "summary:.*" + keys)
	if err != nil{
		glog.Error(keys + " regexp Error: " + err.Error())
		return
	}
	re.ReplaceAllString(*pStr, "")

}

func getDeletedContainersFromPodKey(key string) []*ContainerInfo {
	return []*ContainerInfo {& ContainerInfo{
		Namespace: (strings.Split(key, "/"))[0],
		PodName: (strings.Split(key, "/"))[1],
		Deleted: true,
	}}
}

func getCreatedConatinersFromPodObj(pod *v1.Pod) []*ContainerInfo {
	containers := make([]*ContainerInfo,0)
	for _, c := range pod.Spec.Containers{
		//Cpu Request形如：100m或1   转换为字符串：0.1或1.0
		v := c.Resources.Requests.Cpu().MilliValue()
		if 0 == v{
			continue
		}
		vv := float64(v/1000)
		vvv := strconv.FormatFloat(vv,'f', -1, 64)
		//
		container := &ContainerInfo{
			Namespace:pod.Namespace,
			PodName:pod.Name,
			ContainerName:c.Name,
			//Duration:
			CpuQuata: vvv,
			Deleted:false,
		}
		containers = append(containers, container)
	}
	return containers
}
//处理containerList，更新alert_rules.yml
func (rules *AlertRules)UpdateAlertRulesFile(){
	//更新alert_rules.yml完毕，重启计时器
	defer rules.RestartTimer()

	//无pod变化，不需要更新alert_rules.yml
	listLen := len(rules.ContainerList)
	if listLen == 0{
		return
	}
	glog.Infof("%v obj in list to be flush into file", listLen)

	b, err := ioutil.ReadFile(rules.FileName)
	if err != nil{
		panic("read file " + rules.FileName + " failed: " + err.Error())
	}
	rulesString := string(b)

	//处理containerList, 更新rules string, 清空List
	rules.updateAlertRulesStringAndClearList(&rulesString)

	//通过一个临时文件来判断新生成的rules是否正确
	tmpFile := rules.FileName + ".tmp"
	f, err := os.Create(tmpFile)
	if err != nil{
		panic("create file " + tmpFile + " failed: " + err.Error())
	}
	f.WriteString(rulesString)
	f.Close()

	//promtool检测rule是否正确
	cmd := exec.Command("promtool", "check", "rules", rules.FileName)
	if _, err := cmd.Output();err != nil{
		panic("check rules in " + rules.FileName + " failed: " + err.Error())
	}
	//写入alert_rules.yml文件
	if err := os.Remove(rules.FileName);err != nil{
		panic("remove file " + rules.FileName + " failed: " + err.Error())
	}
	if err := os.Rename(tmpFile, rules.FileName);err != nil{
		panic("rename file " + tmpFile + "to " + rules.FileName + " failed: " + err.Error())
	}
}

func (rules *AlertRules)updateAlertRulesStringAndClearList(pStr *string){
	rules.ListLock.Lock()
	defer rules.ListLock.Unlock()

	for _, c := range rules.ContainerList{

		if nil == c{
			glog.Error("There is nil container in List")
			continue
		}
		deleted := (*c).Deleted
		//默认记录已经存在
		exist := true

		//如果rule已存在，则先抹除该记录(如果实际上不存在，则抹除无效，无负面影响)
		if exist{
			(*c).deleteMeFromAlertRuleString(pStr)
		}
		//如果container不是被删除(可能是创建或更新)，则追加新记录
		if !deleted{
			rule_start, rule_expr, rule_end := (*c).BuildAlertRuleStrings()
			*pStr = *pStr + rule_start + rule_expr + rule_end
			glog.V(4).Infoln("new rule expr: " + rule_expr)
		}

	}

	//list处理完毕，清空list
	rules.ContainerList = rules.ContainerList[0:0]
}

func (rules *AlertRules)AddToList(containers []*ContainerInfo){
	rules.ListLock.Lock()

	for _, c := range containers{
		if nil == c{
			glog.Error("try to add nil container to List")
			continue
		}
		rules.ContainerList = append(rules.ContainerList, c)
	}
	//没有用defer语句来释放锁，原因是：如果ListLock跟Full有重叠的话容易引起死锁
	//用锁保护的临界区范围一定要尽可能小
	rules.ListLock.Unlock()

	if len(rules.ContainerList) >= WAITING_NUMBER_MAX{
		glog.Infoln("List is Full: ", len(rules.ContainerList))
		rules.Full <- true
	}
}

//初始化计时器，并开始计时
func (rules *AlertRules)RestartTimer(){
	if nil == rules.Timer{
		rules.Timer = time.NewTimer(time.Duration(WAITING_SECOND_MAX) * time.Second)
	}else{
		rules.Timer.Reset(time.Duration(WAITING_SECOND_MAX) * time.Second)
	}

}

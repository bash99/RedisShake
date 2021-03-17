// Copyright 2019 Aliyun Cloud.
// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"reflect"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/alibaba/RedisShake/pkg/libs/log"
	"github.com/alibaba/RedisShake/redis-shake"
	"github.com/alibaba/RedisShake/redis-shake/base"
	"github.com/alibaba/RedisShake/redis-shake/common"
	"github.com/alibaba/RedisShake/redis-shake/configure"
	"github.com/alibaba/RedisShake/redis-shake/metric"
	"github.com/alibaba/RedisShake/redis-shake/restful"
	"github.com/gugemichael/nimo4go"
)

type Exit struct{ Code int }

const (
	defaultHttpPort    = 9320
	defaultSystemPort  = 9310
	defaultSenderSize  = 65535
	defaultSenderCount = 1024
)

func main() {
	var err error
	defer handleExit()
	defer utils.Goodbye()

	// argument options
	configuration := flag.String("conf", "", "configuration path")
	tp := flag.String("type", "", "run type: decode, restore, dump, sync, rump")
	version := flag.Bool("version", false, "show version")
	flag.Parse()

	if *version {
		fmt.Println(utils.Version)
		return
	}

	if *configuration == "" || *tp == "" {
		if !*version {
			fmt.Println("Please show me the '-conf' and '-type'")
		}
		fmt.Println(utils.Version)
		flag.PrintDefaults()
		return
	}

	conf.Options.Version = utils.Version
	conf.Options.Type = *tp

	var file *os.File
	if file, err = os.Open(*configuration); err != nil {
		crash(fmt.Sprintf("Configure file open failed. %v", err), -1)
	}

	// read fcv and do comparison
	if _, err := utils.CheckFcv(*configuration, utils.FcvConfiguration.FeatureCompatibleVersion); err != nil {
		crash(err.Error(), -5)
	}

	configure := nimo.NewConfigLoader(file)
	configure.SetDateFormat(utils.GolangSecurityTime)
	if err := configure.Load(&conf.Options); err != nil {
		crash(fmt.Sprintf("Configure file %s parse failed. %v", *configuration, err), -2)
	}

	// verify parameters
	if err = SanitizeOptions(*tp); err != nil {
		crash(fmt.Sprintf("Conf.Options check failed: %s", err.Error()), -4)
	}

	initSignal()
	initFreeOS()
	nimo.Profiling(int(conf.Options.SystemProfile))
	utils.Welcome()
	utils.StartTime = fmt.Sprintf("%v", time.Now().Format(utils.GolangSecurityTime))

	if err = utils.WritePidById(conf.Options.Id, conf.Options.PidPath); err != nil {
		crash(fmt.Sprintf("write pid failed. %v", err), -5)
	}

	// create runner
	var runner base.Runner
	switch *tp {
	case conf.TypeDecode:
		runner = new(run.CmdDecode)
	case conf.TypeRestore:
		runner = new(run.CmdRestore)
	case conf.TypeDump:
		runner = new(run.CmdDump)
	case conf.TypeSync:
		runner = new(run.CmdSync)
	case conf.TypeRump:
		runner = new(run.CmdRump)
	}

	// create metric
	metric.CreateMetric(runner)
	go startHttpServer()

	// print configuration
	if opts, err := json.Marshal(conf.GetSafeOptions()); err != nil {
		crash(fmt.Sprintf("marshal configuration failed[%v]", err), -6)
	} else {
		log.Infof("redis-shake configuration: %s", string(opts))
	}

	// run
	runner.Main()

	log.Infof("execute runner[%v] finished!", reflect.TypeOf(runner))
}

func initSignal() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Info("receive signal: ", sig)

		if utils.LogRotater != nil {
			utils.LogRotater.Rotate()
		}

		os.Exit(0)
	}()
}

func initFreeOS() {
	go func() {
		for {
			debug.FreeOSMemory()
			time.Sleep(5 * time.Second)
		}
	}()
}

func startHttpServer() {
	if conf.Options.HttpProfile == -1 {
		return
	}

	utils.InitHttpApi(conf.Options.HttpProfile)
	utils.HttpApi.RegisterAPI("/conf", nimo.HttpGet, func([]byte) interface{} {
		return conf.GetSafeOptions()
	})
	restful.RestAPI()

	if err := utils.HttpApi.Listen(); err != nil {
		crash(fmt.Sprintf("start http listen error[%v]", err), -4)
	}
}


// sanitize options
func sanitizeOptions(tp string) error {
	var err error
	if tp != TypeDecode && tp != TypeRestore && tp != TypeDump && tp != TypeSync && tp != TypeRump {
		return fmt.Errorf("unknown type[%v]", tp)
	}

	if conf.Options.Id == "" {
		return fmt.Errorf("id shoudn't be empty")
	}

	if conf.Options.NCpu < 0 || conf.Options.NCpu > 1024 {
		return fmt.Errorf("invalid ncpu[%v]", conf.Options.NCpu)
	} else if conf.Options.NCpu == 0 {
		runtime.GOMAXPROCS(runtime.NumCPU())
	} else {
		runtime.GOMAXPROCS(conf.Options.NCpu)
	}

	if conf.Options.Parallel == 0 { // not set
		conf.Options.Parallel = 64 // default is 64
	} else if conf.Options.Parallel > 1024 {
		return fmt.Errorf("parallel[%v] should in (0, 1024]", conf.Options.Parallel)
	} else {
		conf.Options.Parallel = int(math.Max(float64(conf.Options.Parallel), float64(conf.Options.NCpu)))
	}

	// 500 M
	if conf.Options.BigKeyThreshold > 500 * utils.MB {
		return fmt.Errorf("BigKeyThreshold[%v] should <= 500 MB", conf.Options.BigKeyThreshold)
	} else if conf.Options.BigKeyThreshold == 0 {
		conf.Options.BigKeyThreshold = 50 * utils.MB
	}

	if (tp == TypeRestore || tp == TypeSync) && conf.Options.TargetAddress == "" {
		return fmt.Errorf("target address shouldn't be empty when type in {restore, sync}")
	}
	if (tp == TypeDump || tp == TypeSync) && conf.Options.SourceAddress == "" {
		return fmt.Errorf("source address shouldn't be empty when type in {dump, sync}")
	}
	if tp == TypeRump && (conf.Options.SourceAddress == "" || conf.Options.TargetAddress == "") {
		return fmt.Errorf("source and target address shouldn't be empty when type in {rump}")
	}

	if conf.Options.SourcePasswordRaw != "" && conf.Options.SourcePasswordEncoding != "" {
		return fmt.Errorf("only one of source password_raw or password_encoding should be given")
	} else if conf.Options.SourcePasswordEncoding != "" {
		sourcePassword := "" // todo, inner version
		conf.Options.SourcePasswordRaw = string(sourcePassword)
	}

	if conf.Options.TargetPasswordRaw != "" && conf.Options.TargetPasswordEncoding != "" {
		return fmt.Errorf("only one of target password_raw or password_encoding should be given")
	} else if conf.Options.TargetPasswordEncoding != "" {
		targetPassword := "" // todo, inner version
		conf.Options.TargetPasswordRaw = string(targetPassword)
	}

	if conf.Options.LogFile != "" {
		//conf.Options.LogFile = fmt.Sprintf("%s.log", conf.Options.Id)

		utils.LogRotater = &logRotate.Logger{
			Filename:   conf.Options.LogFile,
			MaxSize:    100, //MB
			MaxBackups: 10,
			MaxAge:     0,
		}
		log.StdLog = log.New(utils.LogRotater, "")
	}
	// set log level
	var logDeepLevel log.LogLevel
	switch conf.Options.LogLevel {
	case utils.LogLevelNone:
		logDeepLevel = log.LEVEL_NONE
	case utils.LogLevelError:
		logDeepLevel = log.LEVEL_ERROR
	case utils.LogLevelWarn:
		logDeepLevel = log.LEVEL_WARN
	case "":
		fallthrough
	case utils.LogLevelInfo:
		logDeepLevel = log.LEVEL_INFO
	case utils.LogLevelAll:
		logDeepLevel = log.LEVEL_DEBUG
	default:
		return fmt.Errorf("invalid log level[%v]", conf.Options.LogLevel)
	}
	log.SetLevel(logDeepLevel)

	// heartbeat, 86400 = 1 day
	if conf.Options.HeartbeatInterval > 86400 {
		return fmt.Errorf("HeartbeatInterval[%v] should in [0, 86400]", conf.Options.HeartbeatInterval)
	} else if conf.Options.HeartbeatInterval == 0 {
		conf.Options.HeartbeatInterval = 10
	}

	if conf.Options.HeartbeatNetworkInterface == "" {
		conf.Options.HeartbeatIp = "127.0.0.1"
	} else {
		conf.Options.HeartbeatIp, _, err = utils.GetLocalIp([]string{conf.Options.HeartbeatNetworkInterface})
		if err != nil {
			return fmt.Errorf("get ip failed[%v]", err)
		}
	}

	if conf.Options.FakeTime != "" {
		switch conf.Options.FakeTime[0] {
		case '-', '+':
			if d, err := time.ParseDuration(strings.ToLower(conf.Options.FakeTime)); err != nil {
				return fmt.Errorf("parse fake_time failed[%v]", err)
			} else {
				conf.Options.ShiftTime = d
			}
		case '@':
			if n, err := strconv.ParseInt(conf.Options.FakeTime[1:], 10, 64); err != nil {
				return fmt.Errorf("parse fake_time failed[%v]", err)
			} else {
				conf.Options.ShiftTime = time.Duration(n*int64(time.Millisecond) - time.Now().UnixNano())
			}
		default:
			if t, err := time.Parse("2006-01-02 15:04:05", conf.Options.FakeTime); err != nil {
				return fmt.Errorf("parse fake_time failed[%v]", err)
			} else {
				conf.Options.ShiftTime = time.Duration(t.UnixNano() - time.Now().UnixNano())
			}
		}
	}

	if conf.Options.FilterDB != "" {
		if n, err := strconv.ParseInt(conf.Options.FilterDB, 10, 32); err != nil {
			return fmt.Errorf("parse FilterDB failed[%v]", err)
		} else {
			base.AcceptDB = func(db uint32) bool {
				return db == uint32(n)
			}
		}
	}

	if len(conf.Options.FilterSlot) > 0 {
		for i, val := range conf.Options.FilterSlot {
			if _, err := strconv.Atoi(val); err != nil {
				return fmt.Errorf("parse FilterSlot with index[%v] failed[%v]", i, err)
			}
		}
	}

	if conf.Options.TargetDBString == "" {
		conf.Options.TargetDB = -1
	} else if v, err := strconv.Atoi(conf.Options.TargetDBString); err != nil {
		return fmt.Errorf("parse target.db[%v] failed[%v]", conf.Options.TargetDBString, err)
	} else if v < 0 {
		conf.Options.TargetDB = -1
	} else {
		conf.Options.TargetDB = v
	}

	if conf.Options.HttpProfile < 0 || conf.Options.HttpProfile > 65535 {
		return fmt.Errorf("HttpProfile[%v] should in [0, 65535]", conf.Options.HttpProfile)
	} else if conf.Options.HttpProfile  == 0 {
		// set to default when not set
		conf.Options.HttpProfile = defaultHttpPort
	}

	if conf.Options.SystemProfile < 0 || conf.Options.SystemProfile > 65535 {
		return fmt.Errorf("SystemProfile[%v] should in [0, 65535]", conf.Options.SystemProfile)
	} else if conf.Options.SystemProfile  == 0 {
		// set to default when not set
		conf.Options.SystemProfile = defaultSystemPort
	}

	if conf.Options.SenderSize < 0 || conf.Options.SenderSize >= 1073741824 {
		return fmt.Errorf("SenderSize[%v] should in [0, 1073741824]", conf.Options.SenderSize)
	} else if conf.Options.SenderSize  == 0 {
		// set to default when not set
		conf.Options.SenderSize = defaultSenderSize
	}

	if conf.Options.SenderCount < 0 || conf.Options.SenderCount >= 100000 {
		return fmt.Errorf("SenderCount[%v] should in [0, 100000]", conf.Options.SenderCount)
	} else if conf.Options.SenderCount  == 0 {
		// set to default when not set
		conf.Options.SenderCount = defaultSenderCount
	}

	if conf.Options.SenderDelayChannelSize == 0 {
		conf.Options.SenderDelayChannelSize = 32
	}

	if tp == TypeRestore || tp == TypeSync {
		// get target redis version and set TargetReplace.
		if conf.Options.TargetRedisVersion, err = utils.GetRedisVersion(conf.Options.TargetAddress,
			conf.Options.TargetAuthType, conf.Options.TargetPasswordRaw); err != nil {
			return fmt.Errorf("get target redis version failed[%v]", err)
		} else {
			if strings.HasPrefix(conf.Options.TargetRedisVersion, "4.") ||
				strings.HasPrefix(conf.Options.TargetRedisVersion, "3.") {
				conf.Options.TargetReplace = true
			} else {
				conf.Options.TargetReplace = false
			}
		}
	}

	if tp == TypeRump {
		if conf.Options.ScanKeyNumber == 0 {
			conf.Options.ScanKeyNumber = 100
		}

		if conf.Options.ScanSpecialCloud != "" && conf.Options.ScanSpecialCloud != scanner.TencentCluster &&
				conf.Options.ScanSpecialCloud != scanner.AliyunCluster {
			return fmt.Errorf("special cloud type[%s] is not supported", conf.Options.ScanSpecialCloud)
		}

		if conf.Options.ScanSpecialCloud != "" && conf.Options.ScanKeyFile != "" {
			return fmt.Errorf("scan.special_cloud[%v] and scan.key_file[%v] cann't be given at the same time",
				conf.Options.ScanSpecialCloud, conf.Options.ScanKeyFile)
		}
	}

	return nil
}

func crash(msg string, errCode int) {
	fmt.Println(msg)
	panic(Exit{errCode})
}

func handleExit() {
	if e := recover(); e != nil {
		if exit, ok := e.(Exit); ok == true {
			os.Exit(exit.Code)
		}
		panic(e)
	}
}

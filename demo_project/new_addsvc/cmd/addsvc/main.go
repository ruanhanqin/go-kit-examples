package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/go-kit/kit/log"
	kitgrpc "github.com/go-kit/kit/transport/grpc"
	stdopentracing "github.com/opentracing/opentracing-go"
	"go-util/_go"
	"go-util/_util"
	"gokit_foundation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
	"net"
	"net/http"
	"new_addsvc/config"
	"new_addsvc/internal"
	"new_addsvc/pb/gen-go/addsvcpb"
	"new_addsvc/pkg/endpoint"
	"new_addsvc/pkg/service"
	"new_addsvc/pkg/transport"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func NewAddSrv(logger log.Logger) addsvcpb.AddServer {
	metricsObj := internal.NewMetrics()
	tracer := stdopentracing.GlobalTracer()

	// 依次创建 svc，endpoint，transport三层的对象，每一层都会在上一层基础上封装
	// 在svc和endpoint层以中间件的形式添加【指标上传、api日志】功能
	svc := service.New(logger, metricsObj.Ints, metricsObj.Chars)
	// 在endpoint层和transport层添加路径追踪功能
	endpoints := endpoint.New(svc, logger, metricsObj.Duration, tracer)
	addSrv := transport.NewGRPCServer(endpoints, tracer, logger)
	return addSrv
}

// for test
func init() {
	// 配置consul服务地址，必须是一个有效的consul地址
	os.Setenv("CONSUL_ADDR", "192.168.1.168:8500")
}

/*
new_addsvc服务依赖了一些外部中间件如下：
-	强依赖(若连不上则无法启动)
	-	consul
-	弱依赖(不需要连接或连不上也能启动)
	-	prometheus
*/

var (
	grpcSrv *grpc.Server
	httpSrv *http.Server
	logger  log.Logger

	// 创建一个所有后台运行的init任务共享的ctx，当进程退出时，所有后台任务都应该监听到ctx.Done()，然后graceful exit
	shareCtx, cancelCtx = context.WithCancel(context.Background())
)

func main() {
	// 服务运行的主机地址，必须能够被你的consul-server访问，否则consul的健康检查会失败
	srvHost := "127.0.0.1111"
	var grpcPort = flag.Int("grpc.port", 8080, "grpc listen address")
	var httpPort = flag.Int("http.port", 8081, "http listen address")

	grpcSrvAddr := fmt.Sprintf("%s:%d", srvHost, *grpcPort)
	httpSrvAddr := fmt.Sprintf("%s:%d", srvHost, *httpPort)

	flag.Parse()
	logger = gokit_foundation.NewKvLogger(nil)

	grpcSrv = grpc.NewServer(grpc.UnaryInterceptor(kitgrpc.Interceptor))
	httpSrv = &http.Server{}

	/*
		这里使用 NewSafeAsyncTask 完成程序的一系列启动任务
		在实际项目中可以参考其思路，自行实现
	*/

	stopAllTaskFunc := func(err error) {
		if err != nil {
			logger.Log("func", "stopAllTaskFunc", "err", err)
		}
		cancelCtx()
	}

	// 初始化一个SafeAsyncTask对象
	ak := _go.NewSafeAsyncTask(shareCtx, stopAllTaskFunc)

	/*
		进程退出：
		-	ak异常通知ListenSignal结束
		-	ListenSignal监听到信号，并通知ak清理，然后结束
		-	onclose都在ListenSignal执行
	*/
	ak.AddDo(func(ctx context.Context) error {
		sc := make(chan os.Signal)
		signal.Notify(sc,
			syscall.SIGINT,  // 键盘中断
			syscall.SIGTERM, // 软件终止
		)
		select {
		case s := <-sc:
			return fmt.Errorf("recv-signal:%v", s)
		case <-ctx.Done():
		}
		return nil
	})
	addTaskListenSignal(ak, stopAllTaskFunc)
	addTaskHttpSrv(ak, httpSrvAddr)
	addTaskGRPCSrv(ak, grpcSrvAddr)
	addTaskSvcRegister(ak, srvHost, *grpcPort) // 应当等所有内部服务准备就绪后再上线服务
	logger.Log("main", "started")
	ak.Run()
}

// 添加后台任务：监听退出信号（第一个添加）
func addTaskListenSignal(ak *_go.SafeAsyncTask, stop func(err error)) {
	onClose := func() {}
	onSignalTask := _util.ListenSignalTask(shareCtx, logger, onClose)
	ak.AddDo(onSignalTask).AddClean(nil)
}

// 添加后台任务：service discovery
func addTaskSvcRegister(ak *_go.SafeAsyncTask, srvHost string, grpcPort int) {
	svcRegTask := internal.SvcRegisterTask(shareCtx, logger, config.ServiceName, srvHost, grpcPort)
	ak.AddDo(svcRegTask).AddClean(func(err error) {
		if err == nil {
			// 注意，首先应该先从consul删除实例信息，再执行其他操作
			gokit_foundation.ConsulDeregister()
			logger.Log("SvcRegisterTask", "exited", "clean", nil)
		}
	})
}

func addTaskHttpSrv(ak *_go.SafeAsyncTask, httpSrvAddr string) {
	// http服务监听8080, 目前只提供metric接口给prometheus调用
	httpSrvTask := func(_ context.Context) error {
		logger.Log("NewSafeAsyncTask", "httpSrvTask", "httpSrvAddr", httpSrvAddr)

		httpLis, err := net.Listen("tcp", httpSrvAddr)
		_util.PanicIfErr(err, nil)

		// default use http.DefaultServeMux as handler
		err = httpSrv.Serve(httpLis)
		return err
	}
	ak.AddDo(httpSrvTask).AddClean(func(err error) {
		if err == nil {
			closeCtx, _ := context.WithTimeout(context.Background(), time.Second*2)
			err := httpSrv.Shutdown(closeCtx)
			logger.Log("httpSrvTask", "exited", "clean", err)
		}
	})
}

func addTaskGRPCSrv(ak *_go.SafeAsyncTask, grpcSrvAddr string) {
	// 添加后台任务：启动rpc-srv
	grpcSrvTask := func(_ context.Context) error {
		logger.Log("NewSafeAsyncTask", "grpcSrvTask", "grpcSrvAddr", grpcSrvAddr)

		grpcLis, err := net.Listen("tcp", grpcSrvAddr)
		_util.PanicIfErr(err, nil)

		addSrv := NewAddSrv(logger)
		addsvcpb.RegisterAddServer(grpcSrv, addSrv)

		// 这里注册了AddSrv以及healthSrv
		s := gokit_foundation.NewHealthCheckSrv()
		grpc_health_v1.RegisterHealthServer(grpcSrv, s)

		err = grpcSrv.Serve(grpcLis)
		return err
	}
	ak.AddDo(grpcSrvTask).AddClean(func(err error) {
		if err == nil {
			grpcSrv.GracefulStop()
			logger.Log("grpcSrvTask", "exited", "clean", nil)
		}
	})
}
